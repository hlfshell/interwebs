package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Init creates a config exclusively; it never overwrites an existing file.
func Init(path, dataDir string) (err error) {
	if dataDir == "" {
		return errors.New("data directory is required")
	}
	doc := map[string]any{"data_dir": dataDir, "source_roots": []string{}, "sites": []SiteConfig{}}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

// Add appends a site while preserving existing YAML comments. A folder is an
// explicit operator grant for that folder only. Concurrent editors must coordinate.
func Add(path string, site SiteConfig) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("config editing requires a regular file, not a symlink")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	if site.Folder != "" {
		// CLI paths are interpreted where the operator types them, then stored absolute.
		site.Folder, err = filepath.Abs(site.Folder)
		if err != nil {
			return err
		}
		site.Folder, err = filepath.EvalSymlinks(site.Folder)
		if err != nil {
			return err
		}
		info, err := os.Stat(site.Folder)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("site folder is not a directory")
		}
		c.SourceRoots = append(c.SourceRoots, site.Folder)
	}
	c.Sites = append(c.Sites, site)
	if err := c.validate(base); err != nil {
		return err
	}
	site = c.Sites[len(c.Sites)-1]

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return errors.New("config exceeds 1 MiB")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	var node yaml.Node
	if err := node.Encode(site); err != nil {
		return err
	}
	appendSequence(doc.Content[0], "sites", &node)
	if site.Folder != "" {
		appendSequence(doc.Content[0], "source_roots", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: site.Folder})
	}
	var output bytes.Buffer
	e := yaml.NewEncoder(&output)
	e.SetIndent(2)
	if err := e.Encode(&doc); err != nil {
		return err
	}
	if err := e.Close(); err != nil {
		return err
	}
	if output.Len() > 1<<20 {
		return errors.New("edited config exceeds 1 MiB")
	}
	return replaceFile(path, output.Bytes(), info.Mode().Perm())
}

func appendSequence(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			n := mapping.Content[i+1]
			if n.Kind != yaml.SequenceNode {
				n.Kind = yaml.SequenceNode
				n.Tag = "!!seq"
				n.Value = ""
				n.Content = nil
			}
			n.Content = append(n.Content, value)
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{value}})
}

func replaceFile(path string, b []byte, mode os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".interweb-config-*")
	if err != nil {
		return err
	}
	defer func() {
		if cleanup := os.Remove(f.Name()); cleanup != nil && !errors.Is(cleanup, os.ErrNotExist) {
			err = errors.Join(err, cleanup)
		}
	}()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
