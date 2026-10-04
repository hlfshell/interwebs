package interwebs

import (
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/network"
)

// Availability distinguishes missing metadata from incomplete payloads.
type Availability string

const (
	Unresolved Availability = "unresolved"
	Partial    Availability = "partial"
	Complete   Availability = "complete"
)

// Capabilities describe supported operations, including source and signing requirements.
type Capabilities struct{ Publish, View bool }

// Status is a copied, non-networking snapshot. Errors are separate from progress.
type Status struct {
	// AnnouncedSequence is zero until a signed record announcement succeeds.
	AnnouncedSequence   int64
	Name, Magnet, URL   string
	Identity            identity.Identity
	Record              identity.Record
	History             []identity.Record
	Current, Discovered string
	Availability        Availability
	Seeding             bool
	Bytes, Total        int64
	Peers               int
	Files               []network.FileStatus
	DownloadError       string
	RefreshError        string
	PersistenceError    string
	AnnouncementError   string
	// MappingError reports nonfatal NAT discovery, mapping, or cleanup failures.
	MappingError string
	Capabilities Capabilities
}

func (n *Node) Capabilities() Capabilities {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.capabilities()
}

func (n *Node) capabilities() Capabilities {
	view, browser := n.content.(interface{ EntryPoint() string })
	if browser {
		browser = view.EntryPoint() != ""
		if transfer := n.transfers[n.state.Current]; transfer != nil {
			_, err := transfer.Manifest().File(view.EntryPoint())
			browser = err == nil
		}
	}
	return Capabilities{Publish: n.signer != nil && n.source != nil, View: browser}
}

func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.state
	out := Status{MappingError: errorText(n.transport.MappingError()), AnnouncedSequence: n.announcedSequence, Name: s.Name, Magnet: s.Identity.Magnet(), Identity: s.Identity,
		Record: s.Record.Clone(), Current: s.Current, Discovered: s.Record.Hash,
		History: make([]identity.Record, len(s.History)), Availability: Unresolved,
		Seeding: n.seeding, Capabilities: n.capabilities(), DownloadError: n.lastError,
		RefreshError: n.refreshError, PersistenceError: errorText(n.persistenceErr),
		AnnouncementError: n.announceError, Files: make([]network.FileStatus, 0)}
	for i, r := range s.History {
		out.History[i] = r.Clone()
	}
	if t := n.transfers[s.Current]; t != nil {
		stats := t.Stats()
		out.Bytes, out.Total, out.Peers = stats.Bytes, stats.Total, stats.Peers
		out.Files = t.Files()
		out.Availability = Partial
		if stats.Bytes == stats.Total {
			out.Availability = Complete
		}
	}
	if server := n.servers[s.Current]; server != nil {
		out.URL = server.URL()
	}
	return out
}
