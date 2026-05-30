package challenger

import (
	"strings"
	"testing"
)

const validYAML = `questions:
  - prompt: "What is 2+2?"
    type: numeric
    answer: 4
  - prompt: "Pick a prime"
    type: multiple_choice
    choices: ["4", "5", "6"]
    answer: ["5"]
`

const validJSON = `{
  "questions": [
    {"prompt": "What is 2+2?", "type": "numeric", "answer": 4}
  ]
}`

func TestParseLLMBankDirect(t *testing.T) {
	bank, err := parseLLMBank(validYAML)
	if err != nil {
		t.Fatal(err)
	}
	if len(bank.Questions) != 2 {
		t.Errorf("got %d questions, want 2", len(bank.Questions))
	}
}

func TestParseLLMBankJSON(t *testing.T) {
	bank, err := parseLLMBank(validJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(bank.Questions) != 1 {
		t.Errorf("got %d questions, want 1", len(bank.Questions))
	}
}

func TestParseLLMBankFenced(t *testing.T) {
	wrapped := "Here is your bank:\n\n```yaml\n" + validYAML + "```\n\nEnjoy!"
	bank, err := parseLLMBank(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if len(bank.Questions) != 2 {
		t.Errorf("got %d, want 2", len(bank.Questions))
	}
}

func TestParseLLMBankSalvage(t *testing.T) {
	// First question is malformed (missing answer); second is valid.
	mixed := `questions:
  - prompt: "Broken"
    type: numeric
  - prompt: "Good"
    type: numeric
    answer: 7
`
	bank, err := parseLLMBank(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(bank.Questions) != 1 {
		t.Fatalf("got %d, want 1 (salvaged)", len(bank.Questions))
	}
	if bank.Questions[0].Prompt != "Good" {
		t.Errorf("kept the wrong question: %q", bank.Questions[0].Prompt)
	}
}

func TestParseLLMBankUnparseable(t *testing.T) {
	if _, err := parseLLMBank("just prose, no yaml here"); err == nil {
		t.Error("expected error for prose-only input")
	}
}

func TestUserPromptCarriesLanguage(t *testing.T) {
	withLang := userPrompt("hello", 3, "uk")
	if !strings.Contains(withLang, `"uk"`) {
		t.Errorf("language tag not embedded in prompt: %q", withLang)
	}
	for _, sentinel := range []string{"", "auto", "AUTO", " auto "} {
		got := userPrompt("hello", 3, sentinel)
		if strings.Contains(got, "BCP-47") {
			t.Errorf("language instruction leaked for sentinel %q: %q", sentinel, got)
		}
	}
}
