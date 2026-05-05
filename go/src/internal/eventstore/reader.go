package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/apache/arrow/go/v17/arrow"
	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/memory"
	pqfile "github.com/apache/arrow/go/v17/parquet/file"
	"github.com/apache/arrow/go/v17/parquet/pqarrow"
)

// ReadAll reads every record from a Parquet file written by Writer.
func ReadAll(ctx context.Context, path string) ([]Record, error) {
	f, err := pqfile.OpenParquetFile(path, false)
	if err != nil {
		return nil, fmt.Errorf("eventstore: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	reader, err := pqarrow.NewFileReader(f, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		return nil, fmt.Errorf("eventstore: create reader: %w", err)
	}

	table, err := reader.ReadTable(ctx)
	if err != nil {
		return nil, fmt.Errorf("eventstore: read table: %w", err)
	}
	defer table.Release()

	n := int(table.NumRows())
	records := make([]Record, 0, n)

	// Column indices match schema.go:
	// 0=event_id, 1=meeting_id, 2=timestamp, 3=source, 4=event_type,
	// 5=participant_id, 6=platform_handle, 7=display_name, 8=metadata
	strCols := make([]*strReader, 9)
	for i := range 9 {
		if i == 2 {
			continue // timestamp column handled separately
		}
		strCols[i] = newStrReader(table.Column(i))
	}
	tsCol := newInt64Reader(table.Column(2))

	// Collect raw timestamp values (schema v2: absolute Unix ms for
	// meeting_started, ms offset from meeting start for all others).
	rawTS := make([]int64, n)
	for i := range n {
		rawTS[i] = tsCol.get(i)
	}

	// Identify the meeting start time from the meeting_started event so that
	// all offsets can be reconstructed into absolute wall-clock times.
	var meetingStart time.Time
	for i := range n {
		if strCols[4].get(i) == "meeting_started" {
			meetingStart = time.UnixMilli(rawTS[i]).UTC()
			break
		}
	}

	for i := range n {
		var ts time.Time
		if strCols[4].get(i) == "meeting_started" || meetingStart.IsZero() {
			ts = time.UnixMilli(rawTS[i]).UTC()
		} else {
			ts = meetingStart.Add(time.Duration(rawTS[i]) * time.Millisecond)
		}

		r := Record{
			EventID:   strCols[0].get(i),
			MeetingID: strCols[1].get(i),
			Timestamp: ts,
			Source:    strCols[3].get(i),
			EventType: strCols[4].get(i),
		}
		if !strCols[5].isNull(i) {
			r.ParticipantID = strCols[5].get(i)
		}
		if !strCols[6].isNull(i) {
			r.PlatformHandle = strCols[6].get(i)
		}
		if !strCols[7].isNull(i) {
			r.DisplayName = strCols[7].get(i)
		}
		if !strCols[8].isNull(i) {
			raw := strCols[8].get(i)
			if raw != "" {
				var m map[string]string
				if jsonErr := json.Unmarshal([]byte(raw), &m); jsonErr == nil {
					r.Metadata = m
				}
			}
		}
		records = append(records, r)
	}

	return records, nil
}

// strReader gives row-indexed access across chunked string column data.
type strReader struct {
	chunks []*array.String
	// cumulative row counts per chunk for binary search
	ends []int
}

func newStrReader(col *arrow.Column) *strReader {
	sr := &strReader{}
	offset := 0
	for _, chunk := range col.Data().Chunks() {
		s := chunk.(*array.String)
		sr.chunks = append(sr.chunks, s)
		offset += s.Len()
		sr.ends = append(sr.ends, offset)
	}
	return sr
}

func (sr *strReader) locate(row int) (*array.String, int) {
	for i, end := range sr.ends {
		if row < end {
			start := 0
			if i > 0 {
				start = sr.ends[i-1]
			}
			return sr.chunks[i], row - start
		}
	}
	return nil, 0
}

func (sr *strReader) get(row int) string {
	ch, idx := sr.locate(row)
	if ch == nil {
		return ""
	}
	return ch.Value(idx)
}

func (sr *strReader) isNull(row int) bool {
	ch, idx := sr.locate(row)
	if ch == nil {
		return true
	}
	return ch.IsNull(idx)
}

// int64Reader gives row-indexed access across chunked Int64 column data.
type int64Reader struct {
	chunks []*array.Int64
	ends   []int
}

func newInt64Reader(col *arrow.Column) *int64Reader {
	r := &int64Reader{}
	offset := 0
	for _, chunk := range col.Data().Chunks() {
		a := chunk.(*array.Int64)
		r.chunks = append(r.chunks, a)
		offset += a.Len()
		r.ends = append(r.ends, offset)
	}
	return r
}

func (r *int64Reader) get(row int) int64 {
	for i, end := range r.ends {
		if row < end {
			start := 0
			if i > 0 {
				start = r.ends[i-1]
			}
			return r.chunks[i].Value(row - start)
		}
	}
	return 0
}
