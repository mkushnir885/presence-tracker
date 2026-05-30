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

	const (
		colMeetingID   = 0
		colTimestamp   = 1
		colEventType   = 2
		colDisplayName = 3
		colChallengeID = 4
		colQuestionID  = 5
		colMetadata    = 6
		numCols        = 7
	)
	strCols := make([]*strReader, numCols)
	for i := range numCols {
		if i == colTimestamp {
			continue
		}
		strCols[i] = newStrReader(table.Column(i))
	}
	tsCol := newInt64Reader(table.Column(colTimestamp))

	// Timestamps are stored relative: session_started holds absolute Unix
	// ms, every other event holds a ms offset from it. Reconstruct the
	// session start first, then add each offset back to wall-clock time.
	rawTS := make([]int64, n)
	for i := range n {
		rawTS[i] = tsCol.get(i)
	}

	var sessionStart time.Time
	for i := range n {
		if strCols[colEventType].get(i) == "session_started" {
			sessionStart = time.UnixMilli(rawTS[i]).UTC()
			break
		}
	}

	for i := range n {
		var ts time.Time
		if strCols[colEventType].get(i) == "session_started" || sessionStart.IsZero() {
			ts = time.UnixMilli(rawTS[i]).UTC()
		} else {
			ts = sessionStart.Add(time.Duration(rawTS[i]) * time.Millisecond)
		}

		r := Record{
			MeetingID: strCols[colMeetingID].get(i),
			Timestamp: ts,
			EventType: strCols[colEventType].get(i),
		}
		if !strCols[colDisplayName].isNull(i) {
			r.DisplayName = strCols[colDisplayName].get(i)
		}
		if !strCols[colChallengeID].isNull(i) {
			r.ChallengeID = strCols[colChallengeID].get(i)
		}
		if !strCols[colQuestionID].isNull(i) {
			r.QuestionID = strCols[colQuestionID].get(i)
		}
		if !strCols[colMetadata].isNull(i) {
			raw := strCols[colMetadata].get(i)
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

type strReader struct {
	chunks []*array.String
	ends   []int
}

func newStrReader(col *arrow.Column) *strReader {
	sr := &strReader{}
	offset := 0
	for _, chunk := range col.Data().Chunks() {
		s := chunk.(*array.String) //nolint:forcetypeassert // column dtype is fixed by the Arrow schema
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

type int64Reader struct {
	chunks []*array.Int64
	ends   []int
}

func newInt64Reader(col *arrow.Column) *int64Reader {
	r := &int64Reader{}
	offset := 0
	for _, chunk := range col.Data().Chunks() {
		a := chunk.(*array.Int64) //nolint:forcetypeassert // column dtype is fixed by the Arrow schema
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
