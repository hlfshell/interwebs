// Package interwebs runs independent Nodes, each managing one Content identity.
package interwebs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/internal/profile"
	"github.com/hlfshell/interweb/interwebs/network"
	"github.com/hlfshell/interweb/interwebs/storage"
	"github.com/hlfshell/interweb/interwebs/web"
)

// ErrCapability means the content or credentials do not support an operation.
var ErrCapability = errors.New("content does not support this operation")

type nodeState struct {
	Version  int               `json:"version"`
	Name     string            `json:"name"`
	SourceID string            `json:"sourceID,omitempty"`
	Identity identity.Identity `json:"identity"`
	Record   identity.Record   `json:"record"`
	History  []identity.Record `json:"history"`
	Current  string            `json:"current"`
}

// Publication identifies a durable local version; discovery is asynchronous.
type Publication struct {
	Magnet string
	Record identity.Record
}

// Node owns all runtime resources for exactly one Content. It never shares them.
type Node struct {
	mu                sync.Mutex
	gate              chan struct{}
	state             nodeState
	content           content.Content
	source            content.Source
	signer            *identity.Signer
	profile           *profile.Profile
	stores            *storage.Collection
	transport         *network.Transport
	transfers         map[string]*network.Transfer
	servers           map[string]*web.Server
	seeding           bool
	lastError         string
	announceError     string
	announcedSequence int64
	refreshError      string
	wake              chan struct{}
	done              <-chan struct{}
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	once              sync.Once
	closeErr          error
	persistenceErr    error
	fetchTimeout      time.Duration
}

// New creates an independent Node; ctx owns its lifetime.
// Accepted backends are owned by Node, including cleanup on initialization failure.
// An omitted key path defaults to storage.key in the data directory.
func New(ctx context.Context, opts ...Option) (node *Node, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("create node: %w", resultErr)
		}
	}()

	// Configure before acquiring the profile and its stores.
	o := options{
		fetchTimeout: 90 * time.Second,
		maxBytes:     content.DefaultMaxBytes,
		storageLimit: 2 << 30,
	}
	for _, opt := range opts {
		if err := applyOption(&o, opt); err != nil {
			if o.backend != nil {
				err = errors.Join(err, o.backend.Close())
			}
			return nil, err
		}
	}
	backend := o.backend
	defer func() {
		if resultErr != nil && backend != nil {
			resultErr = errors.Join(resultErr, backend.Close())
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.content == nil || o.dir == "" {
		return nil, errors.New("content and a distinct node data directory are required")
	}
	if o.backend != nil && o.keyFile != "" {
		return nil, errors.New("storage backend and key file are mutually exclusive")
	}
	dir, err := filepath.Abs(o.dir)
	if err != nil {
		return nil, err
	}
	if guard, ok := o.content.(interface{ ValidateStorageDir(string) error }); ok {
		if err := guard.ValidateStorageDir(dir); err != nil {
			return nil, err
		}
	}
	p, err := profile.Open(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("open profile: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, p.Close())
		}
	}()

	// Restore the identity and validate the source and signing authority.
	state, source, err := loadState(ctx, p, o.content, o.signer)
	if err != nil {
		return nil, fmt.Errorf("load node state: %w", err)
	}

	// Build storage before starting network workers.
	if backend == nil {
		key, err := profile.StorageKey(ctx, dir, o.keyFile)
		if err != nil {
			return nil, fmt.Errorf("load storage key: %w", err)
		}
		backend, err = storage.NewSandboxed(ctx, filepath.Join(dir, "data"), key)
		clear(key)
		if err != nil {
			return nil, fmt.Errorf("open sandboxed storage: %w", err)
		}
	}
	stores, err := storage.New(ctx, backend, storage.WithMaxSiteBytes(o.maxBytes), storage.WithStorageLimit(o.storageLimit))
	backend = nil // Collection now owns cleanup, including failed initialization.
	if err != nil {
		return nil, fmt.Errorf("open storage collection: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, stores.Close())
		}
	}()

	// Start transport, then persist state before enabling background work.
	lifetime, cancel := context.WithCancel(ctx)
	transport, err := network.New(lifetime, stores,
		network.WithPort(o.port),
		network.WithOffline(o.offline),
		network.WithMaxBytes(o.maxBytes),
		network.WithDiscovery(o.discovery),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start transport: %w", err)
	}
	n := &Node{
		gate:         make(chan struct{}, 1),
		state:        state,
		content:      o.content,
		source:       source,
		signer:       o.signer,
		profile:      p,
		stores:       stores,
		transport:    transport,
		transfers:    make(map[string]*network.Transfer),
		servers:      make(map[string]*web.Server),
		done:         lifetime.Done(),
		cancel:       cancel,
		wake:         make(chan struct{}, 1),
		fetchTimeout: o.fetchTimeout,
	}

	if err = n.save(ctx); err != nil {
		cancel()
		return nil, errors.Join(fmt.Errorf("persist node state: %w", err), transport.Close())
	}
	n.wg.Add(1)
	go n.maintain(lifetime, o.interval, o.offline && o.discovery == nil)
	go func() { <-lifetime.Done(); n.Close() }()
	return n, nil
}

// operation serializes workflows and binds their cancellation to Node lifetime.
func (n *Node) operation(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	select {
	case <-n.done:
		return nil, nil, fs.ErrClosed
	default:
	}
	select {
	case n.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-n.done:
		return nil, nil, fs.ErrClosed
	}
	work, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-n.done:
			cancel()
		case <-work.Done():
		}
	}()
	return work, func() { cancel(); <-n.gate }, nil
}

func (n *Node) save(ctx context.Context) error {
	n.mu.Lock()
	encoded, err := json.MarshalIndent(n.state, "", "  ")
	n.mu.Unlock()
	if err != nil {
		return err
	}
	err = n.profile.WriteState(ctx, encoded)
	n.mu.Lock()
	n.persistenceErr = err
	n.mu.Unlock()
	return err
}

// Close stops workers and releases resources without deleting stored data.
// Repeated calls return the same cleanup result.
func (n *Node) Close() error {
	n.once.Do(func() {
		n.cancel()
		n.wg.Wait()
		n.gate <- struct{}{}
		defer func() { <-n.gate }()
		var errs []error
		for _, server := range n.servers {
			errs = append(errs, server.Close())
		}
		errs = append(errs, n.transport.Close(), n.stores.Close(), n.profile.Close())
		n.closeErr = errors.Join(errs...)
	})
	return n.closeErr
}
