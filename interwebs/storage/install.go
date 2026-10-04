package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// install avoids decrypting and rewriting a private snapshot when the backend
// can atomically give its closed store the final version name.
func (s *Collection) install(ctx context.Context, stage *staging, hash string, metadata []byte) (bool, error) {
	installer, ok := s.backend.(Installer)
	if !ok {
		return false, nil
	}
	if err := stage.write(ctx, infoName, bytes.NewReader(metadata), int64(len(metadata))); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, fs.ErrClosed
	}
	if store := s.stores[stage.id]; store != nil {
		if err := store.Close(); err != nil {
			return false, err
		}
		delete(s.stores, stage.id)
	}
	err := installer.Install(ctx, stage.id, hash)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	if errors.Is(err, fs.ErrExist) {
		// An incomplete existing version must be repaired through normal writes.
		return false, nil
	}
	s.quota.mu.Lock()
	defer s.quota.mu.Unlock()
	if err != nil {
		// An I/O failure after rename may leave the destination installed. Do not
		// accept further writes with uncertain accounting; reopening scans it.
		s.quota.accountError = err
		return false, fmt.Errorf("install encrypted snapshot: %w", err)
	}
	s.quota.sizes[hash] = s.quota.sizes[stage.id]
	if pending := s.quota.pending[stage.id]; pending > 0 {
		s.quota.pending[hash] = pending
	}
	delete(s.quota.sizes, stage.id)
	delete(s.quota.pending, stage.id)
	return true, nil
}
