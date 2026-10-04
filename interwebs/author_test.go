package interwebs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hlfshell/interweb/interwebs/author"
	"github.com/hlfshell/interweb/interwebs/network"
	"github.com/hlfshell/interweb/interwebs/site"
)

func TestAuthorWebsiteAndDirectoryShareOneVersion(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("Author website"), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := site.New(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := author.New(author.Directory{Name: "Author"}, author.WithPage(page))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	n, err := New(t.Context(), WithContent(profile), WithSigner(testSigner(t)),
		WithDataDir(dir), WithKeyFile(filepath.Join(dir, "key")), WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err := n.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	url, err := n.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "Author website")
	m, err := n.Manifest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.File(author.DirectoryPath); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorDirectoryEndorsesButCannotPublishSite(t *testing.T) {
	siteNode, _, _ := localNode(t, "independent")
	publication, err := siteNode.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	directory := author.Directory{Name: "An author", Sites: []author.Entry{{Name: "Blog", Address: siteNode.Status().Identity}}}
	profile, err := author.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	authorKey := testSigner(t)
	dir := t.TempDir()
	n, err := New(t.Context(), WithContent(profile), WithSigner(authorKey), WithDataDir(dir), WithKeyFile(filepath.Join(dir, "storage.key")), WithOffline(true))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	published, err := n.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if published.Magnet == publication.Magnet {
		t.Fatal("shared author and site identity")
	}
	r, err := n.Read(t.Context(), author.DirectoryPath, network.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := author.Decode(r)
	r.Close()
	if err != nil || decoded.Sites[0].Address != siteNode.Status().Identity {
		t.Fatal(decoded, err)
	}
	forged, err := authorKey.Sign(publication.Record.Sequence+1, publication.Record.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.Sites[0].Address.Verify(forged, publication.Record); err == nil {
		t.Fatal("author endorsement allowed signing site updates")
	}
	remoteSite, err := site.FromMagnet(publication.Magnet)
	if err != nil {
		t.Fatal(err)
	}
	wrongDir := t.TempDir()
	wrong, err := New(t.Context(), WithContent(remoteSite), WithSigner(authorKey), WithDataDir(wrongDir), WithKeyFile(filepath.Join(wrongDir, "key")), WithOffline(true))
	if err == nil {
		wrong.Close()
		t.Fatal("author key granted site authority")
	}
	if _, err := n.View(t.Context()); !errors.Is(err, ErrCapability) {
		t.Fatal(err)
	}

	remote, err := author.FromMagnet(published.Magnet)
	if err != nil {
		t.Fatal(err)
	}
	readerDir := t.TempDir()
	receiver, err := New(t.Context(), WithContent(remote), WithDataDir(readerDir), WithKeyFile(filepath.Join(readerDir, "key")), WithOffline(true), func(o *options) error {
		o.discovery = &discoveryFixture{record: published.Record}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if _, err := receiver.stores.Snapshot(t.Context(), profile); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, err = receiver.Read(t.Context(), author.DirectoryPath, network.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = author.Decode(r)
	r.Close()
	if err != nil || len(decoded.Sites) != 1 {
		t.Fatal(decoded, err)
	}
	if receiver.Capabilities().Publish {
		t.Fatal("received profile acquired key")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	url, err := siteNode.View(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, url, "independent")
}

func TestAuthorDirectoryRepublishFromReconstructedProfile(t *testing.T) {
	profile, err := author.New(author.Directory{Name: "Author"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	opts := []Option{WithSigner(testSigner(t)), WithDataDir(dir), WithKeyFile(filepath.Join(dir, "key")), WithOffline(true)}
	node, err := New(t.Context(), append(opts, WithContent(profile))...)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	first, err := node.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}

	directory := author.Directory{Name: "Author", Sites: []author.Entry{{Name: "Blog", Address: testSigner(t).Identity()}}}
	updated, err := author.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(t.Context(), append(opts, WithContent(updated))...)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second, err := reopened.Publish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.Magnet != second.Magnet || second.Record.Sequence != first.Record.Sequence+1 {
		t.Fatal("directory update lost identity or sequence")
	}
	reader, err := reopened.Read(t.Context(), author.DirectoryPath, network.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := author.Decode(reader)
	err = errors.Join(err, reader.Close())
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sites) != 1 || decoded.Sites[0] != directory.Sites[0] {
		t.Fatal("updated endorsements were not published")
	}
}
