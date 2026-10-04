package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hlfshell/interweb/interwebs/site"
)

type failedCleanupBackend struct {
	Backend
	failure error
}

func (b *failedCleanupBackend) Remove(context.Context, string) error { return b.failure }

func TestSnapshotCleanupFailurePreservesQuotaAccounting(t *testing.T) {
	dir := t.TempDir()
	backend, err := NewSandboxed(t.Context(), dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected cleanup failure")
	s, err := New(t.Context(), &failedCleanupBackend{Backend: backend, failure: failure})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "index.html"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := site.New(t.Context(), sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(t.Context(), source); !errors.Is(err, failure) {
		t.Fatalf("cleanup failure lost: %v", err)
	}
	usage, err := s.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 {
		t.Fatalf("expected accounted staging and final stores: %d", len(usage))
	}
	if _, err := s.Snapshot(t.Context(), source); !errors.Is(err, failure) {
		t.Fatalf("accounting did not fail closed: %v", err)
	}
}

type failedListBackend struct {
	Backend
	failure error
	closed  int
}

func (b *failedListBackend) List(context.Context) ([]Usage, error) { return nil, b.failure }
func (b *failedListBackend) Close() error                          { b.closed++; return b.Backend.Close() }

func TestCollectionInitializationClosesInjectedBackend(t *testing.T) {
	backend, err := NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected accounting failure")
	b := &failedListBackend{Backend: backend, failure: failure}
	if _, err := New(t.Context(), b); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if b.closed != 1 {
		t.Fatalf("backend closed %d times", b.closed)
	}
}
