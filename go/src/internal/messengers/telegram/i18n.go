package telegram

import (
	_ "embed"
	"log/slog"
	"strings"

	"presence-tracker/src/internal/i18n"
	"presence-tracker/src/internal/messengers"
)

//go:embed locales/en.json
var enJSON []byte

//go:embed locales/uk.json
var ukJSON []byte

func newCatalog() *i18n.Catalog {
	c := messengers.SharedCatalog()
	if err := c.Add("en", enJSON); err != nil {
		slog.Warn("telegram: load en locale", "err", err)
	}
	if err := c.Add("uk", ukJSON); err != nil {
		slog.Warn("telegram: load uk locale", "err", err)
	}
	return c
}

// languageFromCode reduces Telegram's BCP-47 hint to one of the
// catalog languages by stripping any region subtag. Unknown primary
// tags fall back to English.
func languageFromCode(code string) string {
	primary, _, _ := strings.Cut(strings.ToLower(code), "-")
	switch primary {
	case "uk":
		return "uk"
	default:
		return "en"
	}
}
