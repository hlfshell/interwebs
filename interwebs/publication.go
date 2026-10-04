package interwebs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/hlfshell/interweb/interwebs/content"
)

// Publish snapshots the local source into an immutable version and enables seeding.
// A local source and its matching signing key are required. Announcement
// happens asynchronously; Status distinguishes persistence and announcement errors.
// An existing stored version resumes seeding before source preparation, and stays
// available if preparing its replacement fails.
func (n *Node) Publish(ctx context.Context) (Publication, error) {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return Publication{}, fmt.Errorf("begin publication: %w", err)
	}
	defer release()
	if n.signer == nil {
		return Publication{}, fmt.Errorf("publish requires a local source and signer: %w", ErrCapability)
	}
	source := n.source
	if source == nil {
		return Publication{}, fmt.Errorf("publish requires a local source and signer: %w", ErrCapability)
	}

	// Restore the published version before doing any work on its replacement.
	n.mu.Lock()
	current := n.state.Current
	n.mu.Unlock()
	if current != "" {
		// A deliberately cleared cache has nothing to restore; rebuild it from
		// the source instead of waiting for peers to return our own publication.
		_, err := n.stores.Manifest(ctx, current)
		if errors.Is(err, fs.ErrNotExist) {
			current = ""
		} else if err != nil {
			return Publication{}, fmt.Errorf("read stored manifest: %w", err)
		}
	}
	if current != "" {
		if _, err := n.load(ctx, current); err != nil {
			return Publication{}, fmt.Errorf("restore published version: %w", err)
		}
		if err := n.seed(ctx); err != nil {
			return Publication{}, fmt.Errorf("seed stored version: %w", err)
		}
	}

	if validator, ok := n.content.(interface {
		ValidateFiles(context.Context, func(context.Context, string) (content.Reader, error)) error
	}); ok {
		if err := validator.ValidateFiles(ctx, source.Open); err != nil {
			return Publication{}, fmt.Errorf("validate publication: %w", err)
		}
	}

	// Snapshot source bytes before signing the immutable version.
	m, err := n.stores.Snapshot(ctx, source)
	if err != nil {
		return Publication{}, fmt.Errorf("snapshot publication: %w", err)
	}
	n.mu.Lock()
	previous := n.state.Record
	n.mu.Unlock()
	record := previous
	if previous.Hash != m.Hash() {
		record, err = n.signer.Sign(previous.Sequence+1, m.Hash())
		if err != nil {
			return Publication{}, fmt.Errorf("sign publication: %w", err)
		}
	}
	if _, err := n.load(ctx, m.Hash()); err != nil {
		return Publication{}, fmt.Errorf("load published version: %w", err)
	}

	// Persist the signed record before enabling uploads and announcements.
	n.mu.Lock()
	n.state.Record = record
	n.state.Current = m.Hash()
	if previous.Hash != m.Hash() {
		n.state.History = append(n.state.History, record)
	}
	n.mu.Unlock()
	if err := n.save(ctx); err != nil {
		return Publication{}, fmt.Errorf("persist publication: %w", err)
	}
	if err := n.seed(ctx); err != nil {
		return Publication{}, fmt.Errorf("seed publication: %w", err)
	}
	return Publication{Magnet: n.state.Identity.Magnet(), Record: record.Clone()}, nil
}
