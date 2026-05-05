package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
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

// Record is one row of the event log.
type Record struct {
	EventID        string
	MeetingID      string
	Timestamp      time.Time
	Source         string
	EventType      string
	ParticipantID  string            // empty → null
	PlatformHandle string            // empty → null
	DisplayName    string            // empty → null
	Metadata       map[string]string // nil → null
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
	compression  string
	rowGroupSize int
}

// NewWriter creates a Writer that will write to <meetingsDir>/<start>.parquet on Flush
// and rename the file to <start>-<end>.parquet on Close.
// dir is created if it does not exist.
//
// TODO: add a custom file name option (per-meeting or global default) so teachers can
// label recordings (e.g. "CS101-lecture3") instead of relying on the timestamp alone.
func NewWriter(meetingsDir string, startTime time.Time, compression string, rowGroupSize int) (*Writer, error) {
	if err := os.MkdirAll(meetingsDir, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", meetingsDir, err)
	}
	startStr := startTime.UTC().Format(fileTimeFormat)
	return &Writer{
		dir:          meetingsDir,
		startTime:    startTime,
		tmpPath:      filepath.Join(meetingsDir, startStr+".parquet"),
		compression:  compression,
		rowGroupSize: rowGroupSize,
	}, nil
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

// Close flushes remaining buffered records and renames the file from
// <start>.parquet to <start>-<end>.parquet.
func (w *Writer) Close(ctx context.Context) error {
	if err := w.Flush(ctx); err != nil {
		return err
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
//   - meeting_started row → absolute Unix ms (the anchor for other events).
//   - all other rows → ms elapsed since startTime (the meeting start).
//
// If startTime is zero, all rows are stored as absolute Unix ms (safe fallback
// when no meeting_started event was recorded).
func buildRecord(rows []Record, startTime time.Time) arrow.Record {
	pool := memory.NewGoAllocator()

	eventID := array.NewStringBuilder(pool)
	meetingID := array.NewStringBuilder(pool)
	ts := array.NewInt64Builder(pool)
	source := array.NewStringBuilder(pool)
	eventType := array.NewStringBuilder(pool)
	participantID := array.NewStringBuilder(pool)
	platformHandle := array.NewStringBuilder(pool)
	displayName := array.NewStringBuilder(pool)
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
		eventID.Append(r.EventID)
		meetingID.Append(r.MeetingID)
		if r.EventType == "meeting_started" || startTime.IsZero() {
			ts.Append(r.Timestamp.UnixMilli())
		} else {
			ts.Append(r.Timestamp.Sub(startTime).Milliseconds())
		}
		source.Append(r.Source)
		eventType.Append(r.EventType)
		appendNullable(participantID, r.ParticipantID)
		appendNullable(platformHandle, r.PlatformHandle)
		appendNullable(displayName, r.DisplayName)
		if r.Metadata == nil {
			metadata.AppendNull()
		} else {
			b, _ := json.Marshal(r.Metadata) //nolint:errchkjson // map[string]string cannot fail JSON encoding
			metadata.Append(string(b))
		}
	}

	cols := []arrow.Array{
		eventID.NewArray(),
		meetingID.NewArray(),
		ts.NewArray(),
		source.NewArray(),
		eventType.NewArray(),
		participantID.NewArray(),
		platformHandle.NewArray(),
		displayName.NewArray(),
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
