package network

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hlfshell/interweb/interwebs/site"
	"github.com/hlfshell/interweb/interwebs/storage"
)

func networkStores(t *testing.T, dir string) *storage.Collection {
	t.Helper()
	b, err := storage.NewSandboxed(t.Context(), dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := storage.New(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func localTransport(t *testing.T, s *storage.Collection) *Transport {
	t.Helper()
	p, err := New(t.Context(), s, WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

type peerAddress string

func (peerAddress) Network() string  { return "tcp" }
func (a peerAddress) String() string { return string(a) }
func connectTransfer(ctx context.Context, receiver *Transport, hash string, port int) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if handle, ok := receiver.client.Torrent(metainfo.NewHashFromHex(hash)); ok {
				handle.AddPeers([]torrent.PeerInfo{{Addr: peerAddress(fmt.Sprintf("127.0.0.1:%d", port))}})
			}
		}
	}
}

func TestSelectiveTransferRestartAndOfflineReseed(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	source := t.TempDir()
	payload := make([]byte, 2<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"index.html": []byte("network page"), "video.webm": payload} {
		if err := os.WriteFile(filepath.Join(source, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	siteContent, err := site.New(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	publisherStores := networkStores(t, t.TempDir())
	m, err := publisherStores.Snapshot(ctx, siteContent)
	if err != nil {
		t.Fatal(err)
	}
	publisher := localTransport(t, publisherStores)
	if err := publisher.SetSeeding(ctx, true); err != nil {
		t.Fatal(err)
	}
	seed, err := publisher.Open(ctx, m.Hash(), siteContent.Validate)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Download(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stores := networkStores(t, dir)
	receiver := localTransport(t, stores)
	go connectTransfer(ctx, receiver, m.Hash(), publisher.Port())
	transfer, err := receiver.Open(ctx, m.Hash(), siteContent.Validate)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := receiver.DiscoveryStatus()
	if diagnostic.Hash != m.Hash() || diagnostic.CachedMetadata || diagnostic.Metadata.Finished.IsZero() || diagnostic.FirstPeer.Finished.IsZero() || diagnostic.Metadata.Error != "" {
		t.Fatalf("missing network discovery timings: %+v", diagnostic)
	}
	r, err := transfer.Open(ctx, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(b) != "network page" {
		t.Fatal(string(b), err)
	}
	if transfer.Stats().Bytes >= transfer.Stats().Total {
		t.Fatal("selective read fetched whole site")
	}

	// Range-oriented media demand does not fetch the entire file.
	media, err := transfer.Read(ctx, "video.webm", ReadOptions{Mode: Sequential})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := media.Seek(int64(len(payload)-32), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(media)
	media.Close()
	if err != nil || !bytes.Equal(tail, payload[len(payload)-32:]) {
		t.Fatal("sequential seek", err)
	}
	if transfer.Stats().Bytes >= transfer.Stats().Total {
		t.Fatal("range read fetched entire site")
	}
	if err := transfer.DownloadFile(ctx, "video.webm", ReadOptions{Mode: Normal}); err != nil {
		t.Fatal(err)
	}
	if err := transfer.Download(ctx); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stores.Close(); err != nil {
		t.Fatal(err)
	}
	publisher.Close()
	restored := localTransport(t, networkStores(t, dir))
	if err := restored.SetSeeding(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Open(ctx, m.Hash(), siteContent.Validate); err != nil {
		t.Fatal(err)
	}
	if diagnostic := restored.DiscoveryStatus(); !diagnostic.CachedMetadata || diagnostic.Metadata.Finished.IsZero() || !diagnostic.RoutingReady.Started.IsZero() {
		t.Fatalf("incorrect offline cached diagnostics: %+v", diagnostic)
	}
	fresh := localTransport(t, networkStores(t, t.TempDir()))
	go connectTransfer(ctx, fresh, m.Hash(), restored.Port())
	final, err := fresh.Open(ctx, m.Hash(), siteContent.Validate)
	if err != nil {
		t.Fatal(err)
	}
	r, err = final.Open(ctx, "video.webm")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Seek(int64(len(payload)-32), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(r)
	if err != nil || !bytes.Equal(b, payload[len(payload)-32:]) {
		t.Fatal("offline reseed mismatch", err)
	}
}
