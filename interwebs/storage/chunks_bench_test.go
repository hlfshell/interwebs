package storage

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hlfshell/sandboxed"
)

// chunkBackend changes only the encrypted chunk size for this experiment.
// The production reservation remains conservative for all tested sizes.
type chunkBackend struct {
	*Sandboxed
	chunkSize int
}

func (b *chunkBackend) Open(_ context.Context, hash string, _ bool) (Store, error) {
	s, err := sandboxed.OpenStore(filepath.Join(b.dir, hash), sandboxed.WithEncryption(b.key), sandboxed.WithChunkSize(b.chunkSize))
	if err != nil {
		return nil, err
	}
	return &sandboxStore{s}, nil
}

func BenchmarkReceiveChunks(b *testing.B) {
	benchmarkReceiveChunks(b, 8<<20)
}

func BenchmarkReceiveLargeChunks(b *testing.B) {
	benchmarkReceiveChunks(b, 32<<20)
}

func benchmarkReceiveChunks(b *testing.B, total int) {
	const block = 16 << 10
	const pieceSize = 256 << 10
	for _, shuffled := range []bool{false, true} {
		for _, chunkSize := range []int{16 << 10, 32 << 10, 64 << 10} {
			b.Run(fmt.Sprintf("shuffled=%t/%dKiB", shuffled, chunkSize>>10), func(b *testing.B) {
				info := metainfo.Info{Name: "video.mp4", Length: int64(total), PieceLength: pieceSize, Pieces: make([]byte, 20*(total/pieceSize))}
				encoded, err := bencode.Marshal(info)
				if err != nil {
					b.Fatal(err)
				}
				hash := sha1.Sum(encoded)
				order := make([]int, total/block)
				for i := range order {
					order[i] = i
				}
				if shuffled {
					rand.New(rand.NewSource(1)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
				}
				payload := bytes.Repeat([]byte{7}, block)
				b.SetBytes(int64(total))
				b.ResetTimer()
				for range b.N {
					b.StopTimer()
					backend, err := NewSandboxed(b.Context(), b.TempDir(), bytes.Repeat([]byte{1}, 32))
					if err != nil {
						b.Fatal(err)
					}
					s, err := New(b.Context(), &chunkBackend{Sandboxed: backend, chunkSize: chunkSize})
					if err != nil {
						b.Fatal(err)
					}
					handle, err := s.OpenTorrent(b.Context(), &info, hash)
					if err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					for _, index := range order {
						offset := index * block
						if n, err := handle.Piece(info.Piece(offset/pieceSize)).WriteAt(payload, int64(offset%pieceSize)); err != nil || n != block {
							b.Fatalf("write %d: %d, %v", offset, n, err)
						}
					}
					if err := s.receive.flush(); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					check := make([]byte, pieceSize)
					for index := range total / pieceSize {
						if _, err := handle.Piece(info.Piece(index)).ReadAt(check, 0); err != nil || !bytes.Equal(check, bytes.Repeat([]byte{7}, pieceSize)) {
							b.Fatalf("readback %d: %v", index, err)
						}
					}
					if err := handle.Close(); err != nil {
						b.Fatal(err)
					}
					if err := s.Close(); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
			})
		}
	}
}
