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
type Meeting struct {
	MeetingID       string  `json:"meeting_id"`
	StartedAt       string  `json:"started_at"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// Participant is one student in canonical display-name order. Rows
// holds one entry per meeting in the request set (with Absent=true for
// meetings the participant did not attend, in cross-meeting mode).
type Participant struct {
	DisplayName string            `json:"display_name"`
	Rows        []ParticipantRow `json:"rows"`
}

// ParticipantRow is the (participant, meeting) cell — the unit one
// row of the timeband list renders from.
type ParticipantRow struct {
	MeetingID         string    `json:"meeting_id"`
	Absent            bool      `json:"absent"`
	PresenceRatio     float64   `json:"presence_ratio"`
	ChallengesIssued  int       `json:"challenges_issued"`
	ChallengesCorrect int       `json:"challenges_correct"`
	Segments          []Segment `json:"segments"`
	Markers           []Marker  `json:"markers"`
}

// Segment is a presence band span, in percent of the meeting duration.
type Segment struct {
	StartPct float64 `json:"start_pct"`
	WidthPct float64 `json:"width_pct"`
	Present  bool    `json:"present"`
}

// Marker is a challenge event positioned along the meeting timeline.
type Marker struct {
	XPct          float64 `json:"x_pct"`
	ChallengeType string  `json:"challenge_type"`
	Result        string  `json:"result"`
	ChallengeID   string  `json:"challenge_id"`
	QuestionID    string  `json:"question_id"`
	TimestampMS   int64   `json:"timestamp_ms"`
}
