package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRestartServesStoredVersionBeforeRepublishing(t *testing.T) {
	c, folder := fixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	first, err := startReady(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	hash := first.sites[0].site.Status().Core.Current
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	r, err := start(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if err := r.update(ctx); err != nil {
		t.Fatal(err)
	}
	op := r.sites[0].operation
	if op == nil || op.Status().Kind != "view" {
		t.Fatal("restart did not restore a view before publication")
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// A failed replacement must not prevent attaching the already restored view.
	if err := os.Remove(filepath.Join(folder, "index.html")); err != nil {
		t.Fatal(err)
	}
	advanceUntil(t, r, func() bool { return r.sites[0].failure != "" })
	response, body := fetch(t, r.status("status").Sites[0].URL, "GET", nil)
	if response.StatusCode != 200 || body != "hello encrypted world" {
		t.Fatalf("stored site unavailable during failed update: %d %q", response.StatusCode, body)
	}
	if r.sites[0].site.Status().Core.Current != hash {
		t.Fatal("failed update replaced stored version")
	}
	if !r.sites[0].site.Status().Core.Seeding {
		t.Fatal("failed source update stopped hosting the stored version")
	}
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	r.sites[0].retryAt = time.Time{}
	advanceUntil(t, r, func() bool { return !r.sites[0].publish && r.sites[0].endpoint.hash != hash })
	response, body = fetch(t, r.status("status").Sites[0].URL, "GET", nil)
	if response.StatusCode != 200 || body != "replacement" {
		t.Fatalf("replacement unavailable: %d %q", response.StatusCode, body)
	}
}

func TestListenerAndStatusAvailableBeforePublication(t *testing.T) {
	c, _ := fixture(t)
	r, err := start(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	}()
	response, _ := fetch(t, r.status("ready").Sites[0].URL, "GET", nil)
	if response.StatusCode != 503 || response.Header.Get("Retry-After") == "" {
		t.Fatal("listener did not advertise preparation", response.StatusCode)
	}
	if err := r.report("ready", io.Discard); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ReadStatus(c, &output); err != nil {
		t.Fatal(err)
	}
	var snapshot status
	if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Sites[0].Phase != "preparing" || snapshot.Updated.IsZero() || snapshot.PID != os.Getpid() {
		t.Fatalf("invalid snapshot: %+v", snapshot)
	}
	if err := r.update(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.status("status").Sites[0].Operation == nil {
		t.Fatal("publication operation not visible")
	}
}

func TestFailedSiteRetriesWithoutBlockingHealthySite(t *testing.T) {
	c, bad := fixture(t)
	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "index.html"), []byte("healthy"), 0600); err != nil {
		t.Fatal(err)
	}
	c.SourceRoots = append(c.SourceRoots, good)
	c.Sites = append(c.Sites, SiteConfig{Name: "healthy", Folder: good, Serve: &ServeConfig{Listen: "127.0.0.1:0"}})
	r, err := start(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	}()
	// Break the source after registration, simulating a runtime publication failure.
	if err := os.Remove(filepath.Join(bad, "index.html")); err != nil {
		t.Fatal(err)
	}
	advanceUntil(t, r, func() bool { return r.sites[0].failure != "" && r.sites[1].endpoint.view != nil })
	if r.status("status").Sites[0].Phase != "retrying" {
		t.Fatal("retry state not reported")
	}
	response, body := fetch(t, r.status("status").Sites[1].URL, "GET", nil)
	if response.StatusCode != 200 || body != "healthy" {
		t.Fatal("healthy site blocked", response.StatusCode, body)
	}
	if err := os.WriteFile(filepath.Join(bad, "index.html"), []byte("recovered"), 0600); err != nil {
		t.Fatal(err)
	}
	r.sites[0].retryAt = time.Time{}
	advanceUntil(t, r, func() bool { return r.sites[0].endpoint.view != nil })
	response, body = fetch(t, r.status("status").Sites[0].URL, "GET", nil)
	if response.StatusCode != 200 || body != "recovered" {
		t.Fatal("site did not recover", response.StatusCode, body)
	}
}

func advanceUntil(t *testing.T, r *running, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
		if err := r.update(t.Context()); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("state transition timed out", r.status("status"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPublicDiscoveryIsDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Init(path, "./data"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Offline {
		t.Fatal("public discovery unexpectedly disabled")
	}
}
