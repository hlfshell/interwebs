package profile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStorageKeyRequiresExactly32Bytes(t *testing.T) {
	for _, size := range []int64{0, 31, 32, 33, 64 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "storage.key")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(size); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			key, err := StorageKey(t.Context(), dir, "")
			if size == 32 {
				if err != nil || len(key) != 32 {
					t.Fatalf("valid key: %d bytes, %v", len(key), err)
				}
			} else if err == nil {
				t.Fatal("invalid key length accepted")
			}
			stat, err := os.Stat(path)
			if err != nil || stat.Size() != size {
				t.Fatal("key file was modified", err)
			}
		})
	}
}

func TestDefaultStorageKeyRejectsMalformedFileWithoutReplacingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.key")
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := StorageKey(t.Context(), dir, ""); err == nil {
		t.Fatal("accepted malformed default key")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != "invalid" {
		t.Fatal("replaced malformed key", err)
	}
}

func TestProfileLockAndExplicitKeyRecovery(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if second, err := Open(t.Context(), dir); err == nil {
		second.Close()
		t.Fatal("shared profile lock")
	}
	keyPath := filepath.Join(dir, "storage.key")
	key, err := StorageKey(t.Context(), dir, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	again, err := StorageKey(t.Context(), dir, keyPath)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatal("key changed", err)
	}
	if implicit, err := StorageKey(t.Context(), dir, ""); err != nil || !bytes.Equal(key, implicit) {
		t.Fatal("default path did not reuse the key", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data", "existing"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := StorageKey(t.Context(), dir, filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("replaced missing key")
	}
	if err := p.WriteState(t.Context(), []byte("state")); err != nil {
		t.Fatal(err)
	}
	read, err := p.ReadState(t.Context())
	if err != nil || string(read) != "state" {
		t.Fatal(string(read), err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}
