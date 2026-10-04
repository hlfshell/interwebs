package interwebs

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/network"
	"github.com/hlfshell/interweb/interwebs/web"
)

// Manifest resolves this Node's current validated content description.
func (n *Node) Manifest(ctx context.Context) (content.Manifest, error) {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return content.Manifest{}, err
	}
	defer release()
	transfer, err := n.resolve(ctx)
	if err != nil {
		return content.Manifest{}, err
	}
	return transfer.Manifest(), nil
}

func (n *Node) load(ctx context.Context, hash string) (*network.Transfer, error) {
	n.mu.Lock()
	transfer := n.transfers[hash]
	n.mu.Unlock()
	if transfer != nil {
		return transfer, nil
	}
	fetch, cancel := context.WithTimeout(ctx, n.fetchTimeout)
	defer cancel()
	transfer, err := n.transport.Open(fetch, hash, n.content.Validate)
	if err != nil {
		return nil, err
	}

	if validator, ok := n.content.(interface {
		ValidateFiles(context.Context, func(context.Context, string) (content.Reader, error)) error
	}); ok {
		if err := validator.ValidateFiles(ctx, transfer.Open); err != nil {
			return nil, errors.Join(err, n.transport.Drop(ctx, hash))
		}
	}
	n.mu.Lock()
	n.transfers[hash] = transfer
	n.mu.Unlock()
	return transfer, nil
}

func (n *Node) resolve(ctx context.Context) (*network.Transfer, error) {
	return n.resolveVersion(ctx, false)
}

func (n *Node) resolveVersion(ctx context.Context, refresh bool) (*network.Transfer, error) {
	n.mu.Lock()
	state := n.state
	n.mu.Unlock()
	if !refresh && state.Current != "" {
		return n.load(ctx, state.Current)
	}
	hash, record := state.Identity.Hash, state.Record
	if state.Identity.Key != "" {
		if refresh || (record.Hash == "" && n.source == nil) {
			lookup, cancel := context.WithTimeout(ctx, 30*time.Second)
			candidate, err := n.transport.Resolve(lookup, state.Identity, state.Record)
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil && (refresh || record.Hash == "") {
				return nil, err
			}
			if err == nil {
				record = candidate
				// Persist the authenticated high-water mark even if its content is unavailable.
				n.mu.Lock()
				n.state.Record = record
				n.mu.Unlock()
				if err := n.save(ctx); err != nil {
					return nil, err
				}
			}
		}
		hash = record.Hash
	}
	if hash == "" {
		return nil, content.ErrUnresolved
	}

	transfer, err := n.load(ctx, hash)
	if err == nil && hash != state.Current {
		if browser, ok := n.content.(interface{ EntryPoint() string }); ok && browser.EntryPoint() != "" {
			if _, exists := transfer.Manifest().File(browser.EntryPoint()); exists == nil {
				err = ready(ctx, transfer, browser.EntryPoint())
			}
		}
	}
	if err != nil {
		if hash != state.Current {

			// Failed candidates must not retain protected storage indefinitely.
			if cleanupErr := n.drop(context.Background(), hash); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
				n.mu.Lock()
				n.lastError = err.Error()
				n.mu.Unlock()
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		n.mu.Lock()
		n.refreshError = err.Error()
		n.mu.Unlock()
		if state.Current != "" && state.Current != hash {
			previous, previousErr := n.load(ctx, state.Current)
			if previousErr == nil {
				return previous, nil
			}
		}
		return nil, err
	}
	n.mu.Lock()
	n.state.Current = hash
	if record.Hash != "" && (len(n.state.History) == 0 || n.state.History[len(n.state.History)-1].Hash != hash) {
		n.state.History = append(n.state.History, record)
	}
	n.mu.Unlock()

	if err := n.save(ctx); err != nil {
		return nil, err
	}
	return transfer, nil
}

func ready(ctx context.Context, transfer *network.Transfer, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	reader, err := transfer.Open(ctx, name)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(io.Discard, reader, make([]byte, 64<<10))
	return errors.Join(err, reader.Close())
}

// View prepares a version-specific loopback URL without launching a browser.
func (n *Node) View(ctx context.Context) (string, error) {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	browser, ok := n.content.(interface{ EntryPoint() string })
	if !ok || browser.EntryPoint() == "" {
		return "", ErrCapability
	}
	transfer, err := n.resolve(ctx)
	if err != nil {
		return "", err
	}
	if _, err := transfer.Manifest().File(browser.EntryPoint()); err != nil {
		return "", ErrCapability
	}
	err = ready(ctx, transfer, browser.EntryPoint())
	if err != nil {
		return "", err
	}
	hash := transfer.Manifest().Hash()
	n.mu.Lock()
	server := n.servers[hash]
	n.mu.Unlock()
	if server == nil {
		// Node explicitly closes servers; do not bind them to this operation's context.
		server, err = web.New(context.Background(), transfer)
		if err != nil {
			return "", err
		}
		n.mu.Lock()
		n.servers[hash] = server
		n.mu.Unlock()
	}
	return server.URL(), nil
}

func (n *Node) Download(ctx context.Context) error {
	ctx, release, err := n.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	transfer, err := n.resolve(ctx)
	if err != nil {
		return err
	}
	err = transfer.Download(ctx)
	n.mu.Lock()
	n.lastError = errorText(err)
	n.mu.Unlock()
	return err
}
