package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	core "github.com/hlfshell/interweb/interwebs"
	"github.com/hlfshell/interweb/interwebs/internal/profile"
)

type appState struct {
	Version  int
	Profiles []string
}

// App owns one exclusively locked data root. It does not authenticate remote callers.
type App struct {
	mu            sync.Mutex
	tx            sync.Mutex
	root          string
	store         *profile.Profile
	state         appState
	profiles      map[string]*Profile
	profileErrors map[string]string
	config        config
	done          chan struct{}
	once          sync.Once
	closeErr      error
	now           func() time.Time
	makeNode      func(context.Context, ...core.Option) (managedNode, error)
	ticks         func() (<-chan time.Time, func())
}

// Open restores profiles and starts policy workers. Canceling ctx closes the App.
func Open(ctx context.Context, root string, opts ...Option) (*App, error) {
	c := config{maxSiteBytes: 512 << 20, nodeStorageLimit: 2 << 30}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil application option")
		}
		if err := opt(&c); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == "" {
		return nil, errors.New("application data directory is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	store, err := profile.Open(ctx, absolute)
	if err != nil {
		return nil, fmt.Errorf("open application: %w", err)
	}
	a := &App{root: absolute, store: store, state: appState{Version: 1, Profiles: []string{}},
		profiles: make(map[string]*Profile), profileErrors: make(map[string]string), config: c, done: make(chan struct{}), now: time.Now,
		makeNode: func(ctx context.Context, opts ...core.Option) (managedNode, error) { return core.New(ctx, opts...) }}
	a.ticks = func() (<-chan time.Time, func()) {
		tick := time.NewTicker(2 * time.Second)
		return tick.C, tick.Stop
	}
	data, err := store.ReadState(ctx)
	if errors.Is(err, fs.ErrNotExist) {
		entries, listErr := os.ReadDir(filepath.Join(absolute, "profiles"))
		if listErr != nil && !errors.Is(listErr, fs.ErrNotExist) {
			err = listErr
		} else if len(entries) != 0 {
			err = errors.New("application registry missing for existing profiles")
		} else {
			err = a.save(ctx, a.state)
		}
	} else if err == nil {
		err = json.Unmarshal(data, &a.state)
		if err == nil && a.state.Version != 1 {
			err = errors.New("unsupported application state")
		}
	}
	if err != nil {
		return nil, errors.Join(err, a.Close())
	}
	seen := make(map[string]bool)
	for _, id := range a.state.Profiles {
		if !validID(id) || seen[id] {
			return nil, errors.Join(errors.New("invalid profile registry"), a.Close())
		}
		seen[id] = true
		p, err := a.loadProfile(ctx, id)
		if err != nil {
			a.profileErrors[id] = err.Error()
			continue
		}
		a.profiles[id] = p
	}
	for _, p := range a.profiles {
		p.start()
	}
	go func() {
		select {
		case <-ctx.Done():
			a.Close()
		case <-a.done:
		}
	}()
	return a, nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func validID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}
func (a *App) save(ctx context.Context, state appState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return a.store.WriteState(ctx, b)
}
func (a *App) alive() error {
	select {
	case <-a.done:
		return ErrClosed
	default:
		return nil
	}
}

func (a *App) Profile(id string) (*Profile, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.alive(); err != nil {
		return nil, err
	}
	p := a.profiles[id]
	if failure := a.profileErrors[id]; failure != "" {
		return nil, fmt.Errorf("profile unavailable: %s", failure)
	}
	if p == nil {
		return nil, ErrNotFound
	}
	return p, nil
}

// Profiles returns sorted profile IDs. It does not expose credentials.
func (a *App) Profiles() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := append([]string(nil), a.state.Profiles...)
	sort.Strings(ids)
	return ids
}

// CreateProfile generates fresh encryption credentials without sharing another profile's state.
func (a *App) CreateProfile(ctx context.Context, name string, opts ...ProfileOption) (*Profile, error) {
	a.tx.Lock()
	defer a.tx.Unlock()
	if err := a.alive(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" || len(name) > 256 {
		return nil, errors.New("invalid profile name")
	}
	c := profileConfig{settings: DefaultSettings()}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil profile option")
		}
		if err := opt(&c); err != nil {
			return nil, err
		}
	}
	roots, err := a.validateRoots(c.roots)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(a.root, "profiles", id)
	store, err := profile.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Profile, error) { return nil, errors.Join(err, store.Close(), os.RemoveAll(dir)) }
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fail(err)
	}
	err = profile.CreateKeyFile(filepath.Join(dir, "keys", "storage.key"), key)
	clear(key)
	if err != nil {
		return fail(err)
	}
	state := profileState{Version: 1, ID: id, Name: name, Roots: roots, Settings: c.settings, Sites: make(map[string]entry)}
	p := newProfile(a, dir, store, state)
	if err := p.persist(ctx, state); err != nil {
		return fail(err)
	}
	next := appState{Version: 1, Profiles: append(append([]string(nil), a.state.Profiles...), id)}
	if err := a.save(ctx, next); err != nil {
		// Rename may have committed before a directory-sync error. Preserve credentials.
		return nil, errors.Join(err, store.Close())
	}
	a.mu.Lock()
	a.state = next
	a.profiles[id] = p
	a.mu.Unlock()
	p.start()
	return p, nil
}

// SetSourceRoots is an operator action, not a permission that a tenant grants itself.
func (a *App) SetSourceRoots(ctx context.Context, id string, roots ...string) error {
	p, err := a.Profile(id)
	if err != nil {
		return err
	}
	checked, err := a.validateRoots(roots)
	if err != nil {
		return err
	}
	return p.change(ctx, func(state *profileState) error { state.Roots = checked; return nil })
}

func (a *App) validateRoots(roots []string) ([]string, error) {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		absolute, err = filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, err
		}
		stat, err := os.Stat(absolute)
		if err != nil {
			return nil, err
		}
		if !stat.IsDir() || within(a.root, absolute) {
			return nil, ErrSource
		}
		out = append(out, absolute)
	}
	return out, nil
}
func within(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func readStorageKey(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	err = errors.Join(err, f.Close())
	defer clear(key)
	if err != nil {
		return err
	}
	if len(key) != 32 {
		return errors.New("profile storage key must contain exactly 32 bytes")
	}
	return nil
}

// Close cancels work, releases views and Nodes, and preserves durable profile data.
func (a *App) Close() error {
	a.once.Do(func() {
		a.tx.Lock()
		close(a.done)
		a.mu.Lock()
		profiles := make([]*Profile, 0, len(a.profiles))
		for _, p := range a.profiles {
			profiles = append(profiles, p)
		}
		a.mu.Unlock()
		a.tx.Unlock()
		var errs []error
		for _, p := range profiles {
			errs = append(errs, p.Close())
		}
		a.closeErr = errors.Join(append(errs, a.store.Close())...)
	})
	return a.closeErr
}

// ProfileErrors reports isolated startup failures; unaffected profiles remain usable.
func (a *App) ProfileErrors() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.profileErrors))
	for id, err := range a.profileErrors {
		out[id] = err
	}
	return out
}
