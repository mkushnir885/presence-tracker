package challenges

import (
	"fmt"
	"regexp"
	"strings"
)

// Score evaluates a submitted answer against a question's answer key.
func Score(q Question, submitted Answer) ScoreResult {
	switch q.QuestionType {
	case MultipleChoice:
		expected, _ := q.Answer.([]string)
		if equalSets(expected, submitted.Selected) {
			return ScoreCorrect
		}
	case Numeric:
		expected, _ := q.Answer.(float64)
		var got float64
		if _, err := fmt.Sscanf(submitted.Text, "%f", &got); err == nil {
			diff := got - expected
			if diff < 0 {
				diff = -diff
			}
			if diff <= q.Tolerance {
				return ScoreCorrect
			}
		}
	case ShortText:
		expected, _ := q.Answer.([]string)
		for _, ans := range expected {
			if matchText(q.MatchMode, submitted.Text, ans) {
				return ScoreCorrect
			}
		}
	}
	return ScoreIncorrect
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
		if m[v] < 0 {
			return false
		}
	}
	return true
}

func matchText(mode, submitted, expected string) bool {
	switch mode {
	case "exact":
		return submitted == expected
	case "regex":
		ok, _ := regexp.MatchString("(?i)"+expected, submitted)
		return ok
	default: // substring_ci
		return strings.Contains(strings.ToLower(submitted), strings.ToLower(expected))
	}
}
