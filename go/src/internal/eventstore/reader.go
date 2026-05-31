package eventstore

import (
	"context"
	"encoding/json"
	"fmt"

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
		colFromStartMS = 1
		colEventType   = 2
		colDisplayName = 3
		colChallengeID = 4
		colQuestionID  = 5
		colMetadata    = 6
		numCols        = 7
	)
	strCols := make([]*strReader, numCols)
	for i := range numCols {
		if i == colFromStartMS {
			continue
		}
		strCols[i] = newStrReader(table.Column(i))
	}
	tsCol := newInt64Reader(table.Column(colFromStartMS))

	// from_start_ms is stored as a ms offset from the meeting start and read
	// back verbatim — callers work in offsets, never reconstructing wall-clock.
	for i := range n {
		r := Record{
			MeetingID:   strCols[colMeetingID].get(i),
			FromStartMS: tsCol.get(i),
			EventType:   strCols[colEventType].get(i),
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

// locateInChunks maps a flat row index to the chunk holding it and the offset
// within that chunk, given the chunks' cumulative end offsets. Returns a chunk
// index of -1 when row is out of range.
func locateInChunks(row int, ends []int) (chunk, offset int) {
	for i, end := range ends {
		if row < end {
			start := 0
			if i > 0 {
				start = ends[i-1]
			}
			return i, row - start
		}
	}
	return -1, 0
}

func (sr *strReader) locate(row int) (*array.String, int) {
	i, off := locateInChunks(row, sr.ends)
	if i < 0 {
		return nil, 0
	}
	return sr.chunks[i], off
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
	i, off := locateInChunks(row, r.ends)
	if i < 0 {
		return 0
	}
	return r.chunks[i].Value(off)
}
