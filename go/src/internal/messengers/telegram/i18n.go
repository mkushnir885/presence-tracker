package telegram

import (
	_ "embed"
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
	c.Load("telegram", map[string][]byte{"en": enJSON, "uk": ukJSON})
	return c
}

func languageFromCode(code string) string {
	primary, _, _ := strings.Cut(strings.ToLower(code), "-")
	switch primary {
	case "uk":
		return "uk"
	default:
		return "en"
	}
}
