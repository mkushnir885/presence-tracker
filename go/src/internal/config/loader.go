package config

import (
	"os"
	"path/filepath"
)

// Load constructs a Config bound to path. When path is empty, the loader
// searches the OS config dir and the current working directory for
// config.json; if none exists, the Config is bound to the canonical
// default location (configDir()/config.json) and the file will be
// created on the first save.
//
// Load runs the same commit pipeline as Reload: any existing file is
// read, validated, pruned to a minimal overrides form, and rewritten
// canonically (with $schema annotation). A loadable but non-canonical
// file is silently normalised on every start; this keeps hand edits
// from drifting.
func Load(path string) (*Config, error) {
	c := &Config{
		path:     resolvePath(path),
		defaults: defaults(),
	}
	// Seed current so Get() works before commit() runs.
	seed := c.defaults
	c.current.Store(&seed)

	c.mu.Lock()
	defer c.mu.Unlock()
	v, err := c.readFromFile()
	if err != nil {
		return nil, err
	}
	if err := c.commit(v); err != nil {
		return nil, err
	}
	return c, nil
}

// resolvePath returns the explicit path when set, otherwise the first
// existing default candidate, otherwise the canonical default path
// (which may not yet exist on disk).
func resolvePath(path string) string {
	if path != "" {
		return path
	}
	for _, p := range defaultCandidates() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(configDir(), "config.json")
}

func defaultCandidates() []string {
	return []string{
		filepath.Join(configDir(), "config.json"),
		"config.json",
	}
}

// Default returns the path of the first existing config.json found,
// searching the OS config directory and the current working directory.
// Used by callers that need to know whether a file actually exists
// (as opposed to where one would be created).
func Default() (string, bool) {
	for _, p := range defaultCandidates() {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

