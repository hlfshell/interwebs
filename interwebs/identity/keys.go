package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
)

// Create generates a signing key in a new, owner-only file. It never overwrites.
func Create(ctx context.Context, path string) (*Signer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("create signing key: %w", err)
	}
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, errors.Join(err, os.Remove(path))
	}
	return NewSigner(key)
}

// Load restores signing authority. A missing or invalid key is never replaced.
func Load(ctx context.Context, path string) (*Signer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("load signing key: %w", err)
	}
	key, err := io.ReadAll(io.LimitReader(f, ed25519.PrivateKeySize+1))
	err = errors.Join(err, f.Close())
	defer clear(key)
	if err != nil {
		return nil, err
	}
	return NewSigner(key)
}
