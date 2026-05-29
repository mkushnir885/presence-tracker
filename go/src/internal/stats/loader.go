package stats

import (
	"context"
	"crypto/sha1" //nolint:gosec // non-crypto: cache key over file path list
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/ptrackpy"
)

// Loader fetches stats Documents through ptrack_py, caching parsed
// payloads on disk so a repeat request for the same file set returns
// without spawning the subprocess. Markers come back from ptrack_py
// with only the event-side fields populated; the Loader reads each
// meeting's questions JSONL and fills in prompt/choices/correct_answer
// etc. before returning. Loader is safe for concurrent use.
//
// questionsDir is a getter so the configured directory is re-resolved
// on every Load — `ptrack reload` and the GUI config editor can change
// the value while the Loader is alive.
type Loader struct {
	cacheDir     string
	questionsDir func() string
}

// New creates a Loader that stores cached JSON payloads under cacheDir.
// The directory is created on demand; callers do not need to MkdirAll
// up front. questionsDir returns the configured questions directory at
// call time; nil is allowed and skips marker enrichment so markers
// surface with empty prompt/answer fields (the GUI then renders the
// "question details unavailable" notice).
func New(cacheDir string, questionsDir func() string) *Loader {
	return &Loader{cacheDir: cacheDir, questionsDir: questionsDir}
}

// Load returns the stats Document for the given Parquet files. The
// cache is consulted first; on a hit the JSON is parsed straight from
// disk. On a miss (or stale cache) ptrack_py is invoked, its stdout is
// written to the cache, and the parsed Document is returned. Question
// payloads are merged from the JSONL files on every call (whether the
// cache hit or missed) so a freshly edited questions file is reflected
// immediately without invalidating the Parquet-derived cache entry.
func (l *Loader) Load(ctx context.Context, files []string) (*Document, error) {
	if len(files) == 0 {
		return nil, errors.New("stats: at least one file is required")
	}

	abs, err := absSorted(files)
	if err != nil {
		return nil, err
	}

	cachePath := filepath.Join(l.cacheDir, cacheName(abs))

	doc, ok := l.tryReadCache(cachePath, abs)
	if !ok {
		out, err := ptrackpy.Run(ctx, append([]string{"stats"}, abs...)...)
		if err != nil {
			return nil, fmt.Errorf("stats: %w", err)
		}
		doc, err = parse(out)
		if err != nil {
			return nil, err
		}
		if err := l.writeCache(cachePath, out); err != nil {
			slog.Warn("stats: cache write failed", "path", cachePath, "err", err)
		}
	}

	l.enrichMarkers(doc, abs)
	return doc, nil
}

// enrichMarkers fills in each marker's question payload from the
// matching JSONL file. Question IDs are UUIDv4 so a single global map
// across all input files is unambiguous. Missing question IDs (deleted
// JSONL, manually-edited cache) are left blank — the GUI popover
// renders its "question details unavailable" notice in that case.
func (l *Loader) enrichMarkers(doc *Document, inputs []string) {
	if doc == nil || l.questionsDir == nil {
		return
	}
	qDir := l.questionsDir()
	if qDir == "" {
		return
	}

	questions := map[string]eventstore.QuestionRecord{}
	for _, f := range inputs {
		path := questionsPathFor(qDir, f)
		if path == "" {
			continue
		}
		records, err := eventstore.LoadQuestions(path)
		if err != nil {
			slog.Warn("stats: load questions", "path", path, "err", err)
			continue
		}
		maps.Copy(questions, records)
	}

	for pi := range doc.Participants {
		for ri := range doc.Participants[pi].Rows {
			markers := doc.Participants[pi].Rows[ri].Markers
			for mi := range markers {
				mk := &markers[mi]
				if mk.QuestionID == "" {
					continue
				}
				q, ok := questions[mk.QuestionID]
				if !ok {
					continue
				}
				mk.Prompt = q.Prompt
				mk.QuestionType = q.QuestionType
				mk.Choices = q.Choices
				mk.CorrectAnswer = stringifyAnswer(q.CorrectAnswer)
				mk.MatchMode = q.MatchMode
				mk.Tolerance = q.Tolerance
			}
		}
	}
}

// stringifyAnswer coerces a question's correct_answer field into a
// single string, matching the Python side's prior behaviour: MCQ
// arrays come back as a JSON-encoded list (rendered as a comma list
// by the templ helper), numerics are stringified verbatim.
func stringifyAnswer(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return fmt.Sprint(x)
	}
}

// tryReadCache returns the cached Document iff the cache file exists
// and is at least as new as every input. Cache misses and stale entries
// both come back as (nil, false) without error. The cache stores only
// the Parquet-derived portion of the document; question payloads are
// re-merged on every Load, so the questions JSONL does not participate
// in invalidation.
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

// questionsPathFor returns the JSONL path that pairs with parquetPath
// under the configured questionsDir: <questionsDir>/<basename>.jsonl.
// Returns "" when questionsDir or the basename are empty so the caller
// can skip the staleness check.
func questionsPathFor(questionsDir, parquetPath string) string {
	if questionsDir == "" {
		return ""
	}
	stem := strings.TrimSuffix(filepath.Base(parquetPath), filepath.Ext(parquetPath))
	if stem == "" {
		return ""
	}
	return filepath.Join(questionsDir, stem+".jsonl")
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
