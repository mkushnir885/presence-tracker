package mock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"presence-tracker/src/internal/providers"
)

type fixtureEvent struct {
	Kind              string            `json:"kind"`
	PlatformID        string            `json:"platform_id"`
	DisplayName       string            `json:"display_name"`
	OffsetMS          int64             `json:"offset_ms"`
	Extra             map[string]string `json:"extra"`
	MeetingInProgress bool              `json:"meeting_in_progress"`
}

type Provider struct {
	fixturePath string
	meetingID   string
	speed       float64
}

func New(fixturePath string) *Provider {
	return &Provider{fixturePath: fixturePath, speed: 1.0}
}

func (p *Provider) WithSpeed(s float64) *Provider { p.speed = s; return p }

func (p *Provider) Name() string { return "mock" }

func (p *Provider) Authenticate(_ context.Context) error { return nil }

func (p *Provider) ParseMeetingID(input string) (string, error) {
	return strings.TrimSpace(input), nil
}

// Subscribe replays the fixture's events.jsonl, emitting each event at its
// recorded offset scaled by speed (WithSpeed(10) is 10× faster; speed 0 fires
// everything immediately).
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
				Kind:              providers.EventKind(fe.Kind),
				PlatformID:        fe.PlatformID,
				DisplayName:       fe.DisplayName,
				Timestamp:         start.Add(time.Duration(fe.OffsetMS) * time.Millisecond),
				Extra:             fe.Extra,
				MeetingInProgress: fe.MeetingInProgress,
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
