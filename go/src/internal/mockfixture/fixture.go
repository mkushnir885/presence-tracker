package mockfixture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// Kind identifiers shared with provider and messenger event vocabularies.
// Listed here so each mock package can filter without re-declaring strings.
const (
	KindMeetingStarted    = "meeting_started"
	KindMeetingEnded      = "meeting_ended"
	KindParticipantJoined = "participant_joined"
	KindParticipantLeft   = "participant_left"

	KindRegistration     = "registration"
	KindJoinConfirmation = "join_confirmation"
	KindAnswerReceived   = "answer_received"

	// KindPoll triggers a POST /poll to the daemon at the scheduled offset.
	KindPoll = "poll"
)

// Entry is one fixture line. Which fields are populated depends on Kind;
// unused ones stay at their zero value.
type Entry struct {
	Kind     string `json:"kind"`
	OffsetMS int64  `json:"offset_ms"`

	PlatformID        string            `json:"platform_id,omitempty"`
	DisplayName       string            `json:"display_name,omitempty"`
	Extra             map[string]string `json:"extra,omitempty"`
	MeetingInProgress bool              `json:"meeting_in_progress,omitempty"`

	Handle      string   `json:"handle,omitempty"`
	Language    string   `json:"language,omitempty"`
	Confirmed   bool     `json:"confirmed,omitempty"`
	ChallengeID string   `json:"challenge_id,omitempty"`
	Answer      string   `json:"answer,omitempty"`
	Selected    []string `json:"selected,omitempty"`

	// poll-kind fields
	Bank             string   `json:"bank,omitempty"`
	DeliveryFailures []string `json:"delivery_failures,omitempty"`
}

// Fixture is a sorted set of entries plus the shared replay clock.
// The clock is armed lazily on the first WaitAt call so independent
// provider/messenger consumers stay aligned on the same baseline.
type Fixture struct {
	entries []Entry
	speed   float64

	armOnce sync.Once
	start   time.Time

	addrMu     sync.RWMutex
	daemonAddr string

	failMu  sync.Mutex
	failSet map[string]bool
}

func Load(path string) (*Fixture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mockfixture: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("mockfixture: %s line %d: %w", path, line, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mockfixture: read %s: %w", path, err)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].OffsetMS < entries[j].OffsetMS
	})

	return &Fixture{entries: entries, speed: 1.0}, nil
}

// WithSpeed scales offsets — 10 plays a 30-min lesson in 3 min. Zero fires
// every event immediately.
func (f *Fixture) WithSpeed(speed float64) *Fixture {
	if speed < 0 {
		speed = 0
	}
	f.speed = speed
	return f
}

// Entries returns the parsed lines in offset order. Callers filter by Kind.
func (f *Fixture) Entries() []Entry { return f.entries }

// WaitAt blocks until the scheduled instant for offsetMS, then returns true.
// Returns false if ctx is cancelled before then. The first caller arms the
// shared start clock; subsequent calls reuse it so multiple consumers replay
// in sync.
func (f *Fixture) WaitAt(ctx context.Context, offsetMS int64) bool {
	f.armOnce.Do(func() { f.start = time.Now() })
	if f.speed == 0 {
		return ctx.Err() == nil
	}
	target := f.start.Add(time.Duration(float64(offsetMS) / f.speed * float64(time.Millisecond)))
	wait := time.Until(target)
	if wait <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// EventTime returns the wall-clock instant the entry should fire at, relative
// to the armed start. Safe to call only after WaitAt has been invoked at least
// once (which guarantees start is set).
func (f *Fixture) EventTime(offsetMS int64) time.Time {
	if f.speed == 0 {
		return f.start
	}
	return f.start.Add(time.Duration(float64(offsetMS) / f.speed * float64(time.Millisecond)))
}

func (f *Fixture) SetDaemonAddr(addr string) {
	f.addrMu.Lock()
	f.daemonAddr = addr
	f.addrMu.Unlock()
}

func (f *Fixture) DaemonAddr() string {
	f.addrMu.RLock()
	defer f.addrMu.RUnlock()
	return f.daemonAddr
}

// SetDeliveryFailures marks the given handles to fail on the next SendChallenge
// call. The set is consumed handle-by-handle via TakeDeliveryFailure.
func (f *Fixture) SetDeliveryFailures(handles []string) {
	f.failMu.Lock()
	f.failSet = make(map[string]bool, len(handles))
	for _, h := range handles {
		f.failSet[h] = true
	}
	f.failMu.Unlock()
}

// TakeDeliveryFailure returns true (and removes the entry) if handle was
// registered as a delivery failure for the current poll.
func (f *Fixture) TakeDeliveryFailure(handle string) bool {
	f.failMu.Lock()
	defer f.failMu.Unlock()
	if f.failSet[handle] {
		delete(f.failSet, handle)
		return true
	}
	return false
}

// ReplayPolls sends each poll entry to the daemon at its scheduled time.
// Intended to run as a goroutine alongside the provider event replay.
func (f *Fixture) ReplayPolls(ctx context.Context) {
	for _, e := range f.entries {
		if e.Kind != KindPoll {
			continue
		}
		if !f.WaitAt(ctx, e.OffsetMS) {
			return
		}
		f.SetDeliveryFailures(e.DeliveryFailures)
		addr := f.DaemonAddr()
		if addr == "" {
			slog.Warn("mockfixture: daemon address not set, skipping poll")
			continue
		}
		if err := f.postPoll(ctx, addr, e.Bank); err != nil {
			slog.Warn("mockfixture: poll POST failed", "err", err)
		}
	}
}

func (f *Fixture) postPoll(ctx context.Context, addr, bank string) error {
	body, _ := json.Marshal(map[string]any{"auto_submitted": false, "bank": bank}) //nolint:errchkjson // map with string/bool values cannot fail
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/poll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}
