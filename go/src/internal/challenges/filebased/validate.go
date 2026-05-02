package filebased

import (
	"errors"
	"fmt"
	"slices"
)

// validate checks a parsed question bank for structural correctness.
// It is called before the bank is used in a poll.
func validate(bank *questionBank) error {
	if bank.Version != 1 {
		return fmt.Errorf("filebased: unsupported bank version %d", bank.Version)
	}
	if len(bank.Questions) == 0 {
		return errors.New("filebased: question bank has no questions")
	}
	var errs []error
	for i, q := range bank.Questions {
		if err := validateQuestion(i, &q); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateQuestion(i int, q *rawQuestion) error {
	prefix := fmt.Sprintf("filebased: question[%d]", i)
	if q.Prompt == "" {
		return fmt.Errorf("%s: prompt is empty", prefix)
	}
	switch q.Type {
	case "multiple_choice":
		if len(q.Choices) < 2 {
			return fmt.Errorf("%s: multiple_choice needs at least 2 choices", prefix)
		}
		if len(q.Answer) == 0 {
			return fmt.Errorf("%s: answer list is empty", prefix)
		}
		answerSet := make(map[string]bool, len(q.Answer))
		for _, a := range q.Answer {
			answerSet[a] = true
		}
		for _, a := range q.Answer {
			found := slices.Contains(q.Choices, a)
			if !found {
				return fmt.Errorf("%s: answer %q not in choices", prefix, a)
			}
			_ = answerSet
		}
	case "numeric":
		// answer is a single number; validation happens during YAML unmarshal
	case "short_text":
		if len(q.Answer) == 0 {
			return fmt.Errorf("%s: short_text answer list is empty", prefix)
		}
		switch q.Match {
		case "", "exact", "substring_ci", "regex":
		default:
			return fmt.Errorf("%s: unknown match mode %q", prefix, q.Match)
		}
	default:
		return fmt.Errorf("%s: unknown question type %q", prefix, q.Type)
	}
	return nil
}
