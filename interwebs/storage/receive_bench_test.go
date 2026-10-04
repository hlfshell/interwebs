package storage

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// BenchmarkReceiveVerified includes disk commits, readback, SHA-1 verification
// and completion barriers, with multiple simulated peers writing distinct pieces.
func BenchmarkReceiveVerified(b *testing.B) {
	const total = 8 << 20
	const pieceSize = 256 << 10
	const blockSize = 16 << 10
	payload := bytes.Repeat([]byte{7}, blockSize)
	digest := sha1.Sum(bytes.Repeat([]byte{7}, pieceSize))
	info := metainfo.Info{Name: "video.mp4", Length: total, PieceLength: pieceSize, Pieces: bytes.Repeat(digest[:], total/pieceSize)}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		b.Fatal(err)
	}
	hash := sha1.Sum(encoded)
	for _, workers := range []int{1, 4} {
		b.Run(fmt.Sprintf("peers=%d", workers), func(b *testing.B) {
			b.SetBytes(total)
			for range b.N {
				b.StopTimer()
				backend, err := NewSandboxed(b.Context(), b.TempDir(), bytes.Repeat([]byte{1}, 32))
				if err != nil {
					b.Fatal(err)
				}
				s, err := New(b.Context(), backend)
				if err != nil {
					b.Fatal(err)
				}
				h, err := s.OpenTorrent(b.Context(), &info, hash)
				if err != nil {
					s.Close()
					b.Fatal(err)
				}
				b.StartTimer()
				done := make(chan error, workers)
				for worker := 0; worker < workers; worker++ {
					go func() {
						var result error
						defer func() { done <- result }()
						check := make([]byte, pieceSize)
						for index := worker; index < total/pieceSize; index += workers {
							p := h.Piece(info.Piece(index))
							for offset := 0; offset < pieceSize; offset += blockSize {
								if n, err := p.WriteAt(payload, int64(offset)); err != nil || n != len(payload) {
									result = fmt.Errorf("write %d: %d, %v", index, n, err)
									return
								}
							}
							if _, err := p.ReadAt(check, 0); err != nil {
								result = err
								return
							}
							if sha1.Sum(check) != digest {
								result = fmt.Errorf("piece %d hash mismatch", index)
								return
							}
							if err := p.MarkComplete(); err != nil {
								result = err
								return
							}
						}
					}()
				}
				for range workers {
					if err := <-done; err != nil {
						b.Error(err)
					}
				}
				b.StopTimer()
				if err := h.Close(); err != nil {
					b.Error(err)
				}
				if err := s.Close(); err != nil {
					b.Error(err)
				}
				b.StartTimer()
			}
		})
	}
}
