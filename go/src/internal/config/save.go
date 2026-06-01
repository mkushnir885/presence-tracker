package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"presence-tracker/src/internal/util"
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
	if err := util.AtomicWrite(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}
