package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DataDir returns the OS-appropriate internal data directory for ptrack
// (BoltDB participant registry, OAuth tokens, etc). Not user-settable —
// users who want this elsewhere should symlink the platform default.
// Stdlib has no UserDataDir, so the platform branches are spelled out
// here.
//
// Linux:   $XDG_DATA_HOME/ptrack or ~/.local/share/ptrack.
// macOS:   ~/Library/Application Support/ptrack.
// Windows: %LOCALAPPDATA%\ptrack.
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

// CacheDir returns the OS-appropriate cache directory for ptrack (model
// weights, transient downloads). Not user-settable — symlink the
// platform default to relocate.
func CacheDir() string {
	dir, _ := os.UserCacheDir()
	return filepath.Join(dir, "ptrack")
}

// configDir returns the OS-appropriate configuration directory.
func configDir() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "ptrack")
}

// expandPath normalises a user-supplied path: resolves a leading "~"
// against the user's home directory and converts forward slashes to the
// OS-native separator so the same JSON config works on Linux, macOS,
// and Windows.
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
