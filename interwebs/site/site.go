// Package site implements browser-viewable content without owning its runtime.
package site

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/identity"
)

// Site describes a local source or a remote address. Nodes own mutable versions.
type Site struct {
	descriptor      content.Descriptor
	folder          string
	filterFileTypes bool
}

// Option configures local folder publication. Callers must synchronize any
// application of options to an existing Site with reads and publication.
type Option func(*Site)

// WithFileTypeFiltering controls whether publication includes only recognized
// site resource types. Filtering defaults to false. Disabling it includes all
// regular files with valid paths, but does not relax path or symlink protections
// or change which resource types the browser server permits.
func WithFileTypeFiltering(enabled bool) Option {
	return func(s *Site) {
		s.filterFileTypes = enabled
	}
}

// New validates a local folder without copying its contents or starting workers.
func New(ctx context.Context, folder string, opts ...Option) (*Site, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	s := &Site{folder: abs, descriptor: content.Descriptor{Name: filepath.Base(abs), SourceID: fmt.Sprintf("%x", sha256.Sum256([]byte(abs)))}}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil site option")
		}
		opt(s)
	}

	files, err := s.Files(ctx)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if file.Path == "index.html" {
			return s, nil
		}
	}
	return nil, errors.New("site requires root index.html")
}

// FromMagnet parses an address without fetching its manifest.
func FromMagnet(raw string) (*Site, error) {
	address, err := identity.ParseMagnet(raw)
	if err != nil {
		return nil, err
	}
	return &Site{descriptor: content.Descriptor{Name: address.ID()[:12], Address: address}}, nil
}

func (s *Site) Descriptor() content.Descriptor { return s.descriptor }

// EntryPoint is the browser capability; generic Content need not implement it.
func (s *Site) EntryPoint() string { return "index.html" }

// Local reports whether this Site has a publishable source, not whether it hosts.
func (s *Site) Local() bool { return s.folder != "" }

// ValidateStorageDir prevents a publication from recursively including Node data.
func (s *Site) ValidateStorageDir(dir string) error {
	if s.folder == "" {
		return nil
	}
	rel, err := filepath.Rel(s.folder, dir)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("source folder must not contain node data")
	}
	return nil
}

func (s *Site) Validate(manifest content.Manifest) error {
	if manifest.Hash() == "" {
		return content.ErrUnresolved
	}
	if _, err := manifest.File("index.html"); err != nil {
		return errors.New("site requires root index.html")
	}
	return nil
}

// ContentType returns empty for files that must not be served as site resources.
func ContentType(name string) string { return mimeTypes[strings.ToLower(path.Ext(name))] }

func (s *Site) Files(ctx context.Context) ([]content.File, error) {
	files := make([]content.File, 0)
	err := s.walk(ctx, func(name string, stat fs.FileInfo) error {
		if stat.Mode().IsRegular() && s.includes(name) {
			files = append(files, content.File{Path: name, Size: stat.Size()})
			if len(files) > content.MaxFiles {
				return errors.New("site file limit exceeded")
			}
		}
		return nil
	})
	return files, err
}

func (s *Site) includes(name string) bool {
	return !s.filterFileTypes || ContentType(name) != ""
}

func (s *Site) Fingerprint(ctx context.Context) ([32]byte, error) {
	hash := sha256.New()
	err := s.walk(ctx, func(name string, stat fs.FileInfo) error {
		fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\n", name, stat.Size(), stat.ModTime().UnixNano(), stat.Mode())
		return nil
	})
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum, err
}

func (s *Site) walk(ctx context.Context, visit func(string, fs.FileInfo) error) error {
	if s.folder == "" {
		return errors.New("remote site has no local source")
	}
	root, err := os.OpenRoot(s.folder)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name != "." && (content.ValidatePath(name) != nil || entry.Type()&os.ModeSymlink != 0) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		if !stat.IsDir() && !stat.Mode().IsRegular() {
			return fmt.Errorf("not a regular file: %s", name)
		}
		return visit(name, stat)
	})
}

// Open confines reads to this site's root and refuses symlink final components.
func (s *Site) Open(ctx context.Context, name string) (content.Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.folder == "" {
		return nil, errors.New("remote site has no local source")
	}
	if err := content.ValidatePath(name); err != nil {
		return nil, err
	}
	if !s.includes(name) {
		return nil, fs.ErrPermission
	}
	root, err := os.OpenRoot(s.folder)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	stat, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, fs.ErrPermission
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(stat, opened) {
		file.Close()
		return nil, errors.New("source changed while opening")
	}
	return &reader{file: file, done: ctx.Done(), err: ctx.Err}, nil
}

type reader struct {
	file *os.File
	done <-chan struct{}
	err  func() error
}

func (r *reader) Read(p []byte) (int, error) {
	select {
	case <-r.done:
		return 0, r.err()
	default:
		return r.file.Read(p)
	}
}
func (r *reader) Seek(offset int64, whence int) (int64, error) {
	select {
	case <-r.done:
		return 0, r.err()
	default:
		return r.file.Seek(offset, whence)
	}
}
func (r *reader) Close() error { return r.file.Close() }
