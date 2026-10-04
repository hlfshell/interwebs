package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"path"
	"sync"

	"github.com/hlfshell/interweb/interwebs/content"
)

// staging is a private leased store used while assembling a publication.
type staging struct {
	collection *Collection
	id         string
	once       sync.Once
	mu         sync.Mutex
	closed     bool
}

// newStaging creates fresh encrypted staging; abandoned staging is never reopened.
func (s *Collection) newStaging(ctx context.Context) (*staging, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var random [20]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(random[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.open(id); err != nil {
		return nil, err
	}
	s.leases[id]++
	return &staging{collection: s, id: id}, nil
}

func (w *staging) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.once.Do(func() { w.closed = true; w.collection.release(w.id) })
	return nil
}

// read returns a leased, seekable decrypted reader without exposing host handles.
func (w *staging) read(ctx context.Context, name string) (content.Reader, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, fs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := content.ValidatePath(name); err != nil {
		return nil, err
	}
	s := w.collection
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.open(w.id)
	if err != nil {
		return nil, err
	}
	f, err := store.Open(name)
	if err != nil {
		return nil, err
	}
	seek, ok := f.(io.ReadSeeker)
	if !ok {
		return nil, errors.Join(errors.New("store reader is not seekable"), f.Close())
	}
	s.leases[w.id]++
	return &stagingReader{ReadSeeker: seek, file: f, release: func() { s.release(w.id) }, done: ctx.Done(), contextErr: ctx.Err}, nil
}

// write replaces a file atomically and rejects both short and oversized input.
// Input is read outside collection/quota locks, so a source may fetch via the
// same collection without deadlocking encrypted torrent writes.
func (w *staging) write(ctx context.Context, name string, input io.Reader, size int64) (resultErr error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fs.ErrClosed
	}
	if err := content.ValidatePath(name); err != nil {
		return err
	}
	if size < 0 || size > content.MaxBytes {
		return errors.New("invalid staging file size")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s := w.collection
	s.mu.Lock()
	store, err := s.open(w.id)
	var writer Writer
	if err == nil {
		err = s.mutate(w.id, 64<<10, func() error {
			if err := store.MkdirAll(path.Dir(name)); err != nil {
				return err
			}
			var e error
			writer, e = store.Create(name)
			return e
		})
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			resultErr = errors.Join(resultErr, writer.Abort())
			s.mu.Lock()
			u, e := s.backend.Usage(context.Background(), w.id)
			s.quota.mu.Lock()
			s.quota.sizes[w.id] = u.Bytes
			if e != nil {
				s.quota.accountError = e
			}
			s.quota.mu.Unlock()
			s.mu.Unlock()
			resultErr = errors.Join(resultErr, e)
		}
	}()
	buf := make([]byte, 64<<10)
	var offset int64
	for offset < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := min(int64(len(buf)), size-offset)
		n, err := io.ReadFull(input, buf[:want])
		if err != nil {
			return err
		}
		s.mu.Lock()
		err = s.mutate(w.id, 2*int64(n)+128<<10, func() error {
			written, e := writer.WriteAt(buf[:n], offset)
			if e == nil && written != n {
				e = io.ErrShortWrite
			}
			return e
		})
		s.mu.Unlock()
		if err != nil {
			return err
		}
		offset += int64(n)
	}
	var extra [1]byte
	n, err := io.ReadFull(input, extra[:])
	if n != 0 {
		return errors.New("staging input exceeds declared size")
	}
	if err != io.EOF {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	err = s.mutate(w.id, 128<<10, writer.Close)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	committed = true
	return nil
}

type stagingReader struct {
	io.ReadSeeker
	file       fs.File
	release    func()
	done       <-chan struct{}
	contextErr func() error
	once       sync.Once
	err        error
}

func (r *stagingReader) Read(p []byte) (int, error) {
	select {
	case <-r.done:
		return 0, r.contextErr()
	default:
	}
	return r.ReadSeeker.Read(p)
}
func (r *stagingReader) Close() error {
	r.once.Do(func() { r.err = r.file.Close(); r.release() })
	return r.err
}
