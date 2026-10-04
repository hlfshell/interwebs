package interwebs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/identity"
)

type durableAnnouncements struct {
	statePath string
	sent      chan identity.Record
	invalid   chan error
}

func (d *durableAnnouncements) Resolve(context.Context, identity.Identity, identity.Record) (identity.Record, error) {
	return identity.Record{}, errors.New("not a reader")
}
func (d *durableAnnouncements) Announce(_ context.Context, _ identity.Identity, record identity.Record) error {
	encoded, err := os.ReadFile(d.statePath)
	var state nodeState
	if err == nil {
		err = json.Unmarshal(encoded, &state)
	}
	if err == nil && (state.Record.Sequence != record.Sequence || state.Record.Hash != record.Hash) {
		err = errors.New("announced a non-durable version")
	}
	if err != nil {
		d.invalid <- err
		return err
	}
	d.sent <- record.Clone()
	return nil
}

func TestNewPublicationAnnouncesWithoutWaitingForRepublishInterval(t *testing.T) {
	initial, opts, folder := localNode(t, "one")
	initial.Close()
	var config options
	for _, opt := range opts {
		opt(&config)
	}
	discovery := &durableAnnouncements{statePath: filepath.Join(config.dir, "state.json"), sent: make(chan identity.Record, 8), invalid: make(chan error, 8)}
	n, err := New(t.Context(), append(opts, func(o *options) error { o.discovery = discovery; return nil })...)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	for sequence, text := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := n.Publish(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-discovery.invalid:
			t.Fatal(err)
		case record := <-discovery.sent:
			if record.Sequence != int64(sequence+1) {
				t.Fatal(record)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("new version waited for periodic re-announcement")
		}
		deadline := time.Now().Add(time.Second)
		for n.Status().AnnouncedSequence < int64(sequence+1) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if n.Status().AnnouncedSequence != int64(sequence+1) {
			t.Fatal("successful announcement missing from status")
		}
	}
}

func TestPublicationRetryPersistsSameVersionAfterSaveFailure(t *testing.T) {
	n, opts, _ := localNode(t, "retry")
	var config options
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(config.dir, "state.json")
	backup := filepath.Join(config.dir, "saved-state.json")
	if err := os.Rename(state, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Publish(t.Context()); err == nil {
		t.Fatal("reported success after state replacement failed")
	}
	failed := n.Status().Record
	if n.Status().PersistenceError == "" {
		t.Fatal("unsaved state was hidden from status")
	}
	if failed.Sequence != 1 {
		t.Fatalf("expected prepared version: %+v", failed)
	}
	if _, err := n.Publish(t.Context()); err == nil {
		t.Fatal("same-version retry bypassed persistence")
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, state); err != nil {
		t.Fatal(err)
	}
	retried, err := n.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if retried.Record.Sequence != failed.Sequence || retried.Record.Hash != failed.Hash {
		t.Fatal("retry created another version")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.Status().Record.Hash != failed.Hash {
		t.Fatal("retry was not durable")
	}
	url, err := restored.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "retry")
}
