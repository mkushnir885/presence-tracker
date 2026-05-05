package eventstore

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/apache/arrow/go/v17/parquet"
	"github.com/apache/arrow/go/v17/parquet/pqarrow"
)

// UpdateDisplayName rewrites all events in parquetPath, setting display_name
// for every event whose participant_id matches the given participantID.
//
// This is called by the GUI when the teacher changes a student's display name
// in the meeting analysis view.
func UpdateDisplayName(parquetPath, participantID, displayName string) error {
	records, err := ReadAll(context.Background(), parquetPath)
	if err != nil {
		return fmt.Errorf("eventstore: read for rename: %w", err)
	}

	for i := range records {
		if records[i].ParticipantID == participantID {
			records[i].DisplayName = displayName
		}
	}

	tmpPath := parquetPath + ".tmp"
	if err := writeAllToFile(tmpPath, records, "zstd", defaultRowGroupSize); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("eventstore: write tmp file: %w", err)
	}

	if err := os.Rename(tmpPath, parquetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("eventstore: rename tmp file: %w", err)
	}

	return nil
}

const defaultRowGroupSize = 10000

// writeAllToFile writes all records to path, overwriting any existing file.
func writeAllToFile(path string, records []Record, compression string, rowGroupSize int) error {
	codec, err := parseCompression(compression)
	if err != nil {
		return err
	}

	props := parquet.NewWriterProperties(
		parquet.WithCompression(codec),
		parquet.WithMaxRowGroupLength(int64(rowGroupSize)),
	)
	arrowProps := pqarrow.NewArrowWriterProperties()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("eventstore: create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	pw, err := pqarrow.NewFileWriter(Schema, f, props, arrowProps)
	if err != nil {
		return fmt.Errorf("eventstore: parquet writer: %w", err)
	}
	defer func() { _ = pw.Close() }()

	if len(records) == 0 {
		return pw.Close()
	}

	// Recover the meeting start time so timestamps are stored correctly.
	var startTime time.Time
	for _, r := range records {
		if r.EventType == "meeting_started" {
			startTime = r.Timestamp
			break
		}
	}

	rec := buildRecord(records, startTime)
	defer rec.Release()

	if err := pw.Write(rec); err != nil {
		return fmt.Errorf("eventstore: write record: %w", err)
	}
	return pw.Close()
}

// ReadQuestion scans all *.jsonl files in questionsDir for a record whose
// question_id matches questionID. Returns nil, nil if not found.
func ReadQuestion(questionsDir, questionID string) (*QuestionRecord, error) {
	pattern := filepath.Join(questionsDir, "*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("eventstore: glob questions: %w", err)
	}

	for _, path := range files {
		q, err := scanJSONL(path, questionID)
		if err != nil {
			return nil, err
		}
		if q != nil {
			return q, nil
		}
	}
	return nil, nil
}

func scanJSONL(path, questionID string) (*QuestionRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("eventstore: open questions file: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var q QuestionRecord
		if err := json.Unmarshal(line, &q); err != nil {
			continue // skip malformed lines
		}
		if q.QuestionID == questionID {
			return &q, nil
		}
	}
	return nil, scanner.Err()
}
