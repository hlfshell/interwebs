package interwebs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hlfshell/interweb/interwebs/site"
	"github.com/hlfshell/interweb/interwebs/storage"
)

func TestExplicitVersionRemovalProtectsServingThenReclaims(t *testing.T) {
	n, _, folder := localNode(t, "first")
	first, err := n.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.View(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := n.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := n.RemoveVersion(t.Context(), first.Record.Hash); !errors.Is(err, storage.ErrInUse) {
		t.Fatal(err)
	}
	if err := n.ReleaseVersion(t.Context(), first.Record.Hash); err != nil {
		t.Fatal(err)
	}
	if err := n.RemoveVersion(t.Context(), first.Record.Hash); err != nil {
		t.Fatal(err)
	}
	versions, err := n.Versions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].Hash != second.Record.Hash {
		t.Fatal(versions)
	}
}

func TestStopOnlyDisablesSeedingAndRestartHasNoPolicy(t *testing.T) {
	n, opts, _ := localNode(t, "still viewable")
	if n.Status().Seeding {
		t.Fatal("new node started seeding")
	}
	if _, err := n.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	url, err := n.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := n.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n.Status().Seeding {
		t.Fatal("Stop did not stop uploads")
	}
	checkPage(t, url, "still viewable")
	if err := n.Download(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n.Status().Seeding {
		t.Fatal("Download resumed uploads")
	}
	if err := n.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !n.Status().Seeding {
		t.Fatal("Seed did not enable uploads")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Status().Seeding {
		t.Fatal("restart restored application policy")
	}
}

func TestSigningKeyRequiredForNewSite(t *testing.T) {
	_, _, folder := localNode(t, "source")
	s, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	n, err := New(t.Context(), WithContent(s), WithDataDir(dir), WithKeyFile(filepath.Join(dir, "storage.key")), WithOffline(true))
	if err == nil {
		n.Close()
		t.Fatal("implicitly generated signing authority")
	}
}
