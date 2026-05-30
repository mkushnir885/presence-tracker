package participants

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrNameTaken = errors.New("participants: display name already registered by another account")

// NormalizeName is the canonical display-name key: case-sensitive, with only
// surrounding whitespace trimmed. The registry and the session coordinator
// both key on it so the two can never diverge.
func NormalizeName(displayName string) string {
	return strings.TrimSpace(displayName)
}

type RegistryEntry struct {
	DisplayName    string
	MessengerName  string
	Handle         string
	MessengerLabel string
	Language       string
	RegisteredAt   time.Time
}

// Filter selects registry entries; set fields are combined with AND. The
// zero Filter matches everything. RegisteredTo is exclusive.
type Filter struct {
	DisplayNames        []string
	DisplayNameContains string
	MessengerName       string
	Handle              string
	RegisteredFrom      time.Time
	RegisteredTo        time.Time
}

func (f Filter) IsZero() bool {
	return len(f.DisplayNames) == 0 && f.DisplayNameContains == "" &&
		f.MessengerName == "" && f.Handle == "" &&
		f.RegisteredFrom.IsZero() && f.RegisteredTo.IsZero()
}

func (f Filter) Match(e RegistryEntry) bool {
	if len(f.DisplayNames) > 0 {
		want := NormalizeName(e.DisplayName)
		var ok bool
		for _, n := range f.DisplayNames {
			if NormalizeName(n) == want {
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

// Registry maps each display name to one messenger account. The display name
// is the participant identity used end to end.
type Registry interface {
	Resolve(displayName string) (RegistryEntry, bool)

	// Register binds displayName to (messengerName, handle). Each handle holds
	// at most one registration, so re-registering replaces the previous name.
	// Returns ErrNameTaken if the name is already held by another account.
	Register(ctx context.Context, messengerName, handle, messengerLabel, displayName, language string) error

	// SetLanguage updates the stored language; the bool reports whether a
	// registration existed.
	SetLanguage(ctx context.Context, messengerName, handle, language string) (bool, error)

	HandleForName(displayName, messengerName string) (string, bool)
	LookupByHandle(messengerName, handle string) (RegistryEntry, bool)

	Find(ctx context.Context, f Filter) ([]RegistryEntry, error)
	Delete(ctx context.Context, f Filter) (int, error)

	Close() error
}
