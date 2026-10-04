package storage

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/site"
)

func testCollection(t *testing.T, opts ...Option) *Collection {
	t.Helper()
	backend, err := NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), backend, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func testSnapshot(t *testing.T, s *Collection) content.Manifest {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("encrypted content"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := site.New(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Snapshot(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCollectionLeasesAndLateCallbacks(t *testing.T) {
	s := testCollection(t)
	m := testSnapshot(t, s)
	v, err := s.Open(t.Context(), m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	r, err := v.Open(t.Context(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(t.Context(), m.Hash()); !errors.Is(err, ErrInUse) {
		t.Fatalf("reader lost lease: %v", err)
	}
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "encrypted content" {
		t.Fatal(string(b), err)
	}
	r.Close()
	r.Close()
	if err := s.Remove(t.Context(), m.Hash()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var info metainfo.Info
	if err := bencode.Unmarshal(m.Metadata(), &info); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenTorrent(t.Context(), &info, metainfo.NewHashFromHex(m.Hash())); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("late callback: %v", err)
	}
}

func TestCollectionQuotaAndOffsetBounds(t *testing.T) {
	s := testCollection(t, WithStorageLimit(1))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("hello"), 0600)
	source, err := site.New(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(t.Context(), source); !errors.Is(err, ErrBufferFull) {
		t.Fatalf("quota: %v", err)
	}
	s = testCollection(t)
	m := testSnapshot(t, s)
	var info metainfo.Info
	bencode.Unmarshal(m.Metadata(), &info)
	torrent, err := s.OpenTorrent(t.Context(), &info, metainfo.NewHashFromHex(m.Hash()))
	if err != nil {
		t.Fatal(err)
	}
	defer torrent.Close()
	piece := torrent.Piece(info.Piece(0))
	if _, err := piece.WriteAt([]byte{1}, -1); err == nil {
		t.Fatal("negative write")
	}
	if _, err := piece.WriteAt([]byte{1}, 1<<62); err == nil {
		t.Fatal("overflow write")
	}
	if _, err := piece.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("negative read")
	}
}

func TestStoredReaderRejectsUnverifiedBytes(t *testing.T) {
	s := testCollection(t)
	m := testSnapshot(t, s)
	var info metainfo.Info
	if err := bencode.Unmarshal(m.Metadata(), &info); err != nil {
		t.Fatal(err)
	}
	handle, err := s.OpenTorrent(t.Context(), &info, metainfo.NewHashFromHex(m.Hash()))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if _, err := handle.Piece(info.Piece(0)).WriteAt([]byte("X"), 0); err != nil {
		t.Fatal(err)
	}
	v, err := s.Open(t.Context(), m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	r, err := v.Open(t.Context(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n, err := r.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatal("unverified content escaped", n, err)
	}
}

func TestSandboxedBackendRejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{1}, 32)
	b, err := NewSandboxed(t.Context(), dir, key)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	m := testSnapshot(t, s)
	s.Close()
	b, err = NewSandboxed(t.Context(), dir, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err = New(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Manifest(t.Context(), m.Hash()); err == nil {
		t.Fatal("accepted wrong key")
	}
}
