package stats

import (
	"context"
	"crypto/sha1" //nolint:gosec // non-crypto: cache key over file path list
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"presence-tracker/src/internal/ptrackpy"
)

// Loader fetches stats Documents through ptrack_py, caching parsed
// payloads on disk so a repeat request for the same file set returns
// without spawning the subprocess. Loader is safe for concurrent use.
type Loader struct {
	cacheDir string
}

// New creates a Loader that stores cached JSON payloads under cacheDir.
// The directory is created on demand; callers do not need to MkdirAll
// up front.
func New(cacheDir string) *Loader {
	return &Loader{cacheDir: cacheDir}
}

// Load returns the stats Document for the given Parquet files. The
// cache is consulted first; on a hit the JSON is parsed straight from
// disk. On a miss (or stale cache) ptrack_py is invoked, its stdout is
// written to the cache, and the parsed Document is returned.
func (l *Loader) Load(ctx context.Context, files []string) (*Document, error) {
	if len(files) == 0 {
		return nil, errors.New("stats: at least one file is required")
	}

	abs, err := absSorted(files)
	if err != nil {
		return nil, err
	}

	cachePath := filepath.Join(l.cacheDir, cacheName(abs))

	if cached, ok := l.tryReadCache(cachePath, abs); ok {
		return cached, nil
	}

	out, err := ptrackpy.Run(ctx, append([]string{"stats"}, abs...)...)
	if err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}

	doc, err := parse(out)
	if err != nil {
		return nil, err
	}

	if err := l.writeCache(cachePath, out); err != nil {
		slog.Warn("stats: cache write failed", "path", cachePath, "err", err)
	}
	return doc, nil
}

// tryReadCache returns the cached Document iff the cache file exists
// and is at least as new as every input. Cache misses and stale entries
// both come back as (nil, false) without error. Sibling
// `../questions/<meeting_id>.jsonl` files are checked too, so a newly
// added or rewritten question bank invalidates the cached marker bodies.
func (l *Loader) tryReadCache(cachePath string, inputs []string) (*Document, bool) {
	cacheInfo, err := os.Stat(cachePath)
	if err != nil {
		return nil, false
	}
	cacheMTime := cacheInfo.ModTime()

	for _, f := range inputs {
		info, err := os.Stat(f)
		if err != nil {
			return nil, false
		}
		if info.ModTime().After(cacheMTime) {
			return nil, false
		}
		if qPath := questionsPathFor(f); qPath != "" {
			if qi, err := os.Stat(qPath); err == nil && qi.ModTime().After(cacheMTime) {
				return nil, false
			}
		}
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, false
	}
	doc, err := parse(data)
	if err != nil {
		// Corrupt cache: drop it so the next call regenerates cleanly.
		_ = os.Remove(cachePath)
		return nil, false
	}
	return doc, true
}

func (l *Loader) writeCache(cachePath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	// Atomic replace via temp file in the same dir.
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), ".stats-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, cachePath)
}

func parse(data []byte) (*Document, error) {
	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("stats: parse JSON: %w", err)
	}
	return &doc, nil
}

func absSorted(files []string) ([]string, error) {
	out := make([]string, 0, len(files))
	for _, f := range files {
		a, err := filepath.Abs(f)
		if err != nil {
			return nil, fmt.Errorf("stats: resolve %q: %w", f, err)
		}
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// questionsPathFor mirrors the sibling-directory convention used by
// ptrack_py: `<base>/meetings/<id>.parquet` pairs with
// `<base>/questions/<id>.jsonl`. Returns "" when the input doesn't
// match the convention.
func questionsPathFor(parquetPath string) string {
	dir := filepath.Dir(parquetPath)
	base := filepath.Base(parquetPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), "questions", stem+".jsonl")
}

// cacheName derives a short, filesystem-safe filename from the sorted
// list of input paths. Collisions in the truncated digest would
// produce a wrong-file cache hit, so the full hex digest is used.
func cacheName(absSortedFiles []string) string {
	h := sha1.New() //nolint:gosec // non-crypto: cache key
	for _, f := range absSortedFiles {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return strings.Join([]string{"stats", hex.EncodeToString(h.Sum(nil))}, "-") + ".json"
}
