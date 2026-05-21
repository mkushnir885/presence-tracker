package challenges

import (
	"slices"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// QuestionType identifies one of the supported question shapes. Values
// double as the on-the-wire discriminator stored in question .jsonl files
// and matched by the bank schema's oneOf variants — do not rename
// without updating the schema (bank_schema.go) and the .jsonl writers.
type QuestionType string

const (
	MultipleChoice QuestionType = "multiple_choice"
	Numeric        QuestionType = "numeric"
	ShortText      QuestionType = "short_text"
)

// Question is one entry from a loaded bank. It carries both the prompt
// shown to the student and the answer key used for scoring.
type Question struct {
	QuestionID   string
	QuestionType QuestionType
	Prompt       string
	Choices      []string // MultipleChoice only
	Answer       any      // []string for MultipleChoice/ShortText; float64 for Numeric
	MatchMode    string   // ShortText only: exact | substring_ci | regex
	Tolerance    float64  // Numeric only
}

// Bank is a parsed and validated question-bank file.
type Bank struct {
	Questions []Question
}

// Answer is a student's submitted response.
type Answer struct {
	Selected   []string // multiple_choice
	Text       string   // numeric / short_text
	MessageRef string   // opaque messenger ref for the answer message; empty for MCQ callbacks
}

// ScoreResult is the outcome of evaluating one answer against one question.
type ScoreResult string

const (
	ScoreCorrect   ScoreResult = "correct"
	ScoreIncorrect ScoreResult = "incorrect"
)

type rawBank struct {
	Version   int           `json:"version"`
	Questions []rawQuestion `json:"questions"`
}

type rawQuestion struct {
	Prompt    string          `json:"prompt"`
	Type      string          `json:"type"`
	Choices   []string        `json:"choices,omitempty"`
	Match     string          `json:"match,omitempty"`
	Tolerance float64         `json:"tolerance,omitempty"`
	Answer    json.RawMessage `json:"answer"`
}

// Load parses and validates a question-bank file (YAML or JSON; JSON is
// parsed transparently because JSON is a subset of YAML). The file is
// validated against the embedded bank schema before any per-question
// conversion, so the per-question code only has to handle the
// type-specific answer decoding.
//
// Every question gets a fresh UUID on load — IDs change per poll round.
func Load(path string) (Bank, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Bank{}, fmt.Errorf("challenges: read bank: %w", err)
	}

	var intermediate any
	if err := yaml.Unmarshal(raw, &intermediate); err != nil {
		return Bank{}, fmt.Errorf("challenges: parse %s: %w", path, err)
	}
	jsonBytes, err := json.Marshal(intermediate)
	if err != nil {
		return Bank{}, fmt.Errorf("challenges: re-encode %s: %w", path, err)
	}

	var validateInput any
	if err := json.Unmarshal(jsonBytes, &validateInput); err != nil {
		return Bank{}, fmt.Errorf("challenges: decode for validation: %w", err)
	}
	schema, err := ResolvedBankSchema()
	if err != nil {
		return Bank{}, err
	}
	if err := schema.Validate(validateInput); err != nil {
		return Bank{}, fmt.Errorf("challenges: %s: %w", path, err)
	}

	var rb rawBank
	if err := json.Unmarshal(jsonBytes, &rb); err != nil {
		return Bank{}, fmt.Errorf("challenges: decode %s: %w", path, err)
	}

	bank := Bank{Questions: make([]Question, 0, len(rb.Questions))}
	var errs []error
	for i, rq := range rb.Questions {
		q, err := buildQuestion(i, &rq)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		bank.Questions = append(bank.Questions, q)
	}
	if len(errs) > 0 {
		return Bank{}, errors.Join(errs...)
	}
	return bank, nil
}

// buildQuestion converts a schema-validated raw question into a Question.
// Structural concerns (prompt presence, MCQ choices count, valid match
// mode, valid type) are already enforced by the schema; this only decodes
// the polymorphic answer field per type and cross-validates that MCQ
// answers reference real choices.
func buildQuestion(i int, rq *rawQuestion) (Question, error) {
	prefix := fmt.Sprintf("challenges: question[%d]", i)
	q := Question{
		QuestionID:   uuid.Must(uuid.NewV7()).String(),
		QuestionType: QuestionType(rq.Type),
		Prompt:       rq.Prompt,
	}

	switch q.QuestionType {
	case MultipleChoice:
		var ans []string
		if err := json.Unmarshal(rq.Answer, &ans); err != nil {
			return Question{}, fmt.Errorf("%s: MCQ answer: %w", prefix, err)
		}
		for _, a := range ans {
			found := slices.Contains(rq.Choices, a)
			if !found {
				return Question{}, fmt.Errorf("%s: answer %q not in choices", prefix, a)
			}
		}
		q.Choices = rq.Choices
		q.Answer = ans

	case Numeric:
		var ans float64
		if err := json.Unmarshal(rq.Answer, &ans); err != nil {
			return Question{}, fmt.Errorf("%s: numeric answer: %w", prefix, err)
		}
		q.Answer = ans
		q.Tolerance = rq.Tolerance

	case ShortText:
		var ans []string
		if err := json.Unmarshal(rq.Answer, &ans); err != nil {
			return Question{}, fmt.Errorf("%s: short_text answer: %w", prefix, err)
		}
		match := rq.Match
		if match == "" {
			match = "substring_ci"
		}
		q.Answer = ans
		q.MatchMode = match
	}

	return q, nil
}
