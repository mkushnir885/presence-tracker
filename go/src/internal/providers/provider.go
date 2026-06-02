package providers

import (
	"context"
	"slices"
	"time"
)

var registered = map[string]string{}

func Register(name, displayName string) {
	registered[name] = displayName
}

func Names() []string {
	out := make([]string, 0, len(registered))
	for n := range registered {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Unknown short names pass through verbatim so legacy Parquet recordings stay readable.
func DisplayName(short string) string {
	if d, ok := registered[short]; ok {
		return d
	}
	return short
}

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
	Name() string        // stable short identifier (e.g. "zoom"); Parquet/CLI/registry key
	DisplayName() string // human-facing brand name
	Authenticate(ctx context.Context) error
	ParseMeetingID(input string) (string, error)
	Subscribe(ctx context.Context, meetingID string) (<-chan Event, error)
}
