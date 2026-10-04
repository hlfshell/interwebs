package interwebs

import (
	"path/filepath"
	"testing"

	"github.com/hlfshell/interweb/interwebs/site"
)

func TestRefreshRejectsForgedUpdateAndViewDoesNotPoll(t *testing.T) {
	owner, _, folder := localNode(t, "valid")
	publication, err := owner.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := site.FromMagnet(publication.Magnet)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &discoveryFixture{record: publication.Record}
	dir := t.TempDir()
	n, err := New(t.Context(), WithContent(remote), WithDataDir(dir), WithKeyFile(filepath.Join(dir, "key")),
		WithOffline(true), func(o *options) error { o.discovery = fixture; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	local, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.stores.Snapshot(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	url, err := n.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	forged := publication.Record.Clone()
	forged.Sequence++
	fixture.set(forged)
	// A normal view reuses current content, without consulting the forged pointer.
	if _, err := n.View(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Refresh(t.Context()); err == nil {
		t.Fatal("forged update accepted")
	}
	if n.Status().RefreshError == "" || n.Status().Record.Sequence != 1 {
		t.Fatal(n.Status())
	}
	checkPage(t, url, "valid")
	if n.Status().Seeding {
		t.Fatal("view/refresh enabled seeding")
	}
}
