// Package runner adapts the shared application backend to a configured headless process.
package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hlfshell/interweb/interwebs/identity"
	"go.yaml.in/yaml/v3"
)

// Config owns a dedicated application directory with one profile. Paths are
// resolved relative to the YAML file, never the process working directory.
type Config struct {
	DataDir        string       `yaml:"data_dir"`
	SourceRoots    []string     `yaml:"source_roots"`
	Offline        bool         `yaml:"offline"`
	MaxSiteMiB     int64        `yaml:"max_site_mib"`
	NodeStorageMiB int64        `yaml:"node_storage_mib"`
	Sites          []SiteConfig `yaml:"sites"`
}

// SiteConfig selects exactly one folder, magnet, or saved application site ID.
type SiteConfig struct {
	Name     string       `yaml:"name"`
	Folder   string       `yaml:"folder,omitempty"`
	Magnet   string       `yaml:"magnet,omitempty"`
	ID       string       `yaml:"id,omitempty"`
	Live     bool         `yaml:"live,omitempty"`
	Favorite bool         `yaml:"favorite,omitempty"`
	Hosting  *bool        `yaml:"hosting,omitempty"`
	Serve    *ServeConfig `yaml:"serve,omitempty"`
}

// ServeConfig exposes content only, on its own origin. TLS belongs to an external
// reverse proxy; PublicURL must be explicit for non-loopback listeners.
type ServeConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url,omitempty"`
}

// Load reads one strict YAML document, bounded to 1 MiB, and validates it before I/O setup.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return Config{}, err
	}
	if len(b) > 1<<20 {
		return Config{}, errors.New("config exceeds 1 MiB")
	}
	c := Config{MaxSiteMiB: 512, NodeStorageMiB: 2048}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("config must contain exactly one YAML document")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, err
	}
	if err := c.validate(base); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}
	return c, nil
}

func (c *Config) validate(base string) error {
	if c.DataDir == "" {
		return errors.New("data_dir is required")
	}
	c.DataDir = absolute(base, c.DataDir)
	if c.MaxSiteMiB < 1 || c.MaxSiteMiB > 1024 || c.NodeStorageMiB < 1 || c.NodeStorageMiB > 1<<40 {
		return errors.New("invalid site or node storage limit")
	}
	for i, root := range c.SourceRoots {
		if root == "" {
			return errors.New("empty source root")
		}
		c.SourceRoots[i] = absolute(base, root)
	}
	if len(c.Sites) > 256 {
		return errors.New("configure at most 256 sites")
	}
	names, sources, origins := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range c.Sites {
		s := &c.Sites[i]
		if strings.TrimSpace(s.Name) == "" || names[s.Name] {
			return errors.New("site names must be nonempty and unique")
		}
		names[s.Name] = true
		count, source := 0, ""
		if s.Folder != "" {
			count++
			s.Folder = absolute(base, s.Folder)
			source = "folder:" + s.Folder
			if len(c.SourceRoots) == 0 {
				return errors.New("folder publication requires source_roots")
			}
		}
		if s.Magnet != "" {
			count++
			address, err := identity.ParseMagnet(s.Magnet)
			if err != nil {
				return fmt.Errorf("site %q magnet: %w", s.Name, err)
			}
			s.Magnet = address.Magnet()
			source = s.Magnet
		}
		if s.ID != "" {
			count++
			source = "id:" + s.ID
		}
		if count != 1 || sources[source] {
			return errors.New("each site needs one unique folder, magnet, or id")
		}
		sources[source] = true
		if s.Live && s.Folder == "" {
			return errors.New("live requires a folder")
		}
		if s.Serve != nil {
			if err := s.Serve.validate(); err != nil {
				return fmt.Errorf("site %q serving: %w", s.Name, err)
			}
			if origin := s.Serve.PublicURL; origin != "" {
				if origins[origin] {
					return errors.New("served sites must have distinct public origins")
				}
				origins[origin] = true
			}
		}
	}
	return nil
}

func absolute(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

func (c *ServeConfig) validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("listen must be an IP:port")
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || err != nil || n < 0 || n > 65535 {
		return errors.New("listen must be an IP with a port from 0 to 65535")
	}
	if c.PublicURL == "" {
		if !ip.IsLoopback() {
			return errors.New("non-loopback listen requires public_url")
		}
		return nil
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" ||
		strings.ContainsAny(u.Host, " \t\r\n;,'\"\\") {
		return errors.New("public_url must be an http(s) origin without credentials, path, query, or fragment")
	}
	if p := u.Port(); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil || v < 1 || v > 65535 {
			return errors.New("invalid public_url port")
		}
		// Explicit default ports denote the same browser origin as omitted ports.
		if (u.Scheme == "http" && v == 80) || (u.Scheme == "https" && v == 443) {
			u.Host = u.Hostname()
			if strings.Contains(u.Host, ":") {
				u.Host = "[" + u.Host + "]"
			}
		}
	}
	c.PublicURL = strings.ToLower(u.Scheme + "://" + u.Host)
	return nil
}
