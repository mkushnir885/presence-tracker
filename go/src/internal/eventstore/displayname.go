package eventstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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

	// Records carry from_start_ms offsets; the rename only rewrites display_name,
	// so they round-trip verbatim with no timestamp re-encoding.
	var buf bytes.Buffer
	if err := writeRecordTo(&buf, records); err != nil {
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
