package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	core "github.com/hlfshell/interweb/interwebs"
	"github.com/hlfshell/interweb/interwebs/app"
	"github.com/hlfshell/interweb/interwebs/network"
)

func TestDashboardExplainsProgressWithoutConfusingPublication(t *testing.T) {
	snapshot := status{Event: "status", Updated: time.Now(), PublicDiscovery: true, PID: 42,
		Sites: []siteStatus{{Name: "blog", Phase: "updating", URL: "http://127.0.0.1:8082/",
			State:     app.SiteStatus{Core: core.Status{Bytes: 1 << 20, Total: 2 << 20, Peers: 3, Seeding: true}},
			Operation: &app.OperationStatus{Kind: "publish", State: app.Running, Started: time.Now()},
		}}}
	record, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, redraw := range []bool{false, true} {
		var output bytes.Buffer
		n, err := NewDashboard(&output, redraw).Write(record)
		if err != nil || n != len(record) {
			t.Fatal(n, err)
		}
		for _, want := range []string{"blog  [updating]", "public DHT", "50.0%", "Peers: 3", "Seeding: on", "Source preparation: byte progress unavailable", "http://127.0.0.1:8082/"} {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("missing %q in %s", want, output.String())
			}
		}
		if strings.Contains(output.String(), "\x1b") != redraw {
			t.Fatal("unexpected terminal control codes")
		}
	}
}

func TestDashboardShowsDiscoveryStages(t *testing.T) {
	now := time.Now()
	snapshot := status{Updated: now, Sites: []siteStatus{{State: app.SiteStatus{Core: core.Status{Discovery: network.DiscoveryStatus{
		Lookup:         network.Stage{Started: now, Elapsed: 1500 * time.Millisecond},
		Metadata:       network.Stage{Started: now, Finished: now, Elapsed: 2 * time.Millisecond},
		CachedMetadata: true,
		FirstPeer:      network.Stage{Started: now, Finished: now, Error: "failed\x1b"},
	}}}}}}
	record, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := NewDashboard(&output, false).Write(record); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Signed lookup: 1.5s (pending)", "Metadata: 2ms (done, cached)", "First peer handshake: 0s (failed: failed )"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q in %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "\x1b") {
		t.Fatal("untrusted terminal controls")
	}
}

func TestDashboardShowsStaleAndUnresolvedStatusAndEscapesRemoteText(t *testing.T) {
	snapshot := status{Event: "status", Updated: time.Now().Add(-time.Minute), Sites: []siteStatus{{Name: "bad\x1b[2J\nname", Phase: "retrying", Error: "timeout\r\x1b"}}}
	record, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	dashboard := NewDashboard(&output, false)
	if _, err := dashboard.Write(record); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"STALE", "waiting for metadata", "Last attempt: timeout", "offline"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.ContainsAny(output.String(), "\x1b\r") {
		t.Fatal("untrusted terminal controls passed through")
	}
	if _, err := dashboard.Write([]byte("invalid")); err == nil {
		t.Fatal("malformed status accepted")
	}
	failure := errors.New("output failed")
	if _, err := NewDashboard(brokenOutput{failure}, false).Write(record); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

type brokenOutput struct{ err error }

func (b brokenOutput) Write([]byte) (int, error) { return 0, b.err }
