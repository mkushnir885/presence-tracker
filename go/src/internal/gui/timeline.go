package gui

import (
	"time"

	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/gui/views"
	"presence-tracker/src/internal/reporter"
)

// event type constants to avoid magic strings.
const (
	evtMeetingStarted              = "meeting_started"
	evtMeetingEnded                = "meeting_ended"
	evtParticipantJoined           = "participant_joined"
	evtParticipantLeft             = "participant_left"
	evtChallengeIssued             = "challenge_issued"
	evtChallengeAnsweredCorrect    = "challenge_answered_correct"
	evtChallengeAnsweredIncorrect  = "challenge_answered_incorrect"
	evtChallengeUnanswered         = "challenge_unanswered"
	evtChallengeSkippedOffline     = "challenge_skipped_offline"
	evtChallengeSkippedUnregistered = "challenge_skipped_unregistered"
)

// segmentBuilder accumulates join/leave events for one participant.
type segmentBuilder struct {
	joinedAt time.Time
}

// ComputeMeetingInfo builds the display timeline from raw Parquet records and optional CSV rows.
func ComputeMeetingInfo(meetingID string, records []eventstore.Record, csvRows []reporter.Row) views.MeetingInfo {
	var startTime, endTime time.Time

	// First pass: find meeting start/end.
	for _, r := range records {
		switch r.EventType {
		case evtMeetingStarted:
			startTime = r.Timestamp
		case evtMeetingEnded:
			endTime = r.Timestamp
		}
	}

	if endTime.IsZero() && len(records) > 0 {
		endTime = records[len(records)-1].Timestamp
	}
	if startTime.IsZero() && len(records) > 0 {
		startTime = records[0].Timestamp
	}

	duration := endTime.Sub(startTime)
	if duration <= 0 {
		duration = time.Second
	}

	// Build per-participant data.
	type pInfo struct {
		displayNames  []string
		displaySeen   map[string]bool
		openedAt      time.Time // current open segment start
		segments      []views.Segment
		markersByID   map[string]*views.Marker // challenge_id → marker
		markers       []views.Marker
		lastJoinIdx   int // unused placeholder
	}

	// Maintain insertion order of participants.
	order := []string{}
	seen := map[string]bool{}
	participants := map[string]*pInfo{}

	getOrCreate := func(pid string) *pInfo {
		if p, ok := participants[pid]; ok {
			return p
		}
		p := &pInfo{
			displaySeen: map[string]bool{},
			markersByID: map[string]*views.Marker{},
		}
		participants[pid] = p
		if !seen[pid] {
			order = append(order, pid)
			seen[pid] = true
		}
		return p
	}

	pctOf := func(t time.Time) float64 {
		if duration <= 0 {
			return 0
		}
		v := t.Sub(startTime).Seconds() / duration.Seconds() * 100
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		return v
	}

	for _, r := range records {
		pid := r.ParticipantID
		if pid == "" {
			// handle challenge results that have no participant_id but do have challenge_id
			if r.EventType == evtChallengeAnsweredCorrect || r.EventType == evtChallengeAnsweredIncorrect || r.EventType == evtChallengeUnanswered {
				cid := r.Metadata["challenge_id"]
				if cid == "" {
					continue
				}
				// Find the marker across all participants.
				for _, p := range participants {
					if m, ok := p.markersByID[cid]; ok {
						switch r.EventType {
						case evtChallengeAnsweredCorrect:
							m.Result = "correct"
						case evtChallengeAnsweredIncorrect:
							m.Result = "incorrect"
						case evtChallengeUnanswered:
							m.Result = "unanswered"
						}
					}
				}
			}
			continue
		}

		p := getOrCreate(pid)

		if r.DisplayName != "" && !p.displaySeen[r.DisplayName] {
			p.displayNames = append(p.displayNames, r.DisplayName)
			p.displaySeen[r.DisplayName] = true
		}

		switch r.EventType {
		case evtParticipantJoined:
			p.openedAt = r.Timestamp

		case evtParticipantLeft:
			if !p.openedAt.IsZero() {
				startPct := pctOf(p.openedAt)
				endPct := pctOf(r.Timestamp)
				p.segments = append(p.segments, views.Segment{
					StartPct: startPct,
					WidthPct: endPct - startPct,
					Present:  true,
				})
				p.openedAt = time.Time{}
			}

		case evtChallengeIssued:
			cid := r.Metadata["challenge_id"]
			qid := r.Metadata["question_id"]
			ctype := r.Metadata["challenge_type"]
			xPct := pctOf(r.Timestamp)
			m := &views.Marker{
				XPct:          xPct,
				ChallengeType: ctype,
				Result:        "unanswered", // default; may be updated by result event
				ChallengeID:   cid,
				QuestionID:    qid,
				TimestampMS:   r.Timestamp.UnixMilli(),
			}
			p.markersByID[cid] = m
			p.markers = append(p.markers, *m)

		case evtChallengeAnsweredCorrect:
			cid := r.Metadata["challenge_id"]
			if m, ok := p.markersByID[cid]; ok {
				m.Result = "correct"
			}

		case evtChallengeAnsweredIncorrect:
			cid := r.Metadata["challenge_id"]
			if m, ok := p.markersByID[cid]; ok {
				m.Result = "incorrect"
			}

		case evtChallengeUnanswered:
			cid := r.Metadata["challenge_id"]
			if m, ok := p.markersByID[cid]; ok {
				m.Result = "unanswered"
			}

		case evtChallengeSkippedOffline:
			xPct := pctOf(r.Timestamp)
			p.markers = append(p.markers, views.Marker{
				XPct:        xPct,
				Result:      "skipped_offline",
				TimestampMS: r.Timestamp.UnixMilli(),
			})

		case evtChallengeSkippedUnregistered:
			xPct := pctOf(r.Timestamp)
			p.markers = append(p.markers, views.Marker{
				XPct:        xPct,
				Result:      "skipped_unregistered",
				TimestampMS: r.Timestamp.UnixMilli(),
			})
		}
	}

	// Close any still-open segments at meeting end.
	for _, p := range participants {
		if !p.openedAt.IsZero() {
			startPct := pctOf(p.openedAt)
			p.segments = append(p.segments, views.Segment{
				StartPct: startPct,
				WidthPct: 100 - startPct,
				Present:  true,
			})
			p.openedAt = time.Time{}
		}
		// Sync marker slice from pointer map so results are reflected.
		for i := range p.markers {
			cid := p.markers[i].ChallengeID
			if m, ok := p.markersByID[cid]; ok {
				p.markers[i].Result = m.Result
			}
		}
	}

	// Build CSV lookup for presence ratios.
	csvByName := map[string]reporter.Row{}
	for _, row := range csvRows {
		csvByName[row.DisplayName] = row
	}

	rows := make([]views.ParticipantRow, 0, len(order))
	for _, pid := range order {
		p := participants[pid]

		presenceRatio := computePresenceRatio(p.segments)
		challengesIssued := 0
		challengesCorrect := 0

		// Use CSV data if available.
		if len(p.displayNames) > 0 {
			if csvRow, ok := csvByName[p.displayNames[0]]; ok {
				presenceRatio = csvRow.PresenceRatio
				challengesIssued = csvRow.ChallengesIssued
				challengesCorrect = csvRow.ChallengesCorrect
			}
		}
		if challengesIssued == 0 {
			challengesIssued = countIssued(p.markers)
			challengesCorrect = countCorrect(p.markers)
		}

		rows = append(rows, views.ParticipantRow{
			ParticipantID:     pid,
			DisplayNames:      p.displayNames,
			HasMultipleNames:  len(p.displayNames) > 1,
			PresenceRatio:     presenceRatio,
			ChallengesIssued:  challengesIssued,
			ChallengesCorrect: challengesCorrect,
			Segments:          p.segments,
			Markers:           p.markers,
		})
	}

	return views.MeetingInfo{
		MeetingID: meetingID,
		StartTime: startTime,
		EndTime:   endTime,
		Duration:  duration,
		Rows:      rows,
	}
}

func computePresenceRatio(segs []views.Segment) float64 {
	var total float64
	for _, s := range segs {
		if s.Present {
			total += s.WidthPct
		}
	}
	return total / 100
}

func countIssued(markers []views.Marker) int {
	n := 0
	for _, m := range markers {
		if m.ChallengeID != "" {
			n++
		}
	}
	return n
}

func countCorrect(markers []views.Marker) int {
	n := 0
	for _, m := range markers {
		if m.Result == "correct" {
			n++
		}
	}
	return n
}
