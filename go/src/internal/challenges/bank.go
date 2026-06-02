package challenges

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// QuestionsFile is the JSONL question sidecar inside every meeting directory.
const QuestionsFile = "questions.jsonl"

type QuestionType string

const (
	MultipleChoice QuestionType = "multiple_choice"
	Numeric        QuestionType = "numeric"
	ShortText      QuestionType = "short_text"
)

// Question is one resolved bank entry. Answer's concrete type depends on
// QuestionType: []string for MultipleChoice/ShortText, float64 for Numeric.
//
// JSON tags exist so the persisted form in questions.jsonl is exactly this
// struct; the stats view consumes the same shape over the questions map.
type Question struct {
	QuestionID    string       `json:"question_id"`
	QuestionType  QuestionType `json:"question_type"`
	Prompt        string       `json:"prompt"`
	Choices       []string     `json:"choices,omitempty"`
	Answer        any          `json:"correct_answer"`
	MatchMode     string       `json:"match_mode,omitempty"`
	Tolerance     float64      `json:"tolerance,omitempty"`
	AutoSubmitted bool         `json:"auto_submitted"`
}

type Bank struct {
	Questions []Question
}

type Answer struct {
	Selected   []string
	Text       string
	MessageRef string
}

type ScoreResult string

const (
	ScoreCorrect   ScoreResult = "correct"
	ScoreIncorrect ScoreResult = "incorrect"
)

type rawBank struct {
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

// Parse decodes a YAML bank. It round-trips through JSON so the bank can be
// checked against the shared JSON Schema, then builds typed Questions.
func Parse(raw []byte) (Bank, error) {
	var intermediate any
	if err := yaml.Unmarshal(raw, &intermediate); err != nil {
		return Bank{}, fmt.Errorf("parse: %w", err)
	}
	jsonBytes, err := json.Marshal(intermediate)
	if err != nil {
		return Bank{}, fmt.Errorf("re-encode: %w", err)
	}

	var validateInput any
	if err := json.Unmarshal(jsonBytes, &validateInput); err != nil {
		return Bank{}, fmt.Errorf("decode for validation: %w", err)
	}
	schema, err := ResolvedBankSchema()
	if err != nil {
		return Bank{}, err
	}
	if err := schema.Validate(validateInput); err != nil {
		return Bank{}, err
	}

	var rb rawBank
	if err := json.Unmarshal(jsonBytes, &rb); err != nil {
		return Bank{}, fmt.Errorf("decode: %w", err)
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
		if dup := firstDuplicate(rq.Choices); dup != "" {
			return Question{}, fmt.Errorf("%s: duplicate choice %q", prefix, dup)
		}
		if dup := firstDuplicate(ans); dup != "" {
			return Question{}, fmt.Errorf("%s: duplicate answer %q", prefix, dup)
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

func firstDuplicate(ss []string) string {
	seen := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; ok {
			return s
		}
		seen[s] = struct{}{}
	}
	return ""
}
