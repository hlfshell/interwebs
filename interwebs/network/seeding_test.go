package network

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/site"
)

func TestUploadsRequireSeedWhileDownloadsRemainEnabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("seed explicitly"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := site.New(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	stores := networkStores(t, t.TempDir())
	manifest, err := stores.Snapshot(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	sender := localTransport(t, stores)
	transfer, err := sender.Open(ctx, manifest.Hash(), source.Validate)
	if err != nil {
		t.Fatal(err)
	}
	if err := transfer.Download(ctx); err != nil {
		t.Fatal(err)
	}
	receiverStores := networkStores(t, t.TempDir())
	// Supply metadata separately so this test isolates payload upload control.
	// A stopped peer is not required to serve metadata to a new connection.
	if err := receiverStores.Prepare(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	receiver := localTransport(t, receiverStores)
	go connectTransfer(ctx, receiver, manifest.Hash(), sender.Port())
	incoming, err := receiver.Open(ctx, manifest.Hash(), source.Validate)
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	r, err := incoming.Open(short, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	r.Close()
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uploaded without Seed: %v", err)
	}
	if incoming.Stats().Bytes != 0 {
		t.Fatal("unseeded node uploaded payload")
	}
	if err := sender.SetSeeding(ctx, true); err != nil {
		t.Fatal(err)
	}
	// The receiver itself still has uploads disabled.
	if err := incoming.DownloadFile(ctx, "index.html", ReadOptions{Mode: Sequential}); err != nil {
		t.Fatal(err)
	}
	r, err = incoming.Open(ctx, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(b) != "seed explicitly" {
		t.Fatal(string(b), err)
	}
	if err := sender.SetSeeding(ctx, false); err != nil {
		t.Fatal(err)
	}
	r, err = transfer.Open(ctx, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(r)
	r.Close()
	if err != nil || string(b) != "seed explicitly" {
		t.Fatal("Stop closed local reads", err)
	}
}
