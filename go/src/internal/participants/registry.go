package participants

import (
	"context"
	"errors"
	"time"
)

// ParticipantID is the stable identifier for one (platform, display-name) registration.
type ParticipantID string

// Handle is a messenger-specific persistent contact identifier (e.g. a
// Telegram chat_id encoded as a decimal string).
type Handle string

// ErrNameTaken is returned by Register when the (platform, displayName) pair
// is already claimed by a different messenger handle.
var ErrNameTaken = errors.New("participants: display name already registered for this platform")

// RegistryEntry is one row in the participant registry.
type RegistryEntry struct {
	ID             ParticipantID
	Platform       string
	DisplayName    string    // canonical casing as supplied at registration
	MessengerName  string
	Handle         Handle
	MessengerLabel string    // human-readable: Telegram @username or first name
	RegisteredAt   time.Time
}

// Registry maps (platform, displayName) pairs to ParticipantIDs and manages
// the display-name pairing flow.
type Registry interface {
	// Resolve looks up a participant by platform and display name.
	// Matching is case-insensitive with leading/trailing whitespace trimmed.
	Resolve(platform, displayName string) (ParticipantID, bool)

	// Register stores a (platform, displayName) → handle binding persistently.
	// Returns ErrNameTaken if that pair is already claimed by a different handle.
	// The same handle may call Register again to overwrite its own previous entry.
	Register(ctx context.Context, messengerName string, handle Handle, messengerLabel, platform, displayName string) (ParticipantID, error)

	// Unregister removes the registry entry for a participant.
	Unregister(ctx context.Context, id ParticipantID) error

	// Handle returns the messenger handle for a participant, if known.
	Handle(p ParticipantID, messengerName string) (Handle, bool)

	// List returns all registry entries.
	List(ctx context.Context) ([]RegistryEntry, error)

	// ClearAll removes all registered participants. Parquet files are not affected.
	ClearAll(ctx context.Context) error

	// Close releases resources held by the registry.
	Close() error
}
