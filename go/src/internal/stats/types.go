package stats

// Document is the GUI stats payload, mirroring the JSON from `ptrack_py stats`.
// Mode is "meeting" (one file) or "cross_meeting" (several).
type Document struct {
	Mode         string        `json:"mode"`
	Meetings     []Meeting     `json:"meetings"`
	Participants []Participant `json:"participants"`
}

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

type Participant struct {
	DisplayName string           `json:"display_name"`
	Rows        []ParticipantRow `json:"rows"`
}

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

// Segment is one presence band as percentage offsets for the SVG timeline.
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

// Marker is one challenge on the timeline. The question payload (Prompt,
// Choices, CorrectAnswer, …) is merged in by stats.Loader from the JSONL;
// Python supplies the event-side fields.
type Marker struct {
	XPct            float64  `json:"x_pct"`
	AutoSubmitted   bool     `json:"auto_submitted"`
	Result          string   `json:"result"`
	SkipReason      string   `json:"skip_reason"`
	ChallengeID     string   `json:"challenge_id"`
	QuestionID      string   `json:"question_id"`
	TimestampMS     int64    `json:"timestamp_ms"`
	LatencyMS       int64    `json:"latency_ms"`
	Prompt          string   `json:"prompt"`
	QuestionType    string   `json:"question_type"`
	Choices         []string `json:"choices"`
	CorrectAnswer   string   `json:"correct_answer"`
	MatchMode       string   `json:"match_mode"`
	Tolerance       float64  `json:"tolerance"`
	SubmittedAnswer string   `json:"submitted_answer"`
}
