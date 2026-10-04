package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	core "github.com/hlfshell/interweb/interwebs"
	"github.com/hlfshell/interweb/interwebs/storage"
)

type fakeNode struct {
	mu                                      sync.Mutex
	seeding                                 bool
	downloads, publishes, closes, refreshes int
	download                                func(context.Context) error
}

func (n *fakeNode) Publish(context.Context) (core.Publication, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.publishes++
	n.seeding = true
	return core.Publication{}, nil
}
func (n *fakeNode) View(context.Context) (string, error) { return "http://127.0.0.1:1234/", nil }
func (n *fakeNode) Download(ctx context.Context) error {
	n.mu.Lock()
	n.downloads++
	fn := n.download
	n.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}
func (n *fakeNode) Refresh(context.Context) (core.RefreshResult, error) {
	n.mu.Lock()
	n.refreshes++
	n.mu.Unlock()
	return core.RefreshResult{Usable: true}, nil
}
func (n *fakeNode) Seed(context.Context) error {
	n.mu.Lock()
	n.seeding = true
	n.mu.Unlock()
	return nil
}
func (n *fakeNode) Stop(context.Context) error {
	n.mu.Lock()
	n.seeding = false
	n.mu.Unlock()
	return nil
}
func (n *fakeNode) Close() error { n.mu.Lock(); n.closes++; n.mu.Unlock(); return nil }
func (n *fakeNode) Status() core.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return core.Status{Seeding: n.seeding}
}
func (n *fakeNode) Versions(context.Context) ([]core.VersionInfo, error)  { return nil, nil }
func (n *fakeNode) StorageUsage(context.Context) ([]storage.Usage, error) { return nil, nil }
func (n *fakeNode) ReleaseVersion(context.Context, string) error          { return nil }
func (n *fakeNode) RemoveVersion(context.Context, string) error           { return nil }

const fixedAddress = "magnet:?xt=urn:btih:1111111111111111111111111111111111111111"

func fakeProfile(t *testing.T, n *fakeNode) (*App, *Profile, *Site) {
	t.Helper()
	a := testApp(t)
	a.makeNode = func(context.Context, ...core.Option) (managedNode, error) { return n, nil }
	p, err := a.CreateProfile(t.Context(), "Test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.AddSite(t.Context(), fixedAddress)
	if err != nil {
		t.Fatal(err)
	}
	return a, p, s
}

func TestViewIsSelectiveAndFavoriteDownloadsThenPauseCancels(t *testing.T) {
	started := make(chan struct{}, 1)
	n := &fakeNode{download: func(ctx context.Context) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	_, p, s := fakeProfile(t, n)
	op, err := s.View(t.Context())
	view := wait(t, op, err).View
	n.mu.Lock()
	downloads := n.downloads
	n.mu.Unlock()
	if downloads != 0 {
		t.Fatal("view requested a full download")
	}
	if err := s.SetFavorite(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("favorite did not download")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := p.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if n.Status().Seeding {
		t.Fatal("pause left uploads enabled")
	}
	op, err = s.View(t.Context())
	second := wait(t, op, err).View
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(t.Context()); !errors.Is(err, ErrPaused) {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledWaitDoesNotCancelWorkAndExplicitCancelDoes(t *testing.T) {
	n := &fakeNode{download: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	_, _, s := fakeProfile(t, n)
	op, err := s.Download(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := op.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-op.done:
		t.Fatal("canceled wait canceled work")
	default:
	}
	op.Cancel()
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if _, err := op.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRetentionRespectsLeasesAndProtectedData(t *testing.T) {
	_, p, s := fakeProfile(t, &fakeNode{})
	op, err := s.View(t.Context())
	view := wait(t, op, err).View
	path := filepath.Join(s.nodeDir(), "data")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "payload"), []byte("cached"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := DefaultSettings()
	settings.StorageTarget = 1
	if err := p.SetSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if err := p.retain(time.Now().Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("evicted leased data", err)
	}
	if err := s.SetFavorite(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.retain(time.Now().Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("evicted favorite data", err)
	}
	if !p.Status().StoragePressure {
		t.Fatal("protected pressure not reported")
	}
	if err := s.SetFavorite(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return s.pending == 0 })
	// Policy skips a busy site rather than blocking; retry after a concurrent pass.
	eventually(t, func() bool {
		if err := p.retain(time.Now().Add(48 * time.Hour)); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(path)
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestLiveNeedsStableObservationsAndExplicitOptIn(t *testing.T) {
	n := &fakeNode{}
	a := testApp(t)
	var clockMu sync.Mutex
	now := time.Now()
	a.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func() { clockMu.Lock(); now = now.Add(2 * time.Second); clockMu.Unlock() }
	a.ticks = func() (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	a.makeNode = func(context.Context, ...core.Option) (managedNode, error) { return n, nil }
	source := folder(t, "live")
	p, err := a.CreateProfile(t.Context(), "Live", WithSourceRoots(source))
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.AddFolder(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if s.currentNode() != nil {
		t.Fatal("folder was automatically published")
	}
	if err := s.SetLive(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return s.stable == 1 && s.pending == 0 })
	n.mu.Lock()
	if n.publishes != 0 {
		t.Fatal("published before stable observations")
	}
	n.mu.Unlock()
	advance()
	p.signal()
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.publishes == 1 })
	if err := p.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "index.html"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	advance()
	if err := p.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return s.stable == 1 && s.pending == 0 })
	advance()
	p.signal()
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.publishes == 2 })
}
