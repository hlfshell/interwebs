package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryRedirectKeepsPublicOriginAndRelativeAssets(t *testing.T) {
	c, folder := fixture(t)
	if err := os.Mkdir(filepath.Join(folder, "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "projects page", "asset.css": "body {}"} {
		if err := os.WriteFile(filepath.Join(folder, "projects", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := startReady(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	address := r.status("ready").Sites[0].URL
	response, body := fetch(t, address+"projects?year=2026", "GET", nil)
	if response.StatusCode != 200 || body != "projects page" || response.Request.URL.String() != address+"projects/?year=2026" {
		t.Fatalf("redirect: %d %q %s", response.StatusCode, body, response.Request.URL)
	}
	asset, err := response.Request.URL.Parse("asset.css")
	if err != nil {
		t.Fatal(err)
	}
	response, body = fetch(t, asset.String(), "GET", nil)
	if response.StatusCode != 200 || body != "body {}" || !strings.HasPrefix(asset.String(), address) {
		t.Fatalf("relative asset: %d %q %s", response.StatusCode, body, asset)
	}
}
