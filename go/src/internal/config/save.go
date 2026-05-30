package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func writeConfigFile(path string, overrides map[string]any) error {
	if path == "" {
		return fmt.Errorf("config: no path bound; cannot save")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config: create dir: %w", err)
	}
	body, err := json.MarshalIndent(overrides, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	return atomicWrite(path, append(body, '\n'), 0o600)
}

// atomicWrite writes to a temp file in the same dir, fsyncs it, then renames
// over path, so a crash can never leave a half-written (corrupt) config.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("config: close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return fmt.Errorf("config: chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("config: rename temp: %w", err)
	}
	return nil
}
