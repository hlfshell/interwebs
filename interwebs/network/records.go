// Package network provides peer transport and mutable torrent discovery.
package network

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/dht/v2/traversal"
	"github.com/anacrolix/torrent/bencode"
	"github.com/hlfshell/interweb/interwebs/identity"
	"sync"
	"sync/atomic"
)

type pointer struct {
	Hash string `bencode:"ih"`
}

func Resolve(ctx context.Context, s *dht.Server, i identity.Identity, previous identity.Record) (identity.Record, error) {
	var mu sync.Mutex
	var best identity.Record
	var conflict bool
	op := traversal.Start(traversal.OperationInput{Alpha: 8, Target: i.Target(), NodeFilter: s.TraversalNodeFilter, DoQuery: func(_ context.Context, addr krpc.NodeAddr) traversal.QueryResult {
		res := s.Get(ctx, dht.NewAddr(addr.UDP()), i.Target(), nil, dht.QueryRateLimiting{})
		if candidate, err := DecodeRecord(i, res.Reply.R, previous); err == nil {
			mu.Lock()
			if candidate.Sequence == best.Sequence && candidate.Hash != best.Hash {
				conflict = true
			}
			if candidate.Sequence > best.Sequence {
				best = candidate
			}
			mu.Unlock()
		}
		return res.TraversalQueryResult(addr)
	}})
	nodes, err := s.TraversalStartingNodes()
	if err != nil {
		op.Stop()
		return identity.Record{}, err
	}
	op.AddNodes(nodes)
	select {
	case <-ctx.Done():
	case <-op.Stalled():
	}
	op.Stop()
	mu.Lock()
	defer mu.Unlock()
	if conflict {
		return identity.Record{}, errors.New("conflicting signed DHT records")
	}
	if best.Hash == "" {
		return identity.Record{}, errors.New("no verified site record found in DHT")
	}
	return best, nil
}

func DecodeRecord(i identity.Identity, result *krpc.Return, previous identity.Record) (identity.Record, error) {
	if result == nil || result.Seq == nil || len(result.V) > 1000 || hex.EncodeToString(result.K[:]) != i.Key {
		return identity.Record{}, errors.New("missing or invalid mutable record fields")
	}
	var p pointer
	if err := bencode.Unmarshal(result.V, &p); err != nil {
		return identity.Record{}, err
	}
	r := identity.Record{Sequence: *result.Seq, Hash: hex.EncodeToString([]byte(p.Hash)), Signature: append([]byte(nil), result.Sig[:]...)}
	return r, i.Verify(r, previous)
}

// Traverse for write tokens, then count acknowledged PUTs rather than treating a
// completed traversal as successful publication.
func Announce(ctx context.Context, s *dht.Server, i identity.Identity, r identity.Record) error {
	if err := i.Verify(r, identity.Record{}); err != nil {
		return err
	}
	var successes atomic.Int32
	p := i.Put(r)
	op := traversal.Start(traversal.OperationInput{Alpha: 8, Target: i.Target(), NodeFilter: s.TraversalNodeFilter, DoQuery: func(_ context.Context, addr krpc.NodeAddr) traversal.QueryResult {
		res := s.Get(ctx, dht.NewAddr(addr.UDP()), i.Target(), nil, dht.QueryRateLimiting{})
		if res.Reply.R != nil && res.Reply.R.Token != nil {
			if out := s.Put(ctx, dht.NewAddr(addr.UDP()), p, *res.Reply.R.Token, dht.QueryRateLimiting{}); out.ToError() == nil {
				successes.Add(1)
			}
		}
		return res.TraversalQueryResult(addr)
	}})
	nodes, err := s.TraversalStartingNodes()
	if err != nil {
		op.Stop()
		return err
	}
	op.AddNodes(nodes)
	select {
	case <-ctx.Done():
	case <-op.Stalled():
	}
	op.Stop()
	if successes.Load() == 0 {
		return errors.New("no DHT nodes acknowledged publication; will retry")
	}
	return nil
}
