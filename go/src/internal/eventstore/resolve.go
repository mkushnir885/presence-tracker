package eventstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ResolveMeetingDirs expands the given path/glob patterns into a sorted,
// deduplicated list of absolute meeting-directory paths. Each match must be a
// directory that contains events.parquet; a literal path with no glob meta
// must exist or the call errors. An empty result errors.
func ResolveMeetingDirs(patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, errors.New("eventstore: at least one path is required")
	}

	seen := map[string]struct{}{}
	var out []string
	for _, pattern := range patterns {
		matches, err := expandPattern(pattern)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			abs, err := filepath.Abs(m)
			if err != nil {
				return nil, fmt.Errorf("eventstore: resolve %q: %w", m, err)
			}
			if _, ok := seen[abs]; ok {
				continue
			}
			if err := ensureMeetingDir(abs); err != nil {
				return nil, err
			}
			seen[abs] = struct{}{}
			out = append(out, abs)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("eventstore: no meeting directories matched: %v", patterns)
	}
	sort.Strings(out)
	return out, nil
}

func expandPattern(pattern string) ([]string, error) {
	if !hasGlobMeta(pattern) {
		if _, err := os.Stat(pattern); err != nil {
			return nil, fmt.Errorf("eventstore: %q: %w", pattern, err)
		}
		return []string{pattern}, nil
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("eventstore: glob %q: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("eventstore: no matches for %q", pattern)
	}
	return matches, nil
}

func ensureMeetingDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("eventstore: %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("eventstore: %q is not a meeting directory", path)
	}
	parquet := filepath.Join(path, EventsFile)
	if _, err := os.Stat(parquet); err != nil {
		return fmt.Errorf("eventstore: %q missing %s: %w", path, EventsFile, err)
	}
	return nil
}

func hasGlobMeta(s string) bool {
	for _, r := range s {
		switch r {
		case '*', '?', '[':
			return true
		}
	}
	return false
}
