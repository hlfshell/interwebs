package interwebs_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	interwebs "github.com/hlfshell/interweb/interwebs"
	"github.com/hlfshell/interweb/interwebs/author"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/site"
)

func ExampleNode() {
	if err := exampleFlow(); err != nil {
		panic(err)
	}
	// Output:
	// standalone publication: 1
	// folder update: 2
	// author directory: 1
}

func exampleFlow() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, err := os.MkdirTemp("", "interweb-example-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	folder := filepath.Join(root, "source")
	if err := os.Mkdir(folder, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("Hello"), 0600); err != nil {
		return err
	}
	signing, err := identity.Create(ctx, filepath.Join(root, "site.key"))
	if err != nil {
		return err
	}
	source, err := site.New(ctx, folder)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "content", "sites", signing.Identity().ID())
	storageKey := filepath.Join(root, "storage.key")
	local, err := interwebs.New(ctx, interwebs.WithContent(source), interwebs.WithSigner(signing),
		interwebs.WithDataDir(dir), interwebs.WithKeyFile(storageKey), interwebs.WithOffline(true))
	if err != nil {
		return err
	}
	defer local.Close()
	published, err := local.Publish(ctx)
	if err != nil {
		return err
	}
	fmt.Println("standalone publication:", published.Record.Sequence)

	// Update the source folder, then publish another immutable version.
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("Updated"), 0600); err != nil {
		return err
	}
	updated, err := local.Publish(ctx)
	if err != nil {
		return err
	}
	fmt.Println("folder update:", updated.Record.Sequence)
	// Viewing only requests the entry page; later browser requests drive downloads.
	url, err := local.View(ctx)
	if err != nil {
		return err
	}
	_ = url // The application hands this URL to the user's browser.
	if err := local.Stop(ctx); err != nil {
		return err
	}
	// Stop does not close that URL. The application may resume uploads explicitly.
	if err := local.Seed(ctx); err != nil {
		return err
	}

	authorKey, err := identity.Create(ctx, filepath.Join(root, "author.key"))
	if err != nil {
		return err
	}
	profile, err := author.New(author.Directory{Name: "Example author", Sites: []author.Entry{
		{Name: "Blog", Address: signing.Identity()},
	}})
	if err != nil {
		return err
	}
	authorDir := filepath.Join(root, "content", "identities", authorKey.Identity().ID(), "profile")
	authorNode, err := interwebs.New(ctx, interwebs.WithContent(profile), interwebs.WithSigner(authorKey),
		interwebs.WithDataDir(authorDir), interwebs.WithKeyFile(storageKey), interwebs.WithOffline(true))
	if err != nil {
		return err
	}
	defer authorNode.Close()
	directory, err := authorNode.Publish(ctx)
	if err != nil {
		return err
	}
	fmt.Println("author directory:", directory.Record.Sequence)
	return nil
}
