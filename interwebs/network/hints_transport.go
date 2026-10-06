package network

import (
	"context"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	pp "github.com/anacrolix/torrent/peer_protocol"
)

// WithCacheFile enables bounded, expiring discovery hints. The caller must own
// the file exclusively. Invalid caches are ignored and reported in status.
// Hints are private local metadata, not downloaded content or trusted records.
func WithCacheFile(file string) Option {
	return func(o *options) error { o.cacheFile = file; return nil }
}

func (h *hints) configure(cfg *torrent.ClientConfig) {
	startingNodes := cfg.DhtStartingNodes
	cfg.DhtStartingNodes = func(network string) dht.StartingNodesGetter {
		return h.startingNodes(startingNodes(network))
	}
	// Callbacks can run under the torrent client lock: never call locking client
	// methods or perform I/O here. Retain scalar addresses, not callback data.
	var mu sync.Mutex
	addresses := make(map[*torrent.Peer]string)
	cfg.Callbacks.ReadExtendedHandshake = func(peer *torrent.PeerConn, message *pp.ExtendedHandshakeMessage) {
		address, err := netip.ParseAddrPort(peer.RemoteAddr.String())
		if err != nil || message.Port < 1 || message.Port > 65535 {
			return
		}
		address = netip.AddrPortFrom(address.Addr().Unmap(), uint16(message.Port))
		mu.Lock()
		if len(addresses) < 256 {
			addresses[&peer.Peer] = address.String()
		}
		mu.Unlock()
	}
	cfg.Callbacks.PeerClosed = append(cfg.Callbacks.PeerClosed, func(peer *torrent.Peer) {
		mu.Lock()
		delete(addresses, peer)
		mu.Unlock()
	})
	cfg.Callbacks.ReceivedUsefulData = append(cfg.Callbacks.ReceivedUsefulData, func(event torrent.ReceivedUsefulDataEvent) {
		mu.Lock()
		address := addresses[event.Peer]
		mu.Unlock()
		if address == "" {
			return
		}
		now := time.Now()
		h.remember(hint{Address: address, Hash: event.Peer.Torrent().InfoHash().HexString(), Seen: now}, now)
	})
}

func (h *hints) startingNodes(fallback dht.StartingNodesGetter) dht.StartingNodesGetter {
	return func() ([]dht.Addr, error) {
		var nodes []dht.Addr
		seen := make(map[string]bool)
		for _, address := range h.addresses("", time.Now()) {
			ap, _ := netip.ParseAddrPort(address)
			nodes = append(nodes, dht.NewAddr(net.UDPAddrFromAddrPort(ap)))
			seen[address] = true
		}
		// Always retain public starting points, even when every cached contact
		// has gone away. Cached contacts supplement rather than replace them.
		public, err := fallback()
		for _, address := range public {
			if !seen[address.String()] {
				nodes = append(nodes, address)
				seen[address.String()] = true
			}
		}
		if len(nodes) > 0 {
			return nodes, nil
		}
		return nil, err
	}
}

func (t *Transport) addKnownPeers(handle *torrent.Torrent, hash string) {
	var peers []torrent.PeerInfo
	for _, address := range t.hints.addresses(hash, time.Now()) {
		ap, _ := netip.ParseAddrPort(address)
		peers = append(peers, torrent.PeerInfo{Addr: net.TCPAddrFromAddrPort(ap)})
	}
	handle.AddPeers(peers)
}

func (t *Transport) maintainHints(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	// The first sweep is delayed to let bootstrap learn candidate contacts.
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, server := range t.client.DhtServers() {
			wrapped, ok := server.(torrent.AnacrolixDhtServerWrapper)
			if !ok {
				continue
			}
			nodes := wrapped.Server.Nodes()
			rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
			for _, node := range nodes[:min(8, len(nodes))] {
				if ctx.Err() != nil {
					return
				}
				t.hints.probe(ctx, wrapped.Server, node.Addr.UDP())
			}
		}
		_ = t.hints.save() // Nonfatal cache failures remain visible in status.
	}
}

func (h *hints) probe(ctx context.Context, server *dht.Server, address *net.UDPAddr) {
	work, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result := server.Query(work, dht.NewAddr(address), "ping", dht.QueryInput{NumTries: 1})
	if result.ToError() != nil || result.Reply.SenderID() == nil {
		return
	}
	now := time.Now()
	h.remember(hint{Address: address.String(), Seen: now}, now)
}
