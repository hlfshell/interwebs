package network

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/torrent"
	"github.com/hlfshell/interweb/interwebs/site"
)

type peerAnnouncement struct {
	peers chan dht.PeersValues
	once  sync.Once
	done  chan struct{}
}

func (a *peerAnnouncement) Peers() <-chan dht.PeersValues { return a.peers }
func (a *peerAnnouncement) Close() {
	a.once.Do(func() { close(a.peers); close(a.done) })
}

type peerDiscovery struct {
	announced chan *peerAnnouncement
	err       error
}

func (*peerDiscovery) Stats() interface{}          { return nil }
func (*peerDiscovery) ID() [20]byte                { return [20]byte{} }
func (*peerDiscovery) Addr() net.Addr              { return &net.UDPAddr{} }
func (*peerDiscovery) AddNode(krpc.NodeInfo) error { return nil }
func (*peerDiscovery) Ping(*net.UDPAddr)           {}
func (*peerDiscovery) WriteStatus(io.Writer)       {}
func (d *peerDiscovery) Announce(_ [20]byte, _ int, _ bool) (torrent.DhtAnnounce, error) {
	if d.err != nil {
		return nil, d.err
	}
	a := &peerAnnouncement{peers: make(chan dht.PeersValues), done: make(chan struct{})}
	d.announced <- a
	return a, nil
}

func TestSeedingAnnouncesPeersWithoutTorrentActivity(t *testing.T) {
	for _, openFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "enable after open", false: "open while enabled"}[openFirst], func(t *testing.T) {
			transport, hash := announcementTransport(t)
			discovery := &peerDiscovery{announced: make(chan *peerAnnouncement, 8)}
			transport.client.AddDhtServer(discovery)
			if !openFirst {
				transport.transfers = make(map[string]*Transfer)
			}
			startAnnouncements(t, transport)
			if err := transport.SetSeeding(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			if !openFirst {
				if _, err := transport.Open(t.Context(), hash, nil); err != nil {
					t.Fatal(err)
				}
			}
			var announcement *peerAnnouncement
			select {
			case announcement = <-discovery.announced:
			case <-time.After(3 * time.Second):
				t.Fatal("seeding did not announce peers")
			}
			if err := transport.SetSeeding(t.Context(), false); err != nil {
				t.Fatal(err)
			}
			select {
			case <-announcement.done:
			case <-time.After(3 * time.Second):
				t.Fatal("stopping seeding did not cancel discovery")
			}
			if err := transport.SetSeeding(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			select {
			case announcement = <-discovery.announced:
			case <-time.After(3 * time.Second):
				t.Fatal("resuming seeding did not announce")
			}
			if err := transport.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-announcement.done:
			case <-time.After(3 * time.Second):
				t.Fatal("close leaked discovery")
			}
			if err := transport.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPeerAnnouncementFailureIsReported(t *testing.T) {
	transport, _ := announcementTransport(t)
	failure := errors.New("discovery unavailable")
	transport.client.AddDhtServer(&peerDiscovery{err: failure})
	startAnnouncements(t, transport)
	if err := transport.SetSeeding(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !errors.Is(transport.PeerAnnouncementError(), failure) {
		select {
		case <-deadline:
			t.Fatal("announcement failure not exposed")
		case <-tick.C:
		}
	}
}

// Add the fake DHT only after the torrent is complete and its automatic
// announcers have been configured. This isolates our seeding wakeup from
// unrelated library events and never contacts public infrastructure.
func announcementTransport(t *testing.T) (*Transport, string) {
	t.Helper()
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	stores := networkStores(t, t.TempDir())
	m, err := stores.Snapshot(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	transport := localTransport(t, stores)
	transfer, err := transport.Open(t.Context(), m.Hash(), source.Validate)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := transfer.Download(ctx); err != nil {
		t.Fatal(err)
	}
	transport.cancel()
	transport.wg.Wait()
	return transport, m.Hash()
}

func startAnnouncements(t *testing.T, transport *Transport) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	transport.cancel, transport.done = cancel, ctx.Done()
	transport.peerWake = make(chan struct{}, 1)
	transport.wg.Add(1)
	go func() { defer transport.wg.Done(); transport.maintainPeers(ctx) }()
}
