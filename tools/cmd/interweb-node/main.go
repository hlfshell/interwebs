// interweb-node is a headless test driver using the same service as Wails.
// Its control interface is stdin/stdout only; it never opens a management port.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	site "github.com/hlfshell/interweb/interwebs"
)

type request struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	ID      string `json:"id"`
	Magnet  string `json:"magnet"`
	Enabled bool   `json:"enabled"`
}

func main() {
	dir := flag.String("data", "", "application data directory")
	keyFile := flag.String("key-file", "", "raw 32-byte storage key file (created if missing); otherwise use OS keychain")
	port := flag.Int("port", 0, "torrent TCP/UDP port")
	offline := flag.Bool("offline", false, "disable public DHT for deterministic tests")
	updates := flag.Duration("update-interval", 5*time.Minute, "signed update poll interval")
	network := flag.Duration("network-interval", 10*time.Second, "network maintenance interval")
	announcements := flag.Duration("announce-interval", 30*time.Minute, "signed record republication interval")
	flag.Parse()
	s, e := site.New(site.Options{DataDir: *dir, KeyFile: *keyFile, Port: *port, Offline: *offline, UpdateInterval: *updates, NetworkInterval: *network, AnnounceInterval: *announcements})
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() { <-signals; s.Close(); os.Exit(0) }()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var r request
		var result any
		e = json.Unmarshal(scanner.Bytes(), &r)
		if e == nil {
			switch r.Op {
			case "status":
				result = s.Status()
			case "add":
				result, e = s.AddFolder(r.Path)
			case "publish":
				e = s.Publish(r.ID)
			case "open":
				result, e = s.Open(r.Magnet)
			case "favorite":
				e = s.SetFavorite(r.ID, r.Enabled)
			case "hosting":
				e = s.SetHosting(r.ID, r.Enabled)
			case "live":
				e = s.SetLive(r.ID, r.Enabled)
			case "stop":
				e = s.Stop(r.ID)
			default:
				e = fmt.Errorf("unknown operation")
			}
		}
		response := map[string]any{"result": result}
		if e != nil {
			response["error"] = e.Error()
		}
		encoder.Encode(response)
	}
	s.Close()
}
