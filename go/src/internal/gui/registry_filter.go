package gui

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"presence-tracker/src/internal/gui/views"
	"presence-tracker/src/internal/participants"
)

const localeKeyBadDatetime = "registry.filter.error.bad_datetime"

// Date filters accept a partial timestamp — just a year, year-month, … up to
// the minute. Widest precision is tried last.
var timePrefixLayouts = []struct {
	layout string
	unit   string
}{
	{"2006-01-02 15:04", "minute"},
	{"2006-01-02 15", "hour"},
	{"2006-01-02", "day"},
	{"2006-01", "month"},
	{"2006", "year"},
}

func parseFromPrefix(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range timePrefixLayouts {
		if t, err := time.ParseInLocation(l.layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseToPrefix advances the parsed time by one unit of its matched precision,
// so a partial "to" date (e.g. "2026") becomes an exclusive upper bound that
// still covers the whole period.
func parseToPrefix(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range timePrefixLayouts {
		t, err := time.ParseInLocation(l.layout, s, time.Local)
		if err != nil {
			continue
		}
		switch l.unit {
		case "minute":
			return t.Add(time.Minute), true
		case "hour":
			return t.Add(time.Hour), true
		case "day":
			return t.AddDate(0, 0, 1), true
		case "month":
			return t.AddDate(0, 1, 0), true
		case "year":
			return t.AddDate(1, 0, 0), true
		}
	}
	return time.Time{}, false
}

func readBodyForm(r *http.Request) (url.Values, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(body))
}

type registryRequest struct {
	Filter       views.RegistryFilterInputs
	DisplayNames []string
}

func parseRegistryRequest(r *http.Request) (registryRequest, error) {
	form, err := readBodyForm(r)
	if err != nil {
		return registryRequest{}, err
	}
	var names []string
	for _, n := range form["display_name"] {
		if t := strings.TrimSpace(n); t != "" {
			names = append(names, t)
		}
	}
	return registryRequest{
		Filter: views.RegistryFilterInputs{
			Name:      strings.TrimSpace(form.Get("name")),
			Messenger: strings.TrimSpace(form.Get("messenger")),
			From:      strings.TrimSpace(form.Get("from")),
			To:        strings.TrimSpace(form.Get("to")),
		},
		DisplayNames: names,
	}, nil
}

func validateInputs(in views.RegistryFilterInputs) (participants.Filter, map[string]string) {
	errs := map[string]string{}
	f := participants.Filter{
		DisplayNameContains: in.Name,
		MessengerName:       in.Messenger,
	}
	if in.From != "" {
		t, ok := parseFromPrefix(in.From)
		if !ok {
			errs["from"] = localeKeyBadDatetime
		} else {
			f.RegisteredFrom = t
		}
	}
	if in.To != "" {
		t, ok := parseToPrefix(in.To)
		if !ok {
			errs["to"] = localeKeyBadDatetime
		} else {
			f.RegisteredTo = t
		}
	}
	return f, errs
}
