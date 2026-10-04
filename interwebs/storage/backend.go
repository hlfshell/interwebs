package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/hlfshell/sandboxed"
)

// Backend owns isolated, authenticated version stores. Collection serializes
// mutations. Implementations must account for temporary and retained ciphertext.
// Open with create=false must never create content. Close is idempotent.
type Backend interface {
	Open(context.Context, string, bool) (Store, error)
	List(context.Context) ([]Usage, error)
	Usage(context.Context, string) (Usage, error)
	Growth(context.Context, string, int64) (int64, error)
	Remove(context.Context, string) error
	Close() error
}

// Store exposes virtual paths, not host paths or an underlying sandboxed handle.
// Returned readers must support Seek. Updates commit atomically on Close.
type Store interface {
	Open(string) (fs.File, error)
	Stat(string) (fs.FileInfo, error)
	MkdirAll(string) error
	Remove(string) error
	WriteFile(string, io.Reader) error
	Create(string) (Writer, error)
	Update(string) (Writer, error)
	Close() error
}

type Writer interface {
	io.WriterAt
	Close() error
	Abort() error
}

type Usage struct {
	Hash                 string
	Bytes, MetadataBytes int64
}

type Sandboxed struct {
	mu     sync.Mutex
	dir    string
	key    []byte
	closed bool
}

// NewSandboxed opens no stores until requested. It copies the encryption key.
func NewSandboxed(ctx context.Context, dir string, key []byte) (*Sandboxed, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("storage key must be 32 bytes")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Sandboxed{dir: dir, key: append([]byte(nil), key...)}, nil
}

func (b *Sandboxed) Open(ctx context.Context, hash string, create bool) (Store, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, fs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validStoreHash(hash) {
		return nil, errors.New("invalid store hash")
	}
	if entry, err := os.Lstat(filepath.Join(b.dir, hash)); err == nil {
		if !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			return nil, fs.ErrPermission
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if !create {
		if _, err := b.Usage(ctx, hash); err != nil {
			return nil, err
		}
	}
	s, err := sandboxed.OpenStore(filepath.Join(b.dir, hash), sandboxed.WithEncryption(b.key), sandboxed.WithChunkSize(storeChunkSize))
	if err != nil {
		return nil, err
	}
	return &sandboxStore{s}, nil
}

func (b *Sandboxed) Usage(ctx context.Context, hash string) (Usage, error) {
	if err := ctx.Err(); err != nil {
		return Usage{}, err
	}
	if !validStoreHash(hash) {
		return Usage{}, errors.New("invalid store hash")
	}
	dir := filepath.Join(b.dir, hash)
	stat, err := os.Stat(filepath.Join(dir, "manifest"))
	if err != nil {
		return Usage{}, err
	}
	size, err := directoryBytes(dir)
	return Usage{Hash: hash, Bytes: size, MetadataBytes: stat.Size()}, err
}

func (b *Sandboxed) List(ctx context.Context) ([]Usage, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Usage, 0, len(entries))
	for _, entry := range entries {
		if !validStoreHash(entry.Name()) {
			continue
		}
		u, err := b.Usage(ctx, entry.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// Growth takes a conservative payload/staging bound from the mutation adapter.
func (b *Sandboxed) Growth(ctx context.Context, hash string, bound int64) (int64, error) {
	u, err := b.Usage(ctx, hash)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if bound < 0 || u.MetadataBytes > (math.MaxInt64-bound)/2 {
		return 0, errors.New("storage reservation overflow")
	}
	return bound + 2*u.MetadataBytes, nil
}

func (b *Sandboxed) Remove(ctx context.Context, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validStoreHash(hash) {
		return errors.New("invalid store hash")
	}
	return os.RemoveAll(filepath.Join(b.dir, hash))
}

func (b *Sandboxed) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	clear(b.key)
	return nil
}

type sandboxStore struct{ store *sandboxed.Store }

func (s *sandboxStore) Open(name string) (fs.File, error)        { return s.store.Open(name) }
func (s *sandboxStore) Stat(name string) (fs.FileInfo, error)    { return s.store.Stat(name) }
func (s *sandboxStore) MkdirAll(name string) error               { return s.store.MkdirAll(name) }
func (s *sandboxStore) WriteFile(name string, r io.Reader) error { return s.store.WriteFile(name, r) }
func (s *sandboxStore) Create(name string) (Writer, error)       { return s.store.Create(name) }
func (s *sandboxStore) Update(name string) (Writer, error)       { return s.store.Update(name) }
func (s *sandboxStore) Remove(name string) error                 { return s.store.Remove(name) }
func (s *sandboxStore) Close() error                             { return s.store.Close() }
