package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hlfshell/interweb/interwebs-hosted/internal/runner"
	"github.com/mattn/go-isatty"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := execute(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "interweb:", err)
		os.Exit(1)
	}
}

const usage = `interweb — headless publishing, seeding, and website serving

  interweb init [--config interweb.yaml] [--data ./data]
  interweb add --folder ./blog [--name blog] [--listen 127.0.0.1:8080] [--live]
  interweb add --magnet 'magnet:?...' --name mirror [--listen 127.0.0.1:8081]
  interweb list [--config interweb.yaml]
  interweb check [--config interweb.yaml]
  interweb status [--config interweb.yaml]
  interweb run [--config interweb.yaml]

All commands accept --config. Use COMMAND --help for its flags.
Config changes take effect on restart. --listen enables HTTP content serving;
without it the site is only published/seeded. --public-url is required for a
non-loopback listener. There is no management HTTP API.
`

func execute(ctx context.Context, args []string, out, diagnostic io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprint(out, usage)
		return err
	}
	command := args[0]
	switch command {
	case "init", "add", "list", "check", "status", "run":
	default:
		return fmt.Errorf("unknown command %q; use interweb help", command)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(diagnostic)
	path := f.String("config", "interweb.yaml", "YAML configuration file")
	var data, name, folder, magnet, id, listen, public *string
	var live, favorite, hosting *bool
	jsonOutput, watch := false, false
	if command == "run" || command == "status" {
		f.BoolVar(&jsonOutput, "json", false, "emit JSON instead of a human-readable dashboard")
	}
	if command == "status" {
		f.BoolVar(&watch, "watch", false, "refresh the saved status every five seconds; does not start or stop the runner")
	}
	if command == "init" {
		data = f.String("data", "./data", "data directory (relative to config)")
	}
	if command == "add" {
		name = f.String("name", "", "unique display name (defaults to folder name)")
		folder = f.String("folder", "", "local folder to publish; grants access to this folder")
		magnet = f.String("magnet", "", "signed or immutable magnet to host")
		id = f.String("id", "", "existing site ID in this data directory")
		listen = f.String("listen", "", "enable HTTP serving at IP:port")
		public = f.String("public-url", "", "browser-facing http(s) origin")
		live = f.Bool("live", false, "publish stable folder changes automatically")
		favorite = f.Bool("favorite", false, "retain and fully download as a favorite")
		hosting = f.Bool("hosting", true, "download/seed content in the background")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments; use command flags")
	}
	switch command {
	case "init":
		if err := runner.Init(*path, *data); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "Created %s. Add a site with interweb add, then interweb run.\n", *path)
		return err
	case "add":
		if *name == "" && *folder != "" {
			*name = filepath.Base(filepath.Clean(*folder))
		}
		site := runner.SiteConfig{Name: *name, Folder: *folder, Magnet: *magnet, ID: *id, Live: *live, Favorite: *favorite, Hosting: hosting}
		if *public != "" && *listen == "" {
			return errors.New("--public-url requires --listen")
		}
		if *listen != "" {
			site.Serve = &runner.ServeConfig{Listen: *listen, PublicURL: *public}
		}
		if err := runner.Add(*path, site); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "Added %q to %s; restart the runner to apply.\n", *name, *path)
		return err
	}
	c, err := runner.Load(*path)
	if err != nil {
		return err
	}
	if (command == "run" || command == "status") && !jsonOutput {
		redraw := false
		if file, ok := out.(*os.File); ok {
			redraw = (command == "run" || watch) && os.Getenv("TERM") != "dumb" && isatty.IsTerminal(file.Fd())
		}
		out = runner.NewDashboard(out, redraw)
	}
	switch command {
	case "status":
		for {
			if err := runner.ReadStatus(c, out); err != nil {
				return err
			}
			if !watch {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
		}
	case "check":
		_, err = fmt.Fprintf(out, "Valid config: %d sites; data directory %s\n", len(c.Sites), c.DataDir)
	case "list":
		for _, s := range c.Sites {
			source := s.Folder
			if source == "" {
				source = s.Magnet
			}
			if source == "" {
				source = "id:" + s.ID
			}
			serving := "off"
			if s.Serve != nil {
				serving = s.Serve.Listen
			}
			if _, err = fmt.Fprintf(out, "%s\t%s\tserve=%s live=%t\n", s.Name, source, serving, s.Live); err != nil {
				return err
			}
		}
	case "run":
		err = runner.Run(ctx, c, out)
	}
	return err
}
