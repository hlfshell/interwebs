package network

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/hlfshell/interweb/interwebs/content"
)

// DownloadFile fetches a complete file using normal scheduling or forward reader
// priority. Reader and full-download demand remain independently reference counted.
func (t *Transfer) DownloadFile(ctx context.Context, name string, options ReadOptions) error {
	if options.Mode == Sequential {
		r, err := t.Read(ctx, name, options)
		if err != nil {
			return err
		}
		_, err = io.CopyBuffer(io.Discard, r, make([]byte, 64<<10))
		return errors.Join(err, r.Close())
	}
	if options.Mode != Normal {
		return errors.New("invalid download mode")
	}
	if err := content.ValidatePath(name); err != nil {
		return err
	}
	f := t.files[name]
	if f == nil {
		return fs.ErrNotExist
	}
	t.demandMu.Lock()
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		t.demandMu.Unlock()
		return fs.ErrClosed
	}

	t.fileDownloads[name]++
	t.active++
	t.mu.Unlock()
	f.SetPriority(torrent.PiecePriorityNormal)
	t.demandMu.Unlock()
	defer func() {
		t.demandMu.Lock()
		defer t.demandMu.Unlock()
		t.mu.Lock()
		t.fileDownloads[name]--
		t.active--
		remaining := t.fileDownloads[name]
		t.mu.Unlock()
		if remaining == 0 {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		t.mu.Lock()
		err := t.writeErr
		t.mu.Unlock()
		if err != nil {
			return err
		}
		if f.BytesCompleted() == f.Length() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.done:
			return fs.ErrClosed
		case <-ticker.C:
		}
	}
}
