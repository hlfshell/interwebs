// Package app manages isolated application profiles above the interweb primitives.
// Desktop and hosted adapters supply presentation, authentication, and browser launch.
package app

import (
	"errors"
	"time"

	core "github.com/hlfshell/interweb/interwebs"
)

var (
	ErrClosed   = errors.New("application is closed")
	ErrNotFound = errors.New("application object not found")
	ErrPaused   = errors.New("profile is paused")
	ErrBusy     = errors.New("operation queue is full")
	ErrSource   = errors.New("source is outside allowed roots")
	ErrInUse    = errors.New("site has active views or operations")
)

// CacheMode controls ordinary visits, never favorite, hosted, or owned content.
type CacheMode string

const (
	Timed       CacheMode = "timed"
	Session     CacheMode = "session"
	NoRetention CacheMode = "none"
)

// Settings are persisted application policy. The storage target is not a hard quota.
type Settings struct {
	Cache           CacheMode
	Retention       time.Duration
	StorageTarget   int64
	RefreshInterval time.Duration
}

func DefaultSettings() Settings {
	return Settings{Cache: Timed, Retention: 24 * time.Hour, StorageTarget: 4 << 30, RefreshInterval: 30 * time.Minute}
}

func (s Settings) validate() error {
	if s.Cache != Timed && s.Cache != Session && s.Cache != NoRetention {
		return errors.New("invalid cache mode")
	}
	if s.Retention < 0 || (s.Cache == Timed && s.Retention == 0) || s.StorageTarget < 1 || s.RefreshInterval < 0 {
		return errors.New("invalid profile settings")
	}
	return nil
}

// SiteStatus separates saved intent from actual core state and application failures.
type SiteStatus struct {
	ID, Name, Kind, Address, Source string
	Favorite, Hosting, Live, Owned  bool
	UnpublishedChanges              bool
	Views                           int
	LastUsed                        time.Time
	Error                           string
	Core                            core.Status
}

type ProfileStatus struct {
	ID, Name        string
	Paused          bool
	Settings        Settings
	Sites           []SiteStatus
	StoredBytes     int64
	StoragePressure bool
	Error           string
}

// Change is a lossy invalidation notification. Read Status for authoritative state.
type Change struct{ SiteID, OperationID string }

type OperationState string

const (
	Queued    OperationState = "queued"
	Running   OperationState = "running"
	Succeeded OperationState = "succeeded"
	Failed    OperationState = "failed"
	Canceled  OperationState = "canceled"
)

type OperationStatus struct {
	ID, SiteID, Kind  string
	State             OperationState
	Error             string
	Started, Finished time.Time
	Bytes, Total      int64
}

type siteConfig struct{ filterTypes bool }
type SiteOption func(*siteConfig) error

// WithFileTypeFiltering opts into the core web-resource allowlist; default off.
func WithFileTypeFiltering(enabled bool) SiteOption {
	return func(c *siteConfig) error { c.filterTypes = enabled; return nil }
}

// Result holds the result appropriate for an operation. View owns a retention lease.
type Result struct {
	Publication *core.Publication
	Refresh     *core.RefreshResult
	View        *View
}

type config struct {
	offline                        bool
	maxSiteBytes, nodeStorageLimit int64
}

type Option func(*config) error

// WithOffline disables public discovery; explicit local operations still work.
func WithOffline(value bool) Option {
	return func(c *config) error { c.offline = value; return nil }
}

// WithMaxSiteBytes sets the per-version limit for all managed Nodes (default 512 MiB).
func WithMaxSiteBytes(size int64) Option {
	return func(c *config) error {
		if size < 1<<20 || size > 1<<30 {
			return errors.New("site maximum must be 1–1024 MiB")
		}
		c.maxSiteBytes = size
		return nil
	}
}

// WithNodeStorageLimit sets each Node's hard ciphertext limit (default 2 GiB).
func WithNodeStorageLimit(size int64) Option {
	return func(c *config) error {
		if size < 1 {
			return errors.New("invalid node storage limit")
		}
		c.nodeStorageLimit = size
		return nil
	}
}

type profileConfig struct {
	roots    []string
	settings Settings
}
type ProfileOption func(*profileConfig) error

// WithSourceRoots grants publication access to folders below these operator-selected roots.
func WithSourceRoots(roots ...string) ProfileOption {
	return func(c *profileConfig) error { c.roots = append([]string(nil), roots...); return nil }
}

func WithSettings(settings Settings) ProfileOption {
	return func(c *profileConfig) error {
		if err := settings.validate(); err != nil {
			return err
		}
		c.settings = settings
		return nil
	}
}
