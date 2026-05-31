package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	// EventsFile is the parquet event log inside every meeting directory.
	EventsFile          = "events.parquet"
	defaultRowGroupSize = 10000
	// TmpSuffix marks an in-progress meeting dir; the GUI skips these so a
	// crashed session never appears in listings.
	TmpSuffix      = ".tmp"
	activeBaseName = "active"
)

// Writer streams events into <meetingsDir>/<dirName>/events.parquet. The
// active dir is named active[_NN].tmp; on Close the DirTemplate is rendered
// with start+end and the dir is renamed to the final name (with _NN if the
// rendered name collides).
type Writer struct {
	mu        sync.Mutex
	buf       []Record
	parentDir string
	dirName   string
	tmpl      DirTemplate
	startTime time.Time
}

// NewWriter creates the writer with an active.tmp directory. dirNameTemplate
// is rendered at Close. Use ParseDirTemplate to validate user input before
// calling.
func NewWriter(meetingsDir string, dirNameTemplate DirTemplate, startTime time.Time) (*Writer, error) {
	if err := os.MkdirAll(meetingsDir, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", meetingsDir, err)
	}
	unique, err := ensureUniqueDir(meetingsDir, activeBaseName, TmpSuffix)
	if err != nil {
		return nil, fmt.Errorf("eventstore: %w", err)
	}
	full := filepath.Join(meetingsDir, unique)
	if err := os.MkdirAll(full, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", full, err)
	}
	return &Writer{
		parentDir: meetingsDir,
		dirName:   unique,
		tmpl:      dirNameTemplate,
		startTime: startTime,
	}, nil
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

// Close flushes, renders the template with start+end, and renames the dir to
// the resulting name (with _NN if it collides). Returns the final name — or ""
// when no events were written and the empty dir was removed.
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

	rendered := w.tmpl.Render(w.startTime.Local(), time.Now().Local())
	finalName, err := ensureUniqueDir(w.parentDir, rendered, "")
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
