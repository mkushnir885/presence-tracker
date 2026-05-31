package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/apache/arrow/go/v17/arrow"
	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/memory"
	"github.com/apache/arrow/go/v17/parquet"
	"github.com/apache/arrow/go/v17/parquet/compress"
	"github.com/apache/arrow/go/v17/parquet/pqarrow"
)

// Record is one event row; empty optional fields are written as null.
// FromStartMS is ms elapsed since the meeting start (0 for session_started);
// the absolute start/end live in session_started/session_ended metadata.
type Record struct {
	MeetingID   string
	FromStartMS int64
	EventType   string
	DisplayName string
	ChallengeID string
	QuestionID  string
	Metadata    map[string]string
}

const (
	dirTimeFormat = "060102-1504"
	// EventsFile is the parquet event log inside every meeting directory.
	EventsFile          = "events.parquet"
	defaultRowGroupSize = 10000
	// TmpSuffix marks an in-progress meeting dir; the GUI skips these so a
	// crashed session never appears in listings.
	TmpSuffix      = ".tmp"
	activeBaseName = "active"
)

// Writer streams events into <meetingsDir>/<dirName>/events.parquet. The
// active dir is named active[_NN].tmp and renamed on Close to
// <start>_<end>[_NN] (or kept as-is for a custom name).
type Writer struct {
	mu         sync.Mutex
	buf        []Record
	parentDir  string
	dirName    string
	startTime  time.Time
	customName bool
}

func NewWriter(meetingsDir, customDirName string, startTime time.Time) (*Writer, error) {
	if err := os.MkdirAll(meetingsDir, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", meetingsDir, err)
	}
	w := &Writer{
		parentDir: meetingsDir,
		startTime: startTime,
	}
	if customDirName == "" {
		unique, err := ensureUniqueDir(meetingsDir, activeBaseName, TmpSuffix)
		if err != nil {
			return nil, fmt.Errorf("eventstore: %w", err)
		}
		w.dirName = unique
	} else {
		clean, err := ValidateDirName(customDirName)
		if err != nil {
			return nil, fmt.Errorf("eventstore: %w", err)
		}
		unique, err := ensureUniqueDir(meetingsDir, clean, "")
		if err != nil {
			return nil, fmt.Errorf("eventstore: %w", err)
		}
		w.dirName = unique
		w.customName = true
	}
	full := filepath.Join(meetingsDir, w.dirName)
	if err := os.MkdirAll(full, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", full, err)
	}
	return w, nil
}

func ensureUniqueDir(parent, baseName, suffix string) (string, error) {
	for i := 0; i <= 99; i++ {
		candidate := baseName + suffix
		if i > 0 {
			candidate = fmt.Sprintf("%s_%02d%s", baseName, i, suffix)
		}
		_, err := os.Stat(filepath.Join(parent, candidate))
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("too many collisions for %q", baseName+suffix)
}

func ValidateDirName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("dir name is empty")
	}
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return "", fmt.Errorf("dir name %q is not allowed", s)
	}
	if strings.ContainsAny(s, `/\`+"\x00") {
		return "", fmt.Errorf("dir name %q contains a path separator", s)
	}
	if strings.HasSuffix(s, TmpSuffix) {
		return "", fmt.Errorf("dir name %q ends with reserved suffix %q", s, TmpSuffix)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == ' ':
		default:
			return "", fmt.Errorf("dir name contains invalid character %q", r)
		}
	}
	return s, nil
}

func (w *Writer) Append(r Record) {
	w.mu.Lock()
	w.buf = append(w.buf, r)
	w.mu.Unlock()
}

func (w *Writer) Flush(_ context.Context) error {
	w.mu.Lock()
	if len(w.buf) == 0 {
		w.mu.Unlock()
		return nil
	}
	rows := make([]Record, len(w.buf))
	copy(rows, w.buf)
	w.buf = w.buf[:0]
	parquetPath := filepath.Join(w.parentDir, w.dirName, EventsFile)
	w.mu.Unlock()

	return w.writeRowGroup(parquetPath, rows)
}

func (w *Writer) SetStartTime(t time.Time) {
	w.mu.Lock()
	w.startTime = t
	w.mu.Unlock()
}

// Dir returns the absolute path of the meeting directory currently being
// written; callers (challenges pipeline) drop questions.jsonl alongside.
func (w *Writer) Dir() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return filepath.Join(w.parentDir, w.dirName)
}

func (w *Writer) DirName() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dirName
}

// Close flushes, renames the dir to <start>_<end>[_NN] (skipped for custom
// names), and returns the final name — or "" when no events were written and
// the empty dir was removed.
func (w *Writer) Close(ctx context.Context) (string, error) {
	if err := w.Flush(ctx); err != nil {
		return "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	currentPath := filepath.Join(w.parentDir, w.dirName)
	parquetPath := filepath.Join(currentPath, EventsFile)

	if _, err := os.Stat(parquetPath); os.IsNotExist(err) {
		// No events were written; remove the empty dir so we leave no trace.
		_ = os.Remove(currentPath)
		return "", nil
	}

	if w.customName {
		slog.Info("eventstore: closed", "dir", currentPath)
		return w.dirName, nil
	}

	startStr := w.startTime.UTC().Format(dirTimeFormat)
	endStr := time.Now().UTC().Format(dirTimeFormat)
	finalName, err := ensureUniqueDir(w.parentDir, startStr+"_"+endStr, "")
	if err != nil {
		slog.Warn("eventstore: could not pick final meeting dir name", "err", err)
		return w.dirName, nil
	}
	finalPath := filepath.Join(w.parentDir, finalName)
	if err := os.Rename(currentPath, finalPath); err != nil {
		slog.Warn("eventstore: could not rename meeting dir", "from", currentPath, "to", finalPath, "err", err)
		return w.dirName, nil
	}
	w.dirName = finalName
	slog.Info("eventstore: closed", "dir", finalPath)
	return finalName, nil
}

func (w *Writer) writeRowGroup(parquetPath string, rows []Record) error {
	flags := os.O_CREATE | os.O_WRONLY
	if _, err := os.Stat(parquetPath); err == nil {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(parquetPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("eventstore: open %s: %w", parquetPath, err)
	}
	defer func() { _ = f.Close() }()

	return writeRecordTo(f, rows)
}

// writeRecordTo writes records as a single row group to w as one complete
// Parquet stream. Each row's from_start_ms is written verbatim (offsets are
// computed upstream), so no session-start anchor is needed here. An empty
// records slice still yields a valid schema-only file.
func writeRecordTo(w io.Writer, records []Record) error {
	// zstd: universally read by the analytics stack.
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithMaxRowGroupLength(int64(defaultRowGroupSize)),
	)
	pw, err := pqarrow.NewFileWriter(Schema, w, props, pqarrow.NewArrowWriterProperties())
	if err != nil {
		return fmt.Errorf("eventstore: parquet writer: %w", err)
	}
	if len(records) == 0 {
		return pw.Close()
	}

	rec := buildRecord(records)
	defer rec.Release()

	if err := pw.Write(rec); err != nil {
		_ = pw.Close()
		return fmt.Errorf("eventstore: write record: %w", err)
	}
	return pw.Close()
}

func buildRecord(rows []Record) arrow.Record {
	pool := memory.NewGoAllocator()

	meetingID := array.NewStringBuilder(pool)
	ts := array.NewInt64Builder(pool)
	eventType := array.NewStringBuilder(pool)
	displayName := array.NewStringBuilder(pool)
	challengeID := array.NewStringBuilder(pool)
	questionID := array.NewStringBuilder(pool)
	metadata := array.NewStringBuilder(pool)

	appendNullable := func(b *array.StringBuilder, v string) {
		if v == "" {
			b.AppendNull()
		} else {
			b.Append(v)
		}
	}

	for i := range rows {
		r := &rows[i]
		meetingID.Append(r.MeetingID)
		ts.Append(r.FromStartMS)
		eventType.Append(r.EventType)
		appendNullable(displayName, r.DisplayName)
		appendNullable(challengeID, r.ChallengeID)
		appendNullable(questionID, r.QuestionID)
		if r.Metadata == nil {
			metadata.AppendNull()
		} else {
			b, _ := json.Marshal(r.Metadata) //nolint:errchkjson // map[string]string cannot fail JSON encoding
			metadata.Append(string(b))
		}
	}

	cols := []arrow.Array{
		meetingID.NewArray(),
		ts.NewArray(),
		eventType.NewArray(),
		displayName.NewArray(),
		challengeID.NewArray(),
		questionID.NewArray(),
		metadata.NewArray(),
	}
	return array.NewRecord(Schema, cols, int64(len(rows)))
}
