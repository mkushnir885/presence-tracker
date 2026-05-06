package bbb

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/providers"
)

// pollAdapter implements Provider by polling the BBB getMeetingInfo API on a
// fixed interval. It requires no public address — only outbound HTTP access to
// the BBB server — and works with every BBB installation.
type pollAdapter struct {
	cfg    *config.BBBConfig
	client *http.Client
	events chan providers.Event
}

func newPollAdapter(cfg *config.BBBConfig) *pollAdapter {
	return &pollAdapter{
		cfg:    cfg,
		client: newHTTPClient(cfg),
		events: make(chan providers.Event, 64),
	}
}

func (a *pollAdapter) Name() string { return "bbb" }

// Authenticate verifies that the BBB server is reachable and the shared secret
// is accepted, identical to the webhook adapter.
func (a *pollAdapter) Authenticate(ctx context.Context) error {
	apiURL := bbbAPIURL(a.cfg.BaseURL, a.cfg.SharedSecret, "getMeetings", "")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("bbb poll: authenticate: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("bbb poll: authenticate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bbb poll: authenticate: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// Subscribe starts polling getMeetingInfo and returns a channel of events.
// The channel is closed when the meeting ends or ctx is cancelled.
func (a *pollAdapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	go a.pollLoop(ctx, meetingID)
	return a.events, nil
}

// FetchPostMeeting is not implemented for the BBB poll adapter.
func (a *pollAdapter) FetchPostMeeting(_ context.Context, _ string) ([]providers.Event, error) {
	return nil, nil
}

type bbbMeetingInfoResponse struct {
	ReturnCode string `xml:"returncode"`
	Running    string `xml:"running"`
	CreateTime int64  `xml:"createTime"`
	Attendees  struct {
		List []struct {
			UserID   string `xml:"userID"`
			FullName string `xml:"fullName"`
			Role     string `xml:"role"`
		} `xml:"attendee"`
	} `xml:"attendees"`
}

func (a *pollAdapter) pollLoop(ctx context.Context, meetingID string) {
	defer close(a.events)

	interval := time.Duration(a.cfg.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	active := map[string]string{} // userID → fullName
	meetingLive := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if done := a.tick(ctx, meetingID, active, &meetingLive); done {
				return
			}
		}
	}
}

// tick performs one poll cycle. Returns true when the meeting has ended and the
// caller should stop the loop.
func (a *pollAdapter) tick(ctx context.Context, meetingID string, active map[string]string, meetingLive *bool) bool {
	info, err := a.fetchMeetingInfo(ctx, meetingID)
	if err != nil {
		slog.Warn("bbb poll: fetchMeetingInfo", "err", err)
		return false
	}

	isRunning := info.ReturnCode == "SUCCESS" && info.Running == "true"

	if !*meetingLive && isRunning {
		*meetingLive = true
		ts := time.Now().UTC()
		if info.CreateTime > 0 {
			ts = time.UnixMilli(info.CreateTime).UTC()
		}
		a.emit(providers.Event{
			Kind:      providers.EventKindMeetingStarted,
			MeetingID: meetingID,
			Timestamp: ts,
		})
	}

	if *meetingLive {
		current := map[string]string{}
		for _, att := range info.Attendees.List {
			current[att.UserID] = att.FullName
		}

		for _, att := range info.Attendees.List {
			if _, seen := active[att.UserID]; !seen {
				a.emit(providers.Event{
					Kind:        providers.EventKindParticipantJoined,
					MeetingID:   meetingID,
					PlatformID:  att.UserID,
					DisplayName: att.FullName,
					Timestamp:   time.Now().UTC(),
					Extra:       map[string]string{"role": att.Role},
				})
				active[att.UserID] = att.FullName
			}
		}

		for id := range active {
			if _, ok := current[id]; !ok {
				a.emit(providers.Event{
					Kind:       providers.EventKindParticipantLeft,
					MeetingID:  meetingID,
					PlatformID: id,
					Timestamp:  time.Now().UTC(),
				})
				delete(active, id)
			}
		}

		if !isRunning {
			a.emit(providers.Event{
				Kind:      providers.EventKindMeetingEnded,
				MeetingID: meetingID,
				Timestamp: time.Now().UTC(),
			})
			return true
		}
	}

	return false
}

func (a *pollAdapter) fetchMeetingInfo(ctx context.Context, meetingID string) (bbbMeetingInfoResponse, error) {
	params := "meetingID=" + url.QueryEscape(meetingID)
	apiURL := bbbAPIURL(a.cfg.BaseURL, a.cfg.SharedSecret, "getMeetingInfo", params)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return bbbMeetingInfoResponse{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return bbbMeetingInfoResponse{}, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var info bbbMeetingInfoResponse
	if err := xml.NewDecoder(resp.Body).Decode(&info); err != nil {
		return bbbMeetingInfoResponse{}, fmt.Errorf("decode XML: %w", err)
	}
	return info, nil
}

func (a *pollAdapter) emit(evt providers.Event) {
	select {
	case a.events <- evt:
	default:
		slog.Warn("bbb poll: event channel full, dropping event", "kind", evt.Kind)
	}
}
