package stats

// Document is the parsed JSON returned by `ptrack_py stats`. Field
// names match the JSON keys via struct tags so the same struct works
// for both single-meeting and cross-meeting modes — only the
// cardinality of Meetings and ParticipantRows changes.
type Document struct {
	Mode         string        `json:"mode"`
	Meetings     []Meeting     `json:"meetings"`
	Participants []Participant `json:"participants"`
}

// Meeting is the per-meeting envelope shared across every Participant
// row that references it via MeetingID.
//
// StartedCause / EndedCause carry the session_started.cause and
// session_ended.cause values ("meeting" or "tracking"); they tell the
// GUI which open-band tooltip variant to use (see docs/EVENT_SCHEMA.md).
type Meeting struct {
	MeetingID       string  `json:"meeting_id"`
	StartedAt       string  `json:"started_at"`
	DurationSeconds float64 `json:"duration_seconds"`
	Platform        string  `json:"platform"`
	StartedCause    string  `json:"started_cause"`
	EndedCause      string  `json:"ended_cause"`
	MaxParticipants int     `json:"max_participants"`
	SourceFile      string  `json:"source_file"`
}

// Participant is one student in canonical display-name order. Rows
// holds one entry per meeting in the request set (with Absent=true for
// meetings the participant did not attend, in cross-meeting mode).
type Participant struct {
	DisplayName string           `json:"display_name"`
	Rows        []ParticipantRow `json:"rows"`
}

// ParticipantRow is the (participant, meeting) cell — the unit one
// row of the timeband list renders from.
type ParticipantRow struct {
	MeetingID            string    `json:"meeting_id"`
	Absent               bool      `json:"absent"`
	PresenceRatio        float64   `json:"presence_ratio"`
	PresenceSeconds      float64   `json:"presence_seconds"`
	ChallengesIssued     int       `json:"challenges_issued"`
	ChallengesCorrect    int       `json:"challenges_correct"`
	ChallengesIncorrect  int       `json:"challenges_incorrect"`
	ChallengesUnanswered int       `json:"challenges_unanswered"`
	Segments             []Segment `json:"segments"`
	Markers              []Marker  `json:"markers"`
}

// Segment is a presence band span. Percent fields drive SVG layout;
// the millisecond offsets and metadata strings feed boundary tooltips.
type Segment struct {
	StartPct     float64 `json:"start_pct"`
	WidthPct     float64 `json:"width_pct"`
	Present      bool    `json:"present"`
	StartMS      int64   `json:"start_ms"`
	EndMS        int64   `json:"end_ms"`
	StillPresent bool    `json:"still_present"`
	JoinMethod   string  `json:"join_method"`
	LeaveReason  string  `json:"leave_reason"`
}

// Marker is a challenge event positioned along the meeting timeline.
// Prompt and CorrectAnswer come from the meeting's questions JSONL
// when one exists; they're empty strings when the file is missing.
type Marker struct {
	XPct          float64 `json:"x_pct"`
	AutoSubmitted bool    `json:"auto_submitted"`
	Result        string  `json:"result"`
	ChallengeID   string  `json:"challenge_id"`
	QuestionID    string  `json:"question_id"`
	TimestampMS   int64   `json:"timestamp_ms"`
	LatencyMS     int64   `json:"latency_ms"`
	Prompt        string  `json:"prompt"`
	CorrectAnswer string  `json:"correct_answer"`
}
