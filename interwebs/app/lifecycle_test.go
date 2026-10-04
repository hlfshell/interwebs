package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQueueBoundAndShutdownCancelAllWork(t *testing.T) {
	_, p, s := fakeProfile(t, &fakeNode{download: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	var operations []*Operation
	for range 64 {
		op, err := s.Download(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		operations = append(operations, op)
	}
	if _, err := s.Download(t.Context()); !errors.Is(err, ErrBusy) {
		t.Fatalf("queue limit: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		if _, err := op.Wait(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown operation: %v", err)
		}
	}
	if _, err := s.Download(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestForegroundViewPreemptsBackgroundDownload(t *testing.T) {
	started := make(chan struct{}, 1)
	_, p, s := fakeProfile(t, &fakeNode{download: func(ctx context.Context) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	if err := s.SetFavorite(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("background download did not start")
	}
	op, err := s.View(t.Context())
	view := wait(t, op, err).View
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionsAreBoundedAndSnapshotsAreCopies(t *testing.T) {
	_, p, s := fakeProfile(t, &fakeNode{})
	ctx, cancel := context.WithCancel(t.Context())
	changes := p.Subscribe(ctx)
	for range 10 {
		if err := s.SetFavorite(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	}
	status := p.Status()
	status.Sites[0].Name = "mutated"
	if p.Status().Sites[0].Name == "mutated" {
		t.Fatal("snapshot aliases internal state")
	}
	cancel()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("subscription did not close")
		}
	}
}

func TestSessionCleanupPreservesFavorites(t *testing.T) {
	_, p, visit := fakeProfile(t, &fakeNode{})
	if err := p.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
	favorite, err := p.AddSite(t.Context(), "magnet:?xt=urn:btih:2222222222222222222222222222222222222222")
	if err != nil {
		t.Fatal(err)
	}
	if err := favorite.SetFavorite(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	settings := DefaultSettings()
	settings.Cache = Session
	if err := p.SetSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Site{visit, favorite} {
		path := filepath.Join(s.nodeDir(), "data")
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "cached"), []byte("ciphertext"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(visit.nodeDir(), "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session cache retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(favorite.nodeDir(), "data", "cached")); err != nil {
		t.Fatalf("favorite removed: %v", err)
	}
}

func TestSourceRevocationPreventsRepublishing(t *testing.T) {
	a := testApp(t)
	source := folder(t, "private")
	p, err := a.CreateProfile(t.Context(), "Owner", WithSourceRoots(source))
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.AddFolder(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.Publish(t.Context())
	wait(t, op, err)
	if err := a.SetSourceRoots(t.Context(), p.ID()); err != nil {
		t.Fatal(err)
	}
	op, err = s.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(t.Context()); !errors.Is(err, ErrSource) {
		t.Fatalf("revoked source was published: %v", err)
	}
}
