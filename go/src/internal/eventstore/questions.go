package eventstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// QuestionRecord is one line in the per-meeting JSON Lines file.
// Fields match docs/CHALLENGES.md § "Question record fields".
type QuestionRecord struct {
	QuestionID    string   `json:"question_id"`
	AutoSubmitted bool     `json:"auto_submitted"`
	QuestionType  string   `json:"question_type"`
	Prompt        string   `json:"prompt"`
	Choices       []string `json:"choices,omitempty"`
	CorrectAnswer any      `json:"correct_answer"`
	MatchMode     string   `json:"match_mode,omitempty"`
	Tolerance     float64  `json:"tolerance,omitempty"`
	IssuedAt      string   `json:"issued_at"` // ISO-8601 UTC
}

// AppendQuestions appends all question records to the meeting's .jsonl file in
// questionsDir, named after the meeting's Parquet basename so the GUI
// stats loader can pair them by file. The file is created if it does
// not exist.
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

// LoadQuestions reads every record from a JSONL file into a map keyed
// by question_id. A missing file is not an error — it returns an empty
// map so callers can use the result unconditionally. Malformed lines
// are skipped (matching the read tolerance of scanJSONL).
func LoadQuestions(path string) (map[string]QuestionRecord, error) {
	out := map[string]QuestionRecord{}
	f, err := os.Open(path) //nolint:gosec // path comes from a validated config dir + parquet basename
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("eventstore: open questions file: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
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
		out[q.QuestionID] = q
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("eventstore: scan questions file: %w", err)
	}
	return out, nil
}
