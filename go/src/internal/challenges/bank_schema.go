package challenges

import (
	"fmt"
	"maps"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// BankSchema is the JSON Schema for question banks: each question must match
// exactly one type variant (OneOf, discriminated by the "type" const).
func BankSchema() *jsonschema.Schema {
	mcq := variantSchema(MultipleChoice,
		map[string]*jsonschema.Schema{
			"choices": {
				Type:     "array",
				Items:    &jsonschema.Schema{Type: "string", MinLength: new(1)},
				MinItems: new(2),
			},
			"answer": {
				Type:     "array",
				Items:    &jsonschema.Schema{Type: "string", MinLength: new(1)},
				MinItems: new(1),
			},
		},
		[]string{"prompt", "type", "choices", "answer"},
	)

	numeric := variantSchema(Numeric,
		map[string]*jsonschema.Schema{
			"answer":    {Type: "number"},
			"tolerance": {Type: "number", Minimum: new(0.0)},
		},
		[]string{"prompt", "type", "answer"},
	)

	shortText := variantSchema(ShortText,
		map[string]*jsonschema.Schema{
			"answer": {
				Type:     "array",
				Items:    &jsonschema.Schema{Type: "string", MinLength: new(1)},
				MinItems: new(1),
			},
			"match": {Type: "string", Enum: []any{"exact", "substring_ci", "regex"}},
		},
		[]string{"prompt", "type", "answer"},
	)

	return &jsonschema.Schema{
		Schema:      "https://json-schema.org/draft/2020-12/schema",
		Title:       "Presence Tracker question bank",
		Description: "Generated from go/src/internal/challenges — do not edit by hand.",
		Type:        "object",
		Required:    []string{"questions"},
		Properties: map[string]*jsonschema.Schema{
			"questions": {
				Type:     "array",
				MinItems: new(1),
				Items: &jsonschema.Schema{
					OneOf: []*jsonschema.Schema{mcq, numeric, shortText},
				},
			},
		},
		AdditionalProperties: falseSchema(),
	}
}

func variantSchema(typeConst QuestionType, extra map[string]*jsonschema.Schema, required []string) *jsonschema.Schema {
	tc := any(string(typeConst))
	props := map[string]*jsonschema.Schema{
		"prompt": {Type: "string", MinLength: new(1)},
		"type":   {Type: "string", Const: &tc},
	}
	maps.Copy(props, extra)
	return &jsonschema.Schema{
		Type:                 "object",
		Required:             required,
		Properties:           props,
		AdditionalProperties: falseSchema(),
	}
}

// falseSchema is the JSON Schema "false" (not(anything) = matches nothing),
// used as additionalProperties to forbid unknown keys.
func falseSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

func ResolvedBankSchema() (*jsonschema.Resolved, error) {
	resolvedBankOnce.Do(func() {
		resolvedBank, resolvedBankErr = BankSchema().Resolve(nil)
		if resolvedBankErr != nil {
			resolvedBankErr = fmt.Errorf("challenges: resolve bank schema: %w", resolvedBankErr)
		}
	})
	return resolvedBank, resolvedBankErr
}

var (
	resolvedBankOnce sync.Once
	resolvedBank     *jsonschema.Resolved
	resolvedBankErr  error
)
