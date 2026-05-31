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

// Loader fetches the GUI stats JSON from `ptrack_py stats`, caching it on disk
// keyed by the input meeting dirs and invalidated when any input's mtime
// advances.
type Loader struct {
	cacheDir string
}

func New(cacheDir string) *Loader {
	return &Loader{cacheDir: cacheDir}
}

// Load resolves the cache for the given meeting-dir paths (absolute) and
// either returns the cached doc or invokes `ptrack_py stats` and caches its
// output.
func (l *Loader) Load(ctx context.Context, dirs []string) (*Document, error) {
	if len(dirs) == 0 {
		return nil, errors.New("stats: at least one meeting dir is required")
	}

	abs, err := absSorted(dirs)
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

// enrichMarkers fills each challenge marker's question payload (prompt,
// choices, correct answer) from each meeting dir's questions.jsonl — the
// Python stats output carries only the event-side fields.
func (l *Loader) enrichMarkers(doc *Document, dirs []string) {
	if doc == nil {
		return
	}

	questions := map[string]eventstore.QuestionRecord{}
	for _, dir := range dirs {
		records, err := eventstore.LoadQuestions(dir)
		if err != nil {
			slog.Warn("stats: load questions", "dir", dir, "err", err)
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

func (l *Loader) tryReadCache(cachePath string, inputs []string) (*Document, bool) {
	cacheInfo, err := os.Stat(cachePath)
	if err != nil {
		return nil, false
	}
	cacheMTime := cacheInfo.ModTime()

	for _, dir := range inputs {
		// Stats depends on both the events parquet and questions sidecar; if
		// either is newer than the cache, recompute.
		for _, name := range []string{eventstore.EventsFile, eventstore.QuestionsFile} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				if os.IsNotExist(err) && name == eventstore.QuestionsFile {
					continue
				}
				return nil, false
			}
			if info.ModTime().After(cacheMTime) {
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
		_ = os.Remove(cachePath)
		return nil, false
	}
	return doc, true
}

func (l *Loader) writeCache(cachePath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
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

func absSorted(dirs []string) ([]string, error) {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		a, err := filepath.Abs(d)
		if err != nil {
			return nil, fmt.Errorf("stats: resolve %q: %w", d, err)
		}
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

func cacheName(absSortedDirs []string) string {
	h := sha1.New() //nolint:gosec // non-crypto: cache key
	for _, d := range absSortedDirs {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	return strings.Join([]string{"stats", hex.EncodeToString(h.Sum(nil))}, "-") + ".json"
}
