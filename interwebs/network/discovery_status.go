package network

import (
	"context"
	"sync"
	"time"
)

// Stage describes the latest attempt. A zero Started means not attempted;
// a zero Finished with a nonzero Started means pending. Elapsed uses a monotonic
// clock, including while pending. Durations in JSON are nanoseconds.
type Stage struct {
	Started, Finished time.Time
	Elapsed           time.Duration
	Error             string
}

// DiscoveryStatus separates overlapping waits; these durations must not be added.
// RoutingReady is time from client creation to the first good DHT routing node,
// sampled every 100ms, not completion of bootstrap or a reachability guarantee.
// Lookup includes record verification. Metadata includes local storage loading
// and validation. FirstPeer measures time from opening that hash to its first
// successful BitTorrent handshake, not just TCP connection time.
// Only the latest metadata attempt is retained, keeping diagnostic memory bounded.
type DiscoveryStatus struct {
	CacheError                                string
	RoutingReady, Lookup, Metadata, FirstPeer Stage
	Hash                                      string
	CachedMetadata                            bool
}

type discoveryStatus struct {
	mu     sync.Mutex
	status DiscoveryStatus
}

func finishStage(s *Stage, err error) {
	s.Finished = time.Now()
	s.Elapsed = s.Finished.Sub(s.Started)
	if err != nil {
		s.Error = err.Error()
	}
}

func (d *discoveryStatus) lookup() func(error) {
	d.mu.Lock()
	started := time.Now()
	d.status.Lookup = Stage{Started: started}
	d.mu.Unlock()
	return func(err error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.status.Lookup.Started == started {
			finishStage(&d.status.Lookup, err)
		}
	}
}

func (d *discoveryStatus) metadata(hash string, cached bool) func(error) {
	d.mu.Lock()
	d.status.Hash, d.status.CachedMetadata = hash, cached
	d.status.Metadata = Stage{Started: time.Now()}
	d.status.FirstPeer = Stage{Started: d.status.Metadata.Started}
	d.mu.Unlock()
	return func(err error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		finishStage(&d.status.Metadata, err)
		if err != nil && d.status.FirstPeer.Finished.IsZero() {
			finishStage(&d.status.FirstPeer, err)
		}
	}
}

func (d *discoveryStatus) peer(hash string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.status.Hash == hash && d.status.FirstPeer.Finished.IsZero() {
		finishStage(&d.status.FirstPeer, nil)
	}
}

func (d *discoveryStatus) routing(ctx context.Context, ready func() bool) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var err error
	for !ready() {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-ticker.C:
		}
		if err != nil {
			break
		}
	}
	d.mu.Lock()
	finishStage(&d.status.RoutingReady, err)
	d.mu.Unlock()
}

// DiscoveryStatus returns a copied snapshot without performing network work.
func (t *Transport) DiscoveryStatus() DiscoveryStatus {
	d := t.diagnostics
	d.mu.Lock()
	out := d.status
	d.mu.Unlock()
	if t.hints != nil {
		out.CacheError = t.hints.failure()
	}
	for _, stage := range []*Stage{&out.RoutingReady, &out.Lookup, &out.Metadata, &out.FirstPeer} {
		if !stage.Started.IsZero() && stage.Finished.IsZero() {
			stage.Elapsed = time.Since(stage.Started)
		}
	}
	return out
}
