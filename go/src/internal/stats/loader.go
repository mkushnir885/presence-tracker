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
// keyed by the input files and invalidated when any input's mtime advances.
type Loader struct {
	cacheDir     string
	questionsDir func() string
}

func New(cacheDir string, questionsDir func() string) *Loader {
	return &Loader{cacheDir: cacheDir, questionsDir: questionsDir}
}

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

// enrichMarkers fills each challenge marker's question payload (prompt,
// choices, correct answer) from the meeting's questions JSONL — the Python
// stats output carries only the event-side fields.
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

func cacheName(absSortedFiles []string) string {
	h := sha1.New() //nolint:gosec // non-crypto: cache key
	for _, f := range absSortedFiles {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return strings.Join([]string{"stats", hex.EncodeToString(h.Sum(nil))}, "-") + ".json"
}
