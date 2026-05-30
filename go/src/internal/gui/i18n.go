package gui

import (
	_ "embed"
	"net/http"
	"sync"

	"presence-tracker/src/internal/gui/views"
	"presence-tracker/src/internal/i18n"
)

//go:embed locales/en.json
var enJSON []byte

//go:embed locales/uk.json
var ukJSON []byte

var (
	catalogOnce sync.Once
	catalog     *i18n.Catalog
)

func sharedCatalog() *i18n.Catalog {
	catalogOnce.Do(func() {
		catalog = i18n.New()
		catalog.Load("gui", map[string][]byte{"en": enJSON, "uk": ukJSON})
	})
	return catalog
}

func localeFromRequest(r *http.Request) views.Locale {
	lang := "en"
	if c, err := r.Cookie("ptrack-lang"); err == nil && c.Value == "uk" {
		lang = "uk"
	}
	return sharedCatalog().Locale(lang)
}
