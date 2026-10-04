package network

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anacrolix/torrent"
)

// PeerAnnouncementError reports errors starting the latest peer announcements.
// A nil error does not prove that public peers can reach this node.
func (t *Transport) PeerAnnouncementError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peerErr
}

func (t *Transport) wakePeers() {
	select {
	case t.peerWake <- struct{}{}:
	default:
	}
}

// The torrent library's AllowDataUpload does not wake a parked DHT announcer.
// Explicit announcements make seeding transitions independent of that wakeup,
// and retry discovery while a newly started DHT learns its routing table.
func (t *Transport) maintainPeers(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	var stops []func()
	stop := func() {
		for _, cancel := range stops {
			cancel()
		}
		stops = nil
	}
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.peerWake:
		case <-tick.C:
		}
		stop()
		if ctx.Err() != nil {
			return
		}

		t.mu.Lock()
		var handles []*torrent.Torrent
		if t.seeding && !t.closed {
			for _, transfer := range t.transfers {
				handles = append(handles, transfer.torrent)
			}
		}
		t.mu.Unlock()

		var failures error
		for _, handle := range handles {
			for _, server := range t.client.DhtServers() {
				_, cancel, err := handle.AnnounceToDht(server)
				if err != nil {
					failures = errors.Join(failures, fmt.Errorf("announce torrent peers: %w", err))
					continue
				}
				stops = append(stops, cancel)
			}
		}
		t.mu.Lock()
		t.peerErr = failures
		t.mu.Unlock()
	}
}
