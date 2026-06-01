package providers

import (
	"context"
	"time"
)

type EventKind string

const (
	EventKindParticipantJoined EventKind = "participant_joined"
	EventKindParticipantLeft   EventKind = "participant_left"
	EventKindMeetingStarted    EventKind = "meeting_started"
	EventKindMeetingEnded      EventKind = "meeting_ended"
)

type Event struct {
	Kind        EventKind
	PlatformID  string
	DisplayName string
	Timestamp   time.Time
	Extra       map[string]string
	// MeetingInProgress is set on MeetingStarted when tracking attached
	// after the meeting had already begun.
	MeetingInProgress bool
}

// Provider abstracts a video-conferencing platform. Subscribe streams
// events until the meeting ends or ctx is cancelled, then closes the channel.
// ParseMeetingID normalizes user input (e.g. a meeting URL) into the
// platform's meeting ID.
type Provider interface {
	Name() string
	Authenticate(ctx context.Context) error
	ParseMeetingID(input string) (string, error)
	Subscribe(ctx context.Context, meetingID string) (<-chan Event, error)
}
