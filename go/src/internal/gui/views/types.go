package views

import (
	"time"

	"presence-tracker/src/internal/session"
)

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

// ConfigData is the data model for the config editor page.
type ConfigData struct {
	MeetingsDir                string
	QuestionsDir               string
	ReportsDir                 string
	DataDir                    string
	RetentionDays              int
	GUIBindAddr                string
	GUIPort                    int
	GUIOpenBrowserOnStart      bool
	LogLevel                   string
	LogFormat                  string
	BBBEnabled                 bool
	BBBBaseURL                 string
	TelegramEnabled            bool
	TelegramBotToken           string
	AnswerWindowSeconds        int
	MinGapBetweenChallengesSec int
	EventStoreCompression      string
	EventStoreRowGroupSize     int
}
