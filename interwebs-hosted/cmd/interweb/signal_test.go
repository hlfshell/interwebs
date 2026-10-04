package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hlfshell/interweb/interwebs/app"
)

func TestHeadlessProcess(t *testing.T) {
	if path := os.Getenv("INTERWEB_TEST_CONFIG"); path != "" {
		os.Args = []string{"interweb", "run", "--json", "--config", path}
		main()
		os.Exit(0)
	}
}

func TestSIGTERMClosesBackend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM process test requires Unix")
	}
	root := t.TempDir()
	path := filepath.Join(root, "interweb.yaml")
	if err := os.WriteFile(path, []byte("data_dir: ./data\noffline: true\nsites: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestHeadlessProcess$")
	cmd.Env = append(os.Environ(), "INTERWEB_TEST_CONFIG="+path)
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && strings.Contains(scanner.Text(), `"event":"ready"`)
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("no ready status")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("shutdown: %v\n%s", err, diagnostic.String())
	}
	a, err := app.Open(t.Context(), filepath.Join(root, "data"), app.WithOffline(true))
	if err != nil {
		t.Fatal("root stayed locked", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}
