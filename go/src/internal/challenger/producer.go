package challenger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"presence-tracker/src/internal/challenges"
)

type Producer struct {
	llm        *LLMClient
	language   string
	extraRules []string
}

func NewProducer(llm *LLMClient, language string, extraRules []string) *Producer {
	return &Producer{llm: llm, language: language, extraRules: extraRules}
}

func (p *Producer) Generate(ctx context.Context, transcript string, n int) (challenges.Bank, error) {
	raw, err := p.llm.Complete(ctx, buildSystemPrompt(p.extraRules), userPrompt(transcript, n, p.language))
	if err != nil {
		return challenges.Bank{}, err
	}
	return parseLLMBank(raw)
}

// parseLLMBank is forgiving about messy model output: it tries the whole
// response, then any fenced ```code``` block, and as a last resort salvages
// the individually valid questions.
func parseLLMBank(raw string) (challenges.Bank, error) {
	candidates := []string{strings.TrimSpace(raw)}
	if fenced := extractFenced(raw); fenced != "" {
		candidates = append(candidates, fenced)
	}

	var lastErr error
	for _, c := range candidates {
		if c == "" {
			continue
		}
		bank, err := challenges.Parse([]byte(c))
		if err == nil {
			return bank, nil
		}
		lastErr = err
		if bank, ok := salvagePerQuestion(c); ok {
			return bank, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no parseable YAML in LLM response")
	}
	return challenges.Bank{}, fmt.Errorf("challenger: producer: %w", lastErr)
}

var fenceRE = regexp.MustCompile("(?s)```(?:[a-zA-Z0-9_+-]*)\\s*\\n(.*?)```")

func extractFenced(s string) string {
	m := fenceRE.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// salvagePerQuestion validates each question on its own and keeps those that
// pass, so a single malformed entry doesn't discard the whole batch.
func salvagePerQuestion(raw string) (challenges.Bank, bool) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return challenges.Bank{}, false
	}
	qsRaw, ok := doc["questions"].([]any)
	if !ok || len(qsRaw) == 0 {
		return challenges.Bank{}, false
	}

	out := challenges.Bank{Questions: make([]challenges.Question, 0, len(qsRaw))}
	for _, q := range qsRaw {
		single := map[string]any{"questions": []any{q}}
		body, err := json.Marshal(single)
		if err != nil {
			continue
		}
		bank, err := challenges.Parse(body)
		if err != nil || len(bank.Questions) == 0 {
			continue
		}
		out.Questions = append(out.Questions, bank.Questions[0])
	}
	if len(out.Questions) == 0 {
		return challenges.Bank{}, false
	}
	return out, true
}
