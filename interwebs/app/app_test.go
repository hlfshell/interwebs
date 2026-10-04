package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	a, err := Open(t.Context(), t.TempDir(), WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}
func folder(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}
func wait(t *testing.T, op *Operation, err error) Result {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestPublishViewLeaseRestartAndExplicitRemoval(t *testing.T) {
	a := testApp(t)
	source := folder(t, "first")
	p, err := a.CreateProfile(t.Context(), "Personal", WithSourceRoots(source))
	if err != nil {
		t.Fatal(err)
	}
	blog, err := p.AddFolder(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := p.AddFolder(t.Context(), source)
	if err != nil || duplicate != blog {
		t.Fatal("registration not idempotent", err)
	}
	if blog.currentNode() != nil {
		t.Fatal("registration started a Node")
	}
	op, err := blog.Publish(t.Context())
	publication := wait(t, op, err).Publication
	op, err = blog.View(t.Context())
	view := wait(t, op, err).View
	response, err := http.Get(view.URL())
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "first" {
		t.Fatal(string(body), err)
	}
	if err := p.RemoveSite(t.Context(), blog.ID()); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if blog.Status().Views != 0 {
		t.Fatal("view lease not released idempotently")
	}
	if err := os.WriteFile(filepath.Join(source, "index.html"), []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	op, err = blog.Publish(t.Context())
	second := wait(t, op, err).Publication
	if second.Magnet != publication.Magnet || second.Record.Sequence != 2 {
		t.Fatal(second)
	}
	id, siteID, root := p.ID(), blog.ID(), a.root
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), root, WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	p, err = reopened.Profile(id)
	if err != nil {
		t.Fatal(err)
	}
	blog, err = p.Site(siteID)
	if err != nil {
		t.Fatal(err)
	}
	op, err = blog.View(t.Context())
	view = wait(t, op, err).View
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	versions, err := blog.Versions(t.Context())
	if err != nil || len(versions) != 2 {
		t.Fatal(versions, err)
	}
	// Pause background hosting so explicit unregistration is idle.
	if err := p.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return blog.pending == 0 })
	if err := p.RemoveSite(t.Context(), siteID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "index.html")); err != nil {
		t.Fatal("deleted source", err)
	}
	if _, err := os.Stat(blog.keyFile()); err != nil {
		t.Fatal("deleted signer", err)
	}
}

func TestProfilesOwnSeparateKeysAndEnforceSourceRoots(t *testing.T) {
	a := testApp(t)
	one, two := folder(t, "one"), folder(t, "two")
	p, err := a.CreateProfile(t.Context(), "One", WithSourceRoots(one))
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.CreateProfile(t.Context(), "Two", WithSourceRoots(two))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddFolder(t.Context(), two); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	link := filepath.Join(one, "escape")
	if err := os.Symlink(two, link); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddFolder(t.Context(), link); !errors.Is(err, ErrSource) {
		t.Fatal("accepted symlink escape", err)
	}
	if _, err := p.AddFolder(t.Context(), a.root); !errors.Is(err, ErrSource) {
		t.Fatal("accepted app state source", err)
	}
	left, err := os.ReadFile(filepath.Join(p.dir, "keys", "storage.key"))
	if err != nil {
		t.Fatal(err)
	}
	right, err := os.ReadFile(filepath.Join(q.dir, "keys", "storage.key"))
	if err != nil || bytes.Equal(left, right) {
		t.Fatal("profiles share keys", err)
	}
	s, err := p.AddFolder(t.Context(), one)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Site(s.ID()); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-profile lookup", err)
	}
	if second, err := Open(t.Context(), a.root); err == nil {
		second.Close()
		t.Fatal("root lock not exclusive")
	}
}

func TestMissingProfileKeyDoesNotBlockOtherProfilesOrRegenerate(t *testing.T) {
	a := testApp(t)
	p, err := a.CreateProfile(t.Context(), "Lost")
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.CreateProfile(t.Context(), "Healthy")
	if err != nil {
		t.Fatal(err)
	}
	root, badID, goodID := a.root, p.ID(), q.ID()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.dir, "keys", "storage.key")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), root, WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Profile(badID); err == nil {
		t.Fatal("lost-key profile opened")
	}
	if _, err := reopened.Profile(goodID); err != nil {
		t.Fatal(err)
	}
	if reopened.ProfileErrors()[badID] == "" {
		t.Fatal("isolated error hidden")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing key was regenerated")
	}
}

func TestAuthorAssociationsRequireExplicitPublication(t *testing.T) {
	a := testApp(t)
	source := folder(t, "blog")
	p, err := a.CreateProfile(t.Context(), "Personal", WithSourceRoots(source))
	if err != nil {
		t.Fatal(err)
	}
	blog, err := p.AddFolder(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	author, err := p.CreateAuthor(t.Context(), "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := author.SetSites(t.Context(), blog.ID()); err != nil {
		t.Fatal(err)
	}
	if author.site.currentNode() != nil {
		t.Fatal("association implicitly published")
	}
	op, err := author.Publish(t.Context())
	first := wait(t, op, err).Publication
	if first.Magnet == blog.Status().Address {
		t.Fatal("author reused site identity")
	}
	if err := author.SetSites(t.Context()); err != nil {
		t.Fatal(err)
	}
	if author.Status().Core.Record.Sequence != 1 {
		t.Fatal("association changed public version")
	}
	op, err = author.Publish(t.Context())
	second := wait(t, op, err).Publication
	if second.Record.Sequence != 2 || second.Magnet != first.Magnet {
		t.Fatal(second)
	}
}
