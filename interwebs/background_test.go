package interwebs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/identity"
)

type announcementProbe struct {
	sent  chan identity.Record
	block bool
}

func (p *announcementProbe) Resolve(context.Context, identity.Identity, identity.Record) (identity.Record, error) {
	return identity.Record{}, errors.New("not a reader")
}

func (p *announcementProbe) Announce(ctx context.Context, _ identity.Identity, record identity.Record) error {
	select {
	case p.sent <- record:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func announcementNode(t *testing.T, block bool) (*Node, *announcementProbe) {
	t.Helper()
	initial, opts, _ := localNode(t, "announcement")
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	probe := &announcementProbe{sent: make(chan identity.Record, 8), block: block}
	n, err := New(t.Context(), append(opts, func(o *options) error { o.discovery = probe; return nil })...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := n.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := n.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	return n, probe
}

func receiveAnnouncement(t *testing.T, p *announcementProbe) identity.Record {
	t.Helper()
	select {
	case record := <-p.sent:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("announcement blocked")
		return identity.Record{}
	}
}

func TestAnnouncementDoesNotWaitForDownloadGate(t *testing.T) {
	n, probe := announcementNode(t, false)
	first := receiveAnnouncement(t, probe)
	// Hold the same workflow gate as Download without depending on live peers.
	_, release, err := n.operation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// A replacement being prepared must not escape before save succeeds.
	n.mu.Lock()
	n.state.Record.Sequence++
	n.mu.Unlock()
	n.wake <- struct{}{}
	got := receiveAnnouncement(t, probe)
	if got.Sequence != first.Sequence || got.Hash != first.Hash {
		t.Fatalf("announced unsaved replacement: %+v", got)
	}
}

func TestCloseCancelsAnnouncement(t *testing.T) {
	n, probe := announcementNode(t, true)
	receiveAnnouncement(t, probe)
	closed := make(chan error, 1)
	go func() { closed <- n.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel discovery")
	}
}
