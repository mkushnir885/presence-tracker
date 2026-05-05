package messengers

import (
	"context"
	"time"
)

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
	Handle    string    // messenger-specific contact identifier
	Timestamp time.Time

	// EventKindRegistration
	Platform    string
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
	EditMessage(ctx context.Context, ref MessageRef, newText string) error
	DeleteMessage(ctx context.Context, ref MessageRef) error
}
