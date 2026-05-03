package reporter

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Row holds per-participant stats from a single-meeting CSV report.
type Row struct {
	DisplayName       string
	PresenceRatio     float64
	ChallengesIssued  int
	ChallengesCorrect int
}

// AggregateRow holds per-participant-per-meeting stats from a cross-meeting report.
type AggregateRow struct {
	DisplayName       string
	Meeting           string // ISO-8601 UTC start time
	PresenceRatio     float64
	ChallengesIssued  int
	ChallengesCorrect int
}

// Generate invokes ptrack_py and returns the CSV report as a string.
// Patterns containing glob wildcards (*, ?, [) trigger the aggregate
// subcommand; otherwise the single-meeting report subcommand is used.
func Generate(ctx context.Context, pattern string) (string, error) {
	bin, err := findBinary()
	if err != nil {
		return "", err
	}

	sub := "report"
	if containsGlob(pattern) {
		sub = "aggregate"
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, sub, "--in", pattern, "--out", "-")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("reporter: ptrack_py %s: %w: %s", sub, err, msg)
		}
		return "", fmt.Errorf("reporter: ptrack_py %s: %w", sub, err)
	}

	slog.Debug("reporter: report generated", "subcommand", sub, "bytes", stdout.Len())
	return stdout.String(), nil
}

// Parse parses the CSV string returned by Generate for a single meeting.
func Parse(csvStr string) ([]Row, error) {
	records, err := csv.NewReader(strings.NewReader(csvStr)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("reporter: parse CSV: %w", err)
	}
	if len(records) < 2 { // header only or empty
		return nil, nil
	}
	rows := make([]Row, 0, len(records)-1)
	for _, rec := range records[1:] {
		if len(rec) < 4 {
			continue
		}
		ratio, _ := strconv.ParseFloat(rec[1], 64)
		issued, _ := strconv.Atoi(rec[2])
		correct, _ := strconv.Atoi(rec[3])
		rows = append(rows, Row{
			DisplayName:       rec[0],
			PresenceRatio:     ratio,
			ChallengesIssued:  issued,
			ChallengesCorrect: correct,
		})
	}
	return rows, nil
}

// ParseAggregate parses the CSV string returned by Generate for a glob pattern.
func ParseAggregate(csvStr string) ([]AggregateRow, error) {
	records, err := csv.NewReader(strings.NewReader(csvStr)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("reporter: parse aggregate CSV: %w", err)
	}
	if len(records) < 2 {
		return nil, nil
	}
	rows := make([]AggregateRow, 0, len(records)-1)
	for _, rec := range records[1:] {
		if len(rec) < 5 {
			continue
		}
		ratio, _ := strconv.ParseFloat(rec[2], 64)
		issued, _ := strconv.Atoi(rec[3])
		correct, _ := strconv.Atoi(rec[4])
		rows = append(rows, AggregateRow{
			DisplayName:       rec[0],
			Meeting:           rec[1],
			PresenceRatio:     ratio,
			ChallengesIssued:  issued,
			ChallengesCorrect: correct,
		})
	}
	return rows, nil
}

// findBinary locates the ptrack_py executable: first next to the running
// ptrack binary, then in PATH.
func findBinary() (string, error) {
	self, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(self)
		for _, name := range []string{"ptrack_py", "ptrack_py.exe"} {
			if candidate := filepath.Join(dir, name); fileExists(candidate) {
				return candidate, nil
			}
		}
	}
	if path, err := exec.LookPath("ptrack_py"); err == nil {
		return path, nil
	}
	return "", errors.New("reporter: ptrack_py not found next to ptrack or in PATH")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func containsGlob(s string) bool {
	return strings.ContainsAny(s, "*?[")
}
