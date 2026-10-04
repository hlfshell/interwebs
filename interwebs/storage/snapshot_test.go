package storage

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hlfshell/interweb/interwebs/site"
)

type countingReadsBackend struct {
	Backend
	reads int
}

func (b *countingReadsBackend) Open(ctx context.Context, hash string, create bool) (Store, error) {
	s, err := b.Backend.Open(ctx, hash, create)
	if err != nil {
		return nil, err
	}
	return &countingReadsStore{Store: s, backend: b}, nil
}

type countingReadsStore struct {
	Store
	backend *countingReadsBackend
}

func (s *countingReadsStore) Open(name string) (fs.File, error) {
	if strings.HasPrefix(name, "site/") {
		s.backend.reads++
	}
	return s.Store.Open(name)
}

func TestSnapshotHashesAcrossFilesWithoutRereadingStaging(t *testing.T) {
	backend, err := NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingReadsBackend{Backend: backend}
	s, err := New(t.Context(), counted)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	folder := t.TempDir()
	payloads := map[string][]byte{
		"a.bin":      bytes.Repeat([]byte{1}, (256<<10)-13),
		"empty.bin":  {},
		"index.html": bytes.Repeat([]byte{2}, 29),
		"z.bin":      bytes.Repeat([]byte{3}, (256<<10)+5),
	}
	info := metainfo.Info{Name: "site", PieceLength: 256 << 10}
	for _, name := range []string{"a.bin", "empty.bin", "index.html", "z.bin"} {
		if err := os.WriteFile(filepath.Join(folder, name), payloads[name], 0600); err != nil {
			t.Fatal(err)
		}
		info.Files = append(info.Files, metainfo.FileInfo{Path: []string{name}, Length: int64(len(payloads[name]))})
	}
	if err := info.GeneratePieces(func(f metainfo.FileInfo) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payloads[f.Path[0]])), nil
	}); err != nil {
		t.Fatal(err)
	}
	want, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	source, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Snapshot(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.Metadata(), want) {
		t.Fatal("streaming snapshot changed torrent metadata")
	}
	if counted.reads != len(payloads) {
		t.Fatalf("expected one staging read per file for installation, got %d", counted.reads)
	}
	v, err := s.Open(t.Context(), m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	for name, want := range payloads {
		r, err := v.Open(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(r)
		closeErr := r.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s readback: %v, %v", name, readErr, closeErr)
		}
	}
}
