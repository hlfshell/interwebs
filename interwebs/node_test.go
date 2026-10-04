package interwebs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/site"
)

func localNode(t *testing.T, text string) (*Node, []Option, string) {
	t.Helper()
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	opts := []Option{WithContent(s), WithDataDir(dir), WithKeyFile(filepath.Join(dir, "storage.key")), WithOffline(true), WithSigner(testSigner(t))}
	n, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := n.Close(); err != nil {
			t.Error(err)
		}
	})
	return n, opts, folder
}

func TestNodePublishViewRestart(t *testing.T) {
	n, opts, _ := localNode(t, "<h1>stored site</h1>")
	publication, err := n.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if publication.Record.Sequence != 1 {
		t.Fatal(publication)
	}
	url, err := n.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "<h1>stored site</h1>")
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.Status().Magnet != publication.Magnet {
		t.Fatal("identity changed")
	}
	url, err = restored.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "<h1>stored site</h1>")
	request, _ := http.NewRequest("GET", url, nil)
	request.Header.Set("Range", "bytes=4-9")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != 206 || string(data) != "stored" {
		t.Fatalf("range %d %q", response.StatusCode, data)
	}
}

func TestNodeRejectsUnsupportedStateWithoutRewriting(t *testing.T) {
	for _, field := range []string{"version", "sourceID", "previousVersion"} {
		t.Run(field, func(t *testing.T) {
			n, opts, _ := localNode(t, "current state")
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			var config options
			for _, opt := range opts {
				if err := opt(&config); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(config.dir, "state.json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]json.RawMessage
			if err := json.Unmarshal(original, &state); err != nil {
				t.Fatal(err)
			}
			if field == "previousVersion" {
				state["version"] = json.RawMessage("3")
			} else {
				delete(state, field)
			}
			unsupported, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, unsupported, 0600); err != nil {
				t.Fatal(err)
			}
			opened, err := New(t.Context(), opts...)
			if err == nil {
				opened.Close()
				t.Fatal("incomplete state accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(unsupported) {
				t.Fatalf("rejected state was rewritten: %v", err)
			}

			// Failure must release the directory lock for a valid reopen.
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			reopened, err := New(t.Context(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func checkPage(t *testing.T, url, want string) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || string(data) != want {
		t.Fatalf("response %d %q", response.StatusCode, data)
	}
	if !strings.Contains(response.Header.Get("Content-Security-Policy"), "sandbox allow-scripts;") {
		t.Fatal("missing sandbox")
	}
}

func TestNodesAreIndependent(t *testing.T) {
	a, opts, _ := localNode(t, "first")
	b, _, _ := localNode(t, "second")
	if _, err := a.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.transport.Port() == b.transport.Port() {
		t.Fatal("shared port")
	}
	if _, err := New(t.Context(), opts...); err == nil {
		t.Fatal("accepted locked directory")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	url, err := b.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "second")
	if _, err := a.View(t.Context()); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed node: %v", err)
	}
}

func TestNodeStatusIsIndependentAndCancellationIsLocal(t *testing.T) {
	n, _, _ := localNode(t, "hello")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := n.Publish(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := n.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := n.Status()
	status.Record.Signature[0] ^= 1
	status.History[0].Signature[0] ^= 1
	current := n.Status()
	if err := current.Identity.Verify(current.Record, current.History[0]); err != nil {
		t.Fatal(err)
	}
}

// A non-Site implementation proves publication has no implicit HTML requirement.
type dataContent struct{}

func (dataContent) Descriptor() content.Descriptor    { return content.Descriptor{Name: "data"} }
func (dataContent) Validate(m content.Manifest) error { _, err := m.File("payload.bin"); return err }
func (dataContent) Files(context.Context) ([]content.File, error) {
	return []content.File{{Path: "payload.bin", Size: 4}}, nil
}
func (dataContent) Fingerprint(context.Context) ([32]byte, error) { return [32]byte{1}, nil }
func (dataContent) Open(context.Context, string) (content.Reader, error) {
	return stringReader{strings.NewReader("data")}, nil
}

type stringReader struct{ *strings.Reader }

func (stringReader) Close() error { return nil }

func TestNodeAcceptsNonSiteContent(t *testing.T) {
	dir := t.TempDir()
	n, err := New(t.Context(), WithContent(dataContent{}), WithSigner(testSigner(t)), WithDataDir(dir), WithKeyFile(filepath.Join(dir, "storage.key")), WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err := n.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := n.View(t.Context()); !errors.Is(err, ErrCapability) {
		t.Fatalf("non-site browser capability: %v", err)
	}
}

func TestNodeFailureReleasesDirectoryAndPort(t *testing.T) {
	n, opts, _ := localNode(t, "test")
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if partial, err := New(t.Context(), append(opts, WithPort(port))...); err == nil {
		partial.Close()
		t.Fatal("accepted occupied port")
	}
	listener.Close()
	reopened, err := New(t.Context(), opts...)
	if err != nil {
		t.Fatal("failed construction kept profile locked", err)
	}
	defer reopened.Close()
}

func TestIdenticalContentUsesIndependentStores(t *testing.T) {
	a, aOptions, _ := localNode(t, "identical")
	b, bOptions, _ := localNode(t, "identical")
	first, err := a.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.Record.Hash != second.Record.Hash {
		t.Fatal("different content hashes")
	}
	if first.Magnet == second.Magnet {
		t.Fatal("shared signing identity")
	}
	var aConfig, bConfig options
	for _, o := range aOptions {
		o(&aConfig)
	}
	for _, o := range bOptions {
		o(&bConfig)
	}
	for _, dir := range []string{aConfig.dir, bConfig.dir} {
		if _, err := os.Stat(filepath.Join(dir, "data", first.Record.Hash, "manifest")); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	url, err := b.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "identical")
}

func testSigner(t *testing.T) *identity.Signer {
	t.Helper()
	p, err := identity.Create(t.Context(), filepath.Join(t.TempDir(), "signing.key"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
