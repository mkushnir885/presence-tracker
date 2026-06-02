package mock

import (
	"context"
	"strings"

	"presence-tracker/src/internal/mockfixture"
	"presence-tracker/src/internal/providers"
)

const (
	Name        = "mock"
	DisplayName = "Mock"
)

func init() { providers.Register(Name, DisplayName) }

type Provider struct {
	fixture   *mockfixture.Fixture
	meetingID string
}

func New(f *mockfixture.Fixture) *Provider {
	return &Provider{fixture: f}
}

func (p *Provider) Name() string        { return Name }
func (p *Provider) DisplayName() string { return DisplayName }

func (p *Provider) Authenticate(_ context.Context) error { return nil }

func (p *Provider) ParseMeetingID(input string) (string, error) {
	return strings.TrimSpace(input), nil
}

// Subscribe replays the provider entries from the shared fixture, emitting
// each at its scheduled time. Poll entries are handled in a sibling goroutine
// via ReplayPolls so they don't stall the provider event loop.
func (p *Provider) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	p.meetingID = meetingID
	ch := make(chan providers.Event, 16)
	go func() {
		defer close(ch)
		go p.fixture.ReplayPolls(ctx)
		for _, e := range p.fixture.Entries() {
			kind, ok := providerKind(e.Kind)
			if !ok {
				continue
			}
			if !p.fixture.WaitAt(ctx, e.OffsetMS) {
				return
			}
			evt := providers.Event{
				Kind:              kind,
				PlatformID:        e.PlatformID,
				DisplayName:       e.DisplayName,
				Timestamp:         p.fixture.EventTime(e.OffsetMS),
				Extra:             e.Extra,
				MeetingInProgress: e.MeetingInProgress,
			}
			select {
			case ch <- evt:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func providerKind(k string) (providers.EventKind, bool) {
	switch k {
	case mockfixture.KindMeetingStarted:
		return providers.EventKindMeetingStarted, true
	case mockfixture.KindMeetingEnded:
		return providers.EventKindMeetingEnded, true
	case mockfixture.KindParticipantJoined:
		return providers.EventKindParticipantJoined, true
	case mockfixture.KindParticipantLeft:
		return providers.EventKindParticipantLeft, true
	}
	return "", false
}
