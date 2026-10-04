package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"
)

// Every future backend must run this same contract suite, not just compile the interface.
func backendContract(t *testing.T, newBackend func(*testing.T) Backend) {
	t.Helper()
	b := newBackend(t)
	defer b.Close()
	hash := strings.Repeat("a", 40)
	if s, err := b.Open(t.Context(), hash, false); !errors.Is(err, fs.ErrNotExist) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("missing open: %v", err)
	}
	store, err := b.Open(t.Context(), hash, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.WriteFile("payload", bytes.NewReader(make([]byte, 128<<10))); err != nil {
		t.Fatal(err)
	}
	writer, err := store.Update("payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt([]byte("changed chunk"), 64<<10); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := store.Open("payload")
	if err != nil {
		t.Fatal(err)
	}
	seeker, ok := reader.(io.Seeker)
	if !ok {
		t.Fatal("nonseekable backend")
	}
	if _, err := seeker.Seek(64<<10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("changed chunk"))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	reader.Close()
	if string(got) != "changed chunk" {
		t.Fatal(string(got))
	}
	if _, err := store.Open("../outside"); err == nil {
		t.Fatal("traversal accepted")
	}
	u, err := b.Usage(t.Context(), hash)
	if err != nil || u.Bytes <= 128<<10 || u.MetadataBytes <= 0 {
		t.Fatal(u, err)
	}
	bound, err := b.Growth(t.Context(), hash, 128<<10)
	if err != nil || bound < 128<<10 {
		t.Fatal(bound, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.Open(ctx, hash, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	if err := store.Remove("payload"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open("payload"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("deleted virtual file remains", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Remove(t.Context(), hash); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(t.Context(), hash, true); !errors.Is(err, fs.ErrClosed) {
		t.Fatal("late open", err)
	}
}

func TestSandboxedBackendContract(t *testing.T) {
	backendContract(t, func(t *testing.T) Backend {
		b, err := NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

func TestConcurrentMutationsReserveQuota(t *testing.T) {
	s := testCollection(t, WithStorageLimit(256<<10))
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, hash := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40)} {
		group.Add(1)
		go func() {
			defer group.Done()
			s.mu.Lock()
			defer s.mu.Unlock()
			store, err := s.open(hash)
			if err == nil {
				err = s.mutate(hash, 256<<10, func() error { return store.WriteFile("payload", bytes.NewReader(make([]byte, 256<<10))) })
			}
			results <- err
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, ErrBufferFull) {
			t.Fatalf("quota mutation: %v", err)
		}
	}
	usage, err := s.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, u := range usage {
		total += u.Bytes
	}
	if total > 256<<10 {
		t.Fatal("quota exceeded", total)
	}
}
