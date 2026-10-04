package storage

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestStreamingHashesMatchTorrentPieces(t *testing.T) {
	const pieceSize = 256 << 10
	for _, size := range []int{0, 1, pieceSize - 1, pieceSize, pieceSize + 1, 3*pieceSize + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i % 251)
			}
			want, err := metainfo.GeneratePieces(bytes.NewReader(payload), pieceSize, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, block := range []int{1, 97, 64 << 10, pieceSize + 3} {
				h := newPieceHashes(pieceSize)
				for offset := 0; offset < len(payload); offset += block {
					p := payload[offset:min(offset+block, len(payload))]
					if n, err := h.Write(p); n != len(p) || err != nil {
						t.Fatal(n, err)
					}
				}
				first, second := h.sum(), h.sum()
				if !bytes.Equal(first, want) || !bytes.Equal(second, want) {
					t.Fatalf("size %d, block %d: digest mismatch", size, block)
				}
				result := h.sum()
				if len(result) > 0 {
					result[0] ^= 1
					if !bytes.Equal(h.sum(), want) {
						t.Fatal("returned digests alias internal state")
					}
				}
				if n, err := io.WriteString(h, ""); n != 0 || err != nil {
					t.Fatal(n, err)
				}
			}
		})
	}
}
