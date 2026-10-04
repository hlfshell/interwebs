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
	"path"
	"path/filepath"
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
	{
		var used int64
		for _, n := range q.sizes {
			used += n
		}
		reserved, err := s.backend.Growth(context.Background(), hash, growth)
		if err != nil {
			return err
		}
		growth = reserved
		if growth > q.limit-used {
			return ErrBufferFull
		}
	}
	err := work()
	size, sizeErr := s.backend.Usage(context.Background(), hash)
	q.sizes[hash] = size.Bytes
	if sizeErr != nil {
		q.accountError = sizeErr
	}
	return errors.Join(err, sizeErr)
}

func directoryBytes(dir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
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
	s.closeErr = errors.Join(append(errs, s.backend.Close())...)
	return s.closeErr
}

type encryptedTorrent struct {
	storage  *Collection
	hash     string
	files    []metainfo.FileInfo
	complete sync.Map
}

type encryptedPiece struct {
	torrent *encryptedTorrent
	piece   metainfo.Piece
}

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
	files := info.UpvertedFiles()
	if len(info.Files) == 0 {
		files[0].Path = []string{info.BestName()}
	}
	t := &encryptedTorrent{storage: s, hash: hash.HexString(), files: files}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return torrentstorage.TorrentImpl{}, fs.ErrClosed
	}
	s.leases[t.hash]++
	s.mu.Unlock()
	var once sync.Once
	return torrentstorage.TorrentImpl{Piece: func(p metainfo.Piece) torrentstorage.PieceImpl { return encryptedPiece{t, p} }, Close: func() error {
		once.Do(func() { s.mu.Lock(); s.leases[t.hash]--; s.mu.Unlock() })
		return nil
	}}, nil
}

func (t *encryptedTorrent) transfer(buffer []byte, offset int64, write bool) (int, error) {
	s := t.storage
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.open(t.hash)
	if err != nil {
		return 0, err
	}
	var start int64
	var done int
	for _, file := range t.files {
		end := start + file.Length
		if offset >= end {
			start = end
			continue
		}
		local := offset - start
		amount := min(int64(len(buffer)), end-offset)
		name := "site/" + strings.Join(file.BestPath(), "/")
		if write {
			// Gaps are encrypted zeros, so reserve their actual extent too.
			var length int64
			if stat, statErr := store.Stat(name); statErr == nil {
				length = stat.Size()
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				return done, statErr
			}
			growth := 2*(max(int64(storeChunkSize), local+amount-length)+int64(storeChunkSize)) + 64<<10
			err = s.mutate(t.hash, growth, func() error {
				if err := store.MkdirAll(path.Dir(name)); err != nil {
					return err
				}
				f, err := store.Update(name)
				if errors.Is(err, fs.ErrNotExist) {
					f, err = store.Create(name)
				}
				if err != nil {
					return err
				}
				defer f.Abort()
				if _, err = f.WriteAt(buffer[:amount], local); err != nil {
					return err
				}
				return f.Close()
			})
		} else {
			var f fs.File
			f, err = store.Open(name)
			if err == nil {
				_, err = f.(io.Seeker).Seek(local, io.SeekStart)
				if err == nil {
					_, err = io.ReadFull(f, buffer[:amount])
				}
				err = errors.Join(err, f.Close())
			}
		}
		if err != nil {
			return done, err
		}
		done += int(amount)
		offset += amount
		buffer = buffer[amount:]
		start = end
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
	return p.torrent.transfer(b, p.piece.Offset()+off, false)
}
func (p encryptedPiece) WriteAt(b []byte, off int64) (int, error) {
	if off < 0 || off > p.piece.Length() || int64(len(b)) > p.piece.Length()-off {
		return 0, io.ErrShortWrite
	}
	if len(b) == 0 {
		return 0, nil
	}
	return p.torrent.transfer(b, p.piece.Offset()+off, true)
}
func (p encryptedPiece) MarkComplete() error {
	p.torrent.complete.Store(p.piece.Index(), true)
	return nil
}
func (p encryptedPiece) MarkNotComplete() error {
	p.torrent.complete.Delete(p.piece.Index())
	return nil
}
func (p encryptedPiece) Completion() torrentstorage.Completion {
	_, ok := p.torrent.complete.Load(p.piece.Index())
	return torrentstorage.Completion{Ok: true, Complete: ok}
}
