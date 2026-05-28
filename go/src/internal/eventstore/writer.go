package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
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

// Record is one row of the event log. DisplayName is the canonical
// registered name and is the participant identity used end-to-end.
// ChallengeID and QuestionID are first-class join keys; empty → null.
type Record struct {
	MeetingID   string
	Timestamp   time.Time
	EventType   string
	DisplayName string            // empty → null
	ChallengeID string            // empty → null
	QuestionID  string            // empty → null
	Metadata    map[string]string // nil → null
}

// fileTimeFormat is the Go time layout for the date-time stamp in file names: ddmmyy_hhmm.
const fileTimeFormat = "020106_1504"

// Writer buffers meeting events and flushes them to a Parquet file.
// It is safe to call from multiple goroutines.
type Writer struct {
	mu           sync.Mutex
	buf          []Record
	dir          string
	startTime    time.Time
	tmpPath      string // <dir>/<start>.parquet — renamed to <start>-<end>.parquet on Close
	customName   string // user-supplied base name (no .parquet); when set, tmpPath is the final path and no rename happens
	compression  string
	rowGroupSize int
}

// NewWriter creates a Writer that writes to a Parquet file under meetingsDir.
//
// When fileName is empty, the file is named after the start time: written
// as <start>.parquet during the session and renamed to <start>-<end>.parquet
// on Close. When fileName is non-empty, it must pass ValidateFileName and is
// used verbatim (with a .parquet extension); no rename happens on Close, and
// an existing file at that path is rejected up front.
//
// dir is created if it does not exist.
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

// ValidateFileName returns a sanitized base name (no .parquet extension)
// suitable for use under the meetings directory, or an error describing
// why the input is unacceptable. Allowed characters: letters, digits,
// space, dot, dash, underscore. The input is trimmed, the .parquet
// suffix is stripped if present, and "." / ".." / names containing path
// separators or "../" segments are rejected.
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

// Append adds a record to the in-memory buffer.
func (w *Writer) Append(r Record) {
	w.mu.Lock()
	w.buf = append(w.buf, r)
	w.mu.Unlock()
}

// Flush writes all buffered records to the Parquet file and clears the buffer.
// If the file already exists, the new records are appended as an additional
// row group.
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

// SetStartTime updates the meeting start time used to build the final file name.
// Safe to call any time before Close; has no effect on data already flushed to disk.
func (w *Writer) SetStartTime(t time.Time) {
	w.mu.Lock()
	w.startTime = t
	w.mu.Unlock()
}

// Close flushes remaining buffered records and, for the default
// timestamp-named file, renames it from <start>.parquet to
// <start>-<end>.parquet. When the Writer was constructed with a custom
// file name, the file is already at its final path and no rename
// happens.
func (w *Writer) Close(ctx context.Context) error {
	if err := w.Flush(ctx); err != nil {
		return err
	}
	if w.customName != "" {
		slog.Info("eventstore: closed", "path", w.tmpPath)
		return nil
	}
	endStr := time.Now().UTC().Format(fileTimeFormat)
	startStr := w.startTime.UTC().Format(fileTimeFormat)
	finalPath := filepath.Join(w.dir, startStr+"-"+endStr+".parquet")
	if err := os.Rename(w.tmpPath, finalPath); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("eventstore: could not rename meeting file", "from", w.tmpPath, "to", finalPath, "err", err)
		}
		return nil
	}
	slog.Info("eventstore: closed", "path", finalPath)
	return nil
}

func (w *Writer) writeRowGroup(rows []Record, startTime time.Time) error {
	codec, err := parseCompression(w.compression)
	if err != nil {
		return err
	}

	props := parquet.NewWriterProperties(
		parquet.WithCompression(codec),
		parquet.WithMaxRowGroupLength(int64(w.rowGroupSize)),
	)
	arrowProps := pqarrow.NewArrowWriterProperties()

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

	pw, err := pqarrow.NewFileWriter(Schema, f, props, arrowProps)
	if err != nil {
		return fmt.Errorf("eventstore: parquet writer: %w", err)
	}
	defer func() { _ = pw.Close() }()

	rec := buildRecord(rows, startTime)
	defer rec.Release()

	if err := pw.Write(rec); err != nil {
		return fmt.Errorf("eventstore: write record: %w", err)
	}
	return pw.Close()
}

// buildRecord encodes rows into an Arrow record batch.
// timestamp encoding (schema v2):
//   - session_started row → absolute Unix ms (the anchor for other events).
//   - all other rows → ms elapsed since startTime (the session start).
//
// If startTime is zero, all rows are stored as absolute Unix ms (safe fallback
// when no session_started event was recorded).
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
