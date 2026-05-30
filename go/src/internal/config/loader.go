package config

import (
	"os"
	"path/filepath"
)

// Load reads the config at path (or the default location) and commits it back
// canonically — so a plain load also normalizes the file on disk (prunes
// default-equal fields, adds the $schema reference).
func Load(path string) (*Config, error) {
	c := &Config{
		path:     resolvePath(path),
		defaults: defaults(),
	}
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

func Default() (string, bool) {
	for _, p := range defaultCandidates() {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}
