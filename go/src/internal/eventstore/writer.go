package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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
)

// Writer streams events into <os.TempDir>/ptrack/meetings/<meetingID> while a
// session is active; Close moves the directory into meetingsDir under the
// rendered template name (with _NN appended if it collides).
type Writer struct {
	mu          sync.Mutex
	buf         []Record
	finalParent string
	activeDir   string
	finalName   string
	tmpl        DirTemplate
	startTime   time.Time
}

// NewWriter validates the meetings dir and creates the active dir under the
// system tmp dir. Use ParseDirTemplate to validate dirNameTemplate first.
func NewWriter(meetingsDir, meetingID string, dirNameTemplate DirTemplate, startTime time.Time) (*Writer, error) {
	if err := os.MkdirAll(meetingsDir, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", meetingsDir, err)
	}
	activeDir := filepath.Join(os.TempDir(), "ptrack", "meetings", meetingID)
	if err := os.MkdirAll(activeDir, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", activeDir, err)
	}
	return &Writer{
		finalParent: meetingsDir,
		activeDir:   activeDir,
		tmpl:        dirNameTemplate,
		startTime:   startTime,
	}, nil
}

func ensureUniqueDir(parent, baseName string) (string, error) {
	for i := 0; i <= 99; i++ {
		candidate := baseName
		if i > 0 {
			candidate = fmt.Sprintf("%s_%02d", baseName, i)
		}
		_, err := os.Stat(filepath.Join(parent, candidate))
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("too many collisions for %q", baseName)
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
	parquetPath := filepath.Join(w.activeDir, EventsFile)
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
	if w.finalName != "" {
		return filepath.Join(w.finalParent, w.finalName)
	}
	return w.activeDir
}

// Close flushes, renders the template, and moves the active dir into
// meetingsDir under the rendered name. Returns the final name — or "" when
// no events were written and the empty dir was removed.
func (w *Writer) Close(ctx context.Context) (string, error) {
	if err := w.Flush(ctx); err != nil {
		return "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	parquetPath := filepath.Join(w.activeDir, EventsFile)
	if _, err := os.Stat(parquetPath); os.IsNotExist(err) {
		_ = os.RemoveAll(w.activeDir)
		return "", nil
	}

	rendered := w.tmpl.Render(w.startTime.Local(), time.Now().Local())
	finalName, err := ensureUniqueDir(w.finalParent, rendered)
	if err != nil {
		slog.Warn("eventstore: could not pick final meeting dir name", "err", err)
		return "", nil
	}
	finalPath := filepath.Join(w.finalParent, finalName)
	if err := moveDir(w.activeDir, finalPath); err != nil {
		slog.Warn("eventstore: could not move meeting dir", "from", w.activeDir, "to", finalPath, "err", err)
		return "", nil
	}
	w.finalName = finalName
	slog.Info("eventstore: closed", "dir", finalPath)
	return finalName, nil
}

// moveDir renames src to dst, falling back to copy+remove when the two are on
// different filesystems (the active dir lives under os.TempDir, which on
// Linux is often a tmpfs separate from the user's home filesystem).
func moveDir(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return os.RemoveAll(src)
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
