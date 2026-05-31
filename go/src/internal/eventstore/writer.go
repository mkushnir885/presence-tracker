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
type Record struct {
	MeetingID   string
	Timestamp   time.Time
	EventType   string
	DisplayName string
	ChallengeID string
	QuestionID  string
	Metadata    map[string]string
}

const fileTimeFormat = "020106_1504"

type Writer struct {
	mu           sync.Mutex
	buf          []Record
	dir          string
	startTime    time.Time
	tmpPath      string
	customName   string
	compression  string
	rowGroupSize int
}

func NewWriter(meetingsDir, fileName string, startTime time.Time, compression string, rowGroupSize int) (*Writer, error) {
	if err := os.MkdirAll(meetingsDir, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", meetingsDir, err)
	}
	w := &Writer{
		dir:          meetingsDir,
		startTime:    startTime,
		compression:  compression,
		rowGroupSize: rowGroupSize,
	}
	if fileName == "" {
		w.tmpPath = filepath.Join(meetingsDir, startTime.UTC().Format(fileTimeFormat)+".parquet")
		return w, nil
	}
	clean, err := ValidateFileName(fileName)
	if err != nil {
		return nil, fmt.Errorf("eventstore: %w", err)
	}
	w.customName = clean
	w.tmpPath = filepath.Join(meetingsDir, clean+".parquet")
	if _, err := os.Stat(w.tmpPath); err == nil {
		return nil, fmt.Errorf("eventstore: file %q already exists", filepath.Base(w.tmpPath))
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("eventstore: stat %s: %w", w.tmpPath, err)
	}
	return w, nil
}

func ValidateFileName(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".parquet")
	if s == "" {
		return "", fmt.Errorf("file name is empty")
	}
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return "", fmt.Errorf("file name %q is not allowed", s)
	}
	if strings.ContainsAny(s, `/\`+"\x00") {
		return "", fmt.Errorf("file name %q contains a path separator", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == ' ':
		default:
			return "", fmt.Errorf("file name contains invalid character %q", r)
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
	w.mu.Unlock()

	startTime := w.startTime
	return w.writeRowGroup(rows, startTime)
}

func (w *Writer) SetStartTime(t time.Time) {
	w.mu.Lock()
	w.startTime = t
	w.mu.Unlock()
}

func (w *Writer) BaseName() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSuffix(filepath.Base(w.tmpPath), ".parquet")
}

// Close flushes and renames the working file to its final
// <start>-<end>.parquet name (unless a custom name was set). Returns the
// final basename, or "" when no rows were ever written.
func (w *Writer) Close(ctx context.Context) (string, error) {
	if err := w.Flush(ctx); err != nil {
		return "", err
	}
	if w.customName != "" {
		slog.Info("eventstore: closed", "path", w.tmpPath)
		return w.customName, nil
	}
	if _, err := os.Stat(w.tmpPath); os.IsNotExist(err) {
		return "", nil
	}
	endStr := time.Now().UTC().Format(fileTimeFormat)
	startStr := w.startTime.UTC().Format(fileTimeFormat)
	finalBase := startStr + "-" + endStr
	finalPath := filepath.Join(w.dir, finalBase+".parquet")
	if err := os.Rename(w.tmpPath, finalPath); err != nil {
		slog.Warn("eventstore: could not rename meeting file", "from", w.tmpPath, "to", finalPath, "err", err)
		return strings.TrimSuffix(filepath.Base(w.tmpPath), ".parquet"), nil
	}
	slog.Info("eventstore: closed", "path", finalPath)
	return finalBase, nil
}

func (w *Writer) writeRowGroup(rows []Record, startTime time.Time) error {
	flags := os.O_CREATE | os.O_WRONLY
	if _, err := os.Stat(w.tmpPath); err == nil {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(w.tmpPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("eventstore: open %s: %w", w.tmpPath, err)
	}
	defer func() { _ = f.Close() }()

	return writeRecordTo(f, rows, startTime, w.compression, w.rowGroupSize)
}

// writeRecordTo writes records as a single row group to w as one complete
// Parquet stream. startTime anchors the relative-timestamp encoding (see
// buildRecord); the caller supplies it because the session_started row may not
// be in this batch. An empty records slice still yields a valid schema-only
// file.
func writeRecordTo(w io.Writer, records []Record, startTime time.Time, compression string, rowGroupSize int) error {
	codec, err := parseCompression(compression)
	if err != nil {
		return err
	}
	props := parquet.NewWriterProperties(
		parquet.WithCompression(codec),
		parquet.WithMaxRowGroupLength(int64(rowGroupSize)),
	)
	pw, err := pqarrow.NewFileWriter(Schema, w, props, pqarrow.NewArrowWriterProperties())
	if err != nil {
		return fmt.Errorf("eventstore: parquet writer: %w", err)
	}
	if len(records) == 0 {
		return pw.Close()
	}

	rec := buildRecord(records, startTime)
	defer rec.Release()

	if err := pw.Write(rec); err != nil {
		_ = pw.Close()
		return fmt.Errorf("eventstore: write record: %w", err)
	}
	return pw.Close()
}

func buildRecord(rows []Record, startTime time.Time) arrow.Record {
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
		// session_started is stored as absolute Unix ms; every other row as
		// a ms offset from it (see Schema). The Python side reverses this.
		if r.EventType == "session_started" || startTime.IsZero() {
			ts.Append(r.Timestamp.UnixMilli())
		} else {
			ts.Append(r.Timestamp.Sub(startTime).Milliseconds())
		}
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
