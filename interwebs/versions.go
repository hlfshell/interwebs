package interwebs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
)

// VersionInfo describes a known published version with locally stored metadata.
// It is a copied summary, not a lease or proof of a complete download.
type VersionInfo struct {
	Hash        string
	Size        int64 // Declared plaintext payload size, not downloaded bytes.
	StoredBytes int64 // Physical ciphertext and storage metadata.
	Current     bool  // Selected by this Node; not necessarily the newest signed version.
}

// Versions lists known published versions with valid local metadata, sorted by hash.
// Partial downloads are included; staging and unreferenced stores are excluded.
// It reads local storage only and does not resolve or download missing content.
func (n *Node) Versions(ctx context.Context) ([]VersionInfo, error) {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	n.mu.Lock()
	current := n.state.Current
	known := make(map[string]bool)
	known[n.state.Identity.Hash] = true
	known[n.state.Record.Hash] = true
	known[current] = true
	for _, record := range n.state.History {
		known[record.Hash] = true
	}
	n.mu.Unlock()
	delete(known, "")

	usage, err := n.stores.Usage(ctx)
	if err != nil {
		return nil, fmt.Errorf("list version storage: %w", err)
	}
	versions := make([]VersionInfo, 0)
	for _, store := range usage {
		if !known[store.Hash] {
			continue
		}
		manifest, err := n.stores.Manifest(ctx, store.Hash)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read version %s: %w", store.Hash, err)
		}
		versions = append(versions, VersionInfo{
			Hash:        store.Hash,
			Size:        manifest.Size(),
			StoredBytes: store.Bytes,
			Current:     store.Hash == current,
		})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Hash < versions[j].Hash })
	return versions, nil
}
