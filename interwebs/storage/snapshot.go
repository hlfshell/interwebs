package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hlfshell/interweb/interwebs/content"
)

// Snapshot streams into encrypted staging before installing an immutable version.
// Slow source reads never hold collection or quota locks.
func (s *Collection) Snapshot(ctx context.Context, source content.Source) (manifest content.Manifest, resultErr error) {
	before, err := source.Fingerprint(ctx)
	if err != nil {
		return manifest, err
	}
	files, err := source.Files(ctx)
	if err != nil {
		return manifest, err
	}
	if len(files) == 0 || len(files) > content.MaxFiles {
		return manifest, errors.New("invalid source file count")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	stage, err := s.newStaging(ctx)
	if err != nil {
		return manifest, err
	}
	installed := false
	defer func() {
		stage.close()
		if installed {
			return
		}
		err := s.Remove(context.Background(), stage.id)
		if err != nil {
			s.quota.mu.Lock()
			s.quota.accountError = err
			s.quota.mu.Unlock()
		}
		resultErr = errors.Join(resultErr, err)
	}()
	info := metainfo.Info{Name: "site", PieceLength: 256 << 10}
	pieces := newPieceHashes(int(info.PieceLength))
	var total int64
	for _, f := range files {
		if err := content.ValidatePath(f.Path); err != nil {
			return manifest, err
		}
		if f.Size < 0 || f.Size > s.limit.Load()-total {
			return manifest, errors.New("source exceeds maximum size")
		}
		total += f.Size
		r, err := source.Open(ctx, f.Path)
		if err != nil {
			return manifest, err
		}
		// Hash the exact bytes being encrypted, without a second staged read.
		err = stage.write(ctx, "site/"+f.Path, io.TeeReader(r, pieces), f.Size)
		err = errors.Join(err, r.Close())
		if err != nil {
			return manifest, err
		}
		info.Files = append(info.Files, metainfo.FileInfo{Path: strings.Split(f.Path, "/"), Length: f.Size})
	}
	after, err := source.Fingerprint(ctx)
	if err != nil {
		return manifest, err
	}
	if before != after {
		return manifest, errors.New("source changed while snapshotting")
	}
	info.Pieces = pieces.sum()
	encoded, err := bencode.Marshal(info)
	if err != nil {
		return manifest, err
	}
	manifest, err = content.ParseMetadata(encoded, s.limit.Load())
	if err != nil {
		return manifest, err
	}
	if err := source.Validate(manifest); err != nil {
		return manifest, err
	}
	// Existing immutable bytes are reused, never rewritten under active readers.
	if existing, err := s.Open(ctx, manifest.Hash()); err == nil {
		defer existing.Close()
		complete := true
		for _, f := range files {
			r, e := existing.Open(ctx, f.Path)
			if e != nil {
				complete = false
				break
			}
			_, e = io.CopyBuffer(io.Discard, r, make([]byte, 64<<10))
			e = errors.Join(e, r.Close())
			if e != nil {
				complete = false
				break
			}
		}
		if complete {
			return manifest, nil
		}
	}
	installed, err = s.install(ctx, stage, manifest.Hash(), encoded)
	if err != nil || installed {
		return manifest, err
	}
	s.mu.Lock()
	_, err = s.open(manifest.Hash())
	if err == nil {
		s.leases[manifest.Hash()]++
	}
	s.mu.Unlock()
	if err != nil {
		return manifest, err
	}
	destination := &staging{collection: s, id: manifest.Hash()}
	defer destination.close()
	for _, f := range files {
		r, err := stage.read(ctx, "site/"+f.Path)
		if err != nil {
			return manifest, err
		}
		err = destination.write(ctx, "site/"+f.Path, r, f.Size)
		err = errors.Join(err, r.Close())
		if err != nil {
			return manifest, err
		}
	}
	if err := destination.write(ctx, infoName, bytes.NewReader(encoded), int64(len(encoded))); err != nil {
		return manifest, err
	}
	return manifest, nil
}
