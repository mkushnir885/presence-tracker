// Package reporter shells out to ptrack_py for CSV report generation.
// The HTTP /report endpoint and the `ptrack report` CLI both flow
// through Generate. Single-file requests use the report subcommand;
// multi-file requests use aggregate.
package reporter

import (
	"context"
	"errors"

	"presence-tracker/src/internal/ptrackpy"
)

// Generate runs ptrack_py over the given Parquet files and returns the
// CSV report bytes. With one file the per-meeting subcommand is used;
// with more than one, the cross-meeting aggregate subcommand.
func Generate(ctx context.Context, files []string) ([]byte, error) {
	if len(files) == 0 {
		return nil, errors.New("reporter: at least one file is required")
	}

	sub := "report"
	if len(files) > 1 {
		sub = "aggregate"
	}

	args := []string{sub}
	for _, f := range files {
		args = append(args, "--in", f)
	}
	args = append(args, "--out", "-")

	return ptrackpy.Run(ctx, args...)
}
