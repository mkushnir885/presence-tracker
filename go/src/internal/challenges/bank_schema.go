package challenges

import (
	"fmt"
	"maps"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// BankSchema returns a fresh JSON Schema describing a question-bank file.
// The schema is hand-built (rather than inferred from rawBank) because
// the answer field is polymorphic per question type — multiple_choice and
// short_text take an array of strings, numeric takes a single number.
//
// Each variant uses the type field as a discriminator (const value);
// oneOf selects the matching variant per question.
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

// variantSchema builds one question-type variant: prompt + type
// discriminator + the type-specific properties.
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

// falseSchema returns the schema {"not": {}} — equivalent to JSON Schema's
// "additionalProperties: false" but expressed via the *Schema field type.
func falseSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

// ResolvedBankSchema returns a cached *jsonschema.Resolved built from
// BankSchema. The same Resolved is safe for concurrent use across Load
// calls.
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
