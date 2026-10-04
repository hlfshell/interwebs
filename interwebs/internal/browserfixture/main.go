// Command browserfixture serves test sites from the public Node API.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	interwebs "github.com/hlfshell/interweb/interwebs"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/site"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() (resultErr error) {
	if len(os.Args) != 3 {
		return errors.New("usage: browserfixture SOURCE DATA")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key, err := identity.Create(ctx, filepath.Join(os.Args[2], "publisher.key"))
	if err != nil {
		return err
	}
	source, err := site.New(ctx, os.Args[1])
	if err != nil {
		return err
	}
	n, err := interwebs.New(ctx, interwebs.WithContent(source), interwebs.WithSigner(key),
		interwebs.WithDataDir(os.Args[2]), interwebs.WithKeyFile(filepath.Join(os.Args[2], "storage.key")), interwebs.WithOffline(true))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, n.Close()) }()
	publication, err := n.Publish(ctx)
	if err != nil {
		return err
	}
	url, err := n.View(ctx)
	if err != nil {
		return err
	}
	output := json.NewEncoder(os.Stdout)
	if err := output.Encode(map[string]string{"url": url, "hash": publication.Record.Hash}); err != nil {
		return err
	}
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		switch input.Text() {
		case "stop":
			if err := n.Stop(ctx); err != nil {
				return err
			}
		case "seed":
			if err := n.Seed(ctx); err != nil {
				return err
			}
		case "status":
		default:
			return errors.New("unknown fixture command")
		}
		if err := output.Encode(n.Status()); err != nil {
			return err
		}
	}
	return input.Err()
}
