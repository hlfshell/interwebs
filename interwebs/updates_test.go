package interwebs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/site"
)

type discoveryFixture struct {
	mu     sync.Mutex
	record identity.Record
}

func (d *discoveryFixture) Resolve(context.Context, identity.Identity, identity.Record) (identity.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.record.Clone(), nil
}
func (d *discoveryFixture) Announce(context.Context, identity.Identity, identity.Record) error {
	return nil
}
func (d *discoveryFixture) set(r identity.Record) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record = r.Clone()
}

func TestNodeFollowsSignedUpdatesAndRetainsUsableVersion(t *testing.T) {
	publisher, _, folder := localNode(t, "first")
	first, err := publisher.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := site.FromMagnet(first.Magnet)
	if err != nil {
		t.Fatal(err)
	}
	discovery := &discoveryFixture{record: first.Record}
	dir := t.TempDir()
	reader, err := New(t.Context(), WithContent(remote), WithDataDir(dir), WithKeyFile(filepath.Join(dir, "storage.key")), WithOffline(true), WithRefreshInterval(20*time.Millisecond), WithFetchTimeout(40*time.Millisecond), func(o *options) error { o.discovery = discovery; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	local, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.stores.Snapshot(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	url, err := reader.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "first")
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := publisher.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.stores.Snapshot(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	discovery.set(second.Record)
	deadline := time.Now().Add(3 * time.Second)
	for reader.Status().Record.Sequence < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if reader.Status().Record.Sequence != 2 {
		t.Fatal("signed update not followed")
	}
	url, err = reader.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "second")
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("not available on receiver"), 0600); err != nil {
		t.Fatal(err)
	}
	third, err := publisher.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	discovery.set(third.Record)
	if _, err := reader.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	manifest, err := reader.Manifest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Hash() != second.Record.Hash {
		t.Fatal("lost usable version")
	}
	if reader.Status().Record.Sequence != 3 {
		t.Fatal("lost signed high-water mark")
	}
}
