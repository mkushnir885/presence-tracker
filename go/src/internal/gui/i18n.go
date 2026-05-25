package gui

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"presence-tracker/src/internal/gui/views"
)

//go:embed locales/us.json
var usJSON []byte

//go:embed locales/ua.json
var uaJSON []byte

func localeFromRequest(r *http.Request) views.Locale {
	lang := "us"
	if c, err := r.Cookie("ptrack-lang"); err == nil && c.Value == "ua" {
		lang = "ua"
	}
	return buildLocale(lang)
}

func buildLocale(lang string) views.Locale {
	data := usJSON
	if lang == "ua" {
		data = uaJSON
	} else {
		lang = "us"
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
