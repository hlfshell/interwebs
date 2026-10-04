package storage

import (
	"crypto/sha1"
	"hash"
)

// pieceHashes hashes the concatenated source stream, including pieces spanning
// file boundaries. It retains digests only, never a plaintext piece buffer.
type pieceHashes struct {
	hash      hash.Hash
	size      int
	remaining int
	digests   []byte
}

func newPieceHashes(size int) *pieceHashes {
	return &pieceHashes{hash: sha1.New(), size: size, remaining: size}
}

func (h *pieceHashes) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), h.remaining)
		h.hash.Write(p[:n])
		h.remaining -= n
		p = p[n:]
		if h.remaining == 0 {
			h.digests = h.hash.Sum(h.digests)
			h.hash.Reset()
			h.remaining = h.size
		}
	}
	return total, nil
}

func (h *pieceHashes) sum() []byte {
	result := append([]byte(nil), h.digests...)
	if h.remaining != h.size {
		result = h.hash.Sum(result)
	}
	return result
}
