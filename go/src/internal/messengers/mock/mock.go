package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"presence-tracker/src/internal/messengers"
)

// SentChallenge records one delivered challenge for test inspection.
type SentChallenge struct {
	Handle  string
	Prompt  messengers.ChallengePrompt
	SentAt  time.Time
	Ref     messengers.MessageRef
	Deleted bool
}

// Name is the canonical identifier this adapter reports through
// Messenger.Name. The mock intentionally does not call
// messengers.Register: it is a test-only adapter and stays out of the
// production catalog returned by messengers.Names.
const Name = "mock"

// Messenger is a no-op messenger suitable for automated tests.
type Messenger struct {
	mu            sync.Mutex
	events        chan messengers.Event
	challenges    []*SentChallenge
	refIdx        int
	confirmations []SentConfirmation
}

// SentConfirmation records a join confirmation request for test inspection.
type SentConfirmation struct {
	Handle    string
	MeetingID string
	Platform  string
	Ref       messengers.MessageRef
}

// New creates a Messenger for testing.
func New() *Messenger {
	return &Messenger{events: make(chan messengers.Event, 64)}
}

func (m *Messenger) Name() string { return Name }

func (m *Messenger) Start(_ context.Context) (<-chan messengers.Event, error) {
	return m.events, nil
}

func (m *Messenger) Stop(_ context.Context) error {
	close(m.events)
	return nil
}

func (m *Messenger) SendJoinConfirmation(_ context.Context, handle, meetingID, platform string) (messengers.MessageRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refIdx++
	ref := messengers.MessageRef{Opaque: fmt.Sprintf("mock-confirm-%d", m.refIdx)}
	m.confirmations = append(m.confirmations, SentConfirmation{
		Handle:    handle,
		MeetingID: meetingID,
		Platform:  platform,
		Ref:       ref,
	})
	return ref, nil
}

func (m *Messenger) SendChallenge(_ context.Context, handle string, c messengers.ChallengePrompt) (messengers.MessageRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refIdx++
	ref := messengers.MessageRef{Opaque: fmt.Sprintf("mock-%d", m.refIdx)}
	m.challenges = append(m.challenges, &SentChallenge{
		Handle: handle,
		Prompt: c,
		SentAt: time.Now().UTC(),
		Ref:    ref,
	})
	return ref, nil
}

func (m *Messenger) Notify(_ context.Context, ref messengers.MessageRef, _ messengers.NotifyKind, _ ...any) error {
	_ = ref
	return nil
}

func (m *Messenger) DeleteMessage(_ context.Context, ref messengers.MessageRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sc := range m.challenges {
		if sc.Ref == ref {
			sc.Deleted = true
			return nil
		}
	}
	return nil
}

// InjectAnswer simulates a student answering a challenge.
func (m *Messenger) InjectAnswer(handle, challengeID, text string, selected []string) {
	m.events <- messengers.Event{
		Kind:        messengers.EventKindAnswerReceived,
		Handle:      handle,
		ChallengeID: challengeID,
		Answer:      text,
		Selected:    selected,
		Timestamp:   time.Now().UTC(),
	}
}

// InjectConfirmation simulates a student tapping Yes or No on a join confirmation.
func (m *Messenger) InjectConfirmation(handle string, confirmed bool) {
	m.events <- messengers.Event{
		Kind:      messengers.EventKindJoinConfirmation,
		Handle:    handle,
		Confirmed: confirmed,
		Timestamp: time.Now().UTC(),
	}
}

// Sent returns all challenges sent so far (safe to call concurrently).
func (m *Messenger) Sent() []SentChallenge {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SentChallenge, len(m.challenges))
	for i, sc := range m.challenges {
		out[i] = *sc
	}
	return out
}

// Confirmations returns all join confirmations sent so far.
func (m *Messenger) Confirmations() []SentConfirmation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SentConfirmation(nil), m.confirmations...)
}
