package eventstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/apache/arrow/go/v17/parquet"
	"github.com/apache/arrow/go/v17/parquet/pqarrow"
)

// UpdateDisplayName rewrites every row whose display_name equals oldName to
// newName, in place. It reads the whole file, re-encodes, then swaps through a
// .backup copy so a failed overwrite can be rolled back instead of corrupting
// the meeting file.
func UpdateDisplayName(parquetPath, oldName, newName string) error {
	records, err := ReadAll(context.Background(), parquetPath)
	if err != nil {
		return fmt.Errorf("eventstore: read for rename: %w", err)
	}

	for i := range records {
		if records[i].DisplayName == oldName {
			records[i].DisplayName = newName
		}
	}

	var buf bytes.Buffer
	if err := writeAllTo(&buf, records, "zstd", defaultRowGroupSize); err != nil {
		return fmt.Errorf("eventstore: encode parquet: %w", err)
	}

	backupPath := parquetPath + ".backup"
	if err := copyFile(parquetPath, backupPath); err != nil {
		return fmt.Errorf("eventstore: create backup: %w", err)
	}

	if err := overwriteFile(parquetPath, buf.Bytes()); err != nil {
		if rerr := copyFile(backupPath, parquetPath); rerr != nil {
			return fmt.Errorf("eventstore: overwrite failed (%w); restore from %s failed (%v) — restore manually", err, backupPath, rerr)
		}
		_ = os.Remove(backupPath)
		return fmt.Errorf("eventstore: overwrite parquet: %w", err)
	}

	if err := os.Remove(backupPath); err != nil {
		slog.Warn("eventstore: could not remove backup", "path", backupPath, "err", err)
	}
	return nil
}

const defaultRowGroupSize = 10000

func writeAllTo(w io.Writer, records []Record, compression string, rowGroupSize int) error {
	codec, err := parseCompression(compression)
	if err != nil {
		return err
	}

	props := parquet.NewWriterProperties(
		parquet.WithCompression(codec),
		parquet.WithMaxRowGroupLength(int64(rowGroupSize)),
	)
	arrowProps := pqarrow.NewArrowWriterProperties()

	pw, err := pqarrow.NewFileWriter(Schema, w, props, arrowProps)
	if err != nil {
		return fmt.Errorf("eventstore: parquet writer: %w", err)
	}

	if len(records) == 0 {
		return pw.Close()
	}

	var startTime time.Time
	for _, r := range records {
		if r.EventType == "session_started" {
			startTime = r.Timestamp
			break
		}
	}

	rec := buildRecord(records, startTime)
	defer rec.Release()

	if err := pw.Write(rec); err != nil {
		_ = pw.Close()
		return fmt.Errorf("eventstore: write record: %w", err)
	}
	return pw.Close()
}

func overwriteFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fmt.Errorf("eventstore: open %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("eventstore: write %s: %w", path, err)
	}
	return f.Close()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("eventstore: open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("eventstore: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("eventstore: copy to %s: %w", dst, err)
	}
	return out.Close()
}

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
			continue
		}
		if q.QuestionID == questionID {
			return &q, nil
		}
	}
	return nil, scanner.Err()
}
