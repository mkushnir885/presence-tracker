package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"presence-tracker/src/internal/paths"
)

// Load reads the config file at path and the companion secrets file in the
// same directory. Secret references of the form ${secrets.key} are replaced
// with values from secrets.yaml.
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

	var cfg Config
	if err := yaml.Unmarshal([]byte(resolved), &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	cfg.defaults()

	if err := expandHomeDirs(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Default returns the path of the first config file found in the standard
// search order: the OS config dir, then ./config.yaml.
func Default() (string, bool) {
	candidates := []string{
		paths.DefaultConfigFile(),
		"config.yaml",
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
	c.Challenges.FileBased.BanksDir = expand(c.Challenges.FileBased.BanksDir)
	return nil
}
