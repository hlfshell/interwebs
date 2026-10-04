package storage

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hlfshell/interweb/interwebs/content"
)

var ErrInUse = errors.New("content is in use")

type options struct{ maxBytes, storageLimit int64 }

// Option configures content and encrypted-storage limits.
type Option func(*options) error

// WithMaxSiteBytes bounds each version; the default is 512 MiB.
func WithMaxSiteBytes(n int64) Option {
	return func(o *options) error {
		if n < 1 || n > content.MaxBytes {
			return errors.New("invalid content maximum")
		}
		o.maxBytes = n
		return nil
	}
}

// WithStorageLimit bounds retained and staged ciphertext; the default is 2 GiB.
func WithStorageLimit(n int64) Option {
	return func(o *options) error {
		if n < 1 {
			return errors.New("invalid storage budget")
		}
		o.storageLimit = n
		return nil
	}
}

// New takes ownership of backend, including on initialization failure.
func New(ctx context.Context, backend Backend, opts ...Option) (*Collection, error) {
	if backend == nil {
		return nil, errors.New("storage backend required")
	}
	o := options{maxBytes: content.DefaultMaxBytes, storageLimit: 2 << 30}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.Join(errors.New("nil storage option"), backend.Close())
		}
		if err := opt(&o); err != nil {
			return nil, errors.Join(err, backend.Close())
		}
	}
	usage, err := backend.List(ctx)
	if err != nil {
		return nil, errors.Join(err, backend.Close())
	}
	s := &Collection{backend: backend, stores: make(map[string]Store), leases: make(map[string]int), limit: &atomic.Int64{}, quota: newQuota(o.storageLimit)}
	s.limit.Store(o.maxBytes)
	for _, u := range usage {
		s.quota.sizes[u.Hash] = u.Bytes
	}
	return s, nil
}

func (s *Collection) Manifest(ctx context.Context, hash string) (content.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return content.Manifest{}, err
	}
	encoded, err := s.info(hash)
	if err != nil {
		return content.Manifest{}, err
	}
	m, err := content.ParseMetadata(encoded, s.limit.Load())
	if err == nil && m.Hash() != hash {
		err = errors.New("stored metadata hash mismatch")
	}
	return m, err
}

func (s *Collection) Prepare(ctx context.Context, m content.Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := content.ParseMetadata(m.Metadata(), s.limit.Load()); err != nil {
		return err
	}
	return s.putInfo(m.Hash(), m.Metadata())
}

func (s *Collection) Usage(ctx context.Context) ([]Usage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fs.ErrClosed
	}
	return s.backend.List(ctx)
}

func (s *Collection) Remove(ctx context.Context, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.remove(hash)
}

// Version is a leased view. Closing it leaves independent readers alive.
type Version struct {
	collection *Collection
	manifest   content.Manifest
	mu         sync.Mutex
	closed     bool
}

func (s *Collection) Open(ctx context.Context, hash string) (*Version, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fs.ErrClosed
	}
	s.leases[hash]++
	s.mu.Unlock()
	m, err := s.Manifest(ctx, hash)
	if err != nil {
		s.release(hash)
		return nil, err
	}
	return &Version{collection: s, manifest: m}, nil
}

func (s *Collection) release(hash string)     { s.mu.Lock(); defer s.mu.Unlock(); s.leases[hash]-- }
func (v *Version) Manifest() content.Manifest { return v.manifest }
func (v *Version) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		v.closed = true
		v.collection.release(v.manifest.Hash())
	}
	return nil
}

func (v *Version) Open(ctx context.Context, name string) (content.Reader, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, fs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := v.manifest.File(name)
	if err != nil {
		return nil, err
	}
	var info metainfo.Info
	if err := bencode.Unmarshal(v.manifest.Metadata(), &info); err != nil {
		return nil, err
	}
	files := info.UpvertedFiles()
	if len(info.Files) == 0 {
		files[0].Path = []string{info.BestName()}
	}
	var offset int64
	for _, f := range v.manifest.Files() {
		if f.Path == name {
			break
		}
		offset += f.Size
	}
	s := v.collection
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fs.ErrClosed
	}
	s.leases[v.manifest.Hash()]++
	reader := &verifiedReader{
		torrent: &encryptedTorrent{storage: s, hash: v.manifest.Hash(), files: files},
		info:    info, offset: offset, size: file.Size, done: ctx.Done(), contextErr: ctx.Err,
	}
	return content.LimitReader(reader, file.Size), nil
}

type verifiedReader struct {
	torrent                *encryptedTorrent
	info                   metainfo.Info
	offset, size, position int64
	done                   <-chan struct{}
	contextErr             func() error
	once                   sync.Once
	closed                 bool
}

func (r *verifiedReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	select {
	case <-r.done:
		return 0, r.contextErr()
	default:
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.position >= r.size {
		return 0, io.EOF
	}
	absolute := r.offset + r.position
	index := int(absolute / r.info.PieceLength)
	piece := r.info.Piece(index)
	buffer := make([]byte, piece.Length())
	if _, err := r.torrent.transfer(buffer, piece.Offset(), false); err != nil {
		return 0, err
	}
	sum := sha1.Sum(buffer)
	if !bytes.Equal(sum[:], r.info.Pieces[index*20:(index+1)*20]) {
		return 0, errors.New("stored piece hash mismatch")
	}
	within := absolute - piece.Offset()
	count := min(int64(len(p)), min(int64(len(buffer))-within, r.size-r.position))
	copy(p, buffer[within:within+count])
	r.position += count
	return int(count), nil
}
func (r *verifiedReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	select {
	case <-r.done:
		return 0, r.contextErr()
	default:
	}
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.position
	case io.SeekEnd:
		base = r.size
	default:
		return 0, errors.New("invalid seek origin")
	}
	if offset < -base || offset > r.size-base {
		return 0, errors.New("seek outside content")
	}
	r.position = base + offset
	return r.position, nil
}
func (r *verifiedReader) Close() error {
	r.once.Do(func() { r.closed = true; r.torrent.storage.release(r.torrent.hash) })
	return nil
}
