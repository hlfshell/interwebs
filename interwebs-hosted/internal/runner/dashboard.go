package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

// Dashboard renders complete JSON status records as readable snapshots. Redraw
// should only be enabled for an interactive terminal; redirected output is plain.
type Dashboard struct {
	output io.Writer
	redraw bool
}

func NewDashboard(output io.Writer, redraw bool) *Dashboard {
	return &Dashboard{output: output, redraw: redraw}
}

func (d *Dashboard) Write(record []byte) (int, error) {
	var snapshot status
	if err := json.Unmarshal(record, &snapshot); err != nil {
		return 0, fmt.Errorf("decode dashboard status: %w", err)
	}
	var b strings.Builder
	if d.redraw {
		b.WriteString("\x1b[H\x1b[2J")
	}
	network := "public DHT"
	if !snapshot.PublicDiscovery {
		network = "offline"
	}
	fmt.Fprintf(&b, "interweb | %s | %s | PID %d\n", clean(snapshot.Event), network, snapshot.PID)
	fmt.Fprintf(&b, "Updated %s", snapshot.Updated.Local().Format("2006-01-02 15:04:05 MST"))
	if time.Since(snapshot.Updated) > 15*time.Second && snapshot.Event != "stopped" {
		b.WriteString("  [STALE: runner may be unavailable]")
	}
	b.WriteString("\n\n")
	for _, site := range snapshot.Sites {
		core := site.State.Core
		seed := "off"
		if core.Seeding {
			seed = "on"
		}
		fmt.Fprintf(&b, "%s  [%s]\n", clean(site.Name), clean(site.Phase))
		if site.URL != "" {
			fmt.Fprintf(&b, "  Browser: %s\n", clean(site.URL))
		}
		progress := "waiting for metadata"
		if core.Total > 0 {
			progress = fmt.Sprintf("%s / %s (%.1f%%)", size(core.Bytes), size(core.Total), 100*float64(core.Bytes)/float64(core.Total))
		}
		fmt.Fprintf(&b, "  Download: %s | Peers: %d | Seeding: %s\n", progress, core.Peers, seed)
		fmt.Fprintf(&b, "  Version: %d | Signed address announced: %d\n", core.Record.Sequence, core.AnnouncedSequence)
		if op := site.Operation; op != nil {
			fmt.Fprintf(&b, "  Work: %s (%s)", clean(op.Kind), clean(string(op.State)))
			if !op.Started.IsZero() && op.Finished.IsZero() {
				fmt.Fprintf(&b, " for %s", time.Since(op.Started).Round(time.Second))
			}
			b.WriteByte('\n')
			if op.Kind == "publish" {
				b.WriteString("  Source preparation: byte progress unavailable (download above is stored content)\n")
			}
		}
		for _, failure := range []struct{ label, message string }{
			{"Last attempt", site.Error}, {"Backend", site.State.Error},
			{"Peer announcement", core.PeerAnnouncementError}, {"Signed announcement", core.AnnouncementError},
			{"NAT mapping", core.MappingError}, {"Download", core.DownloadError},
			{"Refresh", core.RefreshError}, {"Persistence", core.PersistenceError},
		} {
			if failure.message != "" {
				fmt.Fprintf(&b, "  %s: %s\n", failure.label, clean(failure.message))
			}
		}
		b.WriteByte('\n')
	}
	if len(snapshot.Sites) == 0 {
		b.WriteString("No sites configured.\n\n")
	}
	b.WriteString("Status snapshots are not a connectivity guarantee. Ctrl+C stops this command.\n")
	if _, err := io.WriteString(d.output, b.String()); err != nil {
		return 0, err
	}
	return len(record), nil
}

func size(bytes int64) string {
	if bytes < 1<<20 {
		return fmt.Sprintf("%.1f KiB", float64(bytes)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
}

// Names and errors can contain remote input; never let them control the terminal.
func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, value)
}
