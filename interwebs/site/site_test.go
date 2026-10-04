package site

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestFileTypeFilteringIsOptionalWithoutRelaxingPaths(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"index.html", "data.custom", "LICENSE", ".env"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "data.custom"), filepath.Join(dir, "linked.custom")); err != nil {
		t.Fatal(err)
	}

	for _, enabled := range []bool{true, false} {
		s, err := New(t.Context(), dir, WithFileTypeFiltering(enabled))
		if err != nil {
			t.Fatal(err)
		}
		files, err := s.Files(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		want := 3
		if enabled {
			want = 1
		}
		if len(files) != want {
			t.Fatalf("filtering=%t: files=%v", enabled, files)
		}
		for _, name := range []string{"data.custom", "LICENSE"} {
			reader, err := s.Open(t.Context(), name)
			if enabled {
				if !errors.Is(err, fs.ErrPermission) {
					t.Fatalf("filtered read: %v", err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			err = errors.Join(err, reader.Close())
			if err != nil || string(data) != name {
				t.Fatalf("read %s: %q, %v", name, data, err)
			}
		}
		for _, name := range []string{".env", "../data.custom", "linked.custom"} {
			if reader, err := s.Open(t.Context(), name); err == nil {
				reader.Close()
				t.Fatalf("filtering=%t accepted unsafe path %s", enabled, name)
			}
		}
	}
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), dir, WithFileTypeFiltering(false)); err == nil {
		t.Fatal("unfiltered site accepted missing entry page")
	}
}

func TestSiteConfinesSourcesAndRejectsCancellation(t *testing.T) {
	dir := t.TempDir()
	for name, value := range map[string]string{"index.html": "hello", ".env": "secret", "program.exe": "binary"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "index.html"), filepath.Join(dir, "linked.html")); err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), dir, WithFileTypeFiltering(true))
	if err != nil {
		t.Fatal(err)
	}
	files, err := s.Files(t.Context())
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	for _, path := range []string{"../index.html", "linked.html", ".env", "program.exe"} {
		if r, err := s.Open(t.Context(), path); err == nil {
			r.Close()
			t.Fatal("accepted", path)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Open(ctx, "index.html"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.ValidateStorageDir(filepath.Join(dir, "node")); err == nil {
		t.Fatal("accepted recursive storage")
	}
}
