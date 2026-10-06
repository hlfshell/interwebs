package network

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hlfshell/interweb/interwebs/internal/profile"
)

const (
	dhtLifetime  = 24 * time.Hour
	peerLifetime = 2 * time.Hour
	maxHints     = 512
	maxHintBytes = 256 << 10
)

type hint struct {
	Address string
	Hash    string // Empty for DHT contacts; otherwise an immutable torrent hash.
	Seen    time.Time
}

// hints are untrusted connection suggestions, never identity or content authority.
// Only literal IP endpoints are accepted; reading this file cannot trigger DNS.
type hints struct {
	mu                sync.Mutex
	file              string
	entries           map[string]hint
	generation, saved uint64
	err               error
}

func newHints(file string) *hints {
	h := &hints{file: file, entries: make(map[string]hint)}
	if file == "" {
		return h
	}
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return h
	}
	if err != nil {
		h.err = fmt.Errorf("read discovery cache: %w", err)
		return h
	}
	b, err := io.ReadAll(io.LimitReader(f, maxHintBytes+1))
	err = errors.Join(err, f.Close())
	var entries []hint
	if err == nil && len(b) > maxHintBytes {
		err = errors.New("discovery cache exceeds limit")
	}
	if err == nil {
		err = json.Unmarshal(b, &entries)
	}
	if err == nil && len(entries) > maxHints {
		err = errors.New("too many discovery hints")
	}
	if err != nil {
		h.err = fmt.Errorf("read discovery cache: %w", err)
		return h
	}
	now := time.Now()
	for _, entry := range entries {
		h.remember(entry, now)
	}
	// Rewrite on the next checkpoint even if every loaded entry has expired.
	h.generation++
	return h
}

func validHint(entry hint, now time.Time) bool {
	address, err := netip.ParseAddrPort(entry.Address)
	if err != nil || address.Port() == 0 || address.Addr().Zone() != "" || address.Addr().IsUnspecified() || address.Addr().IsMulticast() {
		return false
	}
	// Local/LAN addresses remain useful for explicitly connected local peers.
	if !address.Addr().IsGlobalUnicast() && !address.Addr().IsLoopback() {
		return false
	}
	lifetime := dhtLifetime
	if entry.Hash != "" {
		b, err := hex.DecodeString(entry.Hash)
		if err != nil || len(b) != 20 {
			return false
		}
		lifetime = peerLifetime
	}
	return !entry.Seen.IsZero() && !entry.Seen.After(now) && now.Sub(entry.Seen) < lifetime
}

func (h *hints) remember(entry hint, now time.Time) {
	if !validHint(entry, now) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := entry.Hash + "/" + entry.Address
	if old, ok := h.entries[key]; ok {
		if entry.Seen.Sub(old.Seen) < time.Minute {
			return
		}
	} else {
		// Cap each torrent at 16 peers and DHT contacts at 64, as well as the
		// global bound. Evict oldest first; callbacks never perform disk I/O.
		limit := 16
		if entry.Hash == "" {
			limit = 64
		}
		count, oldest, global := 0, "", ""
		for k, v := range h.entries {
			if !validHint(v, now) {
				delete(h.entries, k)
				continue
			}
			if global == "" || v.Seen.Before(h.entries[global].Seen) {
				global = k
			}
			if v.Hash == entry.Hash {
				count++
				if oldest == "" || v.Seen.Before(h.entries[oldest].Seen) {
					oldest = k
				}
			}
		}
		if count >= limit {
			delete(h.entries, oldest)
		}
		if len(h.entries) >= maxHints {
			delete(h.entries, global)
		}
	}
	h.entries[key] = entry
	h.generation++
}

func (h *hints) addresses(hash string, now time.Time) []string {
	h.mu.Lock()
	var entries []hint
	for _, entry := range h.entries {
		if entry.Hash == hash && validHint(entry, now) {
			entries = append(entries, entry)
		}
	}
	h.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Seen.After(entries[j].Seen) })
	addresses := make([]string, 0, len(entries))
	for _, entry := range entries {
		addresses = append(addresses, entry.Address)
	}
	return addresses
}

// save has one owner: the maintenance worker, then Close after that worker exits.
func (h *hints) save() error {
	if h.file == "" {
		return nil
	}
	h.mu.Lock()
	now := time.Now()
	for key, entry := range h.entries {
		if !validHint(entry, now) {
			delete(h.entries, key)
			h.generation++
		}
	}
	if h.saved == h.generation {
		h.mu.Unlock()
		return nil
	}
	generation := h.generation
	entries := make([]hint, 0, len(h.entries))
	for _, entry := range h.entries {
		entries = append(entries, entry)
	}
	h.mu.Unlock()
	b, err := json.Marshal(entries)
	if err == nil {
		err = profile.AtomicWrite(h.file, b)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
	if err == nil {
		h.saved = generation
	}
	return err
}

func (h *hints) failure() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return h.err.Error()
	}
	return ""
}
