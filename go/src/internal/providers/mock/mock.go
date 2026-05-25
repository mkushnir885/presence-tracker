package mock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"presence-tracker/src/internal/providers"
)

// fixtureEvent is one line of a fixture JSONL file.
type fixtureEvent struct {
	Kind        string            `json:"kind"`
	PlatformID  string            `json:"platform_id"`
	DisplayName string            `json:"display_name"`
	OffsetMS    int64             `json:"offset_ms"` // time offset from meeting start
	Extra       map[string]string `json:"extra"`
}

// Provider replays events from a fixture directory at real-time speed.
// The fixture directory must contain an events.jsonl file.
type Provider struct {
	fixturePath string
	meetingID   string
	speed       float64 // replay speed multiplier; 0 = instant
}

// New creates a mock Provider. fixturePath is the directory containing events.jsonl.
func New(fixturePath string) *Provider {
	return &Provider{fixturePath: fixturePath, speed: 1.0}
}

// WithSpeed sets the replay speed multiplier (e.g. 10.0 = 10× faster).
func (p *Provider) WithSpeed(s float64) *Provider { p.speed = s; return p }

func (p *Provider) Name() string { return "mock" }

func (p *Provider) Authenticate(_ context.Context) error { return nil }

func (p *Provider) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	p.meetingID = meetingID
	path := filepath.Join(p.fixturePath, "events.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mock: open fixture %s: %w", path, err)
	}

	ch := make(chan providers.Event, 16)
	go func() {
		defer close(ch)
		defer func() { _ = f.Close() }()

		start := time.Now()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var fe fixtureEvent
			if err := json.Unmarshal(sc.Bytes(), &fe); err != nil {
				continue
			}
			if p.speed > 0 {
				target := start.Add(time.Duration(float64(fe.OffsetMS) / p.speed * float64(time.Millisecond)))
				wait := time.Until(target)
				if wait > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(wait):
					}
				}
			}
			evt := providers.Event{
				Kind:        providers.EventKind(fe.Kind),
				MeetingID:   meetingID,
				PlatformID:  fe.PlatformID,
				DisplayName: fe.DisplayName,
				Timestamp:   start.Add(time.Duration(fe.OffsetMS) * time.Millisecond),
				Extra:       fe.Extra,
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
