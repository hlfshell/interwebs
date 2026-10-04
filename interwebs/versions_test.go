package interwebs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/hlfshell/interweb/interwebs/site"
	"github.com/hlfshell/interweb/interwebs/storage"
)

func TestVersionsListStoredHistoryAndReturnIndependentSummaries(t *testing.T) {
	node, opts, folder := localNode(t, "first")
	first, err := node.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := node.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	versions, err := node.Versions(t.Context())
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions: %+v, %v", versions, err)
	}
	if !sort.SliceIsSorted(versions, func(i, j int) bool { return versions[i].Hash < versions[j].Hash }) {
		t.Fatal("versions are not ordered by hash")
	}
	for _, version := range versions {
		if version.StoredBytes <= version.Size || version.Current != (version.Hash == second.Record.Hash) {
			t.Fatalf("invalid version summary: %+v", version)
		}
	}
	versions[0].Hash = "changed by caller"
	again, err := node.Versions(t.Context())
	if err != nil || again[0].Hash == versions[0].Hash {
		t.Fatal("caller changed internal versions", err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	versions, err = reopened.Versions(t.Context())
	if err != nil || len(versions) != 2 {
		t.Fatal("restart lost stored history", versions, err)
	}
	if err := reopened.ReleaseVersion(t.Context(), first.Record.Hash); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RemoveVersion(t.Context(), first.Record.Hash); err != nil {
		t.Fatal(err)
	}
	versions, err = reopened.Versions(t.Context())
	if err != nil || len(versions) != 1 || versions[0].Hash != second.Record.Hash {
		t.Fatal("deleted version still listed", versions, err)
	}
}

func TestVersionsIncludeMetadataOnlyFixedReferenceWithoutNetwork(t *testing.T) {
	owner, _, _ := localNode(t, "payload")
	publication, err := owner.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := owner.Manifest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source, err := site.FromMagnet("magnet:?xt=urn:btih:" + publication.Record.Hash)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := New(t.Context(), WithContent(source), WithDataDir(t.TempDir()), WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	versions, err := reader.Versions(t.Context())
	if err != nil || len(versions) != 0 {
		t.Fatal("unresolved reference listed without metadata", versions, err)
	}
	if err := reader.stores.Prepare(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	versions, err = reader.Versions(t.Context())
	if err != nil || len(versions) != 1 || versions[0].Size != int64(len("payload")) || versions[0].Current {
		t.Fatal("metadata-only version missing", versions, err)
	}
	if reader.Status().Availability != Unresolved || reader.Status().Seeding {
		t.Fatal("listing resolved content or started uploads")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.Versions(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := reader.StorageUsage(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Versions(t.Context()); !errors.Is(err, fs.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := reader.StorageUsage(t.Context()); !errors.Is(err, fs.ErrClosed) {
		t.Fatal(err)
	}
}

type retainedStagingBackend struct {
	storage.Backend
	failure error
}

func (b *retainedStagingBackend) Remove(context.Context, string) error {
	return b.failure
}

func TestStorageUsageIncludesFailedPublicationStoresButVersionsDoesNot(t *testing.T) {
	original, _, _ := localNode(t, "unpublished")
	backend, err := storage.NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("cleanup failed")
	node, err := New(t.Context(), WithContent(original.content), WithSigner(original.signer),
		WithDataDir(t.TempDir()), WithOffline(true),
		WithStorageBackend(&retainedStagingBackend{Backend: backend, failure: failure}))
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	if _, err := node.Publish(t.Context()); !errors.Is(err, failure) {
		t.Fatal("publication lost cleanup failure", err)
	}
	usage, err := node.StorageUsage(t.Context())
	if err != nil || len(usage) != 2 {
		t.Fatal("physical usage hid staging or candidate", usage, err)
	}
	for _, store := range usage {
		if store.Bytes <= 0 || store.MetadataBytes <= 0 || store.MetadataBytes > store.Bytes {
			t.Fatal("invalid physical usage", store)
		}
	}
	versions, err := node.Versions(t.Context())
	if err != nil || len(versions) != 0 {
		t.Fatal("unpublished stores presented as versions", versions, err)
	}
}
