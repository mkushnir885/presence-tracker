package messengers

import (
	_ "embed"

	"presence-tracker/src/internal/i18n"
)

//go:embed locales/en.json
var enJSON []byte

//go:embed locales/uk.json
var ukJSON []byte

func SharedCatalog() *i18n.Catalog {
	c := i18n.New()
	c.Load("messengers", map[string][]byte{"en": enJSON, "uk": ukJSON})
	return c
}
