package challenger

import (
	"os"
	"path/filepath"
	"testing"

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

func TestReviewPathWriteCreatesDir(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "nested", "review")
	rp := NewReviewPath(tmp, "generated")
	path, err := rp.Write(sampleBank())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, "generated.yaml")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not present: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	bank, err := challenges.Parse(raw)
	if err != nil {
		t.Fatalf("parse bank: %v", err)
	}
	if len(bank.Questions) != 1 {
		t.Errorf("read back %d questions", len(bank.Questions))
	}
}

func TestReviewPathOverwrites(t *testing.T) {
	dir := t.TempDir()
	rp := NewReviewPath(dir, "generated")
	first, err := rp.Write(sampleBank())
	if err != nil {
		t.Fatal(err)
	}
	second, err := rp.Write(sampleBank())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("second write produced different path %q vs %q", second, first)
	}
	if _, err := os.Stat(rp.FilePath()); err != nil {
		t.Fatalf("expected pending bank to exist after overwrite: %v", err)
	}
}
