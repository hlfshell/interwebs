package web

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hlfshell/interweb/interwebs/site"
	"github.com/hlfshell/interweb/interwebs/storage"
)

func TestStoredSourceRangesAndIsolation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("stored browser content"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := site.New(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := storage.NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := storage.New(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.Snapshot(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Open(t.Context(), m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	server, err := New(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, test := range []struct {
		method, path, host, bytes string
		status                    int
	}{
		{"GET", "", "", "bytes=0-5", 206}, {"GET", "", "evil.example", "", 403}, {"POST", "", "", "", 405}, {"GET", "../storage.key", "", "", 404}, {"OPTIONS", "", "", "", 204},
	} {
		req, err := http.NewRequest(test.method, server.URL()+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if test.host != "" {
			req.Host = test.host
		}
		if test.bytes != "" {
			req.Header.Set("Range", test.bytes)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != test.status {
			t.Fatalf("%+v: %d %s", test, response.StatusCode, body)
		}
		if test.status == 206 && string(body) != "stored" {
			t.Fatal(string(body))
		}
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}
