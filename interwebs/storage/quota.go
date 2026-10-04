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
	accountError error
}

func newQuota(limit int64) *diskQuota {
	return &diskQuota{limit: limit, sizes: make(map[string]int64)}
}
func (q *diskQuota) forget(hash string) { q.mu.Lock(); defer q.mu.Unlock(); delete(q.sizes, hash) }
