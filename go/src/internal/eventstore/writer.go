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
	dirTimeFormat = "020106_1504"
	// EventsFile is the parquet event log inside every meeting directory.
	EventsFile = "events.parquet"
	// QuestionsFile is the JSONL question sidecar inside every meeting directory.
	QuestionsFile = "questions.jsonl"
	// Parquet encoding is fixed: zstd is universally read by the analytics
	// stack and a lesson's few-thousand events fit one row group regardless.
	defaultCompression  = "zstd"
	defaultRowGroupSize = 10000
)

// Writer streams events into <meetingsDir>/<dirName>/events.parquet. The dir
// is created up front under a provisional <start> name and renamed to
// <start>-<end> on Close so the final directory mtime sits at the moment the
// session ends; with a custom name no rename occurs.
type Writer struct {
	mu         sync.Mutex
	buf        []Record
	parentDir  string
	dirName    string
	startTime  time.Time
	customName bool
}

func NewWriter(meetingsDir, dirName string, startTime time.Time) (*Writer, error) {
	if err := os.MkdirAll(meetingsDir, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", meetingsDir, err)
	}
	w := &Writer{
		parentDir: meetingsDir,
		startTime: startTime,
	}
	if dirName == "" {
		w.dirName = startTime.UTC().Format(dirTimeFormat)
	} else {
		clean, err := ValidateDirName(dirName)
		if err != nil {
			return nil, fmt.Errorf("eventstore: %w", err)
		}
		w.dirName = clean
		w.customName = true
	}
	full := filepath.Join(meetingsDir, w.dirName)
	if _, err := os.Stat(full); err == nil {
		if w.customName {
			return nil, fmt.Errorf("eventstore: meeting dir %q already exists", w.dirName)
		}
		// auto-named: append a uniqueness suffix so we never clobber a sibling
		w.dirName = w.dirName + "_" + startTime.UTC().Format("05")
		full = filepath.Join(meetingsDir, w.dirName)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("eventstore: stat %s: %w", full, err)
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", full, err)
	}
	return w, nil
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

// Close flushes and renames the working directory to its final
// <start>-<end> name (unless a custom name was set). Returns the final dir
// name, or "" when no events were ever written and the dir was removed.
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

	endStr := time.Now().UTC().Format(dirTimeFormat)
	startStr := w.startTime.UTC().Format(dirTimeFormat)
	finalName := startStr + "-" + endStr
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
	codec, err := parseCompression(defaultCompression)
	if err != nil {
		return err
	}
	props := parquet.NewWriterProperties(
		parquet.WithCompression(codec),
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

func parseCompression(s string) (compress.Compression, error) {
	switch s {
	case "zstd", "":
		return compress.Codecs.Zstd, nil
	case "snappy":
		return compress.Codecs.Snappy, nil
	case "none":
		return compress.Codecs.Uncompressed, nil
	default:
		return compress.Codecs.Uncompressed, fmt.Errorf("eventstore: unknown compression %q", s)
	}
}
