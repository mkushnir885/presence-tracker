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

// Writer buffers meeting events and flushes them to a Parquet file.
// It is safe to call from multiple goroutines.
type Writer struct {
	mu           sync.Mutex
	buf          []Record
	path         string
	compression  string
	rowGroupSize int
}

// NewWriter creates a Writer that will write to path on Flush/Close.
// dir is created if it does not exist.
func NewWriter(meetingsDir, meetingID, compression string, rowGroupSize int) (*Writer, error) {
	if err := os.MkdirAll(meetingsDir, 0o755); err != nil {
		return nil, fmt.Errorf("eventstore: mkdir %s: %w", meetingsDir, err)
	}
	return &Writer{
		path:         filepath.Join(meetingsDir, meetingID+".parquet"),
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

	return w.writeRowGroup(rows)
}

// Close flushes remaining buffered records and releases resources.
func (w *Writer) Close(ctx context.Context) error {
	if err := w.Flush(ctx); err != nil {
		return err
	}
	slog.Info("eventstore: closed", "path", w.path)
	return nil
}

func (w *Writer) writeRowGroup(rows []Record) error {
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
	if _, err := os.Stat(w.path); err == nil {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(w.path, flags, 0o644)
	if err != nil {
		return fmt.Errorf("eventstore: open %s: %w", w.path, err)
	}
	defer func() { _ = f.Close() }()

	pw, err := pqarrow.NewFileWriter(Schema, f, props, arrowProps)
	if err != nil {
		return fmt.Errorf("eventstore: parquet writer: %w", err)
	}
	defer func() { _ = pw.Close() }()

	rec := buildRecord(rows)
	defer rec.Release()

	if err := pw.Write(rec); err != nil {
		return fmt.Errorf("eventstore: write record: %w", err)
	}
	return pw.Close()
}

func buildRecord(rows []Record) arrow.Record {
	pool := memory.NewGoAllocator()
	tsType := &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: "UTC"}

	eventID := array.NewStringBuilder(pool)
	meetingID := array.NewStringBuilder(pool)
	ts := array.NewTimestampBuilder(pool, tsType)
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
		ts.Append(arrow.Timestamp(r.Timestamp.UnixMilli()))
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
