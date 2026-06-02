package mock

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/mockfixture"
	"presence-tracker/src/internal/participants"
)

const (
	Name        = "mock"
	DisplayName = "Mock"
)

// Messenger replays messenger entries from a shared mockfixture and records
// outgoing messages so the session coordinator can edit/delete them later.
type Messenger struct {
	fixture  *mockfixture.Fixture
	registry participants.Registry

	events chan messengers.Event

	mu     sync.Mutex
	refIdx int

	stop context.CancelFunc
	done chan struct{}
}

func New(f *mockfixture.Fixture, registry participants.Registry) *Messenger {
	return &Messenger{
		fixture:  f,
		registry: registry,
		events:   make(chan messengers.Event, 64),
	}
}

func (m *Messenger) Name() string        { return Name }
func (m *Messenger) DisplayName() string { return DisplayName }

func (m *Messenger) Start(ctx context.Context) (<-chan messengers.Event, error) {
	runCtx, cancel := context.WithCancel(ctx)
	m.stop = cancel
	m.done = make(chan struct{})
	go m.replay(runCtx)
	return m.events, nil
}

func (m *Messenger) Stop(_ context.Context) error {
	if m.stop != nil {
		m.stop()
	}
	if m.done != nil {
		<-m.done
	}
	return nil
}

func (m *Messenger) replay(ctx context.Context) {
	defer close(m.done)
	defer close(m.events)

	for _, e := range m.fixture.Entries() {
		if !isMessengerKind(e.Kind) {
			continue
		}
		if !m.fixture.WaitAt(ctx, e.OffsetMS) {
			return
		}
		evt, ok := m.buildEvent(ctx, e)
		if !ok {
			continue
		}
		select {
		case m.events <- evt:
		case <-ctx.Done():
			return
		}
	}

	<-ctx.Done()
}

func (m *Messenger) buildEvent(ctx context.Context, e mockfixture.Entry) (messengers.Event, bool) {
	ts := m.fixture.EventTime(e.OffsetMS)
	switch e.Kind {
	case mockfixture.KindRegistration:
		if err := m.registry.Register(ctx, Name, e.Handle, e.Handle, e.DisplayName, e.Language); err != nil {
			slog.Warn("mock messenger: registry register", "handle", e.Handle, "err", err)
			return messengers.Event{}, false
		}
		return messengers.Event{
			Kind:        messengers.EventKindRegistration,
			Handle:      e.Handle,
			DisplayName: e.DisplayName,
			Timestamp:   ts,
		}, true
	case mockfixture.KindJoinConfirmation:
		return messengers.Event{
			Kind:      messengers.EventKindJoinConfirmation,
			Handle:    e.Handle,
			Confirmed: e.Confirmed,
			Timestamp: ts,
		}, true
	case mockfixture.KindAnswerReceived:
		return messengers.Event{
			Kind:        messengers.EventKindAnswerReceived,
			Handle:      e.Handle,
			ChallengeID: e.ChallengeID,
			Answer:      e.Answer,
			Selected:    e.Selected,
			Timestamp:   ts,
		}, true
	}
	return messengers.Event{}, false
}

func isMessengerKind(k string) bool {
	switch k {
	case mockfixture.KindRegistration,
		mockfixture.KindJoinConfirmation,
		mockfixture.KindAnswerReceived:
		return true
	}
	return false
}

func (m *Messenger) SendJoinConfirmation(_ context.Context, _, _, _, _ string) (messengers.MessageRef, error) {
	return m.nextRef("confirm"), nil
}

func (m *Messenger) SendChallenge(_ context.Context, _, _ string, _ messengers.ChallengePrompt) (messengers.MessageRef, error) {
	return m.nextRef("challenge"), nil
}

func (m *Messenger) Notify(_ context.Context, _ messengers.MessageRef, _ string, _ messengers.NotifyKind, _ ...any) error {
	return nil
}

func (m *Messenger) SendNotification(_ context.Context, _, _ string, _ messengers.NotifyKind, _ ...any) error {
	return nil
}

func (m *Messenger) DeleteMessage(_ context.Context, _ messengers.MessageRef) error {
	return nil
}

func (m *Messenger) nextRef(prefix string) messengers.MessageRef {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refIdx++
	return messengers.MessageRef{Opaque: fmt.Sprintf("mock-%s-%d", prefix, m.refIdx)}
}
