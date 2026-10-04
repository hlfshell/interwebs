package storage

import (
	"context"
	"errors"
	"testing"
)

// quotaBackend isolates accounting behavior from filesystem commit costs.
type quotaBackend struct {
	Backend
	sizes map[string]int64
	scans int
	err   error
}

func (b *quotaBackend) Growth(_ context.Context, _ string, bound int64) (int64, error) {
	return bound, nil
}

func (b *quotaBackend) Usage(_ context.Context, hash string) (Usage, error) {
	b.scans++
	return Usage{Hash: hash, Bytes: b.sizes[hash]}, b.err
}

func TestQuotaAmortizesScansWithoutUndercounting(t *testing.T) {
	b := &quotaBackend{sizes: make(map[string]int64)}
	s := &Collection{backend: b, quota: newQuota(10000)}
	for i := range 64 {
		if err := s.mutate("store", 100, func() error {
			b.sizes["store"] += 10
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if s.quota.sizes["store"] < b.sizes["store"] {
			t.Fatalf("write %d undercounted storage", i)
		}
	}
	if b.scans != 2 || s.quota.sizes["store"] != 640 {
		t.Fatalf("scans=%d, accounted=%d", b.scans, s.quota.sizes["store"])
	}
}

func TestQuotaReconcilesAllReservationsBeforeRejecting(t *testing.T) {
	b := &quotaBackend{sizes: make(map[string]int64)}
	s := &Collection{backend: b, quota: newQuota(100)}
	for _, hash := range []string{"first", "second", "third"} {
		if err := s.mutate(hash, 40, func() error {
			b.sizes[hash] += 10
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if b.scans != 2 {
		t.Fatalf("expected reconciliation of both prior stores, got %d", b.scans)
	}
	called := false
	err := s.mutate("third", 80, func() error { called = true; return nil })
	if !errors.Is(err, ErrBufferFull) || called {
		t.Fatalf("over-budget mutation: called=%v, err=%v", called, err)
	}
}

func TestQuotaFailureReconcilesPartialWritesAndFailsClosed(t *testing.T) {
	b := &quotaBackend{sizes: make(map[string]int64)}
	s := &Collection{backend: b, quota: newQuota(1000)}
	writeErr := errors.New("partial write")
	err := s.mutate("store", 100, func() error {
		b.sizes["store"] = 20
		return writeErr
	})
	if !errors.Is(err, writeErr) || s.quota.sizes["store"] != 20 || b.scans != 1 {
		t.Fatalf("partial write: %v, sizes=%v, scans=%d", err, s.quota.sizes, b.scans)
	}
	b.err = errors.New("cannot account storage")
	err = s.mutate("store", 100, func() error { return writeErr })
	if !errors.Is(err, writeErr) || !errors.Is(err, b.err) {
		t.Fatalf("lost write/accounting failure: %v", err)
	}
	called := false
	err = s.mutate("store", 1, func() error { called = true; return nil })
	if !errors.Is(err, b.err) || called {
		t.Fatalf("did not fail closed: called=%v, err=%v", called, err)
	}
}
