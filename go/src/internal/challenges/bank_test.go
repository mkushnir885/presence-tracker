package challenges

import (
	"strings"
	"testing"
)

const bankMCQ = `
questions:
  - prompt: "What colour is the sky?"
    type: multiple_choice
    choices: ["blue", "red", "green"]
    answer: ["blue"]
`

const bankNumeric = `
questions:
  - prompt: "How many sides does a triangle have?"
    type: numeric
    answer: 3
`

const bankNumericTolerance = `
questions:
  - prompt: "Approximate pi to two decimal places."
    type: numeric
    answer: 3.14159
    tolerance: 0.01
`

const bankShortText = `
questions:
  - prompt: "Name a planet."
    type: short_text
    answer: ["earth", "mars", "venus"]
`

const bankShortTextExact = `
questions:
  - prompt: "Type the exact passphrase."
    type: short_text
    answer: ["OpenSesame"]
    match: "exact"
`

const bankMultiple = `
questions:
  - prompt: "Choose the prime."
    type: multiple_choice
    choices: ["2", "4", "6"]
    answer: ["2"]
  - prompt: "Temperature of absolute zero in Celsius?"
    type: numeric
    answer: -273.15
    tolerance: 0.5
`

const bankWithSchema = `
$schema: ./bank.schema.json
questions:
  - prompt: "1 + 1 = ?"
    type: numeric
    answer: 2
`

func TestParseMCQ(t *testing.T) {
	bank, err := Parse([]byte(bankMCQ))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bank.Questions) != 1 {
		t.Fatalf("want 1 question, got %d", len(bank.Questions))
	}
	q := bank.Questions[0]
	if q.QuestionType != MultipleChoice {
		t.Errorf("type: got %q, want %q", q.QuestionType, MultipleChoice)
	}
	if q.QuestionID == "" {
		t.Error("UUID not assigned")
	}
	choices, _ := q.Answer.([]string)
	if len(choices) != 1 || choices[0] != "blue" {
		t.Errorf("answer: got %v", choices)
	}
	if len(q.Choices) != 3 {
		t.Errorf("choices: got %v", q.Choices)
	}
}

func TestParseNumeric(t *testing.T) {
	bank, err := Parse([]byte(bankNumeric))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	q := bank.Questions[0]
	if q.QuestionType != Numeric {
		t.Errorf("type: got %q", q.QuestionType)
	}
	ans, _ := q.Answer.(float64)
	if ans != 3 {
		t.Errorf("answer: got %v", ans)
	}
	if q.Tolerance != 0 {
		t.Errorf("tolerance: got %v", q.Tolerance)
	}
}

func TestParseNumericTolerance(t *testing.T) {
	bank, err := Parse([]byte(bankNumericTolerance))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	q := bank.Questions[0]
	if q.Tolerance != 0.01 {
		t.Errorf("tolerance: got %v, want 0.01", q.Tolerance)
	}
}

func TestParseShortText(t *testing.T) {
	bank, err := Parse([]byte(bankShortText))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	q := bank.Questions[0]
	if q.QuestionType != ShortText {
		t.Errorf("type: got %q", q.QuestionType)
	}
	// Default match mode when omitted.
	if q.MatchMode != "substring_ci" {
		t.Errorf("match mode: got %q, want substring_ci", q.MatchMode)
	}
}

func TestParseShortTextExplicitMatch(t *testing.T) {
	bank, err := Parse([]byte(bankShortTextExact))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bank.Questions[0].MatchMode != "exact" {
		t.Errorf("match mode: got %q", bank.Questions[0].MatchMode)
	}
}

func TestParseMultipleQuestions(t *testing.T) {
	bank, err := Parse([]byte(bankMultiple))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bank.Questions) != 2 {
		t.Fatalf("want 2 questions, got %d", len(bank.Questions))
	}
	// UUIDs must be distinct.
	if bank.Questions[0].QuestionID == bank.Questions[1].QuestionID {
		t.Error("duplicate question IDs")
	}
}

func TestParseWithSchemaKey(t *testing.T) {
	// $schema key at the top level must not cause a parse error.
	_, err := Parse([]byte(bankWithSchema))
	if err != nil {
		t.Errorf("unexpected error with $schema key: %v", err)
	}
}

func TestParseMCQAnswerNotInChoices(t *testing.T) {
	input := `
questions:
  - prompt: "Pick one."
    type: multiple_choice
    choices: ["A", "B"]
    answer: ["C"]
`
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("expected error for answer not in choices")
	}
	if !strings.Contains(err.Error(), "not in choices") {
		t.Errorf("error message: %q", err.Error())
	}
}

func TestParseMCQDuplicateChoice(t *testing.T) {
	input := `
questions:
  - prompt: "Duplicate choices."
    type: multiple_choice
    choices: ["A", "A", "B"]
    answer: ["A"]
`
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("expected error for duplicate choice")
	}
	if !strings.Contains(err.Error(), "duplicate choice") {
		t.Errorf("error message: %q", err.Error())
	}
}

func TestParseInvalidYAML(t *testing.T) {
	_, err := Parse([]byte(":::not valid yaml:::"))
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestParseEmpty(t *testing.T) {
	_, err := Parse([]byte("questions: []"))
	// An empty bank fails schema validation (minItems or similar) or is valid
	// but has zero questions; either is acceptable — just must not panic.
	_ = err
}

func TestFirstDuplicate(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{[]string{"a", "b", "c"}, ""},
		{[]string{"a", "b", "a"}, "a"},
		{[]string{"x", "x"}, "x"},
		{nil, ""},
		{[]string{}, ""},
	}
	for _, tc := range tests {
		if got := firstDuplicate(tc.in); got != tc.want {
			t.Errorf("firstDuplicate(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
