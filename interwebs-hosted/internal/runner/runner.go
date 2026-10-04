package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hlfshell/interweb/interwebs/app"
	"github.com/hlfshell/interweb/interwebs/content"
)

type runningSite struct {
	name       string
	site       *app.Site
	endpoint   *endpoint
	spec       SiteConfig
	operation  *app.Operation
	publish    bool
	restore    bool
	configured bool
	retryAt    time.Time
	failure    string
}

type running struct {
	app             *app.App
	profile         *app.Profile
	sites           []runningSite
	statusPath      string
	publicDiscovery bool
}

// Run applies config, serves sites, and emits JSON status until ctx is canceled.
// Shutdown closes listeners before releasing leases and the shared backend.
func Run(ctx context.Context, c Config, output io.Writer) (err error) {
	r, err := start(ctx, c)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, r.close())
		err = errors.Join(err, r.report("stopped", output))
	}()
	if err := r.update(ctx); err != nil {
		return err
	}
	if err := r.report("ready", output); err != nil {
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	report := time.NewTicker(5 * time.Second)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-report.C:
			if err := r.report("status", output); err != nil {
				return err
			}
		case <-tick.C:
			if err := r.update(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func start(ctx context.Context, c Config) (*running, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The runner owns shutdown ordering; startup work still uses the caller context.
	a, err := app.Open(context.Background(), c.DataDir, app.WithOffline(c.Offline), app.WithPausedStart(true),
		app.WithMaxSiteBytes(c.MaxSiteMiB<<20), app.WithNodeStorageLimit(c.NodeStorageMiB<<20))
	if err != nil {
		return nil, err
	}
	r := &running{app: a, sites: make([]runningSite, 0, len(c.Sites)), statusPath: filepath.Join(c.DataDir, "runner-status.json"), publicDiscovery: !c.Offline}
	fail := func(err error) (*running, error) { return nil, errors.Join(err, r.close()) }
	setup := ctx
	ids := a.Profiles()
	if len(ids) > 1 {
		return fail(errors.New("headless runner requires a dedicated data directory with one profile"))
	}
	var p *app.Profile
	if len(ids) == 0 {
		p, err = a.CreateProfile(setup, "Headless", app.WithSourceRoots(c.SourceRoots...))
	} else {
		p, err = a.Profile(ids[0])
		if err == nil {
			err = a.SetSourceRoots(setup, p.ID(), c.SourceRoots...)
		}
	}
	if err != nil {
		return fail(err)
	}
	r.profile = p
	// Config is authoritative for activity, but omitted sites and their keys/data
	// remain registered. Do not delete user data while reconciling configuration.
	for _, saved := range p.Status().Sites {
		s, err := p.Site(saved.ID)
		if err != nil {
			return fail(err)
		}
		if err := s.SetHosting(setup, false); err != nil {
			return fail(err)
		}
		if err := s.SetFavorite(setup, false); err != nil {
			return fail(err)
		}
		if saved.Owned && saved.Kind == "site" {
			if err := s.SetLive(setup, false); err != nil {
				return fail(err)
			}
		}
	}
	seen := make(map[string]bool)
	for _, spec := range c.Sites {
		var s *app.Site
		switch {
		case spec.Folder != "":
			s, err = p.AddFolder(setup, spec.Folder)
		case spec.Magnet != "":
			s, err = p.AddSite(setup, spec.Magnet)
		default:
			s, err = p.Site(spec.ID)
		}
		if err != nil {
			return fail(fmt.Errorf("site %q: %w", spec.Name, err))
		}
		if seen[s.ID()] {
			return fail(errors.New("multiple config entries resolve to the same site"))
		}
		seen[s.ID()] = true
		r.sites = append(r.sites, runningSite{name: spec.Name, site: s, spec: spec, publish: spec.Folder != "", restore: spec.Folder != "" && spec.Serve != nil})
	}

	// Bind listeners before content is available; slow content never holds startup.
	for i := range r.sites {
		item := &r.sites[i]
		if item.spec.Serve != nil {
			e, err := serve(*item.spec.Serve, nil, "")
			if err != nil {
				return fail(err)
			}
			item.endpoint = e
		}
	}
	if err := p.Resume(setup); err != nil {
		return fail(err)
	}

	return r, nil
}

func await(ctx context.Context, start func(context.Context) (*app.Operation, error)) (app.Result, error) {
	op, err := start(ctx)
	if err != nil {
		return app.Result{}, err
	}
	result, err := op.Wait(ctx)
	if ctx.Err() != nil {
		op.Cancel()
		err = errors.Join(err, ctx.Err())
		// Collect a raced successful View so its lease cannot be lost.
		finished, _ := op.Wait(context.Background())
		if finished.View != nil {
			err = errors.Join(err, finished.View.Close())
		}
		return app.Result{}, err
	}
	return result, err
}

func (r *running) update(ctx context.Context) error {
	for i := range r.sites {
		s := &r.sites[i]
		if s.endpoint != nil {
			select {
			case <-s.endpoint.done:
				return fmt.Errorf("site %q listener stopped: %v", s.name, s.endpoint.err)
			default:
			}
		}
		if time.Now().Before(s.retryAt) {
			continue
		}
		if err := s.advance(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.failure = err.Error()
			s.retryAt = time.Now().Add(5 * time.Second)
		}
	}
	return nil
}

// advance never waits on network work: the shared backend owns each operation.
func (s *runningSite) advance(ctx context.Context) error {
	if s.operation != nil {
		state := s.operation.Status()
		if state.State == app.Queued || state.State == app.Running {
			return nil
		}
		result, err := s.operation.Wait(ctx)
		s.operation = nil
		if s.restore {
			s.restore = false
			// A first publication has no stored version to serve yet.
			if errors.Is(err, content.ErrUnresolved) {
				err = nil
			}
		}
		if err != nil {
			return err
		}
		if state.Kind == "publish" {
			s.publish = false
		}
		if result.View != nil {
			if err := s.endpoint.replace(result.View, s.site.Status().Core.Current); err != nil {
				return err
			}
		}
		s.failure = ""
	}
	// Establish a stored-version view before source preparation can occupy the
	// site's workflow gate. Its lease keeps the old HTTP origin usable throughout
	// publication, including failures; successful publication replaces it below.
	if s.restore {
		op, err := s.site.View(ctx)
		s.operation = op
		return err
	}
	if s.publish {
		op, err := s.site.Publish(ctx)
		s.operation = op
		return err
	}
	if !s.configured {
		if err := s.site.SetFavorite(ctx, s.spec.Favorite); err != nil {
			return err
		}
		if err := s.site.SetHosting(ctx, s.spec.Hosting == nil || *s.spec.Hosting); err != nil {
			return err
		}
		if s.spec.Live {
			if err := s.site.SetLive(ctx, true); err != nil {
				return err
			}
		}
		s.configured = true
	}
	if e := s.endpoint; e != nil && (e.view == nil || e.hash != s.site.Status().Core.Current) {
		op, err := s.site.View(ctx)
		s.operation = op
		return err
	}
	s.failure = ""
	return nil
}

func (r *running) close() error {
	var errs []error
	for _, s := range r.sites {
		if s.operation != nil {
			s.operation.Cancel()
		}
	}
	for _, s := range r.sites {
		if s.endpoint != nil {
			errs = append(errs, s.endpoint.close())
		}
	}
	for _, s := range r.sites {
		if s.operation != nil {
			result, _ := s.operation.Wait(context.Background())
			if result.View != nil {
				errs = append(errs, result.View.Close())
			}
		}
	}
	return errors.Join(append(errs, r.app.Close())...)
}

type siteStatus struct {
	Name      string               `json:"name"`
	URL       string               `json:"url,omitempty"`
	Listen    string               `json:"listen,omitempty"`
	State     app.SiteStatus       `json:"state"`
	Phase     string               `json:"phase"`
	Error     string               `json:"error,omitempty"`
	Operation *app.OperationStatus `json:"operation,omitempty"`
}
type status struct {
	Event           string       `json:"event"`
	Updated         time.Time    `json:"updated"`
	PID             int          `json:"pid"`
	PublicDiscovery bool         `json:"public_discovery"`
	Profile         string       `json:"profile"`
	Sites           []siteStatus `json:"sites"`
}

func (r *running) status(event string) status {
	out := status{Event: event, Updated: time.Now().UTC(), PID: os.Getpid(), PublicDiscovery: r.publicDiscovery, Profile: r.profile.ID(), Sites: make([]siteStatus, 0, len(r.sites))}
	for _, s := range r.sites {
		item := siteStatus{Name: s.name, State: s.site.Status(), Phase: "ready", Error: s.failure}
		if s.publish || !s.configured || (s.endpoint != nil && s.endpoint.view == nil) {
			item.Phase = "preparing"
		}
		if s.publish && s.endpoint != nil && s.endpoint.view != nil {
			item.Phase = "updating"
		}
		if s.failure != "" {
			item.Phase = "retrying"
		}
		if s.operation != nil {
			op := s.operation.Status()
			item.Operation = &op
		}
		item.State.Core.Files = nil
		item.State.Core.History = nil
		if s.endpoint != nil {
			item.URL = s.endpoint.origin + "/"
			item.Listen = s.endpoint.address
		}
		out.Sites = append(out.Sites, item)
	}
	return out
}

func (r *running) report(event string, output io.Writer) error {
	b, err := json.Marshal(r.status(event))
	if err != nil {
		return err
	}
	if err := replaceFile(r.statusPath, append(b, '\n'), 0600); err != nil {
		return err
	}
	_, err = output.Write(append(b, '\n'))
	return err
}

// ReadStatus reads the last locally recorded snapshot. Check its timestamp:
// an unclean process exit can leave an old snapshot behind.
func ReadStatus(c Config, output io.Writer) error {
	f, err := os.Open(filepath.Join(c.DataDir, "runner-status.json"))
	if err != nil {
		return fmt.Errorf("read runner status (has it been run?): %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(f, 16<<20))
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	var snapshot status
	if err := json.Unmarshal(b, &snapshot); err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(snapshot)
}
