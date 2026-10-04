package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sync"
	"time"
)

const (
	receiveBlockBytes = 64 << 10
	receiveBatchBytes = 256 << 10
	receiveSlots      = 64
	receiveDelay      = 25 * time.Millisecond
)

type receivedWrite struct {
	torrent *encryptedTorrent
	offset  int64
	data    []byte
	barrier chan error
}

// receiver accepts concurrent downloads without holding the storage I/O lock.
// Slots cover queued AND in-flight payloads: at most 4 MiB per Collection, plus
// one 256 KiB merge buffer. A successful enqueue is not yet a durable write.
// Reads, verification, torrent close and collection close are commit barriers.
// The first asynchronous write failure is sticky; reopen to retry safely.
type receiver struct {
	mu     sync.Mutex
	closed bool
	queue  chan receivedWrite
	slots  chan struct{}
	stop   chan struct{}
	done   chan struct{}
	errMu  sync.Mutex
	err    error
}

func newReceiver() *receiver {
	r := &receiver{
		queue: make(chan receivedWrite, receiveSlots),
		slots: make(chan struct{}, receiveSlots),
		stop:  make(chan struct{}), done: make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *receiver) failure() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

func (r *receiver) write(t *encryptedTorrent, data []byte, offset int64) (int, error) {
	var done int
	for len(data) > 0 {
		select {
		case r.slots <- struct{}{}:
		case <-r.stop:
			return done, fs.ErrClosed
		}
		r.mu.Lock()
		err := r.failure()
		if r.closed {
			err = errors.Join(fs.ErrClosed, err)
		}
		if err != nil {
			r.mu.Unlock()
			<-r.slots
			return done, err
		}
		amount := min(len(data), receiveBlockBytes)
		// Copy before returning: torrent peers immediately reuse their buffers.
		r.queue <- receivedWrite{torrent: t, offset: offset, data: append([]byte(nil), data[:amount]...)}
		r.mu.Unlock()
		done += amount
		offset += int64(amount)
		data = data[amount:]
	}
	return done, nil
}

func (r *receiver) flush() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.Join(fs.ErrClosed, r.failure())
	}
	result := make(chan error, 1)
	r.queue <- receivedWrite{barrier: result}
	r.mu.Unlock()
	return <-result
}

func (r *receiver) close() error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.stop)
	}
	r.mu.Unlock()
	<-r.done
	return r.failure()
}

func (r *receiver) run() {
	defer close(r.done)
	timer := time.NewTimer(receiveDelay)
	timer.Stop()
	defer timer.Stop()
	var tick <-chan time.Time
	pending := make([]receivedWrite, 0, receiveSlots)
	scratch := make([]byte, receiveBatchBytes)
	defer clear(scratch)
	var size int

	flush := func() {
		if len(pending) == 0 {
			return
		}
		if r.failure() == nil {
			if err := commitReceived(pending, scratch); err != nil {
				r.errMu.Lock()
				r.err = fmt.Errorf("commit received data: %w", err)
				r.errMu.Unlock()
			}
		}
		for i := range pending {
			clear(pending[i].data)
			pending[i] = receivedWrite{}
			<-r.slots
		}
		clear(scratch)
		pending, size = pending[:0], 0
		timer.Stop()
		tick = nil
	}
	accept := func(write receivedWrite) {
		if write.barrier != nil {
			flush()
			write.barrier <- r.failure()
			return
		}
		if len(pending) == 0 {
			timer.Reset(receiveDelay)
			tick = timer.C
		}
		pending = append(pending, write)
		size += len(write.data)
		if size >= receiveBatchBytes || len(pending) == receiveSlots || r.failure() != nil {
			flush()
		}
	}
	for {
		select {
		case write := <-r.queue:
			accept(write)
		case <-tick:
			flush()
		case <-r.stop:
			// Enqueue and close serialize under mu, so no new request can
			// arrive after this drain. Wake barriers even after a disk error.
			for {
				select {
				case write := <-r.queue:
					accept(write)
				default:
					flush()
					return
				}
			}
		}
	}
}

type fileWrite struct {
	offset int64
	data   []byte
}

// commitReceived combines edits to each file into one sandboxed transaction.
// Preserve arrival order, including overlapping writes; never fill sparse gaps.
func commitReceived(batch []receivedWrite, scratch []byte) error {
	s := batch[0].torrent.storage
	s.mu.Lock()
	defer s.mu.Unlock()
	type target struct{ hash, name string }
	files := make(map[target][]fileWrite)
	var order []target
	for _, write := range batch {
		t, offset, data := write.torrent, write.offset, write.data
		for _, file := range t.files[t.fileAt(offset):] {
			amount := min(int64(len(data)), file.end-offset)
			key := target{t.hash, file.name}
			if _, ok := files[key]; !ok {
				order = append(order, key)
			}
			files[key] = append(files[key], fileWrite{offset - file.start, data[:amount]})
			offset += amount
			data = data[amount:]
			if len(data) == 0 {
				break
			}
		}
		if len(data) != 0 {
			return io.ErrShortWrite
		}
	}
	for _, key := range order {
		store, err := s.open(key.hash)
		if err != nil {
			return err
		}
		writes := files[key]
		var growth int64 = 64 << 10
		for _, write := range writes {
			chunks := (write.offset%storeChunkSize + int64(len(write.data)) + storeChunkSize - 1) / storeChunkSize
			growth += 2 * chunks * (storeChunkSize + 256)
		}
		if err := s.mutate(key.hash, growth, func() error {
			f, err := store.Update(key.name)
			if errors.Is(err, fs.ErrNotExist) {
				if err := store.MkdirAll(path.Dir(key.name)); err != nil {
					return err
				}
				f, err = store.Create(key.name)
			}
			if err != nil {
				return err
			}
			defer f.Abort()
			for i := 0; i < len(writes); {
				start := writes[i].offset
				n := copy(scratch, writes[i].data)
				i++
				for i < len(writes) && writes[i].offset == start+int64(n) && n+len(writes[i].data) <= len(scratch) {
					n += copy(scratch[n:], writes[i].data)
					i++
				}
				if written, err := f.WriteAt(scratch[:n], start); err != nil {
					return err
				} else if written != n {
					return io.ErrShortWrite
				}
			}
			return f.Close()
		}); err != nil {
			return err
		}
	}
	return nil
}
