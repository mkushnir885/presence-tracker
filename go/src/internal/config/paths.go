package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func DataDir() string {
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "ptrack")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "AppData", "Local", "ptrack")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "ptrack")
	default:
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "ptrack")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".local", "share", "ptrack")
	}
}

func CacheDir() string {
	dir, _ := os.UserCacheDir()
	return filepath.Join(dir, "ptrack")
}

func configDir() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "ptrack")
}

func expandPath(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.FromSlash(p)
}
