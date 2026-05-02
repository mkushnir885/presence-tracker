package providers

import (
	"context"
	"time"
)

// EventKind labels the kind of event a Provider emits.
type EventKind string

const (
	EventKindParticipantJoined EventKind = "participant_joined"
	EventKindParticipantLeft   EventKind = "participant_left"
	EventKindChatMessage       EventKind = "chat_message" // monitored for pairing codes; not persisted
	EventKindMeetingStarted    EventKind = "meeting_started"
	EventKindMeetingEnded      EventKind = "meeting_ended"
)

// Event is a normalised event produced by a Provider adapter.
type Event struct {
	Kind        EventKind
	MeetingID   string
	PlatformID  string // platform-specific participant identifier (email, user id, …)
	DisplayName string // human-readable name as reported by the platform
	Text        string // populated for EventKindChatMessage only
	Timestamp   time.Time
	Extra       map[string]string // provider-specific fields forwarded to metadata
}

// Provider abstracts a video-conferencing platform.
//
// Subscribe closes the returned channel when the meeting ends or ctx is
// cancelled. FetchPostMeeting is idempotent and may be called after the
// channel is closed to collect any events the webhook missed.
type Provider interface {
	Name() string
	Authenticate(ctx context.Context) error
	Subscribe(ctx context.Context, meetingID string) (<-chan Event, error)
	FetchPostMeeting(ctx context.Context, meetingID string) ([]Event, error)
}
