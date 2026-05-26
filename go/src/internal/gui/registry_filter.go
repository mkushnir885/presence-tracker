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

// localeKeyBadDatetime is the translation key for the inline error
// shown under a From/To input the user typed something we cannot parse
// as a date/time prefix.
const localeKeyBadDatetime = "registry.filter.error.bad_datetime"

// timePrefixLayouts lists the accepted "from"/"to" inputs in order of
// most → least specific. Each layout pairs the strptime-style template
// with the time unit the user supplied; the unit determines how the
// "to" bound is rolled forward to the end of the matched span.
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

// parseFromPrefix interprets s as a date/time prefix and returns the
// inclusive lower bound of the matched span. Returns false for blank
// or unparseable input.
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

// parseToPrefix interprets s as a date/time prefix and returns the
// exclusive upper bound: one unit past the matched span (so "2026-05"
// yields 2026-06-01 00:00:00 local). Returns false for blank or
// unparseable input.
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

// readBodyForm parses the request body as application/x-www-form-urlencoded.
// Unlike http.Request.ParseForm, this works for methods like DELETE
// that http.Request.ParseForm intentionally ignores.
func readBodyForm(r *http.Request) (url.Values, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(body))
}

// registryRequest is the parsed body of a POST /registry/filter or
// POST /registry/delete request. DisplayNames carries an explicit list
// of deletion targets (set by the per-row icon button and by the
// header's bulk-selection button); when non-empty the delete handler
// ignores Filter and removes exactly those entries. The filter and
// delete endpoints share this shape so a single helper parses the
// body once.
type registryRequest struct {
	Filter       views.RegistryFilterInputs
	DisplayNames []string
}

// parseRegistryRequest reads the request body once and pulls every
// known field. Trims whitespace; never returns nil values.
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

// validateInputs converts the user-typed inputs into a participants.Filter
// and returns a per-field error map for any input that failed to parse.
// Error map keys are form field names ("from", "to"); values are locale
// keys the template translates.
//
// On error the returned Filter still contains every successfully-parsed
// field — but callers should refuse to act on a partially-valid filter
// (especially for destructive operations) when len(errors) > 0.
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
