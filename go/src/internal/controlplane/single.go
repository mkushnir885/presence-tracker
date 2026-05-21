package controlplane

import "presence-tracker/src/internal/session"

// SingleSession is a Sessions implementation backed by exactly one
// Coordinator, used by ptrack track (which only ever runs one meeting at a
// time). Both an explicit meeting ID and ActiveMeetingID resolve to the
// same coordinator.
type SingleSession struct {
	Coord *session.Coordinator
}

// Resolve implements Sessions.
func (s *SingleSession) Resolve(meetingID string) (*session.Coordinator, error) {
	if s.Coord == nil {
		return nil, ErrNoActiveSession
	}
	if meetingID == ActiveMeetingID || meetingID == s.Coord.MeetingID() {
		return s.Coord, nil
	}
	return nil, ErrMeetingNotFound
}
