package interwebs

import (
	"context"

	"github.com/hlfshell/interweb/interwebs/storage"
)

// Seed enables payload uploads for this Node. Repeated calls are safe.
func (n *Node) Seed(ctx context.Context) error {
	work, release, err := n.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if _, err := n.resolve(work); err != nil {
		return err
	}
	return n.seed(work)
}

func (n *Node) seed(ctx context.Context) error {
	n.mu.Lock()
	if err := n.transport.SetSeeding(ctx, true); err != nil {
		n.mu.Unlock()
		return err
	}
	n.seeding = true
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
	return nil
}

// Stop disables uploads only. Downloads, browser servers and storage are retained.
func (n *Node) Stop(ctx context.Context) error {
	// Do not wait behind a long download or publication.
	n.mu.Lock()
	if err := n.transport.SetSeeding(ctx, false); err != nil {
		n.mu.Unlock()
		return err
	}
	n.seeding = false
	n.mu.Unlock()
	return nil
}

// StorageUsage reports physical encrypted stores, including staging and failed
// publication leftovers. It does not classify stores as published versions.
// Bytes includes MetadataBytes; it excludes Node state, keys, and source folders.
func (n *Node) StorageUsage(ctx context.Context) ([]storage.Usage, error) {
	return n.stores.Usage(ctx)
}

// RemoveVersion removes an explicitly selected idle version. Served versions,
// and active transfers must be released first.
func (n *Node) RemoveVersion(ctx context.Context, hash string) error {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	n.mu.Lock()
	served := n.servers[hash] != nil
	n.mu.Unlock()
	if served {
		return storage.ErrInUse
	}
	if err := n.transport.Drop(ctx, hash); err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.transfers, hash)
	if n.state.Current == hash {
		n.state.Current = ""
	}
	n.mu.Unlock()
	if err := n.save(ctx); err != nil {
		return err
	}
	return n.stores.Remove(ctx, hash)
}

// ReleaseVersion stops serving/transferring an idle version without deleting it.
func (n *Node) ReleaseVersion(ctx context.Context, hash string) error {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	return n.drop(ctx, hash)
}

func (n *Node) drop(ctx context.Context, hash string) error {
	if err := n.transport.Drop(ctx, hash); err != nil {
		return err
	}
	n.mu.Lock()
	server := n.servers[hash]
	delete(n.servers, hash)
	delete(n.transfers, hash)
	n.mu.Unlock()
	if server != nil {
		return server.Close()
	}
	return nil
}
