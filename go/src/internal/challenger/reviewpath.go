package challenger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/util"
)

const bankExt = ".yaml"

type ReviewPath struct {
	dir      string
	basename string
}

func NewReviewPath(dir, basename string) *ReviewPath {
	return &ReviewPath{dir: dir, basename: basename}
}

func (r *ReviewPath) Dir() string { return r.dir }

func (r *ReviewPath) FilePath() string {
	if r.dir == "" || r.basename == "" {
		return ""
	}
	return filepath.Join(r.dir, r.basename+bankExt)
}

func (r *ReviewPath) Write(bank challenges.Bank) (string, error) {
	final := r.FilePath()
	if final == "" {
		return "", errors.New("challenger: review dir or bank basename not configured")
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return "", fmt.Errorf("challenger: mkdir review_dir: %w", err)
	}

	body, err := marshalBank(bank)
	if err != nil {
		return "", err
	}
	if err := util.AtomicWrite(final, body, 0o644); err != nil {
		return "", fmt.Errorf("challenger: %w", err)
	}
	return final, nil
}

func marshalBank(bank challenges.Bank) ([]byte, error) {
	type rawQuestion struct {
		Prompt    string   `yaml:"prompt"`
		Type      string   `yaml:"type"`
		Choices   []string `yaml:"choices,omitempty"`
		Match     string   `yaml:"match,omitempty"`
		Tolerance float64  `yaml:"tolerance,omitempty"`
		Answer    any      `yaml:"answer"`
	}
	type rawBank struct {
		Questions []rawQuestion `yaml:"questions"`
	}
	rb := rawBank{Questions: make([]rawQuestion, 0, len(bank.Questions))}
	for _, q := range bank.Questions {
		rq := rawQuestion{
			Prompt: q.Prompt,
			Type:   string(q.QuestionType),
		}
		switch q.QuestionType {
		case challenges.MultipleChoice:
			rq.Choices = q.Choices
			rq.Answer = q.Answer
		case challenges.Numeric:
			rq.Answer = q.Answer
			rq.Tolerance = q.Tolerance
		case challenges.ShortText:
			rq.Answer = q.Answer
			if q.MatchMode != "" && q.MatchMode != "substring_ci" {
				rq.Match = q.MatchMode
			}
		}
		rb.Questions = append(rb.Questions, rq)
	}
	out, err := yaml.Marshal(rb)
	if err != nil {
		return nil, fmt.Errorf("challenger: marshal bank: %w", err)
	}
	return out, nil
}
