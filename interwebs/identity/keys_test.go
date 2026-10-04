package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitSigningKeysNeverOverwriteOrRegenerate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	original, err := Create(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(t.Context(), path); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	loaded, err := Load(t.Context(), path)
	if err != nil || loaded.Identity() != original.Identity() {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal(stat, err)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(t.Context(), path); err == nil {
		t.Fatal("accepted malformed key")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Create(ctx, filepath.Join(t.TempDir(), "new")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
