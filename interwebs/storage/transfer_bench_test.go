package storage

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// BenchmarkTorrentWrites isolates encrypted receive-side writes from networking.
// Larger writes are an experiment, not a change to the torrent wire block size.
func BenchmarkTorrentWrites(b *testing.B) {
	const total = 8 << 20
	const pieceSize = 256 << 10
	for _, block := range []int{16 << 10, 64 << 10, 256 << 10} {
		b.Run(fmt.Sprintf("%dKiB", block>>10), func(b *testing.B) {
			info := metainfo.Info{Name: "video.mp4", Length: total, PieceLength: pieceSize, Pieces: make([]byte, 20*(total/pieceSize))}
			encoded, err := bencode.Marshal(info)
			if err != nil {
				b.Fatal(err)
			}
			hash := sha1.Sum(encoded)
			payload := bytes.Repeat([]byte{1}, block)
			var elapsed, accounting time.Duration
			b.SetBytes(total)
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				backend, err := NewSandboxed(b.Context(), b.TempDir(), bytes.Repeat([]byte{1}, 32))
				if err != nil {
					b.Fatal(err)
				}
				measured := &measuredBackend{Backend: backend}
				collection, err := New(b.Context(), measured)
				if err != nil {
					b.Fatal(err)
				}
				handle, err := collection.OpenTorrent(b.Context(), &info, hash)
				if err != nil {
					b.Fatal(err)
				}
				measured.accounting = 0
				b.StartTimer()
				start := time.Now()
				for offset := 0; offset < total; offset += block {
					piece := handle.Piece(info.Piece(offset / pieceSize))
					if n, err := piece.WriteAt(payload, int64(offset%pieceSize)); err != nil || n != len(payload) {
						b.Fatalf("write: %d, %v", n, err)
					}
				}
				elapsed += time.Since(start)
				accounting += measured.accounting
				b.StopTimer()
				check := make([]byte, block)
				if _, err := handle.Piece(info.Piece(total/pieceSize-1)).ReadAt(check, pieceSize-int64(block)); err != nil || !bytes.Equal(check, payload) {
					b.Fatalf("readback failed: %v", err)
				}
				if err := handle.Close(); err != nil {
					b.Fatal(err)
				}
				if err := collection.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.ReportMetric(100*float64(accounting)/float64(elapsed), "accounting-%")
		})
	}
}

// BenchmarkSparseTailWrite represents a browser seeking to an uncached video's
// tail: only 16 KiB arrive, but the encrypted file may need to represent the gap.
func BenchmarkSparseTailWrite(b *testing.B) {
	const block = 16 << 10
	const pieceSize = 256 << 10
	for _, mib := range []int{1, 32, 96} {
		b.Run(fmt.Sprintf("%dMiB-file", mib), func(b *testing.B) {
			total := mib << 20
			info := metainfo.Info{Name: "video.mp4", Length: int64(total), PieceLength: pieceSize, Pieces: make([]byte, 20*(total/pieceSize))}
			encoded, err := bencode.Marshal(info)
			if err != nil {
				b.Fatal(err)
			}
			hash := sha1.Sum(encoded)
			payload := bytes.Repeat([]byte{1}, block)
			var physical int64
			b.SetBytes(block)
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				backend, err := NewSandboxed(b.Context(), b.TempDir(), bytes.Repeat([]byte{1}, 32))
				if err != nil {
					b.Fatal(err)
				}
				collection, err := New(b.Context(), backend)
				if err != nil {
					b.Fatal(err)
				}
				handle, err := collection.OpenTorrent(b.Context(), &info, hash)
				if err != nil {
					b.Fatal(err)
				}
				piece := handle.Piece(info.Piece(total/pieceSize - 1))
				b.StartTimer()
				if n, err := piece.WriteAt(payload, pieceSize-block); err != nil || n != block {
					b.Fatalf("tail write: %d, %v", n, err)
				}
				b.StopTimer()
				usage, err := backend.Usage(b.Context(), metainfo.Hash(hash).HexString())
				if err != nil {
					b.Fatal(err)
				}
				physical += usage.Bytes
				check := make([]byte, block)
				if _, err := piece.ReadAt(check, pieceSize-block); err != nil || !bytes.Equal(check, payload) {
					b.Fatalf("tail readback: %v", err)
				}
				if err := handle.Close(); err != nil {
					b.Fatal(err)
				}
				if err := collection.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(physical)/float64(b.N), "stored-bytes/op")
		})
	}
}
