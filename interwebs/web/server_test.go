package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/content"
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

// A pending asset represents a network reader still waiting for verified pieces.
type partialSource struct {
	Source
	waiting chan struct{}
}

func (s *partialSource) Open(ctx context.Context, name string) (content.Reader, error) {
	if name == "video.webm" {
		close(s.waiting)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Source.Open(ctx, name)
}

func TestIndexServedWhileAnotherAssetIsStillDownloading(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	folder := t.TempDir()
	for name, data := range map[string]string{"index.html": "partial site", "video.webm": "unavailable asset"} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := site.New(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := storage.NewSandboxed(ctx, t.TempDir(), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	stores, err := storage.New(ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close()
	manifest, err := stores.Snapshot(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	version, err := stores.Open(ctx, manifest.Hash())
	if err != nil {
		t.Fatal(err)
	}
	defer version.Close()
	partial := &partialSource{Source: version, waiting: make(chan struct{})}
	server, err := New(ctx, partial)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	assetCtx, stopAsset := context.WithCancel(ctx)
	defer stopAsset()
	assetDone := make(chan struct{})
	go func() {
		defer close(assetDone)
		request, _ := http.NewRequestWithContext(assetCtx, "GET", server.URL()+"video.webm", nil)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-partial.waiting:
	case <-ctx.Done():
		t.Fatal("asset request never reached reader")
	}
	request, err := http.NewRequestWithContext(ctx, "GET", server.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "partial site" {
		t.Fatalf("partial index: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
	stopAsset()
	select {
	case <-assetDone:
	case <-ctx.Done():
		t.Fatal("canceled asset request did not stop")
	}
}
