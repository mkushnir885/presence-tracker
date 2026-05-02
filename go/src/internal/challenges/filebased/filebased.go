package filebased

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"presence-tracker/src/internal/challenges"

	"gopkg.in/yaml.v3"
)

// questionBank is the top-level structure of a YAML question-bank file.
type questionBank struct {
	Version   int           `yaml:"version"`
	Questions []rawQuestion `yaml:"questions"`
}

// rawQuestion mirrors the YAML format defined in docs/CHALLENGES.md.
type rawQuestion struct {
	Prompt  string   `yaml:"prompt"`
	Type    string   `yaml:"type"`
	Choices []string `yaml:"choices"`
	Answer  []string `yaml:"answer"`
	Match   string   `yaml:"match"`
}

// ChallengeType implements [challenges.ChallengeType] for file-based question banks.
type ChallengeType struct {
	questions []challenges.Question
}

var _ challenges.ChallengeType = (*ChallengeType)(nil)

func (c *ChallengeType) Name() string { return "filebased" }

func (c *ChallengeType) Configure(_ map[string]any) error { return nil }

// LoadBank parses a YAML question-bank file and prepares the ChallengeType.
// The loaded bank replaces any previously loaded bank.
func (c *ChallengeType) LoadBank(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("filebased: read bank: %w", err)
	}

	// Two-pass decode: first as a generic map to handle numeric answers.
	var genericBank struct {
		Version   int `yaml:"version"`
		Questions []struct {
			Prompt    string    `yaml:"prompt"`
			Type      string    `yaml:"type"`
			Choices   []string  `yaml:"choices"`
			Match     string    `yaml:"match"`
			Tolerance float64   `yaml:"tolerance"`
			Answer    yaml.Node `yaml:"answer"`
		} `yaml:"questions"`
	}
	if err := yaml.Unmarshal(raw, &genericBank); err != nil {
		return fmt.Errorf("filebased: parse bank: %w", err)
	}

	bank := questionBank{Version: genericBank.Version}
	for _, q := range genericBank.Questions {
		bank.Questions = append(bank.Questions, rawQuestion{
			Prompt:  q.Prompt,
			Type:    q.Type,
			Choices: q.Choices,
			Match:   q.Match,
		})
	}

	if err := validate(&bank); err != nil {
		return err
	}

	var loaded []challenges.Question

	for i, q := range genericBank.Questions {
		switch q.Type {
		case "multiple_choice":
			var ans []string
			if err := q.Answer.Decode(&ans); err != nil {
				return fmt.Errorf("filebased: question[%d] MCQ answer: %w", i, err)
			}
			loaded = append(loaded, challenges.NewQuestion(
				"multiple_choice", q.Prompt, q.Choices, ans, "", 0,
			))
		case "numeric":
			var ans float64
			if err := q.Answer.Decode(&ans); err != nil {
				return fmt.Errorf("filebased: question[%d] numeric answer: %w", i, err)
			}
			loaded = append(loaded, challenges.NewQuestion(
				"numeric", q.Prompt, nil, ans, "", q.Tolerance,
			))
		case "short_text":
			var ans []string
			if err := q.Answer.Decode(&ans); err != nil {
				return fmt.Errorf("filebased: question[%d] short_text answer: %w", i, err)
			}
			match := q.Match
			if match == "" {
				match = "substring_ci"
			}
			loaded = append(loaded, challenges.NewQuestion(
				"short_text", q.Prompt, nil, ans, match, 0,
			))
		}
	}

	c.questions = loaded
	return nil
}

// Generate returns up to count questions from the loaded bank, cycling if necessary.
// Each call assigns fresh UUIDs via NewQuestion, so question IDs change across polls.
func (c *ChallengeType) Generate(_ context.Context, count int) ([]challenges.Question, error) {
	if len(c.questions) == 0 {
		return nil, challenges.ErrNoContext
	}
	out := make([]challenges.Question, count)
	for i := range count {
		src := c.questions[i%len(c.questions)]
		// Re-assign a fresh UUID for each generation so each poll round has distinct IDs.
		out[i] = challenges.NewQuestion(
			src.QuestionType, src.Prompt, src.Choices, src.Answer, src.MatchMode, src.Tolerance,
		)
	}
	return out, nil
}

// Score evaluates a submitted answer against a question's answer key.
func (c *ChallengeType) Score(q challenges.Question, submitted challenges.Answer) challenges.ScoreResult {
	switch q.QuestionType {
	case "multiple_choice":
		expected, _ := q.Answer.([]string)
		if equalSets(expected, submitted.Selected) {
			return challenges.ScoreCorrect
		}
	case "numeric":
		expected, _ := q.Answer.(float64)
		var got float64
		if _, err := fmt.Sscanf(submitted.Text, "%f", &got); err == nil {
			diff := got - expected
			if diff < 0 {
				diff = -diff
			}
			if diff <= q.Tolerance {
				return challenges.ScoreCorrect
			}
		}
	case "short_text":
		expected, _ := q.Answer.([]string)
		for _, ans := range expected {
			if matchText(q.MatchMode, submitted.Text, ans) {
				return challenges.ScoreCorrect
			}
		}
	}
	return challenges.ScoreIncorrect
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
		if m[v] < 0 {
			return false
		}
	}
	return true
}

func matchText(mode, submitted, expected string) bool {
	switch mode {
	case "exact":
		return submitted == expected
	case "regex":
		ok, _ := regexp.MatchString("(?i)"+expected, submitted)
		return ok
	default: // substring_ci
		return strings.Contains(strings.ToLower(submitted), strings.ToLower(expected))
	}
}
