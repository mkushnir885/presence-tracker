package messengers

import (
	"context"
	"slices"
	"time"
)

// registeredNames is the catalog of adapter names, appended to from each
// adapter's init via Register so adding an adapter is a one-package change.
var registeredNames []string

func Register(name string) {
	registeredNames = append(registeredNames, name)
	slices.Sort(registeredNames)
}

// Names returns the messenger adapters compiled into this build, sorted.
func Names() []string {
	return slices.Clone(registeredNames)
}

type EventKind string

const (
	EventKindRegistration     EventKind = "registration"      // student ran /register
	EventKindJoinConfirmation EventKind = "join_confirmation" // student tapped Yes/No on a join prompt
	EventKindAnswerReceived   EventKind = "answer_received"   // student replied to a challenge
)

// Event is a normalised message from an adapter. Which fields are set
// depends on Kind (see the per-kind groupings below).
type Event struct {
	Kind      EventKind
	Handle    string // adapter-specific contact ID
	Timestamp time.Time

	DisplayName string // Registration

	Confirmed       bool       // JoinConfirmation
	ConfirmationRef MessageRef // JoinConfirmation

	ChallengeID      string     // AnswerReceived
	Answer           string     // AnswerReceived
	Selected         []string   // AnswerReceived: chosen options for multiple-choice
	AnswerMessageRef MessageRef // AnswerReceived
}

type ChallengePrompt struct {
	ChallengeID  string
	QuestionID   string
	Prompt       string
	QuestionType string
	Choices      []string
}

// MessageRef is an opaque, adapter-encoded handle to a delivered message,
// used to edit or delete it later.
type MessageRef struct {
	Opaque string
}

// NotifyKind selects a localized status message. NotifyJoinCollision takes
// the display name as arg[0].
type NotifyKind int

const (
	NotifyJoinDropped       NotifyKind = iota // left before confirming
	NotifyJoinTimedOut                        // confirmation window elapsed
	NotifyJoinCollision                       // same display name claimed twice
	NotifyChallengeAnswered                   // answer recorded
	NotifyChallengeTimedOut                   // answer window elapsed
)

// Messenger is a message-delivery channel; one adapter per backend.
// In every method lang is the recipient's catalog language (supplied by
// the caller, empty means the adapter default).
type Messenger interface {
	Name() string
	Start(ctx context.Context) (<-chan Event, error)
	Stop(ctx context.Context) error

	SendJoinConfirmation(ctx context.Context, handle, lang, meetingID, platform string) (MessageRef, error)
	SendChallenge(ctx context.Context, handle, lang string, c ChallengePrompt) (MessageRef, error)

	// Notify edits a prior message in place; SendNotification sends a fresh one.
	Notify(ctx context.Context, ref MessageRef, lang string, kind NotifyKind, args ...any) error
	SendNotification(ctx context.Context, handle, lang string, kind NotifyKind, args ...any) error

	DeleteMessage(ctx context.Context, ref MessageRef) error
}
