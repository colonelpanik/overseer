package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SaveProvidersAndRoles rewrites just the providers and roles sections of a
// config file, leaving everything else — including comments and key order —
// exactly as the operator wrote it.
//
// The whole-struct alternative (unmarshal, edit, re-marshal) would silently
// delete every comment in the file and reorder its keys. An operator who
// edited one dropdown on a dashboard has not consented to that, so the write
// goes through the YAML node tree and touches two keys.
//
// The write is atomic: a temporary file in the same directory, then a rename,
// so a crash mid-write cannot leave a half-written config that the daemon
// refuses to start against.
func SaveProvidersAndRoles(path string, providers map[string]Provider, roles map[string]Role) error {
	// Validate before touching the file. Writing a config the daemon would
	// refuse to load is worse than refusing the edit.
	check := Default()
	check.Providers = providers
	check.Roles = roles
	if err := check.validateProviders(); err != nil {
		return err
	}

	var doc yaml.Node
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No file yet: start from an empty mapping so the first edit creates
		// one containing only what was edited.
		doc = yaml.Node{
			Kind:    yaml.DocumentNode,
			Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}},
		}
	case err != nil:
		return fmt.Errorf("read config: %w", err)
	default:
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
		if doc.Kind == 0 || len(doc.Content) == 0 {
			doc = yaml.Node{
				Kind:    yaml.DocumentNode,
				Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}},
			}
		}
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config: the top level of %s is not a mapping", path)
	}
	if err := setKey(root, "providers", providers); err != nil {
		return err
	}
	if err := setKey(root, "roles", roles); err != nil {
		return err
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	return writeAtomic(path, out)
}

// setKey replaces the value of key in a mapping node, appending the key when
// it is not already there.
func setKey(root *yaml.Node, key string, value any) error {
	var encoded yaml.Node
	if err := encoded.Encode(value); err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			// Keep the existing key node so its comments survive; swap only
			// the value.
			root.Content[i+1] = &encoded
			return nil
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&encoded)
	return nil
}

// writeAtomic replaces path's contents in one step.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".overseer-config-*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	defer os.Remove(tmp.Name())

	// 0600: the file names the environment variables holding credentials, and
	// on a shared machine that is a map worth not publishing.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
