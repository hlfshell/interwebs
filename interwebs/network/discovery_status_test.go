package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/identity"
)

type unavailableDiscovery struct{}

func (unavailableDiscovery) Resolve(ctx context.Context, _ identity.Identity, _ identity.Record) (identity.Record, error) {
	<-ctx.Done()
	return identity.Record{}, ctx.Err()
}
func (unavailableDiscovery) Announce(context.Context, identity.Identity, identity.Record) error {
	return nil
}

func TestLookupDiagnosticsRecordCancellation(t *testing.T) {
	transport, err := New(t.Context(), networkStores(t, t.TempDir()), WithOffline(true), WithDiscovery(unavailableDiscovery{}))
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = transport.Resolve(ctx, identity.Identity{}, identity.Record{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s := transport.DiscoveryStatus()
	if s.Lookup.Started.IsZero() || s.Lookup.Finished.IsZero() || s.Lookup.Error != context.Canceled.Error() || s.Lookup.Elapsed < 0 {
		t.Fatalf("missing failed lookup: %+v", s)
	}
}

func TestDiscoverySnapshotsAreIndependentAndLatestAttemptWins(t *testing.T) {
	d := &discoveryStatus{}
	transport := &Transport{diagnostics: d}
	old := d.lookup()
	finish := d.lookup()
	old(errors.New("stale failure"))
	s := transport.DiscoveryStatus()
	if !s.Lookup.Finished.IsZero() || s.Lookup.Error != "" || s.Lookup.Elapsed < 0 {
		t.Fatal(s)
	}
	finish(nil)
	s = transport.DiscoveryStatus()
	if s.Lookup.Finished.IsZero() {
		t.Fatal(s)
	}
	s.Lookup.Error = "mutated copy"
	if transport.DiscoveryStatus().Lookup.Error != "" {
		t.Fatal("snapshot aliases state")
	}
	end := d.metadata("first", false)
	d.peer("other")
	if !transport.DiscoveryStatus().FirstPeer.Finished.IsZero() {
		t.Fatal("unrelated handshake counted")
	}
	d.peer("first")
	first := transport.DiscoveryStatus().FirstPeer
	d.peer("first")
	if transport.DiscoveryStatus().FirstPeer != first {
		t.Fatal("later handshake replaced first")
	}
	end(nil)
	end = d.metadata("second", true)
	end(context.Canceled)
	s = transport.DiscoveryStatus()
	if s.Hash != "second" || !s.CachedMetadata || s.Metadata.Error == "" || s.FirstPeer.Error == "" {
		t.Fatal(s)
	}
}

func TestRoutingReadinessAndCancellation(t *testing.T) {
	for _, ready := range []bool{false, true} {
		d := &discoveryStatus{status: DiscoveryStatus{RoutingReady: Stage{Started: time.Now()}}}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		d.routing(ctx, func() bool { return ready })
		s := (&Transport{diagnostics: d}).DiscoveryStatus().RoutingReady
		if s.Finished.IsZero() || (s.Error == "") != ready {
			t.Fatal(s)
		}
	}
}
