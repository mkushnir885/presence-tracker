package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFile(t *testing.T) {
	// Loading a non-existent path returns defaults and writes the file.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	d := defaults()
	v := cfg.Get()
	if v.RetentionDays != d.RetentionDays {
		t.Errorf("RetentionDays: got %d, want %d", v.RetentionDays, d.RetentionDays)
	}
	if v.GUI.Port != d.GUI.Port {
		t.Errorf("GUI.Port: got %d, want %d", v.GUI.Port, d.GUI.Port)
	}

	// File must have been created.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created: %v", err)
	}
}

func TestLoadExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Write a partial override; Load should merge with defaults.
	content := `{"retention_days": 30}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Get()
	if v.RetentionDays != 30 {
		t.Errorf("RetentionDays: got %d, want 30", v.RetentionDays)
	}
	// Non-overridden fields stay at defaults.
	if v.GUI.Port != defaults().GUI.Port {
		t.Errorf("GUI.Port should be default, got %d", v.GUI.Port)
	}
}

func TestApplyMutation(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := cfg.Apply(func(v *Values) {
		v.RetentionDays = 365
		v.GUI.Port = 9090
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	v := cfg.Get()
	if v.RetentionDays != 365 {
		t.Errorf("RetentionDays: got %d, want 365", v.RetentionDays)
	}
	if v.GUI.Port != 9090 {
		t.Errorf("GUI.Port: got %d, want 9090", v.GUI.Port)
	}

	// Value must be persisted to disk.
	raw, err := os.ReadFile(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if got, _ := m["retention_days"].(float64); int(got) != 365 {
		t.Errorf("retention_days on disk: got %v", m["retention_days"])
	}
}

func TestApplyInvalidValueRejected(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	before := cfg.Get().GUI.Port

	// Port must be in [1, 65535]; 0 is out of range.
	err = cfg.Apply(func(v *Values) {
		v.GUI.Port = 99999
	})
	if err == nil {
		t.Fatal("expected validation error for out-of-range port")
	}

	// State must be unchanged after rejection.
	if cfg.Get().GUI.Port != before {
		t.Errorf("port changed to %d after rejected Apply", cfg.Get().GUI.Port)
	}
}

func TestReloadPicksUpExternalChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// Externally rewrite the file.
	if err := os.WriteFile(path, []byte(`{"retention_days": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cfg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if cfg.Get().RetentionDays != 7 {
		t.Errorf("RetentionDays after Reload: got %d, want 7", cfg.Get().RetentionDays)
	}
}

func TestDiffToMapOnlyWritesOverrides(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := cfg.Apply(func(v *Values) { v.RetentionDays = 99 }); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	// Fields still at their defaults must not appear in the file.
	if _, ok := m["meetings_dir_format"]; ok {
		t.Error("meetings_dir_format should not be written when at default")
	}
	// The changed field must appear.
	if _, ok := m["retention_days"]; !ok {
		t.Error("retention_days missing from file after Apply")
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct {
		in   string
		want string
	}{
		{"~/foo", filepath.Join(home, "foo")},
		{"/absolute/path", "/absolute/path"},
		{"relative", "relative"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := expandPath(tc.in); got != tc.want {
			t.Errorf("expandPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
