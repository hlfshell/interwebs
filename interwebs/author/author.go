// Package author describes optional signed directories endorsing independent sites.
// Directory signatures never grant authority to publish an endorsed site's updates.
package author

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/site"
)

const DirectoryPath = "author.json"
const MaxDirectoryBytes = 1 << 20
const MaxSites = 1000

// Entry endorses a site address, not a transferable signing capability.
type Entry struct {
	Name    string            `json:"name"`
	Address identity.Identity `json:"address"`
}

// Directory is authenticated by the author's signed pointer and torrent hashes.
type Directory struct {
	Name  string  `json:"name"`
	Sites []Entry `json:"sites"`
}

// Profile is Content with an optional website and a bounded directory.
type Profile struct {
	descriptor content.Descriptor
	encoded    []byte
	page       *site.Site
}

type Option func(*Profile) error

// WithPage includes an author's local website alongside the directory.
func WithPage(page *site.Site) Option {
	return func(p *Profile) error {
		if page == nil || !page.Local() {
			return errors.New("author page must be a local site")
		}
		p.page = page
		return nil
	}
}

// New constructs a profile source without starting a Node or generating keys.
func New(directory Directory, opts ...Option) (*Profile, error) {
	encoded, err := Encode(directory)
	if err != nil {
		return nil, err
	}
	p := &Profile{descriptor: content.Descriptor{Name: directory.Name}, encoded: encoded}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil option")
		}
		if err := opt(p); err != nil {
			return nil, err
		}
	}
	if p.page != nil {
		p.descriptor.SourceID = p.page.Descriptor().SourceID
	}
	return p, nil
}

// FromMagnet describes a signed remote author directory without fetching it.
func FromMagnet(magnet string) (*Profile, error) {
	address, err := identity.ParseMagnet(magnet)
	if err != nil {
		return nil, err
	}
	if address.Key == "" {
		return nil, errors.New("author directory requires signed identity")
	}
	return &Profile{descriptor: content.Descriptor{Name: address.ID()[:12], Address: address}}, nil
}

func (p *Profile) Descriptor() content.Descriptor { return p.descriptor }
func (p *Profile) EntryPoint() string {
	if p.page != nil {
		return "index.html"
	}
	// Remote profiles may have no website; View checks the resolved manifest.
	if p.descriptor.Address.Key != "" {
		return "index.html"
	}
	return ""
}
func (p *Profile) ValidateStorageDir(dir string) error {
	if p.page != nil {
		return p.page.ValidateStorageDir(dir)
	}
	return nil
}
func (p *Profile) Validate(m content.Manifest) error {
	f, err := m.File(DirectoryPath)
	if err != nil {
		return err
	}
	if f.Size > MaxDirectoryBytes {
		return errors.New("author directory exceeds limit")
	}
	return nil
}

// ValidateFiles validates directory semantics after authenticated bytes are read.
func (p *Profile) ValidateFiles(ctx context.Context, open func(context.Context, string) (content.Reader, error)) error {
	r, err := open(ctx, DirectoryPath)
	if err != nil {
		return err
	}
	_, err = Decode(r)
	return errors.Join(err, r.Close())
}

func (p *Profile) Files(ctx context.Context) ([]content.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.encoded == nil {
		return nil, content.ErrUnavailable
	}
	out := []content.File{{Path: DirectoryPath, Size: int64(len(p.encoded))}}
	if p.page != nil {
		files, err := p.page.Files(ctx)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.Path != DirectoryPath {
				out = append(out, f)
			}
		}
	}
	return out, nil
}
func (p *Profile) Open(ctx context.Context, name string) (content.Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == DirectoryPath && p.encoded != nil {
		return reader{bytes.NewReader(p.encoded)}, nil
	}
	if p.page != nil {
		return p.page.Open(ctx, name)
	}
	return nil, content.ErrUnavailable
}
func (p *Profile) Fingerprint(ctx context.Context) ([32]byte, error) {
	if err := ctx.Err(); err != nil {
		return [32]byte{}, err
	}
	hash := sha256.New()
	hash.Write(p.encoded)
	if p.page != nil {
		fingerprint, err := p.page.Fingerprint(ctx)
		if err != nil {
			return [32]byte{}, err
		}
		hash.Write(fingerprint[:])
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

type reader struct{ *bytes.Reader }

func (reader) Close() error { return nil }

// Encode validates and copies a directory into a bounded representation.
func Encode(d Directory) ([]byte, error) {
	if err := validate(d); err != nil {
		return nil, err
	}
	b, err := json.Marshal(d)
	if len(b) > MaxDirectoryBytes {
		return nil, errors.New("author directory exceeds limit")
	}
	return b, err
}

// Decode validates structure; authenticity must come from verified Node reads.
func Decode(r io.Reader) (Directory, error) {
	var d Directory
	b, err := io.ReadAll(io.LimitReader(r, MaxDirectoryBytes+1))
	if err != nil {
		return d, err
	}
	if len(b) > MaxDirectoryBytes {
		return d, errors.New("author directory exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return Directory{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Directory{}, errors.New("trailing author directory data")
	}
	return d, validate(d)
}
func validate(d Directory) error {
	if d.Name == "" || len(d.Name) > 256 || len(d.Sites) > MaxSites {
		return errors.New("invalid author directory")
	}
	seen := make(map[string]bool)
	for _, entry := range d.Sites {
		if entry.Name == "" || len(entry.Name) > 256 || entry.Address.Key == "" {
			return errors.New("invalid site endorsement")
		}
		if err := entry.Address.Validate(); err != nil {
			return err
		}
		if seen[entry.Address.ID()] {
			return errors.New("duplicate site endorsement")
		}
		seen[entry.Address.ID()] = true
	}
	return nil
}
