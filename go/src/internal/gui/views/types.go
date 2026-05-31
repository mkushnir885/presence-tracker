package views

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/i18n"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/session"
	"presence-tracker/src/internal/stats"
)

type Locale = i18n.Locale

type RegistryFilterInputs struct {
	Name      string
	Messenger string
	From      string
	To        string
}

type RegistryData struct {
	Entries    []participants.RegistryEntry
	Messengers []string
	Filter     RegistryFilterInputs
	HasAny     bool
}

type RegistryFilterErrors map[string]string

var registryFilterFieldOrder = []string{"name", "messenger", "from", "to"}

func (e RegistryFilterErrors) Ordered() []struct{ Field, Key string } {
	out := make([]struct{ Field, Key string }, 0, len(e))
	for _, f := range registryFilterFieldOrder {
		if k, ok := e[f]; ok {
			out = append(out, struct{ Field, Key string }{f, k})
		}
	}
	return out
}

func RegistryFilterErrorMessage(fieldKey, errorKey string, locale Locale) string {
	return fmt.Sprintf(
		locale.T("registry.filter.error.template"),
		locale.T("registry.filter."+fieldKey),
		locale.T(errorKey),
	)
}

func RegistryExactDeleteVals(displayName string) string {
	b, _ := json.Marshal(map[string]string{"display_name": displayName}) //nolint:errchkjson // a single string field cannot fail to marshal
	return string(b)
}

func registryInfoClass(errors RegistryFilterErrors) string {
	if len(errors) > 0 {
		return "registry-info has-error"
	}
	return "registry-info"
}

type HomeData struct {
	EnabledProviders []ProviderOption
}

type MeetingsData struct {
	Meetings []Meeting
}

type ProviderOption struct {
	Name  string
	Label string
}

type Meeting struct {
	ID        string
	CreatedAt time.Time
}

type StatusData struct {
	MeetingID         string
	ProviderName      string
	StartedAt         time.Time
	MeetingStartedAt  time.Time
	MeetingInProgress bool
	Present           []session.PresenceStatus
	Unregistered      []session.UnregisteredStatus
	LogEntries        []LogEntry
	AutoGenEnabled    bool
	AutoGenAutoSubmit bool
	AutoGenIntervalS  int
	PendingBank       *PendingBank
}

type PendingBank struct {
	Path    string
	Name    string
	ModTime time.Time
}

type LogEntry struct {
	Time    time.Time
	Level   string
	Message string
	Attrs   string
}

type StatsData struct {
	Dirs []string
	Doc  *stats.Document
}

func (d StatsData) Mode() string {
	if d.Doc == nil {
		return ""
	}
	return d.Doc.Mode
}

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

func (d StatsData) FirstDirLabel() string {
	if len(d.Dirs) == 0 {
		return ""
	}
	return filepath.Base(d.Dirs[0])
}

func (d StatsData) DirLabelForMeeting(meetingID string) string {
	if d.Doc != nil {
		for i := range d.Doc.Meetings {
			if d.Doc.Meetings[i].MeetingID == meetingID && d.Doc.Meetings[i].SourceDir != "" {
				return filepath.Base(d.Doc.Meetings[i].SourceDir)
			}
		}
	}
	for _, dir := range d.Dirs {
		base := filepath.Base(dir)
		if base == meetingID {
			return base
		}
	}
	return meetingID
}

func (d StatsData) DirsQuery() string {
	q := url.Values{}
	for _, dir := range d.Dirs {
		q.Add("dir", dir)
	}
	return q.Encode()
}

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

// RowWidthPct scales a meeting's timeline band to the longest meeting in the
// set, so band lengths are visually comparable across the cross-meeting view.
func (d StatsData) RowWidthPct(m stats.Meeting) float64 {
	if max := d.MaxDuration(); max > 0 {
		return m.DurationSeconds / max * 100
	}
	return 100
}

type ConfigData struct {
	V          config.Values
	Schema     *jsonschema.Schema
	DataDir    string
	CacheDir   string
	ConfigPath string
	Error      string
}

// at walks the JSON Schema along the property path to a field's leaf schema;
// the Enum/MinAttr/MaxAttr helpers read each input's constraints from it.
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

func (d ConfigData) MinAttr(path ...string) string {
	s := d.at(path...)
	if s == nil || s.Minimum == nil {
		return ""
	}
	return formatNum(*s.Minimum)
}

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
