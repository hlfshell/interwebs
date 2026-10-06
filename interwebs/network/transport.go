package network

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/storage"
)

type options struct {
	cacheFile string
	port      int
	offline   bool
	maxBytes  int64
	discovery Discovery
}

// Discovery is the signed-address boundary; returned records are verified by Transport.
type Discovery interface {
	Resolve(context.Context, identity.Identity, identity.Record) (identity.Record, error)
	Announce(context.Context, identity.Identity, identity.Record) error
}

// WithDiscovery overrides public signed-record lookup and announcement; nil uses DHT.
func WithDiscovery(d Discovery) Option { return func(o *options) error { o.discovery = d; return nil } }

// Option configures an independent peer transport.
type Option func(*options) error

// WithPort sets the peer port; zero selects an available port.
func WithPort(port int) Option {
	return func(o *options) error {
		if port < 0 || port > 65535 {
			return errors.New("invalid peer port")
		}
		o.port = port
		return nil
	}
}

// WithOffline disables public DHT and NAT mapping, but permits explicit peer connections.
func WithOffline(offline bool) Option {
	return func(o *options) error { o.offline = offline; return nil }
}

// WithMaxBytes bounds received content; the default is 512 MiB.
func WithMaxBytes(limit int64) Option {
	return func(o *options) error {
		if limit < 1 || limit > content.MaxBytes {
			return errors.New("invalid content maximum")
		}
		o.maxBytes = limit
		return nil
	}
}

// Transport owns one independent torrent client and borrows its collection.
type Transport struct {
	hints             *hints
	useHints          bool
	diagnostics       *discoveryStatus
	mu                sync.Mutex
	client            *torrent.Client
	dht               *dht.Server
	discovery         Discovery
	stores            *storage.Collection
	transfers         map[string]*Transfer
	maxBytes          int64
	openGate          chan struct{}
	peerWake          chan struct{}
	peerErr           error
	closed            bool
	seeding           bool
	done              <-chan struct{}
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	once              sync.Once
	closeErr          error
	mappingErr        error
	mappingCleanupErr error
}

// New borrows stores and owns its peer client and background mapping worker.
func New(ctx context.Context, stores *storage.Collection, opts ...Option) (*Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stores == nil {
		return nil, errors.New("storage collection required")
	}
	o := options{maxBytes: content.DefaultMaxBytes}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil option")
		}
		if err := opt(&o); err != nil {
			return nil, err
		}
	}
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = o.port
	cfg.Seed = true
	cfg.NoDefaultPortForwarding = true
	cfg.DefaultStorage = stores
	cfg.DisableWebtorrent = true
	cfg.DisableTrackers = true
	cfg.NoDHT = o.offline
	cfg.MaxUnverifiedBytes = 8 << 20
	hints := newHints(o.cacheFile)
	hints.configure(cfg)
	diagnostics := &discoveryStatus{}
	if !o.offline {
		diagnostics.status.RoutingReady.Started = time.Now()
	}
	cfg.Callbacks.CompletedHandshake = func(_ *torrent.PeerConn, hash torrent.InfoHash) {
		diagnostics.peer(hash.HexString())
	}
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	t := &Transport{client: client, stores: stores, transfers: make(map[string]*Transfer), maxBytes: o.maxBytes, done: lifetime.Done(), cancel: cancel, openGate: make(chan struct{}, 1), discovery: o.discovery}
	t.diagnostics = diagnostics
	t.hints, t.useHints = hints, !o.offline
	t.peerWake = make(chan struct{}, 1)
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.maintainPeers(lifetime)
	}()
	for _, server := range client.DhtServers() {
		if wrapped, ok := server.(torrent.AnacrolixDhtServerWrapper); ok {
			t.dht = wrapped.Server
			break
		}
	}
	if !o.offline {
		if t.dht != nil {
			t.wg.Add(1)
			go func() { defer t.wg.Done(); t.maintainHints(lifetime) }()
			t.wg.Add(1)
			go func() {
				defer t.wg.Done()
				diagnostics.routing(lifetime, func() bool { return t.dht.Stats().GoodNodes > 0 })
			}()
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.mappingCleanupErr = mapPorts(lifetime, client.LocalPort(), discoverMapper, t.recordMappingError)
		}()
	}
	return t, nil
}

func (t *Transport) Port() int { return t.client.LocalPort() }
func (t *Transport) Resolve(ctx context.Context, i identity.Identity, previous identity.Record) (record identity.Record, err error) {
	finish := t.diagnostics.lookup()
	defer func() { finish(err) }()
	if t.discovery != nil {
		r, err := t.discovery.Resolve(ctx, i, previous)
		if err == nil {
			err = i.Verify(r, previous)
		}
		return r, err
	}
	if t.dht == nil {
		return identity.Record{}, errors.New("DHT is disabled")
	}
	return Resolve(ctx, t.dht, i, previous)
}
func (t *Transport) Announce(ctx context.Context, i identity.Identity, r identity.Record) error {
	if err := i.Verify(r, identity.Record{}); err != nil {
		return err
	}
	if t.discovery != nil {
		return t.discovery.Announce(ctx, i, r)
	}
	if t.dht == nil {
		return errors.New("DHT is disabled")
	}
	return Announce(ctx, t.dht, i, r)
}

// Open reuses one transfer per hash within this Transport. Transport owns handles.
func (t *Transport) Open(ctx context.Context, hash string, validate func(content.Manifest) error) (_ *Transfer, err error) {
	select {
	case t.openGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, fs.ErrClosed
	}
	defer func() { <-t.openGate }()
	address, err := identity.ParseMagnet("magnet:?xt=urn:btih:" + hash)
	if err != nil {
		return nil, err
	}
	hash = address.Hash
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fs.ErrClosed
	}
	if transfer := t.transfers[hash]; transfer != nil {
		t.mu.Unlock()
		if validate != nil {
			if err := validate(transfer.Manifest()); err != nil {
				return nil, err
			}
		}
		return transfer, nil
	}
	seeding := t.seeding
	t.mu.Unlock()
	finish := t.diagnostics.metadata(hash, false)
	defer func() { finish(err) }()
	m, err := t.stores.Manifest(ctx, hash)
	var encoded []byte
	if err == nil {
		encoded = m.Metadata()
		t.diagnostics.mu.Lock()
		t.diagnostics.status.CachedMetadata = true
		t.diagnostics.mu.Unlock()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	torrentHandle, _, err := t.client.AddTorrentSpec(&torrent.TorrentSpec{AddTorrentOpts: torrent.AddTorrentOpts{InfoHash: metainfo.NewHashFromHex(hash), InfoBytes: encoded, DisallowDataUpload: !seeding}})
	if err != nil {
		return nil, err
	}
	if t.useHints {
		t.addKnownPeers(torrentHandle, hash)
	}
	select {
	case <-ctx.Done():
		torrentHandle.Drop()
		return nil, ctx.Err()
	case <-t.done:
		return nil, fs.ErrClosed
	case <-torrentHandle.GotInfo():
	}
	m, err = content.ParseMetadata(torrentHandle.Metainfo().InfoBytes, t.maxBytes)
	if err == nil && m.Hash() != hash {
		err = errors.New("torrent metadata hash mismatch")
	}
	if err == nil && validate != nil {
		err = validate(m)
	}
	if err != nil {
		torrentHandle.Drop()
		return nil, err
	}
	if err = t.stores.Prepare(ctx, m); err != nil {
		torrentHandle.Drop()
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, fs.ErrClosed
	}
	if existing := t.transfers[hash]; existing != nil {
		return existing, nil
	}
	transfer := &Transfer{torrent: torrentHandle, manifest: m, done: t.done, files: make(map[string]*torrent.File), fileDownloads: make(map[string]int)}
	// Match by metadata order for single-file and multi-file torrents alike.
	for index, file := range torrentHandle.Files() {
		transfer.files[m.Files()[index].Path] = file
	}
	torrentHandle.SetOnWriteChunkError(func(err error) {
		transfer.mu.Lock()
		transfer.writeErr = err
		transfer.mu.Unlock()
		torrentHandle.DisallowDataDownload()
	})
	if !t.seeding {
		torrentHandle.DisallowDataUpload()
	} else {
		torrentHandle.AllowDataUpload()
	}
	t.transfers[hash] = transfer
	torrentHandle.VerifyData()
	t.wakePeers()
	return transfer, nil
}

// Close stops peer activity and returns client and NAT cleanup errors.
// Repeated calls return the same result.
func (t *Transport) Close() error {
	t.once.Do(func() {
		t.cancel()
		t.wg.Wait()
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		t.closeErr = errors.Join(t.mappingCleanupErr, errors.Join(t.client.Close()...))
		t.closeErr = errors.Join(t.closeErr, t.hints.save())
	})
	return t.closeErr
}

type Transfer struct {
	demandMu      sync.Mutex // Serializes demand transitions without blocking write-error callbacks.
	mu            sync.Mutex
	torrent       *torrent.Torrent
	manifest      content.Manifest
	files         map[string]*torrent.File
	done          <-chan struct{}
	writeErr      error
	active        int
	downloads     int
	fileDownloads map[string]int
	closed        bool
}
type Stats struct {
	Bytes, Total int64
	Peers        int
}

func (t *Transfer) Manifest() content.Manifest { return t.manifest }
func (t *Transfer) Stats() Stats {
	return Stats{Bytes: t.torrent.BytesCompleted(), Total: t.torrent.Length(), Peers: t.torrent.Stats().ActivePeers}
}
func (t *Transfer) Open(ctx context.Context, name string) (content.Reader, error) {
	return t.Read(ctx, name, ReadOptions{})
}

// Read opens a verified file with bounded demand-driven scheduling.
func (t *Transfer) Read(ctx context.Context, name string, options ReadOptions) (content.Reader, error) {
	if options.Mode != Normal && options.Mode != Sequential {
		return nil, errors.New("invalid read mode")
	}
	if _, err := t.manifest.File(name); err != nil {
		return nil, err
	}
	t.mu.Lock()
	err := t.writeErr
	if t.closed {
		err = fs.ErrClosed
	}
	if err == nil {
		t.active++
	}
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case <-t.done:
		t.releaseReader()
		return nil, fs.ErrClosed
	default:
	}
	f := t.files[name]
	if f == nil {
		t.releaseReader()
		return nil, fs.ErrNotExist
	}

	r := f.NewReader()
	r.SetContext(ctx)
	if options.Mode == Sequential {
		r.SetReadahead(1 << 20)
	} else {
		r.SetReadahead(256 << 10)
	}
	return content.LimitReader(&transferReader{Reader: r, transfer: t}, f.Length()), nil
}
func (t *Transfer) Download(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.demandMu.Lock()
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		t.demandMu.Unlock()
		return fs.ErrClosed
	}
	t.downloads++
	t.mu.Unlock()
	t.torrent.DownloadAll()
	t.demandMu.Unlock()
	defer func() {
		t.demandMu.Lock()
		defer t.demandMu.Unlock()
		t.mu.Lock()
		t.downloads--
		remaining := t.downloads
		t.mu.Unlock()
		if remaining == 0 {
			t.torrent.CancelPieces(0, t.torrent.NumPieces())
		}
	}()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		t.mu.Lock()
		err := t.writeErr
		t.mu.Unlock()
		if err != nil {
			return err
		}
		if t.torrent.BytesCompleted() == t.torrent.Length() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.done:
			return fs.ErrClosed
		case <-tick.C:
		}
	}
}

type transferReader struct {
	torrent.Reader
	transfer *Transfer
	once     sync.Once
	err      error
}

func (r *transferReader) Close() error {
	r.once.Do(func() { r.err = r.Reader.Close(); r.transfer.releaseReader() })
	return r.err
}
func (t *Transfer) releaseReader() { t.mu.Lock(); defer t.mu.Unlock(); t.active-- }

// Drop releases a version only when no reader or full-download demand owns it.
func (t *Transport) Drop(ctx context.Context, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	transfer := t.transfers[hash]
	if transfer == nil {
		return nil
	}
	transfer.mu.Lock()
	if transfer.active > 0 || transfer.downloads > 0 {
		transfer.mu.Unlock()
		return storage.ErrInUse
	}
	transfer.closed = true
	transfer.mu.Unlock()
	transfer.torrent.Drop()
	delete(t.transfers, hash)
	t.wakePeers()
	return nil
}
