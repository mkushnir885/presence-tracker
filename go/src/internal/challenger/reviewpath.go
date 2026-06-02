package challenger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

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

func (r *ReviewPath) Write(bank challenges.Bank) (path string, err error) {
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
	questions := make([]map[string]any, 0, len(bank.Questions))
	for _, q := range bank.Questions {
		m := map[string]any{
			"prompt": q.Prompt,
			"type":   string(q.QuestionType),
			"answer": q.Answer,
		}
		if len(q.Choices) > 0 {
			m["choices"] = q.Choices
		}
		if q.Tolerance != 0 {
			m["tolerance"] = q.Tolerance
		}
		if q.MatchMode != "" && q.MatchMode != "substring_ci" {
			m["match"] = q.MatchMode
		}
		questions = append(questions, m)
	}
	out, err := yaml.Marshal(map[string]any{"questions": questions})
	if err != nil {
		return nil, fmt.Errorf("challenger: marshal bank: %w", err)
	}
	return out, nil
}
