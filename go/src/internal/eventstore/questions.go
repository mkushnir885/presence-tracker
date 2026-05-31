package eventstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// QuestionRecord is one line of a meeting's questions JSONL sidecar (paired
// with its Parquet file by basename); challenge_issued events reference it by
// question_id.
type QuestionRecord struct {
	QuestionID    string   `json:"question_id"`
	AutoSubmitted bool     `json:"auto_submitted"`
	QuestionType  string   `json:"question_type"`
	Prompt        string   `json:"prompt"`
	Choices       []string `json:"choices,omitempty"`
	CorrectAnswer any      `json:"correct_answer"`
	MatchMode     string   `json:"match_mode,omitempty"`
	Tolerance     float64  `json:"tolerance,omitempty"`
}

func AppendQuestions(questionsDir, fileBaseName string, questions []QuestionRecord) error {
	if len(questions) == 0 {
		return nil
	}
	if err := os.MkdirAll(questionsDir, 0o755); err != nil {
		return fmt.Errorf("eventstore: mkdir questions: %w", err)
	}
	path := filepath.Join(questionsDir, fileBaseName+".jsonl")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("eventstore: open questions file: %w", err)
	}
	defer func() { _ = f.Close() }()

	for _, q := range questions {
		line, err := json.Marshal(q)
		if err != nil {
			return fmt.Errorf("eventstore: marshal question: %w", err)
		}
		if _, err := fmt.Fprintf(f, "%s\n", line); err != nil {
			return err
		}
	}
	return nil
}

// forEachQuestion opens a questions JSONL sidecar and calls fn for each valid
// record; blank lines, malformed lines, and records with no question_id are
// skipped, and a missing file is treated as empty. fn returning true stops the
// scan early.
func forEachQuestion(path string, fn func(QuestionRecord) bool) error {
	f, err := os.Open(path) //nolint:gosec // path comes from a validated config dir + parquet basename
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("eventstore: open questions file: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Raise the line cap to 1 MiB — a question's prompt and choices can exceed
	// bufio's 64 KB default.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var q QuestionRecord
		if err := json.Unmarshal(line, &q); err != nil {
			continue
		}
		if q.QuestionID == "" {
			continue
		}
		if fn(q) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("eventstore: scan questions file: %w", err)
	}
	return nil
}

func LoadQuestions(path string) (map[string]QuestionRecord, error) {
	out := map[string]QuestionRecord{}
	err := forEachQuestion(path, func(q QuestionRecord) bool {
		out[q.QuestionID] = q
		return false
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReadQuestion finds one question by ID across every JSONL sidecar in
// questionsDir, returning nil when no sidecar holds it.
func ReadQuestion(questionsDir, questionID string) (*QuestionRecord, error) {
	files, err := filepath.Glob(filepath.Join(questionsDir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("eventstore: glob questions: %w", err)
	}
	for _, path := range files {
		var found *QuestionRecord
		if err := forEachQuestion(path, func(q QuestionRecord) bool {
			if q.QuestionID == questionID {
				rec := q
				found = &rec
				return true
			}
			return false
		}); err != nil {
			return nil, err
		}
		if found != nil {
			return found, nil
		}
	}
	return nil, nil
}
