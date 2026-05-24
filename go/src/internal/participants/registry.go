package participants

import (
	"context"
	"errors"
	"time"
)

// MaxNamesPerHandle is the hard cap on how many display names a single
// messenger account may register.
const MaxNamesPerHandle = 5

// ParticipantID is the stable identifier for one display-name registration.
type ParticipantID string

// Handle is a messenger-specific persistent contact identifier (e.g. a
// Telegram chat_id encoded as a decimal string).
type Handle string

// ErrNameTaken is returned by Register when the displayName is already
// claimed by a different messenger handle.
var ErrNameTaken = errors.New("participants: display name already registered by another account")

// ErrTooManyNames is returned by Register when the handle has already
// reached MaxNamesPerHandle entries.
var ErrTooManyNames = errors.New("participants: too many display names registered for this account")

// RegistryEntry is one row in the participant registry.
type RegistryEntry struct {
	ID             ParticipantID
	DisplayName    string // canonical casing as supplied at registration
	MessengerName  string
	Handle         Handle
	MessengerLabel string // human-readable: Telegram @username or first name
	RegisteredAt   time.Time
}

// Registry maps display names to ParticipantIDs and manages the
// display-name pairing flow. A single messenger account may register up
// to MaxNamesPerHandle distinct display names; each registration is its
// own row with its own ParticipantID.
type Registry interface {
	// Resolve looks up a participant by display name.
	// Matching is case-insensitive with leading/trailing whitespace trimmed.
	Resolve(displayName string) (ParticipantID, bool)

	// Register stores a displayName → handle binding persistently.
	// Returns ErrNameTaken if the name is already claimed by a different handle.
	// Returns ErrTooManyNames if the handle has reached MaxNamesPerHandle.
	// Re-registering the same (handle, name) is idempotent: it refreshes the
	// label and canonical casing without consuming an extra slot.
	Register(ctx context.Context, messengerName string, handle Handle, messengerLabel, displayName string) (ParticipantID, error)

	// Unregister removes the registry entry for a participant.
	Unregister(ctx context.Context, id ParticipantID) error

	// UnregisterByName removes the entry for (messengerName, handle, displayName).
	// Returns the deleted ParticipantID and true if an entry was removed.
	UnregisterByName(ctx context.Context, messengerName string, handle Handle, displayName string) (ParticipantID, bool, error)

	// Handle returns the messenger handle for a participant, if known.
	Handle(p ParticipantID, messengerName string) (Handle, bool)

	// ListByHandle returns all registry entries owned by one messenger account.
	ListByHandle(messengerName string, handle Handle) ([]RegistryEntry, error)

	// List returns all registry entries.
	List(ctx context.Context) ([]RegistryEntry, error)

	// ClearAll removes all registered participants. Parquet files are not affected.
	ClearAll(ctx context.Context) error

	// Close releases resources held by the registry.
	Close() error
}
