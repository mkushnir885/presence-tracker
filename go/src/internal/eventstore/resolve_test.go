package eventstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeMeetingDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a minimal events.parquet placeholder so ensureMeetingDir is happy.
	if err := os.WriteFile(filepath.Join(dir, EventsFile), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveMeetingDirsEmpty(t *testing.T) {
	_, err := ResolveMeetingDirs(nil)
	if err == nil {
		t.Fatal("expected error for nil patterns")
	}
}

func TestResolveMeetingDirsNonExistentLiteral(t *testing.T) {
	_, err := ResolveMeetingDirs([]string{"/does/not/exist/at/all"})
	if err == nil {
		t.Fatal("expected error for non-existent path")
	}
}

func TestResolveMeetingDirsMissingParquet(t *testing.T) {
	dir := t.TempDir()
	// Directory exists but has no events.parquet.
	_, err := ResolveMeetingDirs([]string{dir})
	if err == nil {
		t.Fatal("expected error for directory without events.parquet")
	}
	if !strings.Contains(err.Error(), EventsFile) {
		t.Errorf("error should mention %s: %v", EventsFile, err)
	}
}

func TestResolveMeetingDirsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(file, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveMeetingDirs([]string{file})
	if err == nil {
		t.Fatal("expected error for a plain file")
	}
}

func TestResolveMeetingDirsSingle(t *testing.T) {
	root := t.TempDir()
	m := makeMeetingDir(t, root, "m1")

	dirs, err := ResolveMeetingDirs([]string{m})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("want 1 dir, got %d", len(dirs))
	}
	abs, _ := filepath.Abs(m)
	if dirs[0] != abs {
		t.Errorf("got %q, want %q", dirs[0], abs)
	}
}

func TestResolveMeetingDirsGlob(t *testing.T) {
	root := t.TempDir()
	makeMeetingDir(t, root, "2024-01")
	makeMeetingDir(t, root, "2024-02")
	makeMeetingDir(t, root, "other")

	pattern := filepath.Join(root, "2024-*")
	dirs, err := ResolveMeetingDirs([]string{pattern})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dirs) != 2 {
		t.Errorf("want 2 dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestResolveMeetingDirsDeduplicated(t *testing.T) {
	root := t.TempDir()
	m := makeMeetingDir(t, root, "m1")

	dirs, err := ResolveMeetingDirs([]string{m, m, m})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dirs) != 1 {
		t.Errorf("expected deduplication, got %d dirs", len(dirs))
	}
}

func TestResolveMeetingDirsSorted(t *testing.T) {
	root := t.TempDir()
	makeMeetingDir(t, root, "c")
	makeMeetingDir(t, root, "a")
	makeMeetingDir(t, root, "b")

	pattern := filepath.Join(root, "*")
	dirs, err := ResolveMeetingDirs([]string{pattern})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(dirs); i++ {
		if dirs[i] < dirs[i-1] {
			t.Errorf("results not sorted: %v", dirs)
		}
	}
}

func TestResolveMeetingDirsGlobNoMatches(t *testing.T) {
	root := t.TempDir()
	_, err := ResolveMeetingDirs([]string{filepath.Join(root, "no-match-*")})
	if err == nil {
		t.Fatal("expected error for glob with no matches")
	}
}
