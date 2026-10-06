package interwebs

import (
	"context"
	"testing"

	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/site"
)

type canceledDiscovery struct{ calls int }

func (d *canceledDiscovery) Resolve(context.Context, identity.Identity, identity.Record) (identity.Record, error) {
	d.calls++
	return identity.Record{}, context.Canceled
}
func (*canceledDiscovery) Announce(context.Context, identity.Identity, identity.Record) error {
	return nil
}

func TestRestartServesVerifiedCacheWithoutDiscovery(t *testing.T) {
	publisher, _, folder := localNode(t, "cached page")
	publication, err := publisher.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := site.FromMagnet(publication.Magnet)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	opts := []Option{WithContent(remote), WithDataDir(dir), WithOffline(true)}
	reader, err := New(t.Context(), append(opts, func(o *options) error { o.discovery = &discoveryFixture{record: publication.Record}; return nil })...)
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
	if _, err := reader.View(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	discovery := &canceledDiscovery{}
	restored, err := New(t.Context(), append(opts, func(o *options) error { o.discovery = discovery; return nil })...)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	url, err := restored.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "cached page")
	if discovery.calls != 0 {
		t.Fatal("cached view performed discovery")
	}
	if _, err := restored.Refresh(t.Context()); err == nil {
		t.Fatal("explicit refresh hid discovery failure")
	}
	checkPage(t, url, "cached page")
	if restored.Status().Current != publication.Record.Hash {
		t.Fatal("failed refresh replaced verified version")
	}
}
