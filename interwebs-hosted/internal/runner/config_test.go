package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRejectsUnknownDuplicateAndUnsafeValues(t *testing.T) {
	for _, text := range []string{
		"data_dir: data\nunknown: true\n",
		"data_dir: data\ndata_dir: second\n",
		"data_dir: data\n---\ndata_dir: second\n",
		"data_dir: data\nmax_site_mib: 0\n",
		"data_dir: data\nstartup_timeout: 0s\n",
		"data_dir: data\nsites: [{name: bad, folder: ./blog}]\n",
		"data_dir: data\nsites: [{name: bad, magnet: invalid}]\n",
		"data_dir: data\nsites: [{name: bad, id: abc, live: true}]\n",
		"data_dir: data\nsites: [{name: bad, id: abc, serve: {listen: '0.0.0.0:8080'}}]\n",
		"data_dir: data\nsites: [{name: bad, id: abc, serve: {public_url: 'http://example.com/path'}}]\n",
		"data_dir: data\nsites: [{name: bad, id: abc, serve: {public_url: 'http://user:pass@example.com'}}]\n",
		"data_dir: data\nsites: [{name: bad, id: abc, serve: {public_url: 'http://example.com;evil'}}]\n",
		"data_dir: data\nsites: [{name: a, id: abc}, {name: a, id: def}]\n",
		"data_dir: data\nsites: [{name: a, id: abc, serve: {public_url: 'https://blog.test:443'}}, {name: b, id: def, serve: {public_url: 'https://blog.test'}}]\n",
		strings.Repeat("#", (1<<20)+1),
	} {
		t.Run(text[:min(len(text), 75)], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestInitAddPreservesCommentsAndResolvesPaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "interweb.yaml")
	if err := Init(path, "./data"); err != nil {
		t.Fatal(err)
	}
	if err := Init(path, "different"); err == nil {
		t.Fatal("overwrote config")
	}
	if err := os.WriteFile(path, []byte("# operator comment\ndata_dir: ./data # keep here\nsites: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root, "blog")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Add(path, SiteConfig{Name: "blog", Folder: folder, Serve: &ServeConfig{Listen: "127.0.0.1:8080"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "# operator comment") || !strings.Contains(string(b), "# keep here") {
		t.Fatal("lost comments")
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != filepath.Join(root, "data") || c.Sites[0].Folder != folder || c.SourceRoots[0] != folder {
		t.Fatalf("paths: %+v", c)
	}
	if c.MaxSiteMiB != 512 || c.NodeStorageMiB != 2048 {
		t.Fatal("defaults")
	}
	if err := Add(path, SiteConfig{Name: "blog", Folder: folder}); err == nil {
		t.Fatal("duplicate accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(b) {
		t.Fatal("invalid edit changed config")
	}
	files, err := filepath.Glob(filepath.Join(root, ".interweb-config-*"))
	if err != nil || len(files) != 0 {
		t.Fatal("temporary config leaked", files, err)
	}
}
