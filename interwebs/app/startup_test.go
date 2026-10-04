package app

import (
	"context"
	"errors"
	"testing"
)

func TestPausedStartSuspendsNewAndRestoredProfiles(t *testing.T) {
	root := t.TempDir()
	a, err := Open(context.Background(), root, WithOffline(true), WithPausedStart(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	source := folder(t, "startup")
	p, err := a.CreateProfile(t.Context(), "Headless", WithSourceRoots(source))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Status().Paused {
		t.Fatal("new profile started active")
	}
	s, err := p.AddFolder(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(t.Context()); !errors.Is(err, ErrPaused) {
		t.Fatal(err)
	}
	if err := p.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	op, err := s.Publish(t.Context())
	wait(t, op, err)
	if err := s.SetLive(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	id := p.ID()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(context.Background(), root, WithOffline(true), WithPausedStart(true))
	if err != nil {
		t.Fatal(err)
	}
	p, err = a.Profile(id)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Status().Paused {
		t.Fatal("restored profile started active")
	}
	for _, status := range p.Status().Sites {
		if status.Core.Seeding {
			t.Fatal("paused startup seeded")
		}
	}
}
