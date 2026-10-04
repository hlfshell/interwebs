package interwebs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/network"
	"github.com/hlfshell/interweb/interwebs/site"
)

type waitingSource struct {
	content.Source
	entered chan struct{}
	once    sync.Once
}

func (*waitingSource) EntryPoint() string { return "index.html" }
func (s *waitingSource) Fingerprint(ctx context.Context) ([32]byte, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return [32]byte{}, ctx.Err()
}

func TestRestartSeedsStoredVersionWhileReplacementIsPreparing(t *testing.T) {
	first, opts, _ := localNode(t, "stored version")
	publication, err := first.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	source := &waitingSource{Source: first.source, entered: make(chan struct{})}
	node, err := New(t.Context(), append(opts, WithContent(source))...)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	url, err := node.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := node.Publish(ctx); done <- err }()
	defer func() { cancel(); <-done }()
	select {
	case <-source.entered:
	case <-ctx.Done():
		t.Fatal("publication did not reach source preparation")
	}
	status := node.Status()
	if !status.Seeding || status.Current != publication.Record.Hash {
		t.Fatalf("stored version not seeded during preparation: %+v", status)
	}
	checkPage(t, url, "stored version")
}

type failingSource struct {
	content.Source
	failure error
}

func (s failingSource) Fingerprint(context.Context) ([32]byte, error) {
	return [32]byte{}, s.failure
}

func TestPublicationErrorIdentifiesStageAndPreservesCause(t *testing.T) {
	original, opts, _ := localNode(t, "source")
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("source unavailable")
	source := failingSource{Source: original.source, failure: failure}
	node, err := New(t.Context(), append(opts, WithContent(source))...)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	if _, err := node.Publish(t.Context()); !errors.Is(err, failure) || !strings.Contains(err.Error(), "snapshot publication") {
		t.Fatalf("lost failure context or cause: %v", err)
	}
}

func TestPublishIncludesUnfilteredFilesInEncryptedVersion(t *testing.T) {
	initial, opts, folder := localNode(t, "homepage")
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "payload.custom")
	if err := os.WriteFile(path, []byte("stored payload"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	node, err := New(t.Context(), append(opts, WithContent(source))...)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	if _, err := node.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	manifest, err := node.Manifest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.File("payload.custom"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reader, err := node.Read(t.Context(), "payload.custom", network.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	err = errors.Join(err, reader.Close())
	if err != nil || string(data) != "stored payload" {
		t.Fatalf("stored read: %q, %v", data, err)
	}
}

func TestFolderRepublishPreservesIdentityAndExistingURLs(t *testing.T) {
	node, opts, folder := localNode(t, "original")
	if !node.Capabilities().Publish {
		t.Fatal("local source with signing key cannot publish")
	}
	first, err := node.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	oldURL, err := node.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("updated"), 0600); err != nil {
		t.Fatal(err)
	}
	checkPage(t, oldURL, "original")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := node.Publish(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if node.Status().Record.Hash != first.Record.Hash {
		t.Fatal("canceled publication changed the current version")
	}

	second, err := node.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.Magnet != first.Magnet || second.Record.Sequence != first.Record.Sequence+1 || second.Record.Hash == first.Record.Hash {
		t.Fatal("republishing did not preserve identity and advance the version")
	}
	newURL, err := node.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, newURL, "updated")
	checkPage(t, oldURL, "original")

	unchanged, err := node.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Record.Hash != second.Record.Hash || unchanged.Record.Sequence != second.Record.Sequence {
		t.Fatal("unchanged source created another signed version")
	}

	if err := os.Remove(filepath.Join(folder, "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Publish(t.Context()); err == nil {
		t.Fatal("published a site without its entry page")
	}
	checkPage(t, newURL, "updated")

	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("after restart"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	third, err := reopened.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if third.Magnet != first.Magnet || third.Record.Sequence != second.Record.Sequence+1 {
		t.Fatal("restart lost publishing identity or sequence")
	}
	url, err := reopened.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "after restart")
}

func TestRemoteReferenceCannotPublishWithoutLocalSource(t *testing.T) {
	owner, _, _ := localNode(t, "source")
	published, err := owner.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := site.FromMagnet(published.Magnet)
	if err != nil {
		t.Fatal(err)
	}

	for _, withKey := range []bool{false, true} {
		dir := t.TempDir()
		opts := []Option{WithContent(remote), WithDataDir(dir), WithKeyFile(filepath.Join(dir, "storage.key")), WithOffline(true)}
		if withKey {
			opts = append(opts, WithSigner(owner.signer))
		}
		node, err := New(t.Context(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		if node.Capabilities().Publish {
			t.Fatal("remote reference advertised publication without a source")
		}
		if _, err := node.Publish(t.Context()); !errors.Is(err, ErrCapability) {
			t.Fatalf("remote publication: %v", err)
		}
		if err := node.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
