package storage

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestTorrentLayoutBoundaries(t *testing.T) {
	info := metainfo.Info{Files: []metainfo.FileInfo{
		{Path: []string{"empty"}},
		{Path: []string{"a"}, Length: 3},
		{Path: []string{"gap"}},
		{Path: []string{"nested", "b"}, Length: 5},
		{Path: []string{"last"}},
	}}
	torrent := &encryptedTorrent{files: torrentFiles(&info)}
	if len(torrent.files) != 2 || torrent.files[1].name != "site/nested/b" {
		t.Fatal(torrent.files)
	}
	for offset, want := range []int{0, 0, 0, 1, 1, 1, 1, 1, 2} {
		if got := torrent.fileAt(int64(offset)); got != want {
			t.Fatalf("offset %d: file %d, want %d", offset, got, want)
		}
	}
	single := torrentFiles(&metainfo.Info{Name: "index.html", Length: 4})
	if len(single) != 1 || single[0].name != "site/index.html" {
		t.Fatal(single)
	}
}

func TestTorrentWritesAcrossNestedFiles(t *testing.T) {
	info := metainfo.Info{Name: "site", PieceLength: 16 << 10, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{
		{Path: []string{"a"}, Length: 3},
		{Path: []string{"nested", "b"}, Length: 5},
	}}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	s := testCollection(t)
	handle, err := s.OpenTorrent(t.Context(), &info, sha1.Sum(encoded))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	piece := handle.Piece(info.Piece(0))
	for _, payload := range [][]byte{[]byte("abcdefgh"), []byte("12345678")} {
		if n, err := piece.WriteAt(payload, 0); err != nil || n != len(payload) {
			t.Fatalf("write: %d, %v", n, err)
		}
		for _, offset := range []int64{0, 2, 3, 7} {
			got := make([]byte, len(payload)-int(offset))
			if n, err := piece.ReadAt(got, offset); err != nil || n != len(got) || !bytes.Equal(got, payload[offset:]) {
				t.Fatalf("read at %d: %q, %d, %v", offset, got, n, err)
			}
		}
	}
}

// Isolate layout lookup from disk commits; this is not a throughput benchmark.
func BenchmarkTorrentFileLookup(b *testing.B) {
	for _, count := range []int{1, 481, 10000} {
		info := metainfo.Info{Files: make([]metainfo.FileInfo, count)}
		for i := range info.Files {
			info.Files[i] = metainfo.FileInfo{Path: []string{fmt.Sprint(i)}, Length: 16384}
		}
		torrent := &encryptedTorrent{files: torrentFiles(&info)}
		b.Run(fmt.Sprintf("%d/linear", count), func(b *testing.B) {
			for i := range b.N {
				offset := int64(i%count)*16384 + 1
				var end int64
				for index, file := range info.Files {
					end += file.Length
					if end > offset {
						if index != i%count {
							b.Fatal(index)
						}
						break
					}
				}
			}
		})
		b.Run(fmt.Sprintf("%d/indexed", count), func(b *testing.B) {
			for i := range b.N {
				if got := torrent.fileAt(int64(i%count)*16384 + 1); got != i%count {
					b.Fatal(got)
				}
			}
		})
	}
}
