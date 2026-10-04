package app

import (
	"context"
	"errors"

	core "github.com/hlfshell/interweb/interwebs"
	"github.com/hlfshell/interweb/interwebs/identity"
)

// Author manages an independently signed public directory, not an application user.
type Author struct{ site *Site }
type authorConfig struct{ page string }
type AuthorOption func(*authorConfig) error

// WithPage includes a local website; the source binding is fixed after creation.
func WithPage(folder string) AuthorOption {
	return func(c *authorConfig) error { c.page = folder; return nil }
}

func (p *Profile) CreateAuthor(ctx context.Context, name string, opts ...AuthorOption) (*Author, error) {
	if name == "" || len(name) > 256 {
		return nil, errors.New("invalid author name")
	}
	c := authorConfig{}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil author option")
		}
		if err := opt(&c); err != nil {
			return nil, err
		}
	}
	if c.page != "" {
		path, err := p.source(c.page)
		if err != nil {
			return nil, err
		}
		c.page = path
	}
	s, err := p.add(ctx, entry{Name: name, Kind: "author", Owned: true, Source: c.page, Dirty: true})
	if err != nil {
		return nil, err
	}
	return &Author{site: s}, nil
}

func (p *Profile) Author(id string) (*Author, error) {
	s, err := p.Site(id)
	if err != nil {
		return nil, err
	}
	e, err := s.entry()
	if err != nil {
		return nil, err
	}
	if e.Kind != "author" {
		return nil, ErrNotFound
	}
	return &Author{site: s}, nil
}
func (a *Author) ID() string                                      { return a.site.ID() }
func (a *Author) Status() SiteStatus                              { return a.site.Status() }
func (a *Author) Publish(ctx context.Context) (*Operation, error) { return a.site.Publish(ctx) }
func (a *Author) View(ctx context.Context) (*Operation, error)    { return a.site.View(ctx) }
func (a *Author) Versions(ctx context.Context) ([]core.VersionInfo, error) {
	return a.site.Versions(ctx)
}

// SetSites saves directory associations; only Publish changes the public directory.
func (a *Author) SetSites(ctx context.Context, ids ...string) error {
	if err := a.site.lock(ctx); err != nil {
		return err
	}
	defer func() { <-a.site.gate }()
	return a.site.profile.change(ctx, func(state *profileState) error {
		e, ok := state.Sites[a.site.id]
		if !ok {
			return ErrNotFound
		}
		seen := make(map[string]bool)
		if len(ids) > 1000 {
			return errors.New("author directory exceeds 1000 sites")
		}
		for _, id := range ids {
			site, ok := state.Sites[id]
			if !ok || site.Kind != "site" || seen[id] {
				return errors.New("invalid or duplicate site association")
			}
			address, err := identity.ParseMagnet(site.Address)
			if err != nil || address.Key == "" {
				return errors.New("author entries require signed site addresses")
			}
			seen[id] = true
		}
		e.AuthorSites = append([]string(nil), ids...)
		e.Dirty = true
		state.Sites[e.ID] = e
		return nil
	})
}

// RemoveSite unregisters content and deletes its encrypted cache, never its source or signing key.
// Referenced author entries must be explicitly removed first.
func (p *Profile) RemoveSite(ctx context.Context, id string) error {
	s, err := p.Site(id)
	if err != nil {
		return err
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	p.mu.Lock()
	busy := s.pending > 0 || s.pins > 0
	p.mu.Unlock()
	if busy {
		return ErrInUse
	}
	if err := p.change(ctx, func(state *profileState) error {
		for _, e := range state.Sites {
			for _, child := range e.AuthorSites {
				if child == id {
					return ErrInUse
				}
			}
		}
		// Keep registration intact if deleting its cache fails.
		if err := s.clear(ctx); err != nil {
			return err
		}
		delete(state.Sites, id)
		return nil
	}); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.sites, id)
	p.mu.Unlock()
	return nil
}
