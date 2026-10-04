package storage

import (
	"bytes"
	"crypto/sha1"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestSparseTorrentTailFitsPhysicalQuota(t *testing.T) {
	const total = 96 << 20
	const pieceSize = 256 << 10
	const block = 16 << 10
	s := testCollection(t, WithStorageLimit(1<<20))
	info := metainfo.Info{Name: "video.mp4", Length: total, PieceLength: pieceSize, Pieces: make([]byte, 20*(total/pieceSize))}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha1.Sum(encoded)
	handle, err := s.OpenTorrent(t.Context(), &info, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	piece := handle.Piece(info.Piece(total/pieceSize - 1))
	payload := bytes.Repeat([]byte{7}, block)
	if n, err := piece.WriteAt(payload, pieceSize-block); err != nil || n != block {
		t.Fatalf("sparse tail write: %d, %v", n, err)
	}
	check := make([]byte, block)
	if _, err := piece.ReadAt(check, pieceSize-block); err != nil || !bytes.Equal(check, payload) {
		t.Fatalf("tail readback: %v", err)
	}
	usage, err := s.Usage(t.Context())
	if err != nil || len(usage) != 1 {
		t.Fatal(usage, err)
	}
	if usage[0].Bytes > 128<<10 {
		t.Fatalf("sparse tail allocated %d bytes", usage[0].Bytes)
	}
	// A gap may read as zeros at the storage boundary, but those bytes must not
	// escape through the verified content reader when their piece hash differs.
	v, err := s.Open(t.Context(), metainfo.Hash(hash).HexString())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	r, err := v.Open(t.Context(), "video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n, err := r.Read(check); n != 0 || err == nil {
		t.Fatalf("unverified sparse gap escaped: %d, %v", n, err)
	}
	if piece.Completion().Complete {
		t.Fatal("partial tail marked complete")
	}
}
