package polling

import (
	"context"
	"testing"
	"time"

	"presence-tracker/src/internal/providers"
)

func runLoop(t *testing.T, snaps []Snapshot) []providers.Event {
	t.Helper()
	idx := 0
	fetcher := func(ctx context.Context) (Snapshot, error) {
		if idx >= len(snaps) {
			<-ctx.Done()
			return Snapshot{}, ctx.Err()
		}
		s := snaps[idx]
		idx++
		return s, nil
	}
	ch := make(chan providers.Event, 32)
	loop := NewLoop("test", time.Millisecond, fetcher, ch)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	var got []providers.Event
	for evt := range ch {
		got = append(got, evt)
	}
	<-done
	return got
}

func TestLoopColdStartThroughEnd(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	got := runLoop(t, []Snapshot{
		{Live: false},
		{Live: true, MeetingStartedAt: t0, Participants: []Participant{
			{ID: "u1", DisplayName: "Alice", JoinedAt: t0},
		}},
		{Live: true, Participants: []Participant{
			{ID: "u1", DisplayName: "Alice"},
			{ID: "u2", DisplayName: "Bob"},
		}},
		{Live: false, MeetingEndedAt: t1},
	})

	// Alice and Bob are still present at meeting end, so the closing tick
	// emits MeetingEnded directly without fabricating leaves — downstream
	// reads "no left event before meeting_ended" as "present till end".
	wantKinds := []providers.EventKind{
		providers.EventKindMeetingStarted,
		providers.EventKindParticipantJoined,
		providers.EventKindParticipantJoined,
		providers.EventKindMeetingEnded,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(wantKinds), got)
	}
	if got[0].Kind != providers.EventKindMeetingStarted || !got[0].Timestamp.Equal(t0) || got[0].MeetingInProgress {
		t.Fatalf("meeting_started event: %+v", got[0])
	}
	if got[1].Kind != providers.EventKindParticipantJoined || got[1].PlatformID != "u1" || !got[1].Timestamp.Equal(t0) {
		t.Fatalf("first join event: %+v", got[1])
	}
	if got[2].Kind != providers.EventKindParticipantJoined || got[2].PlatformID != "u2" {
		t.Fatalf("second join event: %+v", got[2])
	}
	last := got[len(got)-1]
	if last.Kind != providers.EventKindMeetingEnded || !last.Timestamp.Equal(t1) {
		t.Fatalf("meeting_ended event: %+v", last)
	}
}

func TestLoopMidMeetingAttachSetsInProgress(t *testing.T) {
	got := runLoop(t, []Snapshot{
		{Live: true, Participants: []Participant{{ID: "u1", DisplayName: "A"}}},
		{Live: false},
	})
	if len(got) == 0 || got[0].Kind != providers.EventKindMeetingStarted {
		t.Fatalf("first event: %+v", got)
	}
	if !got[0].MeetingInProgress {
		t.Fatalf("expected MeetingInProgress=true on first-live-without-prior-observation, got %+v", got[0])
	}
}

func TestLoopUsesStoredLeftAtWhenParticipantDisappears(t *testing.T) {
	leftAt := time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC)
	got := runLoop(t, []Snapshot{
		{Live: true, Participants: []Participant{{ID: "u1", DisplayName: "A"}}},
		{Live: true, Participants: []Participant{{ID: "u1", DisplayName: "A", LeftAt: leftAt}}},
		{Live: true},
		{Live: false},
	})

	var leftEvt *providers.Event
	for i := range got {
		if got[i].Kind == providers.EventKindParticipantLeft {
			leftEvt = &got[i]
			break
		}
	}
	if leftEvt == nil {
		t.Fatalf("no participant_left event: %+v", got)
	}
	if !leftEvt.Timestamp.Equal(leftAt) {
		t.Fatalf("left timestamp = %v, want %v", leftEvt.Timestamp, leftAt)
	}
}
