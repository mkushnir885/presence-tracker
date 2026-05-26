package participants

import (
	"context"
	"errors"
	"strings"
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

// Filter narrows a Find or Delete to a subset of the registry. Every
// field is optional; the zero value matches every entry. Set fields
// combine with AND.
type Filter struct {
	// DisplayNames matches any entry whose canonical display name
	// (whitespace-trimmed, case-sensitive — same rule as Resolve)
	// appears in the list. An empty slice imposes no constraint.
	DisplayNames []string

	// DisplayNameContains matches entries whose display name contains
	// this substring, case-insensitive.
	DisplayNameContains string

	// MessengerName matches a specific messenger (e.g. "telegram").
	MessengerName string

	// Handle matches a specific messenger handle. Handles are only
	// unique within one messenger, so callers normally set
	// MessengerName alongside this field.
	Handle string

	// RegisteredFrom matches entries registered at or after this instant.
	RegisteredFrom time.Time

	// RegisteredTo is an exclusive upper bound on RegisteredAt.
	RegisteredTo time.Time
}

// IsZero reports whether f has every field at its zero value (i.e.
// matches every entry).
func (f Filter) IsZero() bool {
	return len(f.DisplayNames) == 0 && f.DisplayNameContains == "" &&
		f.MessengerName == "" && f.Handle == "" &&
		f.RegisteredFrom.IsZero() && f.RegisteredTo.IsZero()
}

// Match reports whether e satisfies every set field of f.
func (f Filter) Match(e RegistryEntry) bool {
	if len(f.DisplayNames) > 0 {
		want := normName(e.DisplayName)
		var ok bool
		for _, n := range f.DisplayNames {
			if normName(n) == want {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.DisplayNameContains != "" &&
		!strings.Contains(strings.ToLower(e.DisplayName), strings.ToLower(f.DisplayNameContains)) {
		return false
	}
	if f.MessengerName != "" && e.MessengerName != f.MessengerName {
		return false
	}
	if f.Handle != "" && e.Handle != f.Handle {
		return false
	}
	if !f.RegisteredFrom.IsZero() && e.RegisteredAt.Before(f.RegisteredFrom) {
		return false
	}
	if !f.RegisteredTo.IsZero() && !e.RegisteredAt.Before(f.RegisteredTo) {
		return false
	}
	return true
}

// Registry maps display names to messenger handles. A messenger account
// holds at most one registration at a time; calling Register again from
// the same handle replaces the previous entry (after freeing the old
// name).
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

	// HandleForName returns the messenger handle bound to displayName,
	// when the registration uses messengerName. Used by the session
	// coordinator to look up where to send the verification DM.
	HandleForName(displayName, messengerName string) (string, bool)

	// LookupByHandle returns the entry owned by (messengerName, handle), if any.
	// Used by the messenger adapter to answer "what name am I registered as?".
	LookupByHandle(messengerName, handle string) (RegistryEntry, bool)

	// Find returns every entry that satisfies f. The zero Filter
	// returns every entry. Results come back in bucket order (sorted
	// by normalized display name).
	Find(ctx context.Context, f Filter) ([]RegistryEntry, error)

	// Delete removes every entry that satisfies f and returns the
	// number removed. The zero Filter clears the entire registry.
	Delete(ctx context.Context, f Filter) (int, error)

	// Close releases resources held by the registry.
	Close() error
}
