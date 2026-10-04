package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIStatusDashboardJSONAndWatchCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "interweb.yaml")
	if err := os.WriteFile(path, []byte("data_dir: ./data\nsites: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "runner-status.json"), []byte(`{"event":"status","updated":"2026-01-01T00:00:00Z","sites":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"human", "json", "watch"} {
		args := []string{"status", "--config", path}
		if mode != "human" {
			args = append(args, "--"+mode)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // Watch must stop promptly after its first read-only snapshot.
		var output bytes.Buffer
		if err := execute(ctx, args, &output, &output); err != nil {
			t.Fatal(mode, err)
		}
		if mode == "json" {
			if !json.Valid(output.Bytes()) {
				t.Fatal(output.String())
			}
		} else if !strings.Contains(output.String(), "interweb |") || strings.Contains(output.String(), "\x1b") {
			t.Fatal(output.String())
		}
	}
}

func TestCLIInitAddListCheckAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "interweb.yaml")
	var out bytes.Buffer
	for _, args := range [][]string{
		{"help"}, {"init", "--config", path}, {"add", "--config", path, "--name", "mirror", "--magnet", "magnet:?xt=urn:btih:1111111111111111111111111111111111111111", "--listen", "127.0.0.1:8080"},
		{"check", "--config", path}, {"list", "--config", path},
	} {
		if err := execute(t.Context(), args, &out, &out); err != nil {
			t.Fatal(args, err)
		}
	}
	if !strings.Contains(out.String(), "mirror") || !strings.Contains(out.String(), "Valid config: 1 sites") {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{{"unknown"}, {"run", "unexpected"}, {"init", "--config", path}, {"add", "--config", path, "--name", "bad", "--public-url", "http://example.com"}, {"check", "--config", path + "missing"}} {
		if err := execute(t.Context(), args, &out, &out); err == nil {
			t.Fatal("accepted", args)
		}
	}
}
