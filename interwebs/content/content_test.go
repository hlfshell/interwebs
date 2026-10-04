package content

import (
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"testing"
)

func TestManifestValidationAndCopies(t *testing.T) {
	info := metainfo.Info{Name: "payload.bin", Length: 4, PieceLength: 256 << 10, Pieces: make([]byte, 20)}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseMetadata(encoded, DefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	m.Files()[0].Path = "changed"
	m.Metadata()[0] = 0
	if _, err := m.File("payload.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseMetadata(m.Metadata(), DefaultMaxBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseMetadata(encoded, 3); err == nil {
		t.Fatal("accepted oversize")
	}
	for _, name := range []string{"../bad", ".env", "a\\b", "a:b", "/abs", "a/../b"} {
		if ValidatePath(name) == nil {
			t.Fatal("accepted", name)
		}
	}
	info.Pieces = nil
	encoded, _ = bencode.Marshal(info)
	if _, err := ParseMetadata(encoded, DefaultMaxBytes); err == nil {
		t.Fatal("accepted truncated pieces")
	}
}
