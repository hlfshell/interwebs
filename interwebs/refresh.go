package interwebs

import (
	"context"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/network"
)

// RefreshResult distinguishes an authenticated update from usable content.
type RefreshResult struct {
	Updated, Usable     bool
	Current, Discovered string
}

// Refresh checks the signed address now. It never enables seeding or reloads tabs.
func (n *Node) Refresh(ctx context.Context) (result RefreshResult, err error) {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	n.mu.Lock()
	before := n.state.Record
	n.refreshError = ""
	n.mu.Unlock()
	transfer, err := n.resolveVersion(ctx, true)
	n.mu.Lock()
	defer n.mu.Unlock()
	result.Current, result.Discovered = n.state.Current, n.state.Record.Hash
	result.Updated = n.state.Record.Sequence > before.Sequence
	result.Usable = transfer != nil && transfer.Manifest().Hash() == n.state.Record.Hash
	if n.state.Identity.Hash != "" {
		result.Usable = transfer != nil
	}
	if err != nil {
		n.refreshError = err.Error()
	}
	return result, err
}

// Read opens one file. The caller must close the reader to release its demand.
func (n *Node) Read(ctx context.Context, path string, options network.ReadOptions) (content.Reader, error) {
	work, release, err := n.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	transfer, err := n.resolve(work)
	if err != nil {
		return nil, err
	}
	// The caller owns the reader context, not the short-lived workflow context.
	return transfer.Read(ctx, path, options)
}

// DownloadFile explicitly fetches one whole file; it does not enable uploading.
func (n *Node) DownloadFile(ctx context.Context, path string, options network.ReadOptions) error {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	transfer, err := n.resolve(ctx)
	if err == nil {
		err = transfer.DownloadFile(ctx, path, options)
	}
	n.mu.Lock()
	n.lastError = errorText(err)
	n.mu.Unlock()
	return err
}
