package challenges

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Question is one entry from a loaded bank. It carries both the prompt
// shown to the student and the answer key used for scoring.
type Question struct {
	QuestionID   string
	QuestionType string // multiple_choice | numeric | short_text
	Prompt       string
	Choices      []string // multiple_choice only
	Answer       any      // []string for multiple_choice/short_text; float64 for numeric
	MatchMode    string   // short_text only: exact | substring_ci | regex
	Tolerance    float64  // numeric only
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
	Version   int           `yaml:"version"`
	Questions []rawQuestion `yaml:"questions"`
}

type rawQuestion struct {
	Prompt    string    `yaml:"prompt"`
	Type      string    `yaml:"type"`
	Choices   []string  `yaml:"choices"`
	Match     string    `yaml:"match"`
	Tolerance float64   `yaml:"tolerance"`
	Answer    yaml.Node `yaml:"answer"`
}

// Load parses and validates a YAML question-bank file. It assigns a fresh
// UUID to every question on every load — the IDs change per poll round.
func Load(path string) (Bank, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Bank{}, fmt.Errorf("challenges: read bank: %w", err)
	}

	var rb rawBank
	if err := yaml.Unmarshal(raw, &rb); err != nil {
		return Bank{}, fmt.Errorf("challenges: parse bank: %w", err)
	}

	if rb.Version != 1 {
		return Bank{}, fmt.Errorf("challenges: unsupported bank version %d", rb.Version)
	}
	if len(rb.Questions) == 0 {
		return Bank{}, errors.New("challenges: question bank has no questions")
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
	if rq.Prompt == "" {
		return Question{}, fmt.Errorf("%s: prompt is empty", prefix)
	}

	q := Question{
		QuestionID:   uuid.Must(uuid.NewV7()).String(),
		QuestionType: rq.Type,
		Prompt:       rq.Prompt,
	}

	switch rq.Type {
	case "multiple_choice":
		if len(rq.Choices) < 2 {
			return Question{}, fmt.Errorf("%s: multiple_choice needs at least 2 choices", prefix)
		}
		var ans []string
		if err := rq.Answer.Decode(&ans); err != nil {
			return Question{}, fmt.Errorf("%s: MCQ answer: %w", prefix, err)
		}
		if len(ans) == 0 {
			return Question{}, fmt.Errorf("%s: answer list is empty", prefix)
		}
		for _, a := range ans {
			if !slices.Contains(rq.Choices, a) {
				return Question{}, fmt.Errorf("%s: answer %q not in choices", prefix, a)
			}
		}
		q.Choices = rq.Choices
		q.Answer = ans

	case "numeric":
		var ans float64
		if err := rq.Answer.Decode(&ans); err != nil {
			return Question{}, fmt.Errorf("%s: numeric answer: %w", prefix, err)
		}
		q.Answer = ans
		q.Tolerance = rq.Tolerance

	case "short_text":
		var ans []string
		if err := rq.Answer.Decode(&ans); err != nil {
			return Question{}, fmt.Errorf("%s: short_text answer: %w", prefix, err)
		}
		if len(ans) == 0 {
			return Question{}, fmt.Errorf("%s: short_text answer list is empty", prefix)
		}
		match := rq.Match
		if match == "" {
			match = "substring_ci"
		}
		switch match {
		case "exact", "substring_ci", "regex":
		default:
			return Question{}, fmt.Errorf("%s: unknown match mode %q", prefix, rq.Match)
		}
		q.Answer = ans
		q.MatchMode = match

	default:
		return Question{}, fmt.Errorf("%s: unknown question type %q", prefix, rq.Type)
	}

	return q, nil
}
