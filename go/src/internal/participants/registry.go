package participants

import (
	"context"
	"errors"
	"time"
)

// ErrNameTaken is returned by Register when the displayName is already
// claimed by a different messenger handle.
var ErrNameTaken = errors.New("participants: display name already registered by another account")

// RegistryEntry is one row in the participant registry. DisplayName is
// the identity end to end: the canonical name written to every
// per-participant Parquet event and the key used everywhere else.
// MessengerName scopes the Handle, which is only unique within one
// messenger.
type RegistryEntry struct {
	DisplayName    string // canonical casing as supplied at registration
	MessengerName  string // stable identifier reported by the Messenger (e.g. "telegram")
	Handle         string // messenger-specific persistent contact identifier
	MessengerLabel string // human-readable: Telegram @username or first name
	RegisteredAt   time.Time
}

// Registry maps display names to messenger handles. A messenger account
// holds at most one registration at a time; calling Register again from
// the same handle replaces the previous entry (after freeing the old
// name). Use UnregisterByName or UnregisterByHandle to drop one.
type Registry interface {
	// Resolve looks up a participant by display name. Matching is
	// case-sensitive and ignores leading/trailing whitespace; the returned
	// entry carries the casing supplied at registration.
	Resolve(displayName string) (RegistryEntry, bool)

	// Register stores a displayName → handle binding persistently.
	// Returns ErrNameTaken if the name is already claimed by a different
	// handle. If the handle already has a registration under a different
	// name, the previous entry is replaced atomically.
	Register(ctx context.Context, messengerName, handle, messengerLabel, displayName string) error

	// UnregisterByName removes the entry for the given display name.
	// Returns true if an entry was removed.
	UnregisterByName(ctx context.Context, displayName string) (bool, error)

	// UnregisterByHandle removes the entry owned by (messengerName, handle).
	// Returns true if an entry was removed.
	UnregisterByHandle(ctx context.Context, messengerName, handle string) (bool, error)

	// HandleForName returns the messenger handle bound to displayName,
	// when the registration uses messengerName. Used by the session
	// coordinator to look up where to send the verification DM.
	HandleForName(displayName, messengerName string) (string, bool)

	// LookupByHandle returns the entry owned by (messengerName, handle), if any.
	// Used by the messenger adapter to answer "what name am I registered as?".
	LookupByHandle(messengerName, handle string) (RegistryEntry, bool)

	// List returns all registry entries.
	List(ctx context.Context) ([]RegistryEntry, error)

	// ClearAll removes all registered participants. Parquet files are not affected.
	ClearAll(ctx context.Context) error

	// Close releases resources held by the registry.
	Close() error
}
