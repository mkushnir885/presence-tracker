package participants

import "context"

// ParticipantID is the stable internal identifier assigned at first pairing.
// It never changes even if the student uses a different platform display name.
type ParticipantID string

// Handle is a messenger-specific persistent contact identifier (e.g. a
// Telegram chat_id encoded as a decimal string).
type Handle string

// Participant holds all known information about one registered student.
type Participant struct {
	ID        ParticipantID
	Platforms map[string]string // platform name → platformID
	Handles   map[string]Handle // messenger name → Handle
}

// Registry maps platform identifiers to stable ParticipantIDs and manages the
// meeting-time pairing flow.
type Registry interface {
	// Resolve returns the ParticipantID for a known platform identifier.
	Resolve(platform, platformID string) (ParticipantID, bool)

	// StartPairing is called when a student sends /start to the messenger bot.
	// It returns a short one-time code the student must type in the meeting chat.
	StartPairing(ctx context.Context, messengerName string, handle Handle) (code string, err error)

	// CompletePairing is called when the provider adapter detects PTRACK:<code>
	// in meeting chat. It binds platformID to the messenger handle that started
	// pairing with that code.
	CompletePairing(ctx context.Context, platform, platformID, code string) (ParticipantID, error)

	// Handle returns the messenger handle for a participant, if known.
	Handle(p ParticipantID, messengerName string) (Handle, bool)

	// All returns every known participant.
	All(ctx context.Context) ([]Participant, error)

	// ClearAll removes all registered participants and pairing codes.
	ClearAll(ctx context.Context) error

	// Close releases resources held by the registry.
	Close() error
}
