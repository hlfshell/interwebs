package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestViewDoesNotWaitForBackgroundLookup(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	n := &fakeNode{refresh: func(ctx context.Context) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		select {
		case canceled <- struct{}{}:
		default:
		}
		return ctx.Err()
	}}
	_, _, s := fakeProfile(t, n)
	op, err := s.View(t.Context())
	view := wait(t, op, err).View
	defer view.Close()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("visited site did not check updates in background")
	}
	if !n.Status().Seeding {
		t.Fatal("lookup began before restoring seeding")
	}
	op, err = s.View(t.Context())
	second := wait(t, op, err).View
	defer second.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("foreground view did not preempt update lookup")
	}
}

func TestFailedUpdateDoesNotPreventCachedDownloadOrView(t *testing.T) {
	failure := errors.New("discovery unavailable")
	n := &fakeNode{}
	n.refresh = func(context.Context) error {
		n.mu.Lock()
		defer n.mu.Unlock()
		if !n.seeding || n.downloads == 0 {
			t.Error("lookup preceded cached seeding/download")
		}
		return failure
	}
	_, _, s := fakeProfile(t, n)
	if err := s.SetFavorite(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Status().Error == failure.Error() })
	op, err := s.View(t.Context())
	view := wait(t, op, err).View
	defer view.Close()
	if !n.Status().Seeding {
		t.Fatal("failed lookup disabled cached seeding")
	}
}
