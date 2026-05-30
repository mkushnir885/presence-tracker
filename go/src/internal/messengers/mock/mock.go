package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"presence-tracker/src/internal/messengers"
)

type SentChallenge struct {
	Handle  string
	Prompt  messengers.ChallengePrompt
	SentAt  time.Time
	Ref     messengers.MessageRef
	Deleted bool
}

const Name = "mock"

// Messenger is an in-memory test double: it records what was sent (challenges,
// confirmations) and lets tests inject incoming events via the Inject* methods.
type Messenger struct {
	mu            sync.Mutex
	events        chan messengers.Event
	challenges    []*SentChallenge
	refIdx        int
	confirmations []SentConfirmation
}

type SentConfirmation struct {
	Handle    string
	MeetingID string
	Platform  string
	Ref       messengers.MessageRef
}

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

func (m *Messenger) SendJoinConfirmation(_ context.Context, handle, _, meetingID, platform string) (messengers.MessageRef, error) {
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

func (m *Messenger) SendChallenge(_ context.Context, handle, _ string, c messengers.ChallengePrompt) (messengers.MessageRef, error) {
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

func (m *Messenger) Notify(_ context.Context, ref messengers.MessageRef, _ string, _ messengers.NotifyKind, _ ...any) error {
	_ = ref
	return nil
}

func (m *Messenger) SendNotification(_ context.Context, _, _ string, _ messengers.NotifyKind, _ ...any) error {
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

func (m *Messenger) InjectConfirmation(handle string, confirmed bool) {
	m.events <- messengers.Event{
		Kind:      messengers.EventKindJoinConfirmation,
		Handle:    handle,
		Confirmed: confirmed,
		Timestamp: time.Now().UTC(),
	}
}

func (m *Messenger) Sent() []SentChallenge {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SentChallenge, len(m.challenges))
	for i, sc := range m.challenges {
		out[i] = *sc
	}
	return out
}

func (m *Messenger) Confirmations() []SentConfirmation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SentConfirmation(nil), m.confirmations...)
}
