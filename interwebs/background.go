package interwebs

import (
	"context"
	"time"
)

// maintain handles protocol renewal and optional caller-configured refresh.
// It never restores application preferences, starts downloads, or evicts data.
func (n *Node) maintain(ctx context.Context, interval time.Duration, offline bool) {
	defer n.wg.Done()
	var refresh <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		refresh = ticker.C
	}
	renew := time.NewTimer(30 * time.Minute)
	defer renew.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh:
			work, cancel := context.WithTimeout(ctx, 90*time.Second)
			_, _ = n.Refresh(work) // Refresh records its observable error.
			cancel()
			continue
		case <-n.wake:
		case <-renew.C:
		}
		if offline {
			continue
		}
		work, cancel := context.WithTimeout(ctx, 20*time.Second)
		sequence, err := n.announce(work)
		cancel()
		n.mu.Lock()
		n.announceError = errorText(err)
		if err == nil && sequence > 0 {
			n.announcedSequence = sequence
		}
		n.mu.Unlock()
		delay := 30 * time.Minute
		if err != nil {
			delay = time.Minute
		}
		renew.Reset(delay)
	}
}

func (n *Node) announce(ctx context.Context) (int64, error) {
	// maintain owns this work's lifetime and Close waits for it. Announcing a
	// durable record must not wait behind a download or an in-progress snapshot.
	n.mu.Lock()
	enabled, record, address, persistErr := n.seeding, n.durableRecord.Clone(), n.state.Identity, n.persistenceErr
	n.mu.Unlock()
	if !enabled {
		return 0, nil
	}
	if persistErr != nil {
		return 0, persistErr
	}
	if record.Hash == "" || address.Key == "" {
		return 0, nil
	}
	err := n.transport.Announce(ctx, address, record)
	return record.Sequence, err
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
