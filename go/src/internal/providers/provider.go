package providers

import (
	"context"
	"strings"
	"time"
)

// EventKind labels the kind of event a Provider emits. These are
// internal provider-to-coordinator signals; the Parquet event_type
// strings are decided by the session coordinator (see
// internal/session for the session_started / session_ended logic).
type EventKind string

const (
	EventKindParticipantJoined EventKind = "participant_joined"
	EventKindParticipantLeft   EventKind = "participant_left"
	EventKindMeetingStarted    EventKind = "meeting_started"
	EventKindMeetingEnded      EventKind = "meeting_ended"
)

// Event is a normalised event produced by a Provider adapter.
//
// MeetingInProgress is meaningful only on EventKindMeetingStarted: it
// is true when the provider attached while the meeting was already
// running, in which case Timestamp is the attach time and the meeting's
// true start time is unknown. When false, Timestamp is the meeting's
// observed start time.
type Event struct {
	Kind              EventKind
	MeetingID         string
	PlatformID        string // platform-specific participant identifier (email, user id, …)
	DisplayName       string // human-readable name as reported by the platform
	Timestamp         time.Time
	Extra             map[string]string // provider-specific fields forwarded to metadata
	MeetingInProgress bool
}

// Provider abstracts a video-conferencing platform.
//
// Subscribe must emit exactly one EventKindMeetingStarted (with
// MeetingInProgress set as documented on Event) and may emit one
// EventKindMeetingEnded if the meeting's actual end is observed. The
// channel is closed when the meeting ends or ctx is cancelled. A
// provider that cannot determine whether the meeting is already in
// progress at attach time must return an error from Subscribe rather
// than guess.
type Provider interface {
	Name() string
	Authenticate(ctx context.Context) error
	Subscribe(ctx context.Context, meetingID string) (<-chan Event, error)
}

// MeetingInputParser is an optional capability for providers that can
// accept richer input than a bare meeting ID — for example, BBB can
// pull the ID out of an invite URL. Providers without this method get
// the user's input passed through unchanged (after trimming).
type MeetingInputParser interface {
	ParseMeetingID(input string) (string, error)
}

// ParseMeetingID normalises the user-supplied meeting input. When prov
// implements [MeetingInputParser] the call is delegated; otherwise the
// trimmed input is returned as-is.
func ParseMeetingID(prov Provider, input string) (string, error) {
	input = strings.TrimSpace(input)
	if p, ok := prov.(MeetingInputParser); ok {
		return p.ParseMeetingID(input)
	}
	return input, nil
}
