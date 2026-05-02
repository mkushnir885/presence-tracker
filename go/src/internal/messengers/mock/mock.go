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

// Messenger is a no-op messenger suitable for automated tests.
type Messenger struct {
	mu         sync.Mutex
	events     chan messengers.Event
	challenges []*SentChallenge
	refIdx     int
}

// New creates a Messenger for testing.
func New() *Messenger {
	return &Messenger{events: make(chan messengers.Event, 64)}
}

func (m *Messenger) Name() string { return "mock" }

func (m *Messenger) Start(_ context.Context) (<-chan messengers.Event, error) {
	return m.events, nil
}

func (m *Messenger) Stop(_ context.Context) error {
	close(m.events)
	return nil
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

func (m *Messenger) EditMessage(_ context.Context, ref messengers.MessageRef, _ string) error {
	// EditMessage is kept in the Messenger interface for future use;
	// the challenge flow no longer calls it (delete is used instead).
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
