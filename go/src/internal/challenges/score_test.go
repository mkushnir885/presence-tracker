package challenges

import "testing"

func TestScoreMultipleChoice(t *testing.T) {
	q := Question{QuestionType: MultipleChoice, Answer: []string{"A", "B"}}
	tests := []struct {
		name     string
		selected []string
		want     ScoreResult
	}{
		{"exact", []string{"A", "B"}, ScoreCorrect},
		{"order independent", []string{"B", "A"}, ScoreCorrect},
		{"wrong answer", []string{"C"}, ScoreIncorrect},
		{"partial subset", []string{"A"}, ScoreIncorrect},
		{"superset", []string{"A", "B", "C"}, ScoreIncorrect},
		{"empty", []string{}, ScoreIncorrect},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Score(q, Answer{Selected: tc.selected}); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScoreNumeric(t *testing.T) {
	tests := []struct {
		name      string
		expected  float64
		tolerance float64
		text      string
		want      ScoreResult
	}{
		{"exact integer", 42, 0, "42", ScoreCorrect},
		{"exact float", 3.14, 0, "3.14", ScoreCorrect},
		{"within tolerance above", 42, 0.5, "42.5", ScoreCorrect},
		{"within tolerance below", 42, 0.5, "41.5", ScoreCorrect},
		{"just outside above", 42, 0.5, "42.51", ScoreIncorrect},
		{"just outside below", 42, 0.5, "41.49", ScoreIncorrect},
		{"negative expected", -5, 0, "-5", ScoreCorrect},
		{"scientific notation", 1000, 0, "1e3", ScoreCorrect},
		{"non-numeric text", 42, 0, "abc", ScoreIncorrect},
		{"empty string", 42, 0, "", ScoreIncorrect},
		{"zero exact", 0, 0, "0", ScoreCorrect},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := Question{QuestionType: Numeric, Answer: tc.expected, Tolerance: tc.tolerance}
			if got := Score(q, Answer{Text: tc.text}); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScoreShortText(t *testing.T) {
	tests := []struct {
		name      string
		answers   []string
		matchMode string
		submitted string
		want      ScoreResult
	}{
		// substring_ci (empty mode = default)
		{"substring_ci: exact", []string{"earth"}, "", "earth", ScoreCorrect},
		{"substring_ci: case insensitive", []string{"Earth"}, "", "EARTH", ScoreCorrect},
		{"substring_ci: is substring", []string{"earth"}, "", "planet earth!", ScoreCorrect},
		{"substring_ci: no match", []string{"mars"}, "", "earth", ScoreIncorrect},
		{"substring_ci: first of many", []string{"earth", "mars"}, "", "earth", ScoreCorrect},
		{"substring_ci: second of many", []string{"earth", "mars"}, "", "mars", ScoreCorrect},
		// explicit substring_ci
		{"substring_ci explicit", []string{"cat"}, "substring_ci", "catfish", ScoreCorrect},
		// exact
		{"exact: match", []string{"Earth"}, "exact", "Earth", ScoreCorrect},
		{"exact: case mismatch", []string{"Earth"}, "exact", "earth", ScoreIncorrect},
		{"exact: substring rejected", []string{"Earth"}, "exact", "planet Earth", ScoreIncorrect},
		// regex
		{"regex: case-insensitive via (?i)", []string{"earth"}, "regex", "EARTH", ScoreCorrect},
		{"regex: alternation", []string{"(earth|terra)"}, "regex", "terra", ScoreCorrect},
		{"regex: anchored no match", []string{"^Mars$"}, "regex", "Mars is red", ScoreIncorrect},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := Question{
				QuestionType: ShortText,
				Answer:       tc.answers,
				MatchMode:    tc.matchMode,
			}
			if got := Score(q, Answer{Text: tc.submitted}); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEqualSets(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{[]string{"x"}, []string{"x"}, true},
		{[]string{"x", "y"}, []string{"y", "x"}, true},
		{[]string{"x"}, []string{"y"}, false},
		{[]string{"x"}, []string{"x", "x"}, false},
		{nil, nil, true},
		{nil, []string{}, true},
	}
	for _, tc := range tests {
		if got := equalSets(tc.a, tc.b); got != tc.want {
			t.Errorf("equalSets(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
