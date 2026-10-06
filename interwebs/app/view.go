package app

import (
	"context"
	"sync"
)

// View pins a site's cache. It does not track browser tabs or launch a browser.
// Close releases the lease, not the URL immediately; policy may then evict it.
type View struct {
	site     *Site
	url      string
	once     sync.Once
	closeErr error
}

func (v *View) URL() string { return v.url }
func (v *View) Close() error {
	v.once.Do(func() {
		p := v.site.profile
		p.mu.Lock()
		v.site.pins--
		p.notifyLocked(Change{SiteID: v.site.id})
		p.mu.Unlock()
		if p.alive() == nil {
			v.closeErr = v.site.update(context.Background(), func(e *entry) error { e.LastUsed = p.app.now(); return nil })
		}
		p.signal()
	})
	return v.closeErr
}

// View prepares a loopback URL and acquires a lease that must be closed by the adapter.
func (s *Site) View(ctx context.Context) (*Operation, error) {
	return s.submit(ctx, "view", false, func(work context.Context) (Result, error) {
		n, err := s.activate(work)
		if err != nil {
			return Result{}, err
		}
		// The core reuses the verified current version, or resolves a cold site.
		// Update checks belong to background policy, not the browser's critical path.
		url, err := n.View(work)
		if err != nil {
			return Result{}, err
		}
		if err := work.Err(); err != nil {
			return Result{}, err
		}
		p := s.profile
		if err := s.update(work, func(e *entry) error { e.LastUsed = p.app.now(); return nil }); err != nil {
			return Result{}, err
		}
		p.mu.Lock()
		if p.alive() != nil {
			p.mu.Unlock()
			return Result{}, ErrClosed
		}
		s.visited = true
		s.pins++
		p.mu.Unlock()
		// Seeding is reconciled by the background policy worker, never by a browser heartbeat.
		p.signal()
		return Result{View: &View{site: s, url: url}}, nil
	})
}
