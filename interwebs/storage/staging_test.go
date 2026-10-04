package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/site"
)

type canceledSource struct {
	content.Source
	cancel context.CancelFunc
}

func (s canceledSource) Open(ctx context.Context, _ string) (content.Reader, error) {
	s.cancel()
	return nil, ctx.Err()
}

func TestCanceledSnapshotRemovesPrivateStaging(t *testing.T) {
	stores := testCollection(t)
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := stores.Snapshot(ctx, canceledSource{Source: source, cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatal("lost cancellation", err)
	}
	usage, err := stores.Usage(t.Context())
	if err != nil || len(usage) != 0 {
		t.Fatal("canceled snapshot retained staging", usage, err)
	}
	if _, err := stores.Snapshot(t.Context(), source); err != nil {
		t.Fatal("canceled snapshot prevented retry", err)
	}
	usage, err = stores.Usage(t.Context())
	if err != nil || len(usage) != 1 {
		t.Fatal("successful snapshot retained staging", usage, err)
	}
}

func TestStagingAtomicFailureLeasesAndCapacity(t *testing.T) {
	backend, err := NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), backend, WithStorageLimit(2<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.newStaging(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.write(t.Context(), "test", bytes.NewBufferString("before"), 6); err != nil {
		t.Fatal(err)
	}
	if err := w.write(t.Context(), "test", bytes.NewBufferString("after"), 1); err == nil {
		t.Fatal("accepted oversized replacement")
	}
	if err := w.write(t.Context(), "test", bytes.NewReader(make([]byte, 3<<20)), 3<<20); !errors.Is(err, ErrBufferFull) {
		t.Fatal(err)
	}
	r, err := w.read(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	// Aborted staging must not consume quota forever.
	if err := w.write(t.Context(), "small", bytes.NewBufferString("ok"), 2); err != nil {
		t.Fatal(err)
	}
	w.close()
	w.close()
	if _, err := w.read(t.Context(), "test"); !errors.Is(err, fs.ErrClosed) {
		t.Fatal(err)
	}
	if err := s.Remove(t.Context(), w.id); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "before" {
		t.Fatal(string(b), err)
	}
	r.Close()
	if err := s.Remove(t.Context(), w.id); err != nil {
		t.Fatal(err)
	}
}
