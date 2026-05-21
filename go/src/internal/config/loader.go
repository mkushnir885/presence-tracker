package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"presence-tracker/src/internal/paths"
)

// Load reads the config file at path and the companion secrets file in the
// same directory. The file may be YAML or JSON; JSON is parsed transparently
// because JSON is a subset of YAML. Secret references of the form
// ${secrets.key} are replaced with values from secrets.yaml.
//
// The user-supplied document is validated against the embedded JSON Schema
// before defaults are merged: an explicit `port: 0` is rejected, an omitted
// `port` is filled by Defaults(). Validation errors include a JSON pointer
// to the offending field so a teacher can find the typo quickly.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	secrets, err := loadSecrets(filepath.Join(filepath.Dir(path), "secrets.yaml"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: read secrets: %w", err)
	}

	resolved := resolveSecrets(string(raw), secrets)

	// Parse via YAML into a generic value (yaml.v3 returns map[string]any),
	// then re-encode as JSON so the validator and json.Unmarshal both see
	// canonical JSON shape regardless of whether the source was YAML or JSON.
	var intermediate any
	if err := yaml.Unmarshal([]byte(resolved), &intermediate); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	jsonBytes, err := json.Marshal(intermediate)
	if err != nil {
		return nil, fmt.Errorf("config: re-encode %s: %w", path, err)
	}

	// Validate user input against the embedded schema before merging defaults
	// so the error message points at what the user actually wrote.
	var validateInput any
	if err := json.Unmarshal(jsonBytes, &validateInput); err != nil {
		return nil, fmt.Errorf("config: decode for validation: %w", err)
	}
	schema, err := ResolvedSchema()
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(validateInput); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	cfg := Defaults()
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}

	if err := expandHomeDirs(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Default returns the path of the first config file found, searching the OS
// config directory and the current working directory for config.yaml,
// config.yml, and config.json (in that order).
func Default() (string, bool) {
	cfgDir := paths.ConfigDir()
	candidates := []string{
		filepath.Join(cfgDir, "config.yaml"),
		filepath.Join(cfgDir, "config.yml"),
		filepath.Join(cfgDir, "config.json"),
		"config.yaml",
		"config.yml",
		"config.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

func loadSecrets(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("config: parse secrets: %w", err)
	}
	return m, nil
}

var secretRef = regexp.MustCompile(`\$\{secrets\.([^}]+)\}`)

func resolveSecrets(src string, secrets map[string]string) string {
	return secretRef.ReplaceAllStringFunc(src, func(match string) string {
		key := secretRef.FindStringSubmatch(match)[1]
		if v, ok := secrets[key]; ok {
			return v
		}
		return match // leave unresolved references as-is
	})
}

// expandHomeDirs replaces leading ~ in path fields with the user home dir.
func expandHomeDirs(c *Config) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("config: resolve home dir: %w", err)
	}
	expand := func(p string) string {
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	c.MeetingsDir = expand(c.MeetingsDir)
	c.QuestionsDir = expand(c.QuestionsDir)
	c.ReportsDir = expand(c.ReportsDir)
	c.DataDir = expand(c.DataDir)
	c.CacheDir = expand(c.CacheDir)
	return nil
}
