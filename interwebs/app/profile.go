package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hlfshell/interweb/interwebs/internal/profile"
)

type entry struct {
	ID, Name, Kind, Address, Source       string
	Favorite, Hosting, Live, Owned, Dirty bool
	FilterTypes                           bool
	AuthorSites                           []string
	LastUsed                              time.Time
}

type profileState struct {
	Version  int
	ID, Name string
	Roots    []string
	Settings Settings
	Paused   bool
	Sites    map[string]entry
}

func (s profileState) clone() profileState {
	s.Roots = append([]string(nil), s.Roots...)
	sites := make(map[string]entry, len(s.Sites))
	for id, e := range s.Sites {
		e.AuthorSites = append([]string(nil), e.AuthorSites...)
		sites[id] = e
	}
	s.Sites = sites
	return s
}

// Profile owns isolated credentials, policy, sites, and operation workers.
type Profile struct {
	app         *App
	dir         string
	store       *profile.Profile
	mu          sync.Mutex
	tx          sync.Mutex
	state       profileState
	sites       map[string]*Site
	operations  map[string]*Operation
	completed   []string
	subscribers map[chan Change]bool
	workers     chan struct{}
	slots       chan struct{}
	done        chan struct{}
	wake        chan struct{}
	wg          sync.WaitGroup
	once        sync.Once
	closeErr    error
	lastError   string
	storedBytes int64
}

func newProfile(a *App, dir string, store *profile.Profile, state profileState) *Profile {
	p := &Profile{app: a, dir: dir, store: store, state: state, sites: make(map[string]*Site),
		operations: make(map[string]*Operation), subscribers: make(map[chan Change]bool),
		workers: make(chan struct{}, 2), slots: make(chan struct{}, 64),
		done: make(chan struct{}), wake: make(chan struct{}, 1)}
	for id := range state.Sites {
		p.sites[id] = newSite(p, id)
	}
	return p
}

func (a *App) loadProfile(ctx context.Context, id string) (*Profile, error) {
	dir := filepath.Join(a.root, "profiles", id)
	store, err := profile.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Profile, error) { return nil, errors.Join(err, store.Close()) }
	b, err := store.ReadState(ctx)
	if err != nil {
		return fail(err)
	}
	var state profileState
	if err := json.Unmarshal(b, &state); err != nil {
		return fail(err)
	}
	if state.Version != 1 || state.ID != id || state.Sites == nil {
		return fail(errors.New("invalid profile state"))
	}
	if err := state.Settings.validate(); err != nil {
		return fail(err)
	}
	for key, e := range state.Sites {
		if !validID(key) || key != e.ID || (e.Kind != "site" && e.Kind != "author") {
			return fail(errors.New("invalid site registry"))
		}
	}
	if err := readStorageKey(filepath.Join(dir, "keys", "storage.key")); err != nil {
		return fail(err)
	}
	p := newProfile(a, dir, store, state)
	if state.Settings.Cache != Timed {
		for _, s := range p.sites {
			if !protected(state.Sites[s.id]) {
				if err := os.RemoveAll(filepath.Join(s.nodeDir(), "data")); err != nil {
					return fail(err)
				}
			}
		}
	}
	return p, nil
}

func (p *Profile) ID() string { p.mu.Lock(); defer p.mu.Unlock(); return p.state.ID }
func (p *Profile) alive() error {
	select {
	case <-p.done:
		return ErrClosed
	default:
		return nil
	}
}
func (p *Profile) persist(ctx context.Context, state profileState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return p.store.WriteState(ctx, b)
}

// change serializes durable configuration transactions, never holding the status lock across I/O.
func (p *Profile) change(ctx context.Context, fn func(*profileState) error) error {
	p.tx.Lock()
	defer p.tx.Unlock()
	if err := p.alive(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	next := p.state.clone()
	p.mu.Unlock()
	if err := fn(&next); err != nil {
		return err
	}
	if err := p.persist(ctx, next); err != nil {
		return fmt.Errorf("save profile: %w", err)
	}
	p.mu.Lock()
	p.state = next
	p.notifyLocked(Change{})
	p.mu.Unlock()
	p.signal()
	return nil
}
func (p *Profile) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *Profile) notifyLocked(change Change) {
	for ch := range p.subscribers {
		select {
		case ch <- change:
		default:
		}
	}
}

// Subscribe supplies bounded invalidations, not an event log. Cancellation closes the channel.
func (p *Profile) Subscribe(ctx context.Context) <-chan Change {
	ch := make(chan Change, 1)
	p.mu.Lock()
	if p.alive() != nil || ctx.Err() != nil {
		close(ch)
		p.mu.Unlock()
		return ch
	}
	p.subscribers[ch] = true
	ch <- Change{}
	p.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-p.done:
		}
		p.mu.Lock()
		if p.subscribers[ch] {
			delete(p.subscribers, ch)
			close(ch)
		}
		p.mu.Unlock()
	}()
	return ch
}

func (p *Profile) Site(id string) (*Site, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.alive(); err != nil {
		return nil, err
	}
	s := p.sites[id]
	if s == nil {
		return nil, ErrNotFound
	}
	return s, nil
}
func (p *Profile) snapshot() (profileState, []*Site) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sites := make([]*Site, 0, len(p.sites))
	for _, s := range p.sites {
		sites = append(sites, s)
	}
	return p.state.clone(), sites
}

func (p *Profile) Status() ProfileStatus {
	state, sites := p.snapshot()
	p.mu.Lock()
	status := ProfileStatus{ID: state.ID, Name: state.Name, Settings: state.Settings, Paused: state.Paused,
		StoredBytes: p.storedBytes, StoragePressure: p.storedBytes > state.Settings.StorageTarget, Error: p.lastError}
	p.mu.Unlock()
	for _, s := range sites {
		status.Sites = append(status.Sites, s.Status())
	}
	sort.Slice(status.Sites, func(i, j int) bool { return status.Sites[i].ID < status.Sites[j].ID })
	return status
}

// SetSettings persists validated policy before scheduling reconciliation.
func (p *Profile) SetSettings(ctx context.Context, settings Settings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	return p.change(ctx, func(s *profileState) error { s.Settings = settings; return nil })
}

func (p *Profile) source(folder string) (string, error) {
	absolute, err := filepath.Abs(folder)
	if err != nil {
		return "", err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	stat, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	// Never import application state or an ancestor containing it.
	if !stat.IsDir() || within(p.app.root, absolute) || within(absolute, p.app.root) {
		return "", ErrSource
	}
	p.mu.Lock()
	roots := append([]string(nil), p.state.Roots...)
	p.mu.Unlock()
	for _, root := range roots {
		current, err := filepath.EvalSymlinks(root)
		if err == nil && current == root && within(root, absolute) {
			return absolute, nil
		}
	}
	return "", ErrSource
}

// Pause persists suspension, cancels publication/background work, and stops uploads.
// Explicit views, downloads, and refreshes remain available.
func (p *Profile) Pause(ctx context.Context) error {
	if err := p.change(ctx, func(s *profileState) error { s.Paused = true; return nil }); err != nil {
		return err
	}
	p.mu.Lock()
	var pending []*Operation
	for _, op := range p.operations {
		if op.background || op.kind == "publish" {
			pending = append(pending, op)
			op.Cancel()
		}
	}
	p.mu.Unlock()
	for _, op := range pending {
		select {
		case <-op.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, sites := p.snapshot()
	var errs []error
	for _, s := range sites {
		if n := s.currentNode(); n != nil {
			errs = append(errs, n.Stop(ctx))
		}
	}
	return errors.Join(errs...)
}

// Resume persists active policy and schedules background reconciliation.
func (p *Profile) Resume(ctx context.Context) error {
	return p.change(ctx, func(s *profileState) error { s.Paused = false; return nil })
}

// Close cancels operations, releases leases and Nodes, then closes persistence.
func (p *Profile) Close() error {
	p.once.Do(func() {
		p.tx.Lock()
		p.mu.Lock()
		close(p.done)
		for _, op := range p.operations {
			op.Cancel()
		}
		p.mu.Unlock()
		p.tx.Unlock()
		p.wg.Wait()
		state, sites := p.snapshot()
		var errs []error
		for _, s := range sites {
			s.gate <- struct{}{}
			errs = append(errs, s.closeNode())
			e := state.Sites[s.id]
			if !protected(e) && state.Settings.Cache != Timed {
				errs = append(errs, os.RemoveAll(filepath.Join(s.nodeDir(), "data")))
			}
			<-s.gate
		}
		p.closeErr = errors.Join(append(errs, p.store.Close())...)
	})
	return p.closeErr
}
