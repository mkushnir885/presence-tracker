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
		out[q.QuestionID] = q
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("eventstore: scan questions file: %w", err)
	}
	return out, nil
}
