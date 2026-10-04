package storage

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	torrentstorage "github.com/anacrolix/torrent/storage"
)

type receiveBackend struct {
	Backend
	commits               atomic.Int64
	started, committed    chan struct{}
	startOnce, commitOnce sync.Once
	block                 <-chan struct{}
	failure               error
}

func (b *receiveBackend) Open(ctx context.Context, hash string, create bool) (Store, error) {
	s, err := b.Backend.Open(ctx, hash, create)
	if err != nil {
		return nil, err
	}
	return &receiveStore{Store: s, backend: b}, nil
}

type receiveStore struct {
	Store
	backend *receiveBackend
}

func (s *receiveStore) Update(name string) (Writer, error) {
	w, err := s.Store.Update(name)
	if err != nil {
		return nil, err
	}
	return &receiveWriter{Writer: w, backend: s.backend}, nil
}
func (s *receiveStore) Create(name string) (Writer, error) {
	w, err := s.Store.Create(name)
	if err != nil {
		return nil, err
	}
	return &receiveWriter{Writer: w, backend: s.backend}, nil
}

type receiveWriter struct {
	Writer
	backend *receiveBackend
}

func (w *receiveWriter) Close() error {
	b := w.backend
	b.startOnce.Do(func() { close(b.started) })
	if b.block != nil {
		<-b.block
	}
	if b.failure != nil {
		return b.failure
	}
	if err := w.Writer.Close(); err != nil {
		return err
	}
	b.commits.Add(1)
	b.commitOnce.Do(func() { close(b.committed) })
	return nil
}

func receiveCollection(t *testing.T, block <-chan struct{}, failure error) (*Collection, *receiveBackend) {
	t.Helper()
	backend, err := NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	b := &receiveBackend{Backend: backend, block: block, failure: failure, started: make(chan struct{}), committed: make(chan struct{})}
	s, err := New(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, b
}

func receiveTorrent(t *testing.T, s *Collection, name string) (torrentstorage.TorrentImpl, metainfo.Info) {
	t.Helper()
	info := metainfo.Info{Name: name, Length: 8 << 20, PieceLength: 256 << 10, Pieces: make([]byte, 20*32)}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.OpenTorrent(t.Context(), &info, sha1.Sum(encoded))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, info
}

func awaitReceive(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("receive worker did not make progress")
	}
}

func TestReceiveCopiesBuffersAndFlushesPartialBatch(t *testing.T) {
	s, b := receiveCollection(t, nil, nil)
	h, info := receiveTorrent(t, s, "video.mp4")
	p := h.Piece(info.Piece(0))
	data := bytes.Repeat([]byte{7}, 16<<10)
	if _, err := p.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	clear(data)
	// No read/verification barrier: the timer must make this durable itself.
	awaitReceive(t, b.committed)
	if _, err := p.ReadAt(data, 0); err != nil || !bytes.Equal(data, bytes.Repeat([]byte{7}, len(data))) {
		t.Fatal("buffer reused or partial batch lost", err)
	}
	if p.Completion().Complete {
		t.Fatal("unverified data marked complete")
	}
}

func TestReceiveBackpressureAndConcurrentDownloads(t *testing.T) {
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	s, b := receiveCollection(t, unblock, nil)
	// Release before fixture cleanup, even on a failed assertion.
	defer release()
	h, info := receiveTorrent(t, s, "one")
	other, otherInfo := receiveTorrent(t, s, "two")
	p := h.Piece(info.Piece(0))
	q := other.Piece(otherInfo.Piece(0))
	data := bytes.Repeat([]byte{3}, receiveBlockBytes)
	for i := 0; i < receiveBatchBytes/len(data); i++ {
		if _, err := p.WriteAt(data, int64(i*len(data))); err != nil {
			t.Fatal(err)
		}
	}
	awaitReceive(t, b.started)
	// The disk is deliberately blocked. Another download can still enqueue.
	for i := receiveBatchBytes / len(data); i < receiveSlots; i++ {
		if _, err := q.WriteAt(data, 0); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.receive.slots) != receiveSlots {
		t.Fatal("payload budget not enforced")
	}
	finished := make(chan error, 1)
	go func() { _, err := q.WriteAt(data, 0); finished <- err }()
	select {
	case err := <-finished:
		t.Fatalf("write bypassed full budget: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backpressure never released")
	}
	if err := q.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if len(s.receive.slots) != 0 {
		t.Fatal("committed payload retained budget")
	}
	check := make([]byte, len(data))
	if _, err := q.ReadAt(check, 0); err != nil || !bytes.Equal(check, data) {
		t.Fatal("second download lost", err)
	}
}

func TestReceiveFailureCannotMarkComplete(t *testing.T) {
	failure := errors.New("disk failed")
	s, _ := receiveCollection(t, nil, failure)
	h, info := receiveTorrent(t, s, "index.html")
	p := h.Piece(info.Piece(0))
	if _, err := p.WriteAt([]byte("bad"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); !errors.Is(err, failure) {
		t.Fatal("commit failure hidden", err)
	}
	if p.Completion().Complete {
		t.Fatal("failed data marked complete")
	}
	if _, err := p.ReadAt(make([]byte, 3), 0); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := p.WriteAt([]byte("retry"), 0); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := h.Close(); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := s.Close(); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := s.Close(); !errors.Is(err, failure) {
		t.Fatal("repeated close lost error", err)
	}
	if len(s.receive.slots) != 0 {
		t.Fatal("failed payload retained")
	}
}

func TestReceiveShutdownUnblocksWritersAndReportsLateFailure(t *testing.T) {
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	failure := errors.New("late disk failure")
	s, b := receiveCollection(t, unblock, failure)
	defer release()
	h, info := receiveTorrent(t, s, "video.mp4")
	p := h.Piece(info.Piece(0))
	data := make([]byte, receiveBlockBytes)
	for range receiveSlots {
		if _, err := p.WriteAt(data, 0); err != nil {
			t.Fatal(err)
		}
	}
	awaitReceive(t, b.started)
	writerDone := make(chan error, 1)
	go func() { _, err := p.WriteAt(data, 0); writerDone <- err }()
	collectionDone := make(chan error, 1)
	go func() { collectionDone <- s.Close() }()
	awaitReceive(t, s.receive.stop)
	select {
	case err := <-writerDone:
		if !errors.Is(err, fs.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown left producer blocked on capacity")
	}
	torrentDone := make(chan error, 1)
	go func() { torrentDone <- h.Close() }()
	select {
	case err := <-torrentDone:
		t.Fatalf("torrent close returned before its commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	for _, done := range []<-chan error{torrentDone, collectionDone} {
		select {
		case err := <-done:
			if !errors.Is(err, failure) {
				t.Fatal("shutdown lost commit failure", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shutdown drain stuck")
		}
	}
	if len(s.receive.slots) != 0 {
		t.Fatal("shutdown retained payloads")
	}
}

func TestReceiveCloseDrainsAndRejectsLateWrites(t *testing.T) {
	s, b := receiveCollection(t, nil, nil)
	h, info := receiveTorrent(t, s, "index.html")
	p := h.Piece(info.Piece(0))
	if _, err := p.WriteAt([]byte("saved"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if b.commits.Load() != 1 {
		t.Fatal("close lost queued write")
	}
	if _, err := p.WriteAt([]byte("late"), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); !errors.Is(err, fs.ErrClosed) {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	backend, err := NewSandboxed(t.Context(), b.Backend.(*Sandboxed).dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	hash := metainfo.Hash(sha1.Sum(encoded)).HexString()
	store, err := backend.Open(t.Context(), hash, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	data, err := fs.ReadFile(store, "site/index.html")
	if err != nil || string(data) != "saved" {
		t.Fatal("close did not persist payload", string(data), err)
	}
}

func TestReceiveQuotaFailureIsReportedByBarrier(t *testing.T) {
	s := testCollection(t, WithStorageLimit(100<<10))
	h, info := receiveTorrent(t, s, "video.mp4")
	p := h.Piece(info.Piece(0))
	if _, err := p.WriteAt(make([]byte, 16<<10), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); !errors.Is(err, ErrBufferFull) {
		t.Fatal("quota failure hidden", err)
	}
	if p.Completion().Complete || len(s.receive.slots) != 0 {
		t.Fatal("quota failure retained or completed data")
	}
}

func TestReceiveBatchPreservesOverlapAndSparseGaps(t *testing.T) {
	s, b := receiveCollection(t, nil, nil)
	h, info := receiveTorrent(t, s, "data.bin")
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	torrent := &encryptedTorrent{storage: s, hash: metainfo.Hash(sha1.Sum(encoded)).HexString(), files: torrentFiles(&info)}
	batch := []receivedWrite{
		{torrent: torrent, offset: 0, data: []byte("abcd")},
		{torrent: torrent, offset: 1, data: []byte("XY")},
		{torrent: torrent, offset: 5, data: []byte("Z")},
	}
	if err := commitReceived(batch, make([]byte, receiveBatchBytes)); err != nil {
		t.Fatal(err)
	}
	if b.commits.Load() != 1 {
		t.Fatal("batch used multiple file commits")
	}
	data := make([]byte, 6)
	if _, err := h.Piece(info.Piece(0)).ReadAt(data, 0); err != nil || string(data) != "aXYd\x00Z" {
		t.Fatal(data, err)
	}
}

func TestReceiveConcurrentOutOfOrderWrites(t *testing.T) {
	s, _ := receiveCollection(t, nil, nil)
	h, info := receiveTorrent(t, s, "video.mp4")
	p := h.Piece(info.Piece(0))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := bytes.Repeat([]byte{byte(i + 1)}, 16<<10)
			if _, err := p.WriteAt(data, int64(i*(16<<10))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	check := make([]byte, 16<<10)
	for i := 0; i < 16; i++ {
		if _, err := p.ReadAt(check, int64(i*len(check))); err != nil || !bytes.Equal(check, bytes.Repeat([]byte{byte(i + 1)}, len(check))) {
			t.Fatalf("block %d: %v", i, err)
		}
	}
}
