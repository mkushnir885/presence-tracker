package polling

import (
	"context"
	"log/slog"
	"time"

	"presence-tracker/src/internal/providers"
)

type Participant struct {
	ID          string
	DisplayName string
	JoinedAt    time.Time
	LeftAt      time.Time
	Extra       map[string]string
}

// Snapshot is the per-tick view a Fetcher returns. Participants lists the
// currently present participants. MeetingStartedAt / MeetingEndedAt are used
// on the live and end transitions respectively; zero means "use time.Now()".
type Snapshot struct {
	Live             bool
	Participants     []Participant
	MeetingStartedAt time.Time
	MeetingEndedAt   time.Time
}

type Fetcher func(ctx context.Context) (Snapshot, error)

type Loop struct {
	Name     string
	Interval time.Duration
	Fetch    Fetcher
	Events   chan<- providers.Event
	tickNow  chan struct{}
}

func NewLoop(name string, interval time.Duration, fetch Fetcher, events chan<- providers.Event) *Loop {
	return &Loop{
		Name:     name,
		Interval: interval,
		Fetch:    fetch,
		Events:   events,
		tickNow:  make(chan struct{}, 1),
	}
}

// Refresh triggers an immediate off-schedule tick. Safe to call from any
// goroutine after the loop is started; no-ops if a tick is already queued.
func (l *Loop) Refresh() {
	select {
	case l.tickNow <- struct{}{}:
	default:
	}
}

func (l *Loop) Run(ctx context.Context) {
	defer close(l.Events)

	st := state{active: map[string]Participant{}}
	l.tick(ctx, &st)

	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if l.tick(ctx, &st) {
				return
			}
		case <-l.tickNow:
			if l.tick(ctx, &st) {
				return
			}
		}
	}
}

type state struct {
	active          map[string]Participant
	meetingLive     bool
	observedNotLive bool
}

func (l *Loop) tick(ctx context.Context, st *state) bool {
	snap, err := l.Fetch(ctx)
	if err != nil {
		slog.Warn(l.Name+": fetch", "err", err)
		return false
	}

	if !st.meetingLive {
		if !snap.Live {
			st.observedNotLive = true
			return false
		}
		st.meetingLive = true
		midMeeting := !st.observedNotLive
		ts := snap.MeetingStartedAt
		if midMeeting || ts.IsZero() {
			ts = time.Now().UTC()
		}
		l.emit(providers.Event{
			Kind:              providers.EventKindMeetingStarted,
			Timestamp:         ts,
			MeetingInProgress: midMeeting,
		})
	}

	// End the meeting before diffing participants: anyone still in st.active
	// at this point was present at meeting end, and emitting closing leaves
	// would collapse that into "left at end" downstream.
	if !snap.Live {
		ts := snap.MeetingEndedAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		l.emit(providers.Event{
			Kind:      providers.EventKindMeetingEnded,
			Timestamp: ts,
		})
		return true
	}

	current := make(map[string]Participant, len(snap.Participants))
	for _, p := range snap.Participants {
		current[p.ID] = p
		if _, seen := st.active[p.ID]; !seen {
			ts := p.JoinedAt
			if ts.IsZero() {
				ts = time.Now().UTC()
			}
			l.emit(providers.Event{
				Kind:        providers.EventKindParticipantJoined,
				PlatformID:  p.ID,
				DisplayName: p.DisplayName,
				Timestamp:   ts,
				Extra:       p.Extra,
			})
		}
		// Always store the latest record so an adapter can stamp LeftAt
		// on the final tick before a participant disappears.
		st.active[p.ID] = p
	}

	for id, prev := range st.active {
		if _, ok := current[id]; ok {
			continue
		}
		ts := prev.LeftAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		l.emit(providers.Event{
			Kind:        providers.EventKindParticipantLeft,
			PlatformID:  id,
			DisplayName: prev.DisplayName,
			Timestamp:   ts,
		})
		delete(st.active, id)
	}

	return false
}

func (l *Loop) emit(evt providers.Event) {
	select {
	case l.Events <- evt:
	default:
		slog.Warn(l.Name+": event channel full, dropping event", "kind", evt.Kind)
	}
}
