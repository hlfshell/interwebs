package interwebs

import (
	"errors"
	"time"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/network"
	"github.com/hlfshell/interweb/interwebs/storage"
)

type options struct {
	content                content.Content
	dir, keyFile           string
	backend                storage.Backend
	maxBytes, storageLimit int64
	port                   int
	offline                bool
	interval               time.Duration
	fetchTimeout           time.Duration
	discovery              network.Discovery
	signer                 *identity.Signer
}

// Option configures a Node before it acquires runtime resources.
type Option func(*options) error

// WithFetchTimeout bounds metadata discovery when a version has no reachable seed.
func WithFetchTimeout(d time.Duration) Option {
	return func(o *options) error {
		if d <= 0 {
			return errors.New("invalid fetch timeout")
		}
		o.fetchTimeout = d
		return nil
	}
}

// WithContent selects the single content identity managed by the Node.
func WithContent(c content.Content) Option {
	return func(o *options) error {
		if c == nil {
			return errors.New("content is required")
		}
		o.content = c
		return nil
	}
}

// WithDataDir selects an exclusively owned directory; it is required.
func WithDataDir(dir string) Option { return func(o *options) error { o.dir = dir; return nil } }

// WithKeyFile overrides the default storage.key in the Node data directory.
// Files must contain exactly 32 bytes; oversized files are rejected with bounded reads.
// A missing file is generated only when no encrypted stores exist.
// It cannot be combined with WithStorageBackend.
func WithKeyFile(name string) Option { return func(o *options) error { o.keyFile = name; return nil } }

// WithStorageBackend replaces the default sandboxed backend. Only one is accepted.
// Ownership transfers when this option succeeds; rejected or unevaluated backends
// remain caller-owned. New closes an accepted backend if later setup fails.
func WithStorageBackend(b storage.Backend) Option {
	return func(o *options) error {
		if b == nil {
			return errors.New("nil storage backend")
		}
		if o.backend != nil {
			return errors.New("storage backend already configured")
		}
		o.backend = b
		return nil
	}
}

// WithMaxSiteBytes sets the per-version content limit; default 512 MiB, range 1–1024 MiB.
func WithMaxSiteBytes(n int64) Option {
	return func(o *options) error {
		if n < 1<<20 || n > content.MaxBytes {
			return errors.New("site maximum must be 1–1024 MiB")
		}
		o.maxBytes = n
		return nil
	}
}

// WithStorageLimit bounds total ciphertext, including retained versions and staging.
// The default is 2 GiB. No eviction is implicit.
func WithStorageLimit(n int64) Option {
	return func(o *options) error {
		if n < 1 {
			return errors.New("invalid storage limit")
		}
		o.storageLimit = n
		return nil
	}
}

// WithPort sets the peer listening port; zero selects an available port.
func WithPort(port int) Option {
	return func(o *options) error {
		if port < 0 || port > 65535 {
			return errors.New("invalid peer port")
		}
		o.port = port
		return nil
	}
}

// WithOffline disables public discovery and NAT mapping, not local peer transfers.
func WithOffline(offline bool) Option {
	return func(o *options) error { o.offline = offline; return nil }
}

// WithRefreshInterval opts into periodic refresh. Zero disables refresh (the default).
func WithRefreshInterval(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return errors.New("invalid refresh interval")
		}
		o.interval = d
		return nil
	}
}

// WithSigner supplies signing authority without transferring key ownership.
// Node never writes the private key into its data directory.
func WithSigner(p *identity.Signer) Option {
	return func(o *options) error {
		if p == nil {
			return errors.New("nil signer")
		}
		o.signer = p
		return nil
	}
}

func applyOption(o *options, opt Option) error {
	if opt == nil {
		return errors.New("nil node option")
	}
	return opt(o)
}
