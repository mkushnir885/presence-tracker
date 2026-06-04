package challenger

import (
	"fmt"
	"strings"
)

const systemPromptHeader = `You generate short quiz banks for an attendance tool.
Given a transcript of part of a lesson and a target question count N, return a YAML question bank that tests understanding of what was said.

Output a YAML document only. No prose, no Markdown fences, no commentary.

Schema (by example):

` + "```yaml\n" + `questions:
  - prompt: "Which of the following is a prime number?"
    type: multiple_choice
    choices: ["21", "23", "27", "51"]
    answer: ["23"]
  - prompt: "Which of these are even numbers? Select all that apply."
    type: multiple_choice
    choices: ["2", "3", "4", "5", "6"]
    answer: ["2", "4", "6"]
  - prompt: "What is 7 factorial?"
    type: numeric
    answer: 5040
  - prompt: "Name one property of an isosceles triangle."
    type: short_text
    answer: ["two equal sides", "two equal angles"]
    match: substring_ci
` + "```"

var baseRules = []string{
	`"type" is one of "multiple_choice", "numeric", "short_text".`,
	`multiple_choice: "choices" is a 2..6 element list of distinct short strings; "answer" is a list of one or more entries, each present verbatim in "choices".`,
	`numeric: "answer" is a plain number (integer or decimal). Optional "tolerance" defaults to 0.`,
	`short_text: "answer" is a list of acceptable strings. Optional "match" is "exact", "substring_ci" (default), or "regex".`,
	`Keep prompts under 200 characters.`,
	`Produce exactly the number of questions requested.`,
}

func buildSystemPrompt(extra []string) string {
	var b strings.Builder
	b.WriteString(systemPromptHeader)
	b.WriteString("\n\nRules:")
	for _, r := range baseRules {
		b.WriteString("\n- ")
		b.WriteString(r)
	}
	for _, r := range extra {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		b.WriteString("\n- ")
		b.WriteString(r)
	}
	return b.String()
}

func userPrompt(transcript string, n int, language string) string {
	langLine := ""
	if normalized := strings.ToLower(strings.TrimSpace(language)); normalized != "" && normalized != "auto" {
		langLine = fmt.Sprintf("\nWrite every prompt, choice, and answer in language %q (BCP-47).\n", language)
	}
	return fmt.Sprintf(`Produce %d question(s) based on this transcript.%s
Transcript:
<<<
%s
>>>

Return only the YAML bank.`, n, langLine, transcript)
}
