package messengers

import (
	_ "embed"
	"log/slog"

	"presence-tracker/src/internal/i18n"
)

//go:embed locales/en.json
var enJSON []byte

//go:embed locales/uk.json
var ukJSON []byte

// SharedCatalog returns a fresh Catalog pre-loaded with the
// messenger-agnostic translations (registration replies, verification
// DM, generic challenge prompts). Adapters call this once at New()
// time and then Add their own messenger-specific JSON on top.
func SharedCatalog() *i18n.Catalog {
	c := i18n.New()
	if err := c.Add("en", enJSON); err != nil {
		slog.Warn("messengers: load en locale", "err", err)
	}
	if err := c.Add("uk", ukJSON); err != nil {
		slog.Warn("messengers: load uk locale", "err", err)
	}
	return c
}
