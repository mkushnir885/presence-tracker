package challenger

import (
	"fmt"
	"strings"
)

// This file is the documented public contract of the auto-generator's
// LLM interface. Treat changes as breaking — the prompt shape is the
// only specification the model has of the bank YAML format.
//
// Output-format strategy: prose-and-examples in the prompt only. No
// response_format field on the request, no embedded JSON Schema. Small
// local models (Qwen 2.5 3B class) follow worked examples more reliably
// than abstract schemas, and `response_format: json_schema` is not
// portably supported across the backends ptrack must work with.
//
// Producer.Generate tolerates JSON or YAML, with or without
// surrounding prose / Markdown fences. challenges.Validate is the
// single source of truth for what counts as valid — bad questions are
// dropped silently.

const systemPromptHeader = `You generate short quiz banks for an attendance tool.
Given a transcript of part of a lesson and a target question count N, return a YAML question bank that tests understanding of what was said.

Output a YAML document only. No prose, no Markdown fences, no commentary.

Schema (by example):

` + "```yaml\n" + `version: 1
questions:
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

// baseRules are the non-negotiable instructions enforcing the schema
// contract parseLLMBank depends on. They are always emitted; user
// extra_rules are appended after them so a teacher can steer style or
// topic focus but cannot break the format.
var baseRules = []string{
	`"version" is always 1.`,
	`"type" is one of "multiple_choice", "numeric", "short_text".`,
	`multiple_choice: "choices" is a 2..6 element list of distinct short strings; "answer" is a list of one or more entries, each present verbatim in "choices".`,
	`numeric: "answer" is a plain number (integer or decimal). Optional "tolerance" defaults to 0.`,
	`short_text: "answer" is a list of acceptable strings. Optional "match" is "exact", "substring_ci" (default), or "regex".`,
	`Questions must be answerable purely from the transcript. Do not invent facts.`,
	`Keep prompts under 200 characters.`,
	`Avoid yes/no questions and trivia unrelated to the transcript topic.`,
	`Produce exactly the number of questions requested.`,
}

// buildSystemPrompt joins the immutable header, the base rules, and any
// teacher-supplied extra rules into a single system message. Empty or
// whitespace-only extras are skipped so a stray blank row in the GUI does
// not pollute the prompt.
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

// userPrompt builds the per-call instruction. The transcript is wrapped
// in fenced markers so the model does not confuse it with the schema
// example above. When language is a concrete BCP-47 / ISO 639-1 tag
// (e.g. "en", "uk") it is injected as a hard constraint on the output
// language; the empty string and the "auto" sentinel both opt out and
// let the model match the transcript.
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
