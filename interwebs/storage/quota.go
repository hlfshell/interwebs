package storage

import (
	"errors"
	"sync"
)

// ErrBufferFull means an encrypted mutation could exceed its budget.
var ErrBufferFull = errors.New("encrypted storage capacity exceeded")

type diskQuota struct {
	mu           sync.Mutex
	limit        int64
	sizes        map[string]int64
	pending      map[string]int
	accountError error
}

func newQuota(limit int64) *diskQuota {
	return &diskQuota{limit: limit, sizes: make(map[string]int64), pending: make(map[string]int)}
}
func (q *diskQuota) forget(hash string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.sizes, hash)
	delete(q.pending, hash)
}

func (q *diskQuota) used() int64 {
	var total int64
	for _, size := range q.sizes {
		total += size
	}
	return total
}
