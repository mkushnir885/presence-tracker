package challenger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"presence-tracker/src/internal/challenges"
)

func sampleBank() challenges.Bank {
	return challenges.Bank{
		Questions: []challenges.Question{
			{
				QuestionID:   "00000000-0000-0000-0000-000000000001",
				QuestionType: challenges.Numeric,
				Prompt:       "What is 6 * 7?",
				Answer:       float64(42),
			},
		},
	}
}

func TestReviewDirWriteCreatesDir(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "nested", "review")
	rd := NewReviewDir(tmp)
	path, err := rd.Write(sampleBank())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(path), "auto-") {
		t.Errorf("filename = %q, want auto-...", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not present: %v", err)
	}
	// Round-trips: written YAML must re-parse through challenges.Load.
	bank, err := rd.Read(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(bank.Questions) != 1 {
		t.Errorf("read back %d questions", len(bank.Questions))
	}
}

func TestReviewDirNewestWins(t *testing.T) {
	dir := t.TempDir()
	rd := NewReviewDir(dir)
	if _, err := rd.Write(sampleBank()); err != nil {
		t.Fatal(err)
	}
	// Ensure distinct mtime + filename second.
	time.Sleep(1100 * time.Millisecond)
	second, err := rd.Write(sampleBank())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := rd.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (older swept)", len(entries))
	}
	if entries[0].Path != second {
		t.Errorf("kept %q, want %q", entries[0].Path, second)
	}
}

func TestReviewDirSweep(t *testing.T) {
	dir := t.TempDir()
	rd := NewReviewDir(dir)
	if _, err := rd.Write(sampleBank()); err != nil {
		t.Fatal(err)
	}
	// Drop a teacher-prepared file in the same dir; Sweep must not touch it.
	teacher := filepath.Join(dir, "lesson1.yaml")
	if err := os.WriteFile(teacher, []byte("version: 1\nquestions:\n  - prompt: q\n    type: numeric\n    answer: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rd.Sweep(); err != nil {
		t.Fatal(err)
	}
	entries, err := rd.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no auto-*.yaml after sweep, got %d", len(entries))
	}
	if _, err := os.Stat(teacher); err != nil {
		t.Errorf("teacher's file was deleted: %v", err)
	}
}
