// Package paths provides OS-appropriate directory paths for ptrack data.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// ConfigDir returns the OS-appropriate configuration directory for ptrack.
//
// Windows: %APPDATA%\ptrack
// Linux/macOS: $XDG_CONFIG_HOME/ptrack or ~/.config/ptrack.
func ConfigDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "ptrack")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "AppData", "Roaming", "ptrack")
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "ptrack")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ptrack")
}

// DataDir returns the OS-appropriate data directory for ptrack.
//
// Windows: %LOCALAPPDATA%\ptrack
// Linux/macOS: $XDG_DATA_HOME/ptrack or ~/.local/share/ptrack.
func DataDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "ptrack")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "AppData", "Local", "ptrack")
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "ptrack")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ptrack")
}

// CacheDir returns the OS-appropriate cache directory for ptrack.
//
// Windows: %LOCALAPPDATA%\ptrack\cache
// Linux/macOS: $XDG_CACHE_HOME/ptrack or ~/.cache/ptrack.
func CacheDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "ptrack", "cache")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "AppData", "Local", "ptrack", "cache")
	}
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "ptrack")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "ptrack")
}

// DefaultConfigFile returns the default config file path.
func DefaultConfigFile() string {
	return filepath.Join(ConfigDir(), "config.yaml")
}

// ControlSocketPath returns the path of the Unix domain socket that a running
// tracker session listens on for control commands (e.g. triggering a poll).
// Using os.TempDir ensures the path is writable on both Linux and Windows.
func ControlSocketPath() string {
	return filepath.Join(os.TempDir(), "ptrack-control.sock")
}
