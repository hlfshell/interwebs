package runner

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (Config, string) {
	t.Helper()
	root := t.TempDir()
	folder := filepath.Join(root, "blog")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("hello encrypted world"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{DataDir: filepath.Join(root, "data"), SourceRoots: []string{folder}, Offline: true, MaxSiteMiB: 512,
		NodeStorageMiB: 2048, Sites: []SiteConfig{{Name: "blog", Folder: folder, Serve: &ServeConfig{Listen: "127.0.0.1:0"}}}}
	return c, folder
}

func fetch(t *testing.T, address, method string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		t.Fatal(err)
	}
	return response, string(b)
}

func TestPublishServeRestartAndStableIdentity(t *testing.T) {
	c, folder := fixture(t)
	r, err := startReady(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := r.close(); err != nil {
				t.Error(err)
			}
		}
	})
	first := r.status("ready")
	address := first.Sites[0].URL
	response, body := fetch(t, address, "GET", nil)
	if response.StatusCode != 200 || body != "hello encrypted world" {
		t.Fatalf("%d %q", response.StatusCode, body)
	}
	csp := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts") || !strings.Contains(csp, strings.TrimSuffix(address, "/")) {
		t.Fatal(csp)
	}
	if strings.Contains(csp, r.sites[0].endpoint.target.Host) {
		t.Fatal("internal origin leaked in CSP")
	}
	response, body = fetch(t, address, "GET", map[string]string{"Range": "bytes=6-14"})
	if response.StatusCode != 206 || body != "encrypted" {
		t.Fatalf("range: %d %q", response.StatusCode, body)
	}
	response, body = fetch(t, address, "HEAD", nil)
	if response.StatusCode != 200 || body != "" {
		t.Fatal("HEAD", response.StatusCode, body)
	}
	response, _ = fetch(t, address, "GET", map[string]string{"Host": "evil.invalid"})
	if response.StatusCode != 403 {
		t.Fatal("host check", response.StatusCode)
	}
	response, _ = fetch(t, address, "POST", nil)
	if response.StatusCode != 405 {
		t.Fatal("write method", response.StatusCode)
	}
	response, _ = fetch(t, address+"keys/storage.key", "GET", nil)
	if response.StatusCode != 404 {
		t.Fatal("private key exposure", response.StatusCode)
	}
	// The server reads stores, not an edited source folder.
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("updated"), 0600); err != nil {
		t.Fatal(err)
	}
	_, body = fetch(t, address, "GET", nil)
	if body != "hello encrypted world" {
		t.Fatal("served unpublished source")
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get(address); err == nil {
		response.Body.Close()
		t.Fatal("listener survived shutdown")
	}
	r, err = startReady(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	closed = false
	second := r.status("ready")
	if first.Profile != second.Profile || first.Sites[0].State.ID != second.Sites[0].State.ID || first.Sites[0].State.Address != second.Sites[0].State.Address {
		t.Fatal("restart changed identity")
	}
	if second.Sites[0].State.Core.Record.Sequence != 2 {
		t.Fatal("update not published")
	}
	_, body = fetch(t, second.Sites[0].URL, "GET", nil)
	if body != "updated" {
		t.Fatal(body)
	}
}

func TestServingFollowsPublishedUpdates(t *testing.T) {
	c, folder := fixture(t)
	r, err := startReady(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	}()
	address := r.status("ready").Sites[0].URL
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("next version"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := await(t.Context(), r.sites[0].site.Publish); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.sites[0].endpoint.hash != r.sites[0].site.Status().Core.Current {
		if err := r.update(t.Context()); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("view did not update")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, body := fetch(t, address, "GET", nil)
	if body != "next version" {
		t.Fatal(body)
	}
	if r.sites[0].site.Status().Views != 1 {
		t.Fatal("old view lease leaked")
	}
}

func TestStartupFailureReleasesBackendAndListeners(t *testing.T) {
	c, _ := fixture(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.Sites[0].Serve.Listen = occupied.Addr().String()
	if r, err := startReady(t.Context(), c); err == nil {
		r.close()
		t.Fatal("accepted occupied listener")
	}
	if err := occupied.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := startReady(t.Context(), c)
	if err != nil {
		t.Fatal("restart after failure", err)
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
}

type readyWriter struct{ ready chan struct{} }

func (w *readyWriter) Write(b []byte) (int, error) {
	select {
	case w.ready <- struct{}{}:
	default:
	}
	return len(b), nil
}

func TestRunCancellationClosesAndReleasesRoot(t *testing.T) {
	c, _ := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &readyWriter{ready: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, w) }()
	select {
	case <-w.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("startup timeout")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown timeout")
	}
	r, err := startReady(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledStartupLeavesNoLock(t *testing.T) {
	c, _ := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r, err := start(ctx, c); err == nil {
		r.close()
		t.Fatal("accepted canceled startup")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r, err := startReady(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicOriginServingAndLiveUpdate(t *testing.T) {
	c, folder := fixture(t)
	c.Sites[0].Live = true
	c.Sites[0].Serve.PublicURL = "https://blog.example.test"
	r, err := startReady(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	}()
	e := r.sites[0].endpoint
	address := "http://" + e.address + "/"
	headers := map[string]string{"Host": "blog.example.test"}
	response, _ := fetch(t, address, "GET", headers)
	if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Security-Policy"), "https://blog.example.test") {
		t.Fatal("public origin not served", response.StatusCode, response.Header)
	}
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("live update"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := r.update(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, body := fetch(t, address, "GET", headers)
		if body == "live update" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Live did not reach the stable HTTP listener")
}

func startReady(ctx context.Context, c Config) (*running, error) {
	r, err := start(ctx, c)
	if err != nil {
		return nil, err
	}
	for {
		if err := r.update(ctx); err != nil {
			return nil, errors.Join(err, r.close())
		}
		ready := true
		for _, s := range r.sites {
			if s.publish || s.operation != nil || !s.configured || (s.endpoint != nil && (s.endpoint.view == nil || s.endpoint.hash != s.site.Status().Core.Current)) {
				ready = false
			}
		}
		if ready {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), r.close())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
