package author

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hlfshell/interweb/interwebs/identity"
)

func TestDirectoryBoundsAndIndependentEndorsements(t *testing.T) {
	siteKey, err := identity.Create(t.Context(), filepath.Join(t.TempDir(), "site.key"))
	if err != nil {
		t.Fatal(err)
	}
	d := Directory{Name: "Author", Sites: []Entry{{Name: "Blog", Address: siteKey.Identity()}}}
	p, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	d.Sites[0].Name = "changed"
	r, err := p.Open(t.Context(), DirectoryPath)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(r)
	r.Close()
	if err != nil || decoded.Sites[0].Name != "Blog" {
		t.Fatal(decoded, err)
	}
	if p.EntryPoint() != "" {
		t.Fatal("profile without a page acquired a browser entry")
	}
	if _, err := Decode(strings.NewReader(strings.Repeat("x", MaxDirectoryBytes+1))); err == nil {
		t.Fatal("unbounded directory")
	}
	if _, err := Decode(bytes.NewBufferString(`{"name":"x","sites":[],"unexpected":true}`)); err == nil {
		t.Fatal("unknown fields accepted")
	}
	d.Sites = append(d.Sites, d.Sites[0])
	if _, err := Encode(d); err == nil {
		t.Fatal("duplicate endorsement")
	}
}
