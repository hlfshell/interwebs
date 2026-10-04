package interwebs

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitStorageKeyDoesNotCreateDefaultKey(t *testing.T) {
	initial, _, _ := localNode(t, "source")
	defer initial.Close()
	dir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "explicit.key")
	node, err := New(t.Context(), WithContent(initial.content), WithSigner(initial.signer),
		WithDataDir(dir), WithKeyFile(keyPath), WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	key, err := os.ReadFile(keyPath)
	if err != nil || len(key) != 32 {
		t.Fatal("explicit key not created", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "storage.key")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("created an unnecessary default key", err)
	}
}

func TestNodeGeneratesAndReusesDefaultStorageKey(t *testing.T) {
	initial, _, _ := localNode(t, "encrypted page")
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	opts := []Option{WithContent(initial.content), WithSigner(initial.signer), WithDataDir(dir), WithOffline(true)}
	node, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	keyPath := filepath.Join(dir, "storage.key")
	key, err := os.ReadFile(keyPath)
	if err != nil || len(key) != 32 {
		t.Fatalf("generated key: length=%d, err=%v", len(key), err)
	}
	if _, err := node.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatal("default key changed on reopen", err)
	}
	url, err := reopened.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "encrypted page")
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	rejected, err := New(t.Context(), opts...)
	if err == nil {
		rejected.Close()
		t.Fatal("replaced a lost default key for encrypted stores")
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed reopen created a replacement key", err)
	}

	// Failed initialization must release the lock so recovery can succeed.
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	url, err = recovered.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "encrypted page")
}
