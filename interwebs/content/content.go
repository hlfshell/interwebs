// Package content describes immutable torrent versions independently of their use.
package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hlfshell/interweb/interwebs/identity"
)

const DefaultMaxBytes int64 = 512 << 20
const MaxBytes int64 = 1 << 30
const MaxFiles = 10000
const MaxMetadataBytes = 8 << 20

var ErrUnresolved = errors.New("content manifest is not resolved")
var ErrUnavailable = errors.New("content is unavailable")

// Descriptor is a local source or a remote reference. No mutable runtime state is held here.
type Descriptor struct {
	Name    string
	Address identity.Identity
	// SourceID distinguishes local sources without exposing their host paths.
	SourceID string
}

// Content supplies content-specific rules; Node owns resolution and publication.
type Content interface {
	Descriptor() Descriptor
	Validate(Manifest) error
}

// Reader never exposes a host handle. Implementations bind reads to the open context.
type Reader interface {
	io.ReadSeeker
	io.Closer
}

// Source is an optional, locally publishable content capability. Fingerprint must
// change when the source layout or file metadata changes. Open must reject escapes.
type Source interface {
	Content
	Files(context.Context) ([]File, error)
	Open(context.Context, string) (Reader, error)
	Fingerprint(context.Context) ([32]byte, error)
}

type File struct {
	Path string
	Size int64
}

// Manifest keeps its validated representation private; every accessor returns copies.
type Manifest struct {
	encoded []byte
	hash    string
	size    int64
	files   []File
}

func ParseMetadata(encoded []byte, limit int64) (Manifest, error) {
	if limit < 1 || limit > MaxBytes || len(encoded) == 0 || len(encoded) > MaxMetadataBytes {
		return Manifest{}, errors.New("invalid content or metadata limit")
	}
	var info metainfo.Info
	if err := bencode.Unmarshal(encoded, &info); err != nil {
		return Manifest{}, fmt.Errorf("decode torrent metadata: %w", err)
	}
	if info.MetaVersion != 0 || info.PieceLength <= 0 || info.PieceLength > 4<<20 {
		return Manifest{}, errors.New("only bounded BitTorrent v1 content is supported")
	}
	files := info.UpvertedFiles()
	if len(files) == 0 || len(files) > MaxFiles {
		return Manifest{}, errors.New("content file limit exceeded")
	}
	manifest := Manifest{encoded: append([]byte(nil), encoded...), files: make([]File, 0, len(files))}
	seen := make(map[string]bool)
	for _, file := range files {
		name := strings.Join(file.BestPath(), "/")
		if len(info.Files) == 0 {
			name = info.BestName()
		}
		if err := ValidatePath(name); err != nil {
			return Manifest{}, err
		}
		if seen[strings.ToLower(name)] || strings.Contains(file.Attr, "l") {
			return Manifest{}, fmt.Errorf("duplicate or linked content path: %q", name)
		}
		if file.Length < 0 || file.Length > limit-manifest.size {
			return Manifest{}, errors.New("content exceeds maximum size")
		}
		seen[strings.ToLower(name)] = true
		manifest.files = append(manifest.files, File{Path: name, Size: file.Length})
		manifest.size += file.Length
	}
	if int64(len(info.Pieces)) != ((manifest.size+info.PieceLength-1)/info.PieceLength)*20 {
		return Manifest{}, errors.New("invalid piece hashes")
	}
	meta := metainfo.MetaInfo{InfoBytes: encoded}
	manifest.hash = meta.HashInfoBytes().HexString()
	return manifest, nil
}

func ValidatePath(name string) error {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
		return fmt.Errorf("unsafe content path: %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return fmt.Errorf("hidden content path: %q", name)
		}
	}
	return nil
}

func (m Manifest) Hash() string     { return m.hash }
func (m Manifest) Size() int64      { return m.size }
func (m Manifest) Files() []File    { return append([]File(nil), m.files...) }
func (m Manifest) Metadata() []byte { return append([]byte(nil), m.encoded...) }
func (m Manifest) File(name string) (File, error) {
	if err := ValidatePath(name); err != nil {
		return File{}, err
	}
	for _, file := range m.files {
		if file.Path == name {
			return file, nil
		}
	}
	return File{}, fs.ErrNotExist
}
