package messengers

import (
	"context"
	"slices"
	"time"
)

// registeredNames is the catalog of adapter names appended to from
// each adapter subpackage's init via Register. Read-only after init.
var registeredNames []string

// Register adds name to the catalog returned by Names. Adapter
// subpackages call this from their init function so adding or removing
// an adapter is a single-package change. Test-only adapters (e.g.
// mock) intentionally do not register.
func Register(name string) {
	registeredNames = append(registeredNames, name)
	slices.Sort(registeredNames)
}

// Names returns the messenger adapters available in this build, in
// alphabetical order. The result is a fresh slice safe for the caller
// to mutate.
//
// The GUI uses this for its messenger filter dropdown so options stay
// stable regardless of whether anyone has actually registered through
// a given messenger — a participant from a previously-enabled adapter
// is still filterable.
func Names() []string {
	return slices.Clone(registeredNames)
}

// EventKind labels the kind of event a Messenger emits.
type EventKind string

const (
	// EventKindRegistration fires when a student sends /register and the adapter
	// has stored the binding. The coordinator uses this to send a join confirmation
	// if the student is already present in the current meeting.
	EventKindRegistration EventKind = "registration"

	// EventKindJoinConfirmation fires when a student taps Yes or No on a
	// join-confirmation message.
	EventKindJoinConfirmation EventKind = "join_confirmation"

	// EventKindAnswerReceived fires when a student replies to a challenge prompt.
	EventKindAnswerReceived EventKind = "answer_received"
)

// Event is a normalised event produced by a Messenger adapter.
type Event struct {
	Kind      EventKind
	Handle    string // messenger-specific contact identifier
	Timestamp time.Time

	// EventKindRegistration
	DisplayName string

	// EventKindJoinConfirmation
	Confirmed       bool
	ConfirmationRef MessageRef // reference to the confirmation message for cleanup

	// EventKindAnswerReceived
	ChallengeID      string
	Answer           string
	Selected         []string
	AnswerMessageRef MessageRef
}

// ChallengePrompt is the question payload delivered to a student via the messenger.
type ChallengePrompt struct {
	ChallengeID  string
	QuestionID   string
	Prompt       string
	QuestionType string   // multiple_choice | numeric | short_text
	Choices      []string // populated for multiple_choice
}

// MessageRef is an opaque reference to a delivered message, used to edit or
// delete it after the answer window closes.
type MessageRef struct {
	// Encoded as JSON by each adapter; not inspected by callers.
	Opaque string
}

// NotifyKind labels a semantic edit applied to a previously-sent message.
// The messenger resolves the kind to a localized string in the
// recipient's language and overwrites the message text (clearing inline
// keyboards, if any).
type NotifyKind int

const (
	// NotifyJoinDropped — verification cancelled because the participant
	// left the meeting before tapping Yes/No.
	NotifyJoinDropped NotifyKind = iota
	// NotifyJoinTimedOut — the verification window elapsed with no answer.
	// The text includes a hint to rejoin the meeting to retry.
	NotifyJoinTimedOut
	// NotifyJoinCollision — another participant joined under the same
	// display name while verification was pending. arg[0] is the display
	// name (string); the text hints at contacting the teacher.
	NotifyJoinCollision
	// NotifyChallengeAnswered — the participant's answer was received.
	NotifyChallengeAnswered
	// NotifyChallengeTimedOut — the answer window elapsed with no reply.
	NotifyChallengeTimedOut
)

// Messenger abstracts a message delivery channel.
//
// Start begins processing incoming updates and returns a channel of events.
// The channel is closed when Stop is called or ctx is cancelled.
// Stop gracefully shuts down the adapter and drains the channel.
type Messenger interface {
	Name() string
	Start(ctx context.Context) (<-chan Event, error)
	Stop(ctx context.Context) error

	// SendJoinConfirmation sends a "Did you just join [meeting] on [platform]?"
	// DM with Yes/No inline buttons. The student's response arrives as a
	// EventKindJoinConfirmation event on the Start channel.
	SendJoinConfirmation(ctx context.Context, handle, meetingID, platform string) (MessageRef, error)

	SendChallenge(ctx context.Context, handle string, c ChallengePrompt) (MessageRef, error)

	// Notify edits a previously-sent message to a localized result text
	// keyed by kind. Inline keyboards are cleared. args are interpreted
	// per kind (see NotifyKind docs).
	Notify(ctx context.Context, ref MessageRef, kind NotifyKind, args ...any) error

	DeleteMessage(ctx context.Context, ref MessageRef) error
}
