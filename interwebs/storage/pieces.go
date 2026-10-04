// Package storage manages encrypted content stores and torrent piece I/O.
package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	torrentstorage "github.com/anacrolix/torrent/storage"
	"github.com/hlfshell/interweb/interwebs/content"
)

const storeChunkSize = 64 << 10
const infoName = "metadata/torrent"

// Collection owns one sandboxed instance per torrent. Short-lived read
// handles avoid retaining obsolete ciphertext while HTTP clients stream slowly.
type Collection struct {
	mu       sync.Mutex
	stores   map[string]Store
	limit    *atomic.Int64
	quota    *diskQuota
	closed   bool
	backend  Backend
	leases   map[string]int
	closeErr error
	receive  *receiver
}

func validStoreHash(hash string) bool {
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == 20 && strings.ToLower(hash) == hash
}

func (s *Collection) open(hash string) (Store, error) {
	if s.closed {
		return nil, fs.ErrClosed
	}
	if !validStoreHash(hash) {
		return nil, errors.New("invalid encrypted store identifier")
	}
	var err error
	if store := s.stores[hash]; store != nil {
		return store, nil
	}
	var store Store
	create := func() error {
		var openErr error
		store, openErr = s.backend.Open(context.Background(), hash, true)
		return openErr
	}
	_, statErr := s.backend.Usage(context.Background(), hash)
	if errors.Is(statErr, os.ErrNotExist) {
		err = s.mutate(hash, 16<<10, create)
	} else if statErr != nil {
		err = statErr
	} else {
		err = create()
	}
	if err != nil {
		if store != nil {
			store.Close()
		}
		return nil, fmt.Errorf("open encrypted store %s: %w", hash, err)
	}
	s.stores[hash] = store
	return store, nil
}

func (s *Collection) info(hash string) ([]byte, error) {
	if !validStoreHash(hash) {
		return nil, errors.New("invalid encrypted store identifier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.backend.Usage(context.Background(), hash); err != nil {
		return nil, err
	}
	store, err := s.open(hash)
	if err != nil {
		return nil, err
	}
	f, err := store.Open(infoName)
	if err != nil {
		return nil, err
	}
	encoded, err := io.ReadAll(io.LimitReader(f, content.MaxMetadataBytes+1))
	err = errors.Join(err, f.Close())
	if len(encoded) > content.MaxMetadataBytes {
		return nil, errors.New("metadata too large")
	}
	return encoded, err
}

func (s *Collection) putInfo(hash string, info []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.open(hash)
	if err != nil {
		return err
	}
	return s.mutate(hash, int64(len(info))*4+64<<10, func() error {
		if err := store.MkdirAll("metadata"); err != nil {
			return err
		}
		return store.WriteFile(infoName, bytes.NewReader(info))
	})
}

// mutate reserves staging and manifest headroom before any encrypted write.
// The quota lock serializes reservations with cache accounting and other writes.
// Between exact scans, sizes include conservative upper bounds, never estimates
// below actual usage. Near the limit we reconcile before rejecting a write.
func (s *Collection) mutate(hash string, growth int64, work func() error) error {
	if s.quota == nil {
		return work()
	}
	q := s.quota
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.accountError != nil {
		return fmt.Errorf("account encrypted storage: %w", q.accountError)
	}
	reserved, err := s.backend.Growth(context.Background(), hash, growth)
	if err != nil {
		return err
	}
	if reserved < 0 {
		return errors.New("negative storage reservation")
	}
	if reserved > q.limit-q.used() {
		for dirty := range q.pending {
			if err := s.reconcile(dirty); err != nil {
				return err
			}
		}
		if reserved > q.limit-q.used() {
			return ErrBufferFull
		}
	}
	q.sizes[hash] += reserved
	q.pending[hash]++
	err = work()
	// Failed writes may leave temporary ciphertext. Reconcile even on failure,
	// and fail closed if its size cannot be established.
	if err != nil || q.pending[hash] >= 32 {
		return errors.Join(err, s.reconcile(hash))
	}
	return nil
}

// reconcile requires the quota lock. It releases unused reservations only after
// obtaining an exact count, including staged and retained ciphertext.
func (s *Collection) reconcile(hash string) error {
	size, sizeErr := s.backend.Usage(context.Background(), hash)
	if sizeErr != nil {
		s.quota.accountError = sizeErr
		return sizeErr
	}
	// Preserve checkpoint headroom even after releasing unused write reserves:
	// sandboxed Close can checkpoint a WAL after the last payload mutation.
	headroom, err := s.backend.Growth(context.Background(), hash, 0)
	if err != nil || headroom < 0 || size.Bytes > math.MaxInt64-headroom {
		if err == nil {
			err = errors.New("invalid storage checkpoint reservation")
		}
		s.quota.accountError = err
		return err
	}
	s.quota.sizes[hash] = size.Bytes + headroom
	delete(s.quota.pending, hash)
	return nil
}

func directoryBytes(dir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		// The store's deferred garbage collector can unlink obsolete chunks
		// while we scan. Missing children contribute no bytes; other failures
		// (including a missing store root) must still fail closed.
		if name != dir && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in storage directory")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > math.MaxInt64-size {
			return errors.New("invalid storage entry size or type")
		}
		size += info.Size()
		return nil
	})
	return size, err
}

func (s *Collection) remove(hash string) error {
	if !validStoreHash(hash) {
		return errors.New("invalid encrypted store identifier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fs.ErrClosed
	}
	if s.leases[hash] > 0 {
		return ErrInUse
	}
	if store := s.stores[hash]; store != nil {
		if err := store.Close(); err != nil {
			return err
		}
		delete(s.stores, hash)
	}
	if err := s.backend.Remove(context.Background(), hash); err != nil {
		return err
	}
	s.quota.forget(hash)
	return nil
}

func (s *Collection) Close() error {
	// Drain accepted downloads before closing the stores they target.
	receiveErr := s.receive.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	var errs []error
	for hash, store := range s.stores {
		if err := store.Close(); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(s.stores, hash)
	}
	s.closeErr = errors.Join(append(errs, receiveErr, s.backend.Close())...)
	return s.closeErr
}

type encryptedTorrent struct {
	mu       sync.RWMutex
	closed   bool
	storage  *Collection
	hash     string
	files    []torrentFile
	complete sync.Map
}

type torrentFile struct {
	name       string
	start, end int64
}

// torrentFiles builds the immutable layout once, rather than walking metadata
// and rebuilding paths for every incoming block and verification read.
func torrentFiles(info *metainfo.Info) []torrentFile {
	files := info.UpvertedFiles()
	layout := make([]torrentFile, 0, len(files))
	var offset int64
	for _, file := range files {
		name := strings.Join(file.BestPath(), "/")
		if len(info.Files) == 0 {
			name = info.BestName()
		}
		if file.Length > 0 {
			layout = append(layout, torrentFile{name: "site/" + name, start: offset, end: offset + file.Length})
		}
		offset += file.Length
	}
	return layout
}

func (t *encryptedTorrent) fileAt(offset int64) int {
	return sort.Search(len(t.files), func(i int) bool { return t.files[i].end > offset })
}

type encryptedPiece struct {
	torrent *encryptedTorrent
	piece   metainfo.Piece
}

// OpenTorrent adapts a validated manifest to torrent piece storage. Writes copy
// into a bounded asynchronous queue; reads, MarkComplete and Close wait for prior
// writes to commit. A crash can lose queued, unverified blocks (downloaded again
// on restart). Async failures surface on subsequent I/O and Close; reopen the
// collection to retry after fixing the underlying storage failure.
func (s *Collection) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (torrentstorage.TorrentImpl, error) {
	if err := ctx.Err(); err != nil {
		return torrentstorage.TorrentImpl{}, err
	}
	encodedManifest, encodeErr := bencode.Marshal(info)
	if encodeErr != nil {
		return torrentstorage.TorrentImpl{}, encodeErr
	}
	manifest, err := content.ParseMetadata(encodedManifest, s.limit.Load())
	if err != nil {
		return torrentstorage.TorrentImpl{}, err
	}
	if manifest.Hash() != hash.HexString() {
		return torrentstorage.TorrentImpl{}, errors.New("torrent storage metadata hash mismatch")
	}
	// Metadata is persisted inside the encrypted manifest boundary as a virtual file.
	encoded, err := bencode.Marshal(info)
	if err != nil {
		return torrentstorage.TorrentImpl{}, err
	}
	if err := s.putInfo(hash.HexString(), encoded); err != nil {
		return torrentstorage.TorrentImpl{}, err
	}
	t := &encryptedTorrent{storage: s, hash: hash.HexString(), files: torrentFiles(info)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return torrentstorage.TorrentImpl{}, fs.ErrClosed
	}
	s.leases[t.hash]++
	s.mu.Unlock()
	var once sync.Once
	var closeErr error
	return torrentstorage.TorrentImpl{Piece: func(p metainfo.Piece) torrentstorage.PieceImpl { return encryptedPiece{t, p} }, Close: func() error {
		once.Do(func() {
			t.mu.Lock()
			t.closed = true
			t.mu.Unlock()
			closeErr = s.receive.flush()
			if errors.Is(closeErr, fs.ErrClosed) {
				// Collection shutdown owns the drain now. Do not release this
				// store's lease or lose a late commit error before it finishes.
				<-s.receive.done
				closeErr = s.receive.failure()
			}
			s.mu.Lock()
			s.leases[t.hash]--
			s.mu.Unlock()
		})
		return closeErr
	}}, nil
}

func (t *encryptedTorrent) readAt(buffer []byte, offset int64) (int, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed {
		return 0, fs.ErrClosed
	}
	s := t.storage
	if err := s.receive.flush(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.open(t.hash)
	if err != nil {
		return 0, err
	}
	var done int
	for _, file := range t.files[t.fileAt(offset):] {
		local := offset - file.start
		amount := min(int64(len(buffer)), file.end-offset)
		name := file.name
		var f fs.File
		f, err = store.Open(name)
		if err == nil {
			_, err = f.(io.Seeker).Seek(local, io.SeekStart)
			if err == nil {
				_, err = io.ReadFull(f, buffer[:amount])
			}
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			return done, err
		}
		done += int(amount)
		offset += amount
		buffer = buffer[amount:]
		if len(buffer) == 0 {
			return done, nil
		}
	}
	return done, io.EOF
}

func (p encryptedPiece) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 || off > p.piece.Length() || int64(len(b)) > p.piece.Length()-off {
		return 0, io.EOF
	}
	if len(b) == 0 {
		return 0, nil
	}
	return p.torrent.readAt(b, p.piece.Offset()+off)
}
func (p encryptedPiece) WriteAt(b []byte, off int64) (int, error) {
	if off < 0 || off > p.piece.Length() || int64(len(b)) > p.piece.Length()-off {
		return 0, io.ErrShortWrite
	}
	if len(b) == 0 {
		return 0, nil
	}
	t := p.torrent
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed {
		return 0, fs.ErrClosed
	}
	t.complete.Delete(p.piece.Index())
	return t.storage.receive.write(t, b, p.piece.Offset()+off)
}
func (p encryptedPiece) MarkComplete() error {
	p.torrent.mu.Lock()
	defer p.torrent.mu.Unlock()
	if p.torrent.closed {
		return fs.ErrClosed
	}
	if err := p.torrent.storage.receive.flush(); err != nil {
		return err
	}
	p.torrent.complete.Store(p.piece.Index(), true)
	return nil
}
func (p encryptedPiece) MarkNotComplete() error {
	p.torrent.complete.Delete(p.piece.Index())
	return nil
}
func (p encryptedPiece) Completion() torrentstorage.Completion {
	if p.torrent.storage.receive.failure() != nil {
		return torrentstorage.Completion{Ok: false}
	}
	_, ok := p.torrent.complete.Load(p.piece.Index())
	return torrentstorage.Completion{Ok: true, Complete: ok}
}
