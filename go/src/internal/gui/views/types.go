package views

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/session"
	"presence-tracker/src/internal/stats"
)

// RegistryFilterInputs holds the raw, user-typed values from the
// registry page's filter form. All fields are blank when the user has
// not narrowed the list. From / To accept a date/time prefix (year,
// year-month, year-month-day, with optional hour, minute, or second);
// the prefix is parsed into a time.Time bound by the gui handler before
// being handed to the registry.
type RegistryFilterInputs struct {
	Name      string
	Messenger string
	From      string
	To        string
}

// Active reports whether any filter field is set.
func (f RegistryFilterInputs) Active() bool {
	return f.Name != "" || f.Messenger != "" || f.From != "" || f.To != ""
}

// RegistryData is the data model for the registry page.
type RegistryData struct {
	Entries    []participants.RegistryEntry
	Messengers []string
	Filter     RegistryFilterInputs
	// HasAny is true when the registry contains any entries at all,
	// regardless of the current filter — used to decide whether to show
	// the filter form vs. the global empty-state hint.
	HasAny bool
}

// RegistryFilterErrors maps a form field name to a translation key for
// the error to render in the results container. An empty map means
// every input was acceptable.
type RegistryFilterErrors map[string]string

// registryFilterFieldOrder is the order in which invalid-filter
// messages are rendered. Go map iteration is unordered; this keeps
// repeated renders stable so a refresh does not reshuffle the lines.
var registryFilterFieldOrder = []string{"name", "messenger", "from", "to"}

// Ordered yields (fieldKey, errorKey) pairs in a fixed order, skipping
// fields that have no error.
func (e RegistryFilterErrors) Ordered() []struct{ Field, Key string } {
	out := make([]struct{ Field, Key string }, 0, len(e))
	for _, f := range registryFilterFieldOrder {
		if k, ok := e[f]; ok {
			out = append(out, struct{ Field, Key string }{f, k})
		}
	}
	return out
}

// RegistryFilterErrorMessage builds the localized sentence shown in
// the results card for one invalid filter input. The shape is
// "Error in filter "<label>": <detail>" — labels and the connector
// template both come from the locale catalogue so word order can
// change per language.
func RegistryFilterErrorMessage(fieldKey, errorKey string, locale Locale) string {
	return fmt.Sprintf(
		locale.T("registry.filter.error.template"),
		locale.T("registry.filter."+fieldKey),
		locale.T(errorKey),
	)
}

// RegistryExactDeleteVals encodes the hx-vals payload that the per-row
// delete icon sends. It posts only exact_name so the handler removes
// that single entry without consulting the filter form.
func RegistryExactDeleteVals(displayName string) string {
	b, _ := json.Marshal(map[string]string{"exact_name": displayName}) //nolint:errchkjson // a single string field cannot fail to marshal
	return string(b)
}

// Locale carries the active language and a translation lookup function.
type Locale struct {
	Lang string
	T    func(key string) string
}

// DashboardData is the data model for the dashboard page.
type DashboardData struct {
	Meetings         []MeetingFile
	EnabledProviders []ProviderOption
	ActiveSession    bool
	ActiveMeetingID  string
	SortField        string // "name", "modified", "size"
	SortOrder        string // "asc" or "desc"
}

// ProviderOption is one option in the Connect form's provider dropdown.
// Name is the value submitted to /session; Label is the human-readable
// display name (e.g. "BigBlueButton").
type ProviderOption struct {
	Name  string
	Label string
}

// MeetingFile represents one Parquet file in the meetings directory.
type MeetingFile struct {
	ID      string // filename without .parquet
	ModTime time.Time
	SizeKB  int64
}

// StatusData is the data model for the live status page.
type StatusData struct {
	MeetingID    string
	ProviderName string
	StartedAt    time.Time
	Present      []session.PresenceStatus
	Unregistered []session.UnregisteredStatus
	LogEntries   []LogEntry
}

// LogEntry mirrors gui.LogEntry for use in templates (views does not import gui).
type LogEntry struct {
	Time    time.Time
	Level   string
	Message string
	Attrs   string
}

// StatsData is the data model for the unified /stats page. Files is
// the file= query that produced Doc — held alongside so the template
// can build self-referential links (rename PATCH, CSV export, paging
// state) without re-parsing the URL.
type StatsData struct {
	Files []string
	Doc   *stats.Document
}

// Mode reports whether the page renders per-meeting (single file) or
// cross-meeting (multiple) layout.
func (d StatsData) Mode() string {
	if d.Doc == nil {
		return ""
	}
	return d.Doc.Mode
}

// MeetingByID returns the meeting envelope for id, or nil when absent.
func (d StatsData) MeetingByID(id string) *stats.Meeting {
	if d.Doc == nil {
		return nil
	}
	for i := range d.Doc.Meetings {
		if d.Doc.Meetings[i].MeetingID == id {
			return &d.Doc.Meetings[i]
		}
	}
	return nil
}

// FirstFileLabel returns the basename of the first input file, with the
// .parquet suffix stripped. Used in the per-meeting header in place of
// the opaque meeting ID.
func (d StatsData) FirstFileLabel() string {
	if len(d.Files) == 0 {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(d.Files[0]), ".parquet")
}

// FileLabelForMeeting returns the user-visible file name (basename
// without `.parquet`) that produced this meeting's data. Prefers the
// source_file emitted by Python; falls back to scanning Files by the
// `<meeting_id>.parquet` naming convention, and finally to the meeting
// ID itself.
func (d StatsData) FileLabelForMeeting(meetingID string) string {
	if d.Doc != nil {
		for i := range d.Doc.Meetings {
			if d.Doc.Meetings[i].MeetingID == meetingID && d.Doc.Meetings[i].SourceFile != "" {
				return strings.TrimSuffix(filepath.Base(d.Doc.Meetings[i].SourceFile), ".parquet")
			}
		}
	}
	want := meetingID + ".parquet"
	for _, f := range d.Files {
		base := filepath.Base(f)
		if base == want {
			return strings.TrimSuffix(base, ".parquet")
		}
	}
	return meetingID
}

// FilesQuery returns the file= query string (no leading ?) so templates
// can append it to outbound URLs.
func (d StatsData) FilesQuery() string {
	q := url.Values{}
	for _, f := range d.Files {
		q.Add("file", f)
	}
	return q.Encode()
}

// MaxDuration returns the longest meeting duration in seconds across
// every loaded meeting. The cross-meeting layout scales presence bands
// relative to this so longer meetings get visually longer bands.
func (d StatsData) MaxDuration() float64 {
	if d.Doc == nil {
		return 0
	}
	var max float64
	for _, m := range d.Doc.Meetings {
		if m.DurationSeconds > max {
			max = m.DurationSeconds
		}
	}
	return max
}

// RowWidthPct returns the relative width (0–100) for a per-meeting
// band row inside the cross-meeting layout: 100 for the longest
// meeting, proportionally less for shorter ones.
func (d StatsData) RowWidthPct(m stats.Meeting) float64 {
	if max := d.MaxDuration(); max > 0 {
		return m.DurationSeconds / max * 100
	}
	return 100
}

// ConfigData is the data model for the config editor page. It carries the
// current resolved Values plus the JSON Schema, so the template can read
// enum lists, min/max, and writeOnly markers straight from the schema
// instead of duplicating them.
type ConfigData struct {
	V          config.Values
	Schema     *jsonschema.Schema
	DataDir    string
	CacheDir   string
	ConfigPath string
}

// at walks Schema.Properties along path and returns the leaf schema, or
// nil if any segment is missing. Used by the field-rendering helpers.
func (d ConfigData) at(path ...string) *jsonschema.Schema {
	cur := d.Schema
	for _, p := range path {
		if cur == nil {
			return nil
		}
		next, ok := cur.Properties[p]
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// WriteOnly reports whether the field at path is marked writeOnly in the
// schema (i.e. a secret that must not be echoed back to the form).
func (d ConfigData) WriteOnly(path ...string) bool {
	s := d.at(path...)
	return s != nil && s.WriteOnly
}

// Enum returns the string enum values declared for the field at path.
func (d ConfigData) Enum(path ...string) []string {
	s := d.at(path...)
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Enum))
	for _, v := range s.Enum {
		if str, ok := v.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// MinAttr returns the field's minimum as a string suitable for an HTML
// input's min= attribute, or "" if unset.
func (d ConfigData) MinAttr(path ...string) string {
	s := d.at(path...)
	if s == nil || s.Minimum == nil {
		return ""
	}
	return formatNum(*s.Minimum)
}

// MaxAttr returns the field's maximum, formatted like MinAttr.
func (d ConfigData) MaxAttr(path ...string) string {
	s := d.at(path...)
	if s == nil || s.Maximum == nil {
		return ""
	}
	return formatNum(*s.Maximum)
}

func formatNum(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
