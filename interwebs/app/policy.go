package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hlfshell/interweb/interwebs/content"
)

func protected(e entry) bool { return e.Owned || e.Favorite || e.Hosting }

func (p *Profile) start() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticks, stop := p.app.ticks()
		defer stop()
		p.signal()
		for {
			select {
			case <-p.done:
				return
			case <-p.wake:
			case <-ticks:
			}
			p.reconcile(p.app.now())
		}
	}()
}

func (p *Profile) reconcile(now time.Time) {
	if err := p.retain(now); err != nil {
		p.recordError(err)
	}
	state, sites := p.snapshot()
	for _, s := range sites {
		e, ok := state.Sites[s.id]
		if !ok {
			continue
		}
		p.mu.Lock()
		visited, idle := s.visited, s.pending == 0 && s.foreground == 0
		due := !now.Before(s.nextAttempt)
		p.mu.Unlock()
		if state.Paused {
			if n := s.currentNode(); n != nil {
				if err := n.Stop(context.Background()); err != nil {
					p.recordError(err)
				}
			}
			continue
		}
		if !idle || !due {
			continue
		}
		if e.Hosting || e.Favorite || e.Live || visited {
			_, err := s.submit(context.Background(), "maintain", true, func(ctx context.Context) (Result, error) {
				err := s.maintain(ctx, now)
				p.mu.Lock()
				if err != nil {
					if s.retry == 0 {
						s.retry = time.Second
					} else {
						s.retry = min(2*s.retry, time.Minute)
					}
					s.nextAttempt = now.Add(s.retry)
				} else {
					s.retry = 0
					s.nextAttempt = now.Add(2 * time.Second)
				}
				p.mu.Unlock()
				return Result{}, err
			})
			if err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrPaused) && !errors.Is(err, ErrClosed) {
				p.recordError(err)
			}
		} else if n := s.currentNode(); n != nil {
			if err := n.Stop(context.Background()); err != nil {
				p.recordError(err)
			}
		}
	}
}

func (p *Profile) recordError(err error) {
	p.mu.Lock()
	p.lastError = err.Error()
	p.notifyLocked(Change{})
	p.mu.Unlock()
}

func (s *Site) maintain(ctx context.Context, now time.Time) error {
	e, err := s.entry()
	if err != nil {
		return err
	}
	p := s.profile
	p.mu.Lock()
	settings, paused, visited := p.state.Settings, p.state.Paused, s.visited
	p.mu.Unlock()
	if paused {
		return ErrPaused
	}
	if e.Live {
		c, err := s.source(ctx, e)
		if err != nil {
			return err
		}
		source, ok := c.(content.Source)
		if !ok {
			return errors.New("live source is not publishable")
		}
		fingerprint, err := source.Fingerprint(ctx)
		if err != nil {
			return err
		}
		p.mu.Lock()
		if fingerprint == s.observed {
			s.stable++
		} else {
			s.observed = fingerprint
			s.stable = 1
		}
		publish := s.stable >= 2 && s.published != fingerprint
		p.mu.Unlock()
		if publish {
			if _, err := s.publish(ctx); err != nil {
				return err
			}
			p.mu.Lock()
			s.published = fingerprint
			p.mu.Unlock()
			e, _ = s.entry()
		}
	}
	if !e.Hosting && !e.Favorite && !visited {
		return nil
	}
	n, err := s.activate(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	refresh := !e.Owned && settings.RefreshInterval > 0 && now.Sub(s.refreshed) >= settings.RefreshInterval
	p.mu.Unlock()
	// Restore cached service before checking for a replacement. Full-download
	// policy finishes the current version first; foreground views can preempt it.
	if err := n.Seed(ctx); err != nil {
		return err
	}
	if !e.Owned && (e.Favorite || e.Hosting) {
		if err := n.Download(ctx); err != nil {
			return err
		}
	}
	if refresh {
		if _, err := n.Refresh(ctx); err != nil {
			return err
		}
		p.mu.Lock()
		s.refreshed = now
		p.mu.Unlock()
	}
	return nil
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in managed encrypted storage")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (p *Profile) retain(now time.Time) error {
	state, sites := p.snapshot()
	sizes := make(map[string]int64)
	var total int64
	for _, s := range sites {
		size, err := directorySize(filepath.Join(s.nodeDir(), "data"))
		if err != nil {
			return err
		}
		sizes[s.id] = size
		total += size
	}
	sort.Slice(sites, func(i, j int) bool {
		left, right := state.Sites[sites[i].id].LastUsed, state.Sites[sites[j].id].LastUsed
		if left.Equal(right) {
			return sites[i].id < sites[j].id
		}
		return left.Before(right)
	})
	for _, s := range sites {
		e := state.Sites[s.id]
		expired := state.Settings.Cache == NoRetention ||
			(state.Settings.Cache == Timed && now.Sub(e.LastUsed) >= state.Settings.Retention)
		if protected(e) || (!expired && total <= state.Settings.StorageTarget) {
			continue
		}
		select {
		case s.gate <- struct{}{}:
		default:
			continue
		}
		// Serialize deletion with durable policy changes (especially favoriting).
		p.tx.Lock()
		p.mu.Lock()
		// Recheck intent after acquiring the workflow gate.
		current, exists := p.state.Sites[s.id]
		settings := p.state.Settings
		expired = settings.Cache == NoRetention ||
			(settings.Cache == Timed && now.Sub(current.LastUsed) >= settings.Retention)
		eligible := p.alive() == nil && exists && !protected(current) && s.pins == 0 && s.pending == 0 &&
			(expired || total > settings.StorageTarget)
		if eligible {
			s.visited = false
		}
		p.mu.Unlock()
		if eligible {
			err := s.clear(context.Background())
			if err != nil {
				p.tx.Unlock()
				<-s.gate
				return err
			}
			total -= sizes[s.id]
		}
		p.tx.Unlock()
		<-s.gate
	}
	p.mu.Lock()
	p.storedBytes = total
	p.notifyLocked(Change{})
	p.mu.Unlock()
	return nil
}
