package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

type installingReadsBackend struct {
	*countingReadsBackend
	installer Installer
}

func (b *installingReadsBackend) Install(ctx context.Context, from, to string) error {
	return b.installer.Install(ctx, from, to)
}

func TestSnapshotInstallsWithoutPayloadCopyAndReopens(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{1}, 32)
	b, err := NewSandboxed(t.Context(), dir, key)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingReadsBackend{Backend: b}
	s, err := New(t.Context(), &installingReadsBackend{countingReadsBackend: counted, installer: b})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := testSnapshot(t, s)
	if counted.reads != 0 {
		t.Fatalf("snapshot reread %d payload files", counted.reads)
	}
	usage, err := s.Usage(t.Context())
	if err != nil || len(usage) != 1 || usage[0].Hash != m.Hash() {
		t.Fatal("installation left extra stores", usage, err)
	}
	if len(s.quota.sizes) != 1 || s.quota.sizes[m.Hash()] < usage[0].Bytes {
		t.Fatal("quota did not follow installed store", s.quota.sizes)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = NewSandboxed(t.Context(), dir, key)
	if err != nil {
		t.Fatal(err)
	}
	s, err = New(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.Open(t.Context(), m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	r, err := v.Open(t.Context(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "encrypted content" {
		t.Fatal(string(got), err)
	}
}

func TestInstallDoesNotReplaceExistingStore(t *testing.T) {
	b, err := NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	from, to := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, hash := range []string{from, to} {
		s, err := b.Open(t.Context(), hash, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteFile("payload", strings.NewReader(hash)); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Install(t.Context(), from, to); !errors.Is(err, fs.ErrExist) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := b.Install(ctx, from, strings.Repeat("c", 40)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := b.Install(t.Context(), "../outside", strings.Repeat("c", 40)); err == nil {
		t.Fatal("accepted invalid installation path")
	}
	for _, hash := range []string{from, to} {
		s, err := b.Open(t.Context(), hash, false)
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Open("payload")
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(r)
		closeErr := errors.Join(r.Close(), s.Close())
		if readErr != nil || closeErr != nil || string(got) != hash {
			t.Fatal("store changed", string(got), readErr, closeErr)
		}
	}
}

type failedInstaller struct {
	Backend
	installer Installer
	err       error
}

func (b *failedInstaller) Install(ctx context.Context, from, to string) error {
	if b.installer != nil {
		if err := b.installer.Install(ctx, from, to); err != nil {
			return err
		}
	}
	return b.err
}

func TestInstallFailureBlocksUncertainQuotaButCancellationAllowsRetry(t *testing.T) {
	for _, name := range []string{"after-rename", "canceled"} {
		t.Run(name, func(t *testing.T) {
			canceled := name == "canceled"
			b, err := NewSandboxed(t.Context(), t.TempDir(), bytes.Repeat([]byte{1}, 32))
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("sync failed after installation")
			wrapped := &failedInstaller{Backend: b, installer: b, err: failure}
			if canceled {
				wrapped.installer = nil
				wrapped.err = context.Canceled
			}
			s, err := New(t.Context(), wrapped)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			stage, err := s.newStaging(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stage.close()
			installed, err := s.install(t.Context(), stage, strings.Repeat("b", 40), []byte("metadata"))
			if installed || !errors.Is(err, wrapped.err) {
				t.Fatal(installed, err)
			}
			if canceled && s.quota.accountError != nil {
				t.Fatal("cancellation poisoned quota", s.quota.accountError)
			}
			if canceled {
				wrapped.installer, wrapped.err = b, nil
				installed, err = s.install(t.Context(), stage, strings.Repeat("b", 40), []byte("metadata"))
				if err != nil || !installed {
					t.Fatal("retry after cancellation", installed, err)
				}
			}
			if !canceled && !errors.Is(s.quota.accountError, failure) {
				t.Fatal("uncertain quota not blocked", s.quota.accountError)
			}
			if !canceled {
				called := false
				err := s.mutate(strings.Repeat("b", 40), 1, func() error { called = true; return nil })
				if called || !errors.Is(err, failure) {
					t.Fatal("write with uncertain accounting", called, err)
				}
			}
		})
	}
}

func TestSnapshotRepairsAnIncompleteExistingVersion(t *testing.T) {
	s := testCollection(t)
	m := testSnapshot(t, s)
	var info metainfo.Info
	if err := bencode.Unmarshal(m.Metadata(), &info); err != nil {
		t.Fatal(err)
	}
	handle, err := s.OpenTorrent(t.Context(), &info, metainfo.NewHashFromHex(m.Hash()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Piece(info.Piece(0)).WriteAt([]byte("X"), 0); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	repaired := testSnapshot(t, s)
	if repaired.Hash() != m.Hash() {
		t.Fatal("repair changed the content address")
	}
	v, err := s.Open(t.Context(), m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	r, err := v.Open(t.Context(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "encrypted content" {
		t.Fatal("repair failed", string(got), err)
	}
	usage, err := s.Usage(t.Context())
	if err != nil || len(usage) != 1 {
		t.Fatal("repair retained staging", usage, err)
	}
}
