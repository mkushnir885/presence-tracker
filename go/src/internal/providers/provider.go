package providers

import (
	"context"
	"strings"
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
	MeetingID   string
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
type Provider interface {
	Name() string
	Authenticate(ctx context.Context) error
	Subscribe(ctx context.Context, meetingID string) (<-chan Event, error)
}

// MeetingInputParser is optionally implemented by a Provider to normalize
// user input (e.g. a meeting URL) into the platform's meeting ID.
type MeetingInputParser interface {
	ParseMeetingID(input string) (string, error)
}

func ParseMeetingID(prov Provider, input string) (string, error) {
	input = strings.TrimSpace(input)
	if p, ok := prov.(MeetingInputParser); ok {
		return p.ParseMeetingID(input)
	}
	return input, nil
}
