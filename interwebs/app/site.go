package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	core "github.com/hlfshell/interweb/interwebs"
	"github.com/hlfshell/interweb/interwebs/author"
	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/site"
	"github.com/hlfshell/interweb/interwebs/storage"
)

type managedNode interface {
	Publish(context.Context) (core.Publication, error)
	View(context.Context) (string, error)
	Download(context.Context) error
	Refresh(context.Context) (core.RefreshResult, error)
	Seed(context.Context) error
	Stop(context.Context) error
	Close() error
	Status() core.Status
	Versions(context.Context) ([]core.VersionInfo, error)
	StorageUsage(context.Context) ([]storage.Usage, error)
	ReleaseVersion(context.Context, string) error
	RemoveVersion(context.Context, string) error
}

// Site is a profile-scoped managed content handle, not a raw core Node.
type Site struct {
	profile    *Profile
	id         string
	gate       chan struct{}
	nodeMu     sync.Mutex
	node       managedNode
	nodeCancel context.CancelFunc
	// The following fields are protected by profile.mu.
	pending, foreground, pins int
	visited                   bool
	background                *Operation
	lastError                 string
	nextAttempt, refreshed    time.Time
	retry                     time.Duration
	observed, published       [32]byte
	stable                    int
}

func newSite(p *Profile, id string) *Site {
	return &Site{profile: p, id: id, gate: make(chan struct{}, 1)}
}
func (s *Site) ID() string               { return s.id }
func (s *Site) nodeDir() string          { return filepath.Join(s.profile.dir, "nodes", s.id) }
func (s *Site) keyFile() string          { return filepath.Join(s.profile.dir, "keys", s.id+".key") }
func (s *Site) currentNode() managedNode { s.nodeMu.Lock(); defer s.nodeMu.Unlock(); return s.node }
func (s *Site) entry() (entry, error) {
	p := s.profile
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.state.Sites[s.id]
	if !ok {
		return entry{}, ErrNotFound
	}
	e.AuthorSites = append([]string(nil), e.AuthorSites...)
	return e, nil
}

func (s *Site) Status() SiteStatus {
	p := s.profile
	p.mu.Lock()
	e := p.state.Sites[s.id]
	status := SiteStatus{ID: s.id, Name: e.Name, Kind: e.Kind, Address: e.Address, Source: e.Source,
		Favorite: e.Favorite, Hosting: e.Hosting, Live: e.Live, Owned: e.Owned,
		LastUsed: e.LastUsed, Views: s.pins, Error: s.lastError, UnpublishedChanges: e.Dirty}
	p.mu.Unlock()
	if node := s.currentNode(); node != nil {
		status.Core = node.Status()
	}
	return status
}

// AddFolder registers a source and signer without publishing. Repeated folders return the existing handle.
func (p *Profile) AddFolder(ctx context.Context, folder string, opts ...SiteOption) (*Site, error) {
	config := siteConfig{}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil site option")
		}
		if err := opt(&config); err != nil {
			return nil, err
		}
	}
	path, err := p.source(folder)
	if err != nil {
		return nil, err
	}
	if _, err := site.New(ctx, path, site.WithFileTypeFiltering(config.filterTypes)); err != nil {
		return nil, err
	}
	return p.add(ctx, entry{Name: filepath.Base(path), Kind: "site", Source: path, Owned: true, FilterTypes: config.filterTypes})
}

// AddSite registers a remote address without resolving, downloading, or seeding it.
func (p *Profile) AddSite(ctx context.Context, magnet string) (*Site, error) {
	address, err := identity.ParseMagnet(magnet)
	if err != nil {
		return nil, err
	}
	return p.add(ctx, entry{Name: address.ID()[:12], Kind: "site", Address: address.Magnet()})
}

func (p *Profile) add(ctx context.Context, e entry) (*Site, error) {
	p.tx.Lock()
	defer p.tx.Unlock()
	if err := p.alive(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	for id, old := range p.state.Sites {
		if (e.Kind == "site" && e.Source != "" && e.Kind == old.Kind && e.Source == old.Source) || (e.Address != "" && e.Address == old.Address) {
			s := p.sites[id]
			p.mu.Unlock()
			return s, nil
		}
	}
	next := p.state.clone()
	p.mu.Unlock()
	id, err := newID()
	if err != nil {
		return nil, err
	}
	e.ID, e.LastUsed = id, p.app.now()
	s := newSite(p, id)
	if e.Owned {
		signer, err := identity.Create(ctx, s.keyFile())
		if err != nil {
			return nil, err
		}
		e.Address = signer.Identity().Magnet()
	}
	next.Sites[id] = e
	if err := p.persist(ctx, next); err != nil {
		// Preserve the signer if the atomic rename committed before a sync error.
		return nil, err
	}
	p.mu.Lock()
	p.state = next
	p.sites[id] = s
	p.notifyLocked(Change{SiteID: id})
	p.mu.Unlock()
	p.signal()
	return s, nil
}

func (s *Site) source(ctx context.Context, e entry) (content.Content, error) {
	p := s.profile
	if !e.Owned {
		if e.Kind == "author" {
			return author.FromMagnet(e.Address)
		}
		return site.FromMagnet(e.Address)
	}
	if e.Kind == "author" {
		p.mu.Lock()
		directory := author.Directory{Name: e.Name}
		for _, id := range e.AuthorSites {
			child, ok := p.state.Sites[id]
			if !ok {
				p.mu.Unlock()
				return nil, ErrNotFound
			}
			address, err := identity.ParseMagnet(child.Address)
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			directory.Sites = append(directory.Sites, author.Entry{Name: child.Name, Address: address})
		}
		p.mu.Unlock()
		var opts []author.Option
		if e.Source != "" {
			path, err := p.source(e.Source)
			if err != nil {
				return nil, err
			}
			page, err := site.New(ctx, path)
			if err != nil {
				return nil, err
			}
			opts = append(opts, author.WithPage(page))
		}
		return author.New(directory, opts...)
	}
	path, err := p.source(e.Source)
	if err != nil {
		return nil, err
	}
	return site.New(ctx, path, site.WithFileTypeFiltering(e.FilterTypes))
}

// activate is called only while the site's workflow gate is held.
func (s *Site) activate(ctx context.Context) (managedNode, error) {
	if n := s.currentNode(); n != nil {
		return n, nil
	}
	e, err := s.entry()
	if err != nil {
		return nil, err
	}
	source, err := s.source(ctx, e)
	if err != nil {
		return nil, err
	}
	p := s.profile
	if err := readStorageKey(filepath.Join(p.dir, "keys", "storage.key")); err != nil {
		return nil, err
	}
	opts := []core.Option{core.WithContent(source), core.WithDataDir(s.nodeDir()),
		core.WithKeyFile(filepath.Join(p.dir, "keys", "storage.key")), core.WithOffline(p.app.config.offline),
		core.WithMaxSiteBytes(p.app.config.maxSiteBytes), core.WithStorageLimit(p.app.config.nodeStorageLimit)}
	if e.Owned {
		signer, err := identity.Load(ctx, s.keyFile())
		if err != nil {
			return nil, err
		}
		opts = append(opts, core.WithSigner(signer))
	}
	lifetime, cancel := context.WithCancel(context.Background())
	n, err := p.app.makeNode(lifetime, opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	s.nodeMu.Lock()
	s.node, s.nodeCancel = n, cancel
	s.nodeMu.Unlock()
	return n, nil
}

func (s *Site) closeNode() error {
	s.nodeMu.Lock()
	n, cancel := s.node, s.nodeCancel
	s.node, s.nodeCancel = nil, nil
	s.nodeMu.Unlock()
	if n == nil {
		return nil
	}
	cancel()
	return n.Close()
}

func (s *Site) update(ctx context.Context, change func(*entry) error) error {
	return s.profile.change(ctx, func(state *profileState) error {
		e, ok := state.Sites[s.id]
		if !ok {
			return ErrNotFound
		}
		if err := change(&e); err != nil {
			return err
		}
		state.Sites[s.id] = e
		return nil
	})
}

// SetFavorite saves full-download and retention intent; completion is reported through status.
func (s *Site) SetFavorite(ctx context.Context, value bool) error {
	return s.setPolicy(ctx, func(e *entry) error { e.Favorite = value; return nil })
}

// SetHosting saves hosting intent independently of the favorite flag.
func (s *Site) SetHosting(ctx context.Context, value bool) error {
	return s.setPolicy(ctx, func(e *entry) error { e.Hosting = value; return nil })
}

// SetLive opts an owned folder into stable-source automatic publication.
func (s *Site) SetLive(ctx context.Context, value bool) error {
	return s.setPolicy(ctx, func(e *entry) error {
		if !e.Owned || e.Kind != "site" {
			return errors.New("live mode requires a local site")
		}
		e.Live = value
		return nil
	})
}

func (s *Site) setPolicy(ctx context.Context, change func(*entry) error) error {
	if err := s.update(ctx, change); err != nil {
		return err
	}
	p := s.profile
	p.mu.Lock()
	if s.background != nil {
		s.background.Cancel()
	}
	s.nextAttempt = time.Time{}
	p.mu.Unlock()
	p.signal()
	return nil
}

func (s *Site) publish(ctx context.Context) (Result, error) {
	e, err := s.entry()
	if err != nil {
		return Result{}, err
	}
	if !e.Owned {
		return Result{}, errors.New("publication requires an owned source")
	}
	// Revalidate source permissions even when its Node is already active.
	if _, err := s.source(ctx, e); err != nil {
		return Result{}, err
	}
	if e.Kind == "author" {
		p := s.profile
		p.mu.Lock()
		pinned := s.pins > 0
		p.mu.Unlock()
		if pinned {
			return Result{}, ErrInUse
		}
		if err := s.closeNode(); err != nil {
			return Result{}, err
		}
	}
	n, err := s.activate(ctx)
	if err != nil {
		return Result{}, err
	}
	publication, err := n.Publish(ctx)
	if err != nil {
		return Result{}, err
	}
	err = s.update(ctx, func(e *entry) error { e.Hosting = true; e.Dirty = false; return nil })
	return Result{Publication: &publication}, err
}

// Publish queues a source snapshot and enables hosting after successful publication.
func (s *Site) Publish(ctx context.Context) (*Operation, error) {
	return s.submit(ctx, "publish", false, s.publish)
}

// Download queues a whole-version download, without changing favorite or hosting intent.
func (s *Site) Download(ctx context.Context) (*Operation, error) {
	return s.submit(ctx, "download", false, func(work context.Context) (Result, error) {
		n, err := s.activate(work)
		if err != nil {
			return Result{}, err
		}
		return Result{}, n.Download(work)
	})
}

// Refresh queues a signed-address update check without changing retention intent.
func (s *Site) Refresh(ctx context.Context) (*Operation, error) {
	return s.submit(ctx, "refresh", false, func(work context.Context) (Result, error) {
		n, err := s.activate(work)
		if err != nil {
			return Result{}, err
		}
		result, err := n.Refresh(work)
		return Result{Refresh: &result}, err
	})
}

func (s *Site) lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.profile.done:
		return ErrClosed
	case s.gate <- struct{}{}:
		if err := s.profile.alive(); err != nil {
			<-s.gate
			return err
		}
		return nil
	}
}

// Versions lists known versions; it may activate the Node but does not download content.
func (s *Site) Versions(ctx context.Context) ([]core.VersionInfo, error) {
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.gate }()
	n, err := s.activate(ctx)
	if err != nil {
		return nil, err
	}
	return n.Versions(ctx)
}

// StorageUsage reports physical stores, including retained versions and staging.
func (s *Site) StorageUsage(ctx context.Context) ([]storage.Usage, error) {
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.gate }()
	n, err := s.activate(ctx)
	if err != nil {
		return nil, err
	}
	return n.StorageUsage(ctx)
}

// RemoveVersion releases and deletes a version. Active View leases prevent deletion.
func (s *Site) RemoveVersion(ctx context.Context, hash string) (*Operation, error) {
	return s.submit(ctx, "remove-version", false, func(work context.Context) (Result, error) {
		s.profile.mu.Lock()
		pinned := s.pins > 0
		s.profile.mu.Unlock()
		if pinned {
			return Result{}, ErrInUse
		}
		n, err := s.activate(work)
		if err != nil {
			return Result{}, err
		}
		if err := n.ReleaseVersion(work, hash); err != nil {
			return Result{}, err
		}
		return Result{}, n.RemoveVersion(work, hash)
	})
}

// ClearCache deletes encrypted payloads, preserving keys, source, intent, and version high-water state.
// Favorites/hosting may download the content again; active View leases prevent deletion.
func (s *Site) ClearCache(ctx context.Context) (*Operation, error) {
	return s.submit(ctx, "clear-cache", false, func(work context.Context) (Result, error) {
		return Result{}, s.clear(work)
	})
}
func (s *Site) clear(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.profile.mu.Lock()
	pinned := s.pins > 0
	s.profile.mu.Unlock()
	if pinned {
		return ErrInUse
	}
	if err := s.closeNode(); err != nil {
		return err
	}
	// Only the exact generated node's encrypted data is deleted; preserve high-water state and keys.
	if err := os.RemoveAll(filepath.Join(s.nodeDir(), "data")); err != nil {
		return fmt.Errorf("clear site cache: %w", err)
	}
	return nil
}
