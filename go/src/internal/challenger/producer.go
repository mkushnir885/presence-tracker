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

// Producer turns an LLM chat completion into a challenges.Bank. It is
// tolerant of the model's output format: JSON or YAML, with or without
// Markdown fences, with or without prose around the bank.
type Producer struct {
	llm *LLMClient
}

// NewProducer builds a Producer around an LLM client.
func NewProducer(llm *LLMClient) *Producer {
	return &Producer{llm: llm}
}

// Generate calls the LLM with the configured prompts and returns a
// validated bank. Invalid individual questions are dropped silently; if
// every question is invalid the returned bank has zero questions and
// the caller treats it as a failure.
func (p *Producer) Generate(ctx context.Context, transcript string, n int) (challenges.Bank, error) {
	raw, err := p.llm.Complete(ctx, systemPrompt, userPrompt(transcript, n))
	if err != nil {
		return challenges.Bank{}, err
	}
	return parseLLMBank(raw)
}

// parseLLMBank applies several tolerant extraction strategies to the
// model's response in turn:
//
//  1. Parse the raw text as YAML or JSON directly.
//  2. Extract the first fenced ```yaml / ```json / ``` block and parse it.
//  3. Walk the parsed structure question-by-question, validating each
//     individually and keeping only the ones that pass.
//
// (1) and (2) cover well-behaved output; (3) salvages mostly-good output
// where one or two questions are malformed.
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

// fenceRE matches a Markdown fenced code block. The first capture is
// the body. Optional language tag is ignored.
var fenceRE = regexp.MustCompile("(?s)```(?:[a-zA-Z0-9_+-]*)\\s*\\n(.*?)```")

func extractFenced(s string) string {
	m := fenceRE.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// salvagePerQuestion walks a YAML/JSON document, validating each entry
// of questions[] in isolation by wrapping it in a minimal one-question
// bank. Questions that fail validation are dropped silently; the
// surviving ones make up the returned bank.
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
		single := map[string]any{"version": 1, "questions": []any{q}}
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
