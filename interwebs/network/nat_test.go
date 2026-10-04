package network

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	natpmp "github.com/jackpal/go-nat-pmp"
)

type mappingCall struct {
	protocol                     string
	internal, external, lifetime int
}
type fakeMapper struct {
	mu                 sync.Mutex
	calls              []mappingCall
	alternate, failUDP bool
	cleanupErr         error
}

func (f *fakeMapper) AddPortMapping(protocol string, internal, external, lifetime int) (*natpmp.AddPortMappingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, mappingCall{protocol, internal, external, lifetime})
	if lifetime == 0 && f.cleanupErr != nil {
		return nil, f.cleanupErr
	}
	if f.failUDP && protocol == "udp" && lifetime > 0 {
		return nil, errors.New("router unavailable")
	}
	port := external
	if f.alternate && lifetime > 0 {
		port++
	}
	return &natpmp.AddPortMappingResult{MappedExternalPort: uint16(port), PortMappingLifetimeInSeconds: 20}, nil
}

func TestGatewayFailureIsObservableWithoutStoppingTransport(t *testing.T) {
	failure := errors.New("no gateway")
	transport := &Transport{}
	err := mapPorts(t.Context(), 1234, func() (portMapper, error) {
		return nil, failure
	}, transport.recordMappingError)
	if err != nil {
		t.Fatal("nonfatal discovery failure treated as cleanup failure", err)
	}
	if !errors.Is(transport.MappingError(), failure) || !strings.Contains(transport.MappingError().Error(), "discover NAT gateway") {
		t.Fatal("gateway failure not reported", transport.MappingError())
	}
}

func TestTransportCloseReturnsMappingCleanupError(t *testing.T) {
	stores := networkStores(t, t.TempDir())
	transport := localTransport(t, stores)
	failure := errors.New("mapping cleanup failed")
	transport.mappingCleanupErr = failure // Offline fixture has no mapping worker.
	for range 2 {
		if err := transport.Close(); !errors.Is(err, failure) {
			t.Fatal("close lost mapping cleanup error", err)
		}
	}
}

func TestMappingRecoveryClearsErrorAndCleanupPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failure := errors.New("router cleanup failed")
	mapper := &fakeMapper{failUDP: true, cleanupErr: failure}
	transport := &Transport{}
	renewals := 0
	timer := func(time.Duration) (<-chan time.Time, func()) {
		renewals++
		if renewals == 1 {
			if err := transport.MappingError(); err == nil || !strings.Contains(err.Error(), "map udp port") {
				t.Fatal("mapping failure not visible", err)
			}
			mapper.failUDP = false
		} else {
			if err := transport.MappingError(); err != nil {
				t.Fatal("successful renewal retained error", err)
			}
			cancel()
		}
		tick := make(chan time.Time, 1)
		if renewals == 1 {
			tick <- time.Now()
		}
		return tick, func() {}
	}
	err := maintainMappings(ctx, mapper, 42069, timer, transport.recordMappingError)
	if !errors.Is(err, failure) || !errors.Is(transport.MappingError(), failure) {
		t.Fatalf("cleanup error lost: returned=%v status=%v", err, transport.MappingError())
	}
}
func TestNATMappingRenewalAndCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeMapper{}
	ticks := make(chan time.Time)
	scheduled := make(chan time.Duration, 2)
	done := make(chan struct{})
	go func() {
		maintainMappings(ctx, f, 42069, func(d time.Duration) (<-chan time.Time, func()) { scheduled <- d; return ticks, func() {} }, func(error) {})
		close(done)
	}()
	if d := <-scheduled; d != 10*time.Second {
		t.Fatalf("did not renew before router's actual lease expiry: %v", d)
	}
	ticks <- time.Now()
	<-scheduled
	cancel()
	<-done
	if len(f.calls) != 6 {
		t.Fatalf("expected two mappings, two renewals, two removals: %v", f.calls)
	}
	for _, c := range f.calls[4:] {
		if c.external != 0 || c.lifetime != 0 {
			t.Fatal("mapping not removed")
		}
	}
}
func TestNATRejectsAlternatePortAndRetriesFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeMapper{alternate: true, failUDP: true}
	scheduled := make(chan time.Duration, 1)
	done := make(chan struct{})
	go func() {
		maintainMappings(ctx, f, 42069, func(d time.Duration) (<-chan time.Time, func()) {
			scheduled <- d
			return make(chan time.Time), func() {}
		}, func(error) {})
		close(done)
	}()
	if d := <-scheduled; d != time.Minute {
		t.Fatalf("expected retry after failed mapping: %v", d)
	}
	cancel()
	<-done
	if len(f.calls) != 3 || f.calls[1].external != 0 || f.calls[1].lifetime != 0 {
		t.Fatalf("alternate mapping was not revoked: %v", f.calls)
	}
}
