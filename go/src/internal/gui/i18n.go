package gui

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"presence-tracker/src/internal/gui/views"
)

//go:embed locales/en.json
var enJSON []byte

//go:embed locales/uk.json
var ukJSON []byte

func localeFromRequest(r *http.Request) views.Locale {
	lang := "en"
	if c, err := r.Cookie("ptrack-lang"); err == nil && c.Value == "uk" {
		lang = "uk"
	}
	return buildLocale(lang)
}

func buildLocale(lang string) views.Locale {
	data := enJSON
	if lang == "uk" {
		data = ukJSON
	} else {
		lang = "en"
	}
	var t map[string]string
	if err := json.Unmarshal(data, &t); err != nil {
		t = map[string]string{}
	}
	return views.Locale{
		Lang: lang,
		T: func(key string) string {
			if v, ok := t[key]; ok {
				return v
			}
			return key
		},
	}
}
