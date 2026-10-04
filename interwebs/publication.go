package interwebs

import (
	"context"
	"fmt"

	"github.com/hlfshell/interweb/interwebs/content"
)

// Publish snapshots the local source into an immutable version and enables seeding.
// A local source and its matching signing key are required. Announcement
// happens asynchronously; Status distinguishes persistence and announcement errors.
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
