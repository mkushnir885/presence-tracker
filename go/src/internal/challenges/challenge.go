package challenges

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrNoContext is returned by Generate when the challenge type cannot
// produce questions yet (e.g. the AI model lacks sufficient transcript).
var ErrNoContext = errors.New("challenges: insufficient context for generation")

// ScoreResult is the outcome of evaluating a student's answer.
type ScoreResult string

const (
	ScoreCorrect   ScoreResult = "correct"
	ScoreIncorrect ScoreResult = "incorrect"
)

// Question is the complete record for one challenge: it contains both the
// prompt delivered to the student and the answer key used for scoring.
type Question struct {
	QuestionID   string
	QuestionType string // multiple_choice | numeric | short_text
	Prompt       string
	Choices      []string // populated for multiple_choice
	Answer       any      // []string for multiple_choice/short_text; float64 for numeric
	MatchMode    string   // short_text only: exact | substring_ci | regex
	Tolerance    float64  // numeric only: allowed ± deviation
}

// NewQuestion creates a Question with a time-ordered UUID assigned.
func NewQuestion(questionType, prompt string, choices []string, answer any, matchMode string, tolerance float64) Question {
	return Question{
		QuestionID:   newUUID(),
		QuestionType: questionType,
		Prompt:       prompt,
		Choices:      choices,
		Answer:       answer,
		MatchMode:    matchMode,
		Tolerance:    tolerance,
	}
}

func newUUID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// Answer is a student's submitted response.
type Answer struct {
	Selected   []string // multiple_choice: selected choice labels
	Text       string   // numeric and short_text: raw submitted text
	MessageRef string   // opaque messenger ref for the answer message; empty for MCQ callbacks
}

// ChallengeType generates questions and scores answers for one challenge kind.
type ChallengeType interface {
	Name() string
	Configure(cfg map[string]any) error
	// Generate returns up to count questions. It may return fewer.
	// ErrNoContext signals that the caller should retry later.
	Generate(ctx context.Context, count int) ([]Question, error)
	Score(q Question, submitted Answer) ScoreResult
}
