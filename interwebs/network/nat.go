package network

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackpal/gateway"
	natpmp "github.com/jackpal/go-nat-pmp"
)

type portMapper interface {
	AddPortMapping(string, int, int, int) (*natpmp.AddPortMappingResult, error)
}
type mappingTimer func(time.Duration) (<-chan time.Time, func())

func realMappingTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// maintainMappings reports nonfatal mapping failures and returns cleanup failures.
func maintainMappings(ctx context.Context, client portMapper, port int, newTimer mappingTimer, report func(error)) (cleanupErr error) {
	active := make(map[string]bool)
	defer func() {
		for protocol := range active {
			if _, err := client.AddPortMapping(protocol, port, 0, 0); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove %s NAT mapping: %w", protocol, err))
			}
		}
		if cleanupErr != nil {
			report(cleanupErr)
		}
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}

		renew := 30 * time.Minute
		var mappingErr error
		for _, protocol := range []string{"tcp", "udp"} {
			result, err := client.AddPortMapping(protocol, port, port, 3600)
			if err == nil && result == nil {
				err = errors.New("router returned no mapping")
			}
			if err != nil {
				mappingErr = errors.Join(mappingErr, fmt.Errorf("map %s port: %w", protocol, err))
				renew = min(renew, time.Minute)
				continue
			}

			active[protocol] = true
			if int(result.MappedExternalPort) != port {
				mappingErr = errors.Join(mappingErr, fmt.Errorf("map %s port: requested %d, router assigned %d", protocol, port, result.MappedExternalPort))
				if _, err := client.AddPortMapping(protocol, port, 0, 0); err != nil {
					mappingErr = errors.Join(mappingErr, fmt.Errorf("remove alternate %s mapping: %w", protocol, err))
				} else {
					delete(active, protocol)
				}
				renew = min(renew, time.Minute)
				continue
			}

			lease := time.Duration(result.PortMappingLifetimeInSeconds) * time.Second
			renew = min(renew, max(time.Second, lease/2))
		}
		report(mappingErr)

		tick, stop := newTimer(renew)
		select {
		case <-ctx.Done():
			stop()
			return nil
		case <-tick:
			stop()
		}
	}
}

func discoverMapper() (portMapper, error) {
	ip, err := gateway.DiscoverGateway()
	if err != nil {
		return nil, err
	}
	return natpmp.NewClientWithTimeout(ip, 2*time.Second), nil
}

func mapPorts(ctx context.Context, port int, discover func() (portMapper, error), report func(error)) error {
	client, err := discover()
	if err != nil {
		report(fmt.Errorf("discover NAT gateway: %w", err))
		return nil // Outbound peer connections remain usable.
	}
	return maintainMappings(ctx, client, port, realMappingTimer, report)
}

// MappingError reports the latest NAT discovery, renewal, or cleanup failure.
// A successful renewal clears prior mapping errors. Offline transports return nil.
func (t *Transport) MappingError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mappingErr
}

func (t *Transport) recordMappingError(err error) {
	t.mu.Lock()
	t.mappingErr = err
	t.mu.Unlock()
}
