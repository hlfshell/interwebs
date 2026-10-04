package network

import (
	"context"
	"io/fs"
)

// SetSeeding controls uploads independently of downloads and readers.
func (t *Transport) SetSeeding(ctx context.Context, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fs.ErrClosed
	}
	changed := t.seeding != enabled
	t.seeding = enabled
	for _, transfer := range t.transfers {
		if enabled {
			transfer.torrent.AllowDataUpload()
		} else {
			transfer.torrent.DisallowDataUpload()
		}
	}
	if changed {
		t.wakePeers()
	}
	return nil
}

// ReadMode controls demand scheduling, not the arrival order of network packets.
type ReadMode uint8

const (
	Normal ReadMode = iota
	Sequential
)

// ReadOptions keeps reader demand bounded in either scheduling mode.
type ReadOptions struct{ Mode ReadMode }

// FileStatus reports verified availability of a file; overlapping pieces may
// contribute bytes to multiple neighboring files.
type FileStatus struct {
	Path         string
	Bytes, Total int64
}

func (t *Transfer) Files() []FileStatus {
	result := make([]FileStatus, 0, len(t.files))
	for _, file := range t.manifest.Files() {
		f := t.files[file.Path]
		result = append(result, FileStatus{Path: file.Path, Bytes: f.BytesCompleted(), Total: f.Length()})
	}
	return result
}
