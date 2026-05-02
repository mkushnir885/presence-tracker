package messengers

import (
	"context"
	"time"
)

// EventKind labels the kind of event a Messenger emits.
type EventKind string

const (
	// EventKindPairingStarted fires when a student sends /start to the bot and
	// the bot has sent them the pairing code. No further coordinator action is
	// needed for this event; it is logged for diagnostics.
	EventKindPairingStarted EventKind = "pairing_started"

	// EventKindAnswerReceived fires when a student replies to a challenge prompt.
	EventKindAnswerReceived EventKind = "answer_received"
)

// Event is a normalised event produced by a Messenger adapter.
type Event struct {
	Kind             EventKind
	Handle           string     // messenger-specific contact identifier
	ChallengeID      string     // populated for EventKindAnswerReceived
	Answer           string     // raw submitted text or choice label
	Selected         []string   // populated for multiple-choice (may overlap with Answer)
	AnswerMessageRef MessageRef // opaque ref for the student's answer message; empty for MCQ callbacks
	Timestamp        time.Time
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

	SendChallenge(ctx context.Context, handle string, c ChallengePrompt) (MessageRef, error)
	EditMessage(ctx context.Context, ref MessageRef, newText string) error
	DeleteMessage(ctx context.Context, ref MessageRef) error
}
