package interwebs

import (
	"errors"
	"strings"
	"testing"

	"github.com/hlfshell/interweb/interwebs/storage"
)

type trackedBackend struct {
	storage.Backend
	closes   int
	closeErr error
}

func (b *trackedBackend) Close() error {
	b.closes++
	return b.closeErr
}

func TestOptionsRejectNilAndReleaseAcceptedBackend(t *testing.T) {
	failure := errors.New("cleanup failed")
	backend := &trackedBackend{closeErr: failure}
	node, err := New(t.Context(), WithStorageBackend(backend), nil)
	if node != nil || !errors.Is(err, failure) || !strings.Contains(err.Error(), "nil node option") {
		t.Fatalf("unexpected result: %v, %v", node, err)
	}
	if backend.closes != 1 {
		t.Fatalf("backend closed %d times", backend.closes)
	}
}

func TestDuplicateBackendRejectsSecondWithoutTakingOwnership(t *testing.T) {
	for _, same := range []bool{false, true} {
		first := &trackedBackend{}
		second := &trackedBackend{}
		if same {
			second = first
		}
		node, err := New(t.Context(), WithStorageBackend(first), WithStorageBackend(second))
		if node != nil || err == nil || !strings.Contains(err.Error(), "already configured") {
			t.Fatalf("duplicate backend accepted: %v", err)
		}
		if first.closes != 1 {
			t.Fatalf("accepted backend closed %d times", first.closes)
		}
		if !same && second.closes != 0 {
			t.Fatal("closed a backend whose option was rejected")
		}
	}
}

func TestValueOptionsKeepLastValue(t *testing.T) {
	var config options
	for _, opt := range []Option{WithStorageLimit(1), WithStorageLimit(4096), WithPort(80), WithPort(0)} {
		if err := applyOption(&config, opt); err != nil {
			t.Fatal(err)
		}
	}
	if config.storageLimit != 4096 || config.port != 0 {
		t.Fatalf("options did not retain last value: %+v", config)
	}
}

func TestStorageNilOptionReleasesBackend(t *testing.T) {
	backend := &trackedBackend{}
	if _, err := storage.New(t.Context(), backend, nil); err == nil {
		t.Fatal("nil storage option accepted")
	}
	if backend.closes != 1 {
		t.Fatal("failed storage initialization leaked backend")
	}
}
