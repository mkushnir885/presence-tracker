package challenger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"presence-tracker/src/internal/challenges"
)

const autoFilePrefix = "auto-"
const autoFileExt = ".yaml"

type Entry struct {
	Path    string
	ModTime time.Time
}

type ReviewDir struct {
	dir string
}

func NewReviewDir(dir string) *ReviewDir {
	return &ReviewDir{dir: dir}
}

func (r *ReviewDir) Dir() string { return r.dir }

// Write saves a generated bank atomically (temp file + rename), then sweeps
// every older auto-*.yaml so only the latest pending bank remains for review.
func (r *ReviewDir) Write(bank challenges.Bank) (string, error) {
	if r.dir == "" {
		return "", errors.New("challenger: review dir not configured")
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return "", fmt.Errorf("challenger: mkdir review_dir: %w", err)
	}

	body, err := marshalBank(bank)
	if err != nil {
		return "", err
	}

	name := autoFilePrefix + time.Now().UTC().Format("20060102T150405Z") + autoFileExt
	final := filepath.Join(r.dir, name)
	tmp, err := os.CreateTemp(r.dir, "."+name+".*")
	if err != nil {
		return "", fmt.Errorf("challenger: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if _, err := os.Stat(tmpName); err == nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("challenger: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("challenger: close temp: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("challenger: rename: %w", err)
	}

	r.sweepExcept(final)
	return final, nil
}

func (r *ReviewDir) List() ([]Entry, error) {
	if r.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("challenger: read review_dir: %w", err)
	}
	var out []Entry
	for _, e := range entries {
		if e.IsDir() || !isAutoFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Path:    filepath.Join(r.dir, e.Name()),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

func (r *ReviewDir) Sweep() error {
	if r.dir == "" {
		return nil
	}
	entries, err := r.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.Remove(e.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *ReviewDir) sweepExcept(keep string) {
	entries, err := r.List()
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Path == keep {
			continue
		}
		_ = os.Remove(e.Path)
	}
}

func isAutoFile(name string) bool {
	return strings.HasPrefix(name, autoFilePrefix) && strings.HasSuffix(name, autoFileExt)
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
