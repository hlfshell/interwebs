package storage

import (
	"bytes"
	"crypto/sha1"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestWALRecoveryAfterProcessExit(t *testing.T) {
	payload := []byte("<h1>committed before process exit</h1>")
	pieceHash := sha1.Sum(payload)
	info := metainfo.Info{Name: "index.html", Length: int64(len(payload)), PieceLength: 16 << 10, Pieces: pieceHash[:]}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	hash := metainfo.Hash(sha1.Sum(encoded))
	dir := os.Getenv("INTERWEB_WAL_FIXTURE")
	child := dir != ""
	if !child {
		dir = t.TempDir()
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWALRecoveryAfterProcessExit$")
		command.Env = append(os.Environ(), "INTERWEB_WAL_FIXTURE="+dir)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("WAL writer: %v\n%s", err, out)
		}
		wal, err := os.Stat(filepath.Join(dir, hash.HexString(), "wal"))
		if err != nil || wal.Size() == 0 {
			t.Fatal("fixture did not leave a committed WAL", err)
		}
	}
	backend, err := NewSandboxed(t.Context(), dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), backend)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if child {
		h, err := s.OpenTorrent(t.Context(), &info, hash)
		if err != nil {
			t.Fatal(err)
		}
		piece := h.Piece(info.Piece(0))
		if _, err := piece.WriteAt(payload, 0); err != nil {
			t.Fatal(err)
		}
		if err := piece.MarkComplete(); err != nil {
			t.Fatal(err)
		}
		// Intentionally bypass deferred Close/checkpointing, like process loss.
		os.Exit(0)
	}
	v, err := s.Open(t.Context(), hash.HexString())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	r, err := v.Open(t.Context(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatal("recovered bytes failed verification", string(data), err)
	}
	u, err := backend.Usage(t.Context(), hash.HexString())
	if err != nil {
		t.Fatal(err)
	}
	if s.quota.sizes[hash.HexString()] < u.Bytes+2*u.MetadataBytes {
		t.Fatal("reopened WAL lost checkpoint headroom")
	}
}
