package storage

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/site"
)

type measuredBackend struct {
	Backend
	accounting time.Duration
	calls      int
}

func (b *measuredBackend) Install(ctx context.Context, from, to string) error {
	return b.Backend.(Installer).Install(ctx, from, to)
}

func (b *measuredBackend) Usage(ctx context.Context, hash string) (Usage, error) {
	start := time.Now()
	usage, err := b.Backend.Usage(ctx, hash)
	b.accounting += time.Since(start)
	b.calls++
	return usage, err
}

func (b *measuredBackend) Growth(ctx context.Context, hash string, size int64) (int64, error) {
	start := time.Now()
	growth, err := b.Backend.Growth(ctx, hash, size)
	b.accounting += time.Since(start)
	b.calls++
	return growth, err
}

// BenchmarkSnapshot measures the real encrypted path, including quota checks.
// The backend is owned by one synchronous snapshot; counters need no mutex.
func BenchmarkSnapshot(b *testing.B) {
	for _, test := range []struct {
		mib, files int
		unchanged  bool
	}{{8, 1, false}, {32, 1, false}, {64, 1, false}, {8, 256, false}, {32, 256, false}, {64, 256, false}, {64, 256, true}} {
		name := fmt.Sprintf("%dMiB-%dfiles", test.mib, test.files)
		if test.unchanged {
			name += "-unchanged"
		}
		b.Run(name, func(b *testing.B) {
			folder := b.TempDir()
			if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("benchmark"), 0600); err != nil {
				b.Fatal(err)
			}
			for i := range test.files {
				if err := os.WriteFile(filepath.Join(folder, fmt.Sprintf("video-%04d.mp4", i)), make([]byte, (test.mib<<20)/test.files), 0600); err != nil {
					b.Fatal(err)
				}
			}
			source, err := site.New(b.Context(), folder)
			if err != nil {
				b.Fatal(err)
			}
			var elapsed, accounting time.Duration
			var calls int
			b.SetBytes(int64(test.mib << 20))
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				backend, err := NewSandboxed(b.Context(), b.TempDir(), bytes.Repeat([]byte{1}, 32))
				if err != nil {
					b.Fatal(err)
				}
				measured := &measuredBackend{Backend: backend}
				collection, err := New(b.Context(), measured)
				if err != nil {
					b.Fatal(err)
				}
				if test.unchanged {
					if _, err := collection.Snapshot(b.Context(), source); err != nil {
						b.Fatal(err)
					}
					measured.accounting, measured.calls = 0, 0
				}
				b.StartTimer()
				start := time.Now()
				_, err = collection.Snapshot(b.Context(), source)
				elapsed += time.Since(start)
				b.StopTimer()
				closeErr := collection.Close()
				if err != nil {
					b.Fatal(err)
				}
				if closeErr != nil {
					b.Fatal(closeErr)
				}
				accounting += measured.accounting
				calls += measured.calls
				b.StartTimer()
			}
			b.ReportMetric(100*float64(accounting)/float64(elapsed), "accounting-%")
			b.ReportMetric(float64(calls)/float64(b.N), "accounting-calls/op")
		})
	}
}
