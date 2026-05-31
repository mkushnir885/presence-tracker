package eventstore

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lestrrat-go/strftime"
)

// DirTemplate is a validated meeting-dir name template. Placeholders
// {start:<strftime>} and {end:<strftime>} are substituted at render time;
// everything else is literal. Build with ParseDirTemplate.
type DirTemplate string

var placeholderRe = regexp.MustCompile(`\{(start|end):([^}]*)\}`)

// Union of Windows (<>:"/\|?*), Linux (/), and macOS (/, :) forbidden
// filename chars, plus all C0 control characters and DEL.
func badNameChar(r rune) bool {
	return r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r)
}

func ParseDirTemplate(s string) (DirTemplate, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("template is empty")
	}

	literals := placeholderRe.ReplaceAllString(s, "")
	if i := strings.IndexFunc(literals, badNameChar); i >= 0 {
		return "", fmt.Errorf("template contains character not allowed in filenames: %q", literals[i])
	}

	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		if _, err := strftime.New(m[2]); err != nil {
			return "", fmt.Errorf("placeholder %s: %w", m[0], err)
		}
	}
	return DirTemplate(s), nil
}

func (t DirTemplate) Render(start, end time.Time) string {
	return placeholderRe.ReplaceAllStringFunc(string(t), func(match string) string {
		m := placeholderRe.FindStringSubmatch(match)
		f, _ := strftime.New(m[2]) // validated at parse time
		when := start
		if m[1] == "end" {
			when = end
		}
		return f.FormatString(when)
	})
}
