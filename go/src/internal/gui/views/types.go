package views

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/session"
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
	Meetings        []MeetingFile
	ActiveSession   bool
	ActiveMeetingID string
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

// MeetingData is the data model for the meeting analysis page.
type MeetingData struct {
	MeetingID string
	Info      MeetingInfo
}

// MeetingInfo contains the computed timeline data for one meeting.
type MeetingInfo struct {
	MeetingID string
	StartTime time.Time
	EndTime   time.Time
	Duration  time.Duration
	Rows      []ParticipantRow
}

// ParticipantRow is the timeline data for one participant in a meeting.
// DisplayName is the canonical registered name and the row's identity end
// to end — there is no separate participant ID at the Parquet layer.
type ParticipantRow struct {
	DisplayName       string
	PresenceRatio     float64
	ChallengesIssued  int
	ChallengesCorrect int
	Segments          []Segment
	Markers           []Marker
}

// Segment is a contiguous present/absent span expressed as percentages.
type Segment struct {
	StartPct float64
	WidthPct float64
	Present  bool
}

// Marker is a challenge event at a position in the timeline.
type Marker struct {
	XPct          float64
	ChallengeType string
	Result        string
	ChallengeID   string
	QuestionID    string
	TimestampMS   int64
}

// ParticipantData is the data model for the cross-meeting participant view.
type ParticipantData struct {
	DisplayName string
	Meetings    []ParticipantMeetingRow
}

// ParticipantMeetingRow is per-meeting data in the participant view.
type ParticipantMeetingRow struct {
	MeetingID         string
	StartTime         time.Time
	EndTime           time.Time
	PresenceRatio     float64
	ChallengesIssued  int
	ChallengesCorrect int
	Absent            bool
	Segments          []Segment
	MeetingDuration   time.Duration
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
