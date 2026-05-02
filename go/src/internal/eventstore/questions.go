package eventstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// QuestionRecord is one line in the per-meeting JSON Lines file.
// Fields match docs/CHALLENGES.md § "Question record fields".
type QuestionRecord struct {
	QuestionID    string   `json:"question_id"`
	ChallengeType string   `json:"challenge_type"`
	QuestionType  string   `json:"question_type"`
	Prompt        string   `json:"prompt"`
	Choices       []string `json:"choices,omitempty"`
	CorrectAnswer any      `json:"correct_answer"`
	MatchMode     string   `json:"match_mode,omitempty"`
	Tolerance     float64  `json:"tolerance,omitempty"`
	IssuedAt      string   `json:"issued_at"` // ISO-8601 UTC
}

// AppendQuestions appends all question records to the meeting's .jsonl file in
// questionsDir. The file is created if it does not exist.
func AppendQuestions(questionsDir, meetingID string, questions []QuestionRecord) error {
	if len(questions) == 0 {
		return nil
	}
	if err := os.MkdirAll(questionsDir, 0o755); err != nil {
		return fmt.Errorf("eventstore: mkdir questions: %w", err)
	}
	path := filepath.Join(questionsDir, meetingID+".jsonl")

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
