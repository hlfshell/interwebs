package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/hlfshell/interweb/interwebs/site"
)

const hintHash = "1111111111111111111111111111111111111111"

func TestHintsExpiryBoundsPersistenceAndInvalidInput(t *testing.T) {
	now := time.Now()
	file := filepath.Join(t.TempDir(), "discovery.json")
	h := newHints(file)
	for i := 0; i < 100; i++ {
		h.remember(hint{Address: fmt.Sprintf("1.2.3.4:%d", i+1), Seen: now}, now)
		h.remember(hint{Address: fmt.Sprintf("1.2.3.4:%d", i+1), Hash: hintHash, Seen: now}, now)
	}
	if len(h.addresses("", now)) != 64 || len(h.addresses(hintHash, now)) != 16 {
		t.Fatal("per-content bounds not enforced")
	}
	if len(h.addresses(hintHash, now.Add(peerLifetime))) != 0 || len(h.addresses("", now.Add(dhtLifetime))) != 0 {
		t.Fatal("expired hints returned")
	}
	for _, address := range []string{"example.com:80", "0.0.0.0:80", "224.0.0.1:80", "1.2.3.4:0", "[fe80::1%eth0]:80"} {
		if validHint(hint{Address: address, Seen: now}, now) {
			t.Fatal("invalid address accepted", address)
		}
	}
	if validHint(hint{Address: "1.2.3.4:1", Seen: now.Add(time.Second)}, now) {
		t.Fatal("future timestamp accepted")
	}
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	restored := newHints(file)
	if restored.failure() != "" || len(restored.addresses(hintHash, now)) != 16 {
		t.Fatal("cache restore failed", restored.failure())
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("cache permissions", err)
	}
	if len(newHints("").addresses(hintHash, now)) != 0 {
		t.Fatal("independent cache leaked hints")
	}
	for i := 0; i < 1000; i++ {
		h.remember(hint{Address: "1.2.3.4:1234", Hash: fmt.Sprintf("%040x", i), Seen: now}, now)
	}
	if len(h.entries) > maxHints {
		t.Fatal("global bound exceeded")
	}
}

func TestCorruptHintsDoNotDisablePublicBootstrap(t *testing.T) {
	file := filepath.Join(t.TempDir(), "discovery.json")
	for _, data := range [][]byte{[]byte("broken"), bytes.Repeat([]byte{' '}, maxHintBytes+1)} {
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		h := newHints(file)
		if h.failure() == "" {
			t.Fatal("corrupt cache not reported")
		}
		calls := 0
		fallback := func() ([]dht.Addr, error) {
			calls++
			return []dht.Addr{dht.NewAddr(&net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 6881})}, nil
		}
		nodes, err := h.startingNodes(fallback)()
		if err != nil || len(nodes) != 1 || calls != 1 {
			t.Fatal(nodes, err, calls)
		}
		now := time.Now()
		h.remember(hint{Address: "[2001:4860:4860::8888]:6881", Seen: now}, now)
		nodes, err = h.startingNodes(fallback)()
		if err != nil || len(nodes) != 2 || calls != 2 {
			t.Fatal("cached nodes suppressed public fallback")
		}
		nodes, err = h.startingNodes(func() ([]dht.Addr, error) { return nil, errors.New("DNS unavailable") })()
		if err != nil || len(nodes) != 1 {
			t.Fatal("usable cache lost on bootstrap DNS failure")
		}
	}
}

func TestDHTHintsRequireAnAnsweredProbe(t *testing.T) {
	newServer := func() *dht.Server {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s, err := dht.NewServer(&dht.ServerConfig{Conn: conn, NoSecurity: true})
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
	local, remote := newServer(), newServer()
	h := newHints("")
	h.probe(t.Context(), local, remote.Addr().(*net.UDPAddr))
	if len(h.addresses("", time.Now())) != 1 {
		t.Fatal("responsive contact not remembered")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.probe(ctx, local, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})
	if len(h.addresses("", time.Now())) != 1 {
		t.Fatal("failed probe remembered")
	}
	unresponsive, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer unresponsive.Close()
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	h.probe(ctx, local, unresponsive.LocalAddr().(*net.UDPAddr))
	if time.Since(start) > time.Second || len(h.addresses("", time.Now())) != 1 {
		t.Fatal("probe did not cancel cleanly")
	}
}

func TestHintsCheckpointFailuresExpiryAndConcurrentAccess(t *testing.T) {
	file := filepath.Join(t.TempDir(), "hints.json")
	h := newHints(file)
	now := time.Now()
	h.remember(hint{Address: "1.2.3.4:12", Seen: now}, now)
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.save(); err == nil || h.failure() == "" {
		t.Fatal("write failure hidden")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := h.save(); err != nil || h.failure() != "" {
		t.Fatal("write did not recover", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.remember(hint{Address: fmt.Sprintf("1.2.3.4:%d", i*100+j+1), Hash: hintHash, Seen: now}, now)
				h.addresses(hintHash, now)
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		if err := h.save(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	h.mu.Lock()
	for key, entry := range h.entries {
		entry.Seen = now.Add(-48 * time.Hour)
		h.entries[key] = entry
	}
	h.mu.Unlock()
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "[]" {
		t.Fatal("expired addresses not purged", string(b), err)
	}
}

func TestKnownContentPeerReconnectsAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("cached peer page"), 0600); err != nil {
		t.Fatal(err)
	}
	content, err := site.New(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	stores := networkStores(t, t.TempDir())
	manifest, err := stores.Snapshot(ctx, content)
	if err != nil {
		t.Fatal(err)
	}
	publisher := localTransport(t, stores)
	if err := publisher.SetSeeding(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Open(ctx, manifest.Hash(), content.Validate); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "hints.json")
	receiver, err := New(ctx, networkStores(t, t.TempDir()), WithOffline(true), WithCacheFile(file))
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	connectCtx, stopConnect := context.WithCancel(ctx)
	defer stopConnect()
	go connectTransfer(connectCtx, receiver, manifest.Hash(), publisher.Port())
	transfer, err := receiver.Open(ctx, manifest.Hash(), content.Validate)
	if err != nil {
		t.Fatal(err)
	}
	if err := transfer.Download(ctx); err != nil {
		t.Fatal(err)
	}
	stopConnect()
	if len(receiver.hints.addresses(manifest.Hash(), time.Now())) == 0 {
		t.Fatal("useful peer not remembered")
	}
	if err := receiver.Close(); err != nil {
		t.Fatal(err)
	}
	// Fresh empty stores: the saved address must supply metadata AND payload.
	restored, err := New(ctx, networkStores(t, t.TempDir()), WithOffline(true), WithCacheFile(file))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.useHints {
		t.Fatal("offline mode automatically enables cached dialing")
	}
	// Enable just the replay path, keeping public DHT/NAT disabled in this test.
	restored.useHints = true
	transfer, err = restored.Open(ctx, manifest.Hash(), content.Validate)
	if err != nil {
		t.Fatal(err)
	}
	r, err := transfer.Open(ctx, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(b) != "cached peer page" {
		t.Fatal(string(b), err)
	}
}
