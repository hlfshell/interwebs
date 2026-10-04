// Package profile owns exclusive local persistence, not content policy.
package profile

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

type Profile struct {
	dir      string
	lock     *flock.Flock
	once     sync.Once
	closeErr error
}

func Open(ctx context.Context, dir string) (*Profile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, "instance.lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("node directory is already in use")
	}
	return &Profile{dir: dir, lock: lock}, nil
}

func (p *Profile) ReadState(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(p.dir, "state.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if len(b) > 16<<20 {
		return nil, errors.New("profile state exceeds limit")
	}
	return b, err
}
func (p *Profile) WriteState(ctx context.Context, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(p.dir, "state.json"), b)
}
func (p *Profile) Close() error {
	p.once.Do(func() { p.closeErr = p.lock.Unlock() })
	return p.closeErr
}

func AtomicWrite(name string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), name); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// StorageKey defaults to storage.key in dir when name is empty.
// It accepts exactly 32 bytes and reads at most 33 to detect oversized files.
// It never regenerates a missing key when encrypted stores exist.
func StorageKey(ctx context.Context, dir, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == "" {
		name = filepath.Join(dir, "storage.key")
	}
	file, err := os.Open(name)
	if err == nil {
		key, readErr := io.ReadAll(io.LimitReader(file, 33))
		if err := errors.Join(readErr, file.Close()); err != nil {
			clear(key)
			return nil, err
		}
		if len(key) != 32 {
			clear(key)
			return nil, errors.New("storage key must be 32 bytes")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "data"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, errors.New("storage key missing for existing encrypted data")
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	if err = CreateKeyFile(name, key); err != nil {
		return nil, err
	}
	return key, nil
}

func CreateKeyFile(name string, key []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(key)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return errors.Join(err, os.Remove(name))
	}
	d, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
