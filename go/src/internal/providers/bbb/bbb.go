package bbb

import (
	"context"
	"crypto/sha1" //nolint:gosec // BBB API uses SHA-1 by specification
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/providers"
)

// Adapter polls the BBB getMeetingInfo API for live participant state.
type Adapter struct {
	cfg    *config.BBBConfig
	client *http.Client
	events chan providers.Event
}

// New creates a BBB poll adapter.
func New(cfg *config.BBBConfig) *Adapter {
	return &Adapter{
		cfg:    cfg,
		client: newHTTPClient(cfg),
		events: make(chan providers.Event, 64),
	}
}

func newHTTPClient(cfg *config.BBBConfig) *http.Client {
	if cfg.TLSSkipVerify {
		return &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed cert in dev; controlled by explicit config flag
			},
		}
	}
	return &http.Client{}
}

func (a *Adapter) Name() string { return "bbb" }

// Authenticate verifies that the BBB server is reachable and the shared
// secret is accepted by calling the getMeetings API endpoint.
func (a *Adapter) Authenticate(ctx context.Context) error {
	apiURL := bbbAPIURL(a.cfg.BaseURL, a.cfg.SharedSecret, "getMeetings", "")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("bbb: authenticate: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("bbb: authenticate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bbb: authenticate: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// Subscribe starts polling getMeetingInfo and returns a channel of events.
// The channel is closed when the meeting ends or ctx is cancelled.
func (a *Adapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	go a.pollLoop(ctx, meetingID)
	return a.events, nil
}

// FetchPostMeeting is not implemented; BBB recordings/events APIs aren't
// consumed in v1.
func (a *Adapter) FetchPostMeeting(_ context.Context, _ string) ([]providers.Event, error) {
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

func (a *Adapter) pollLoop(ctx context.Context, meetingID string) {
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

// tick performs one poll cycle. Returns true when the meeting has ended and
// the caller should stop the loop.
func (a *Adapter) tick(ctx context.Context, meetingID string, active map[string]string, meetingLive *bool) bool {
	info, err := a.fetchMeetingInfo(ctx, meetingID)
	if err != nil {
		slog.Warn("bbb: fetchMeetingInfo", "err", err)
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

func (a *Adapter) fetchMeetingInfo(ctx context.Context, meetingID string) (bbbMeetingInfoResponse, error) {
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

func (a *Adapter) emit(evt providers.Event) {
	select {
	case a.events <- evt:
	default:
		slog.Warn("bbb: event channel full, dropping event", "kind", evt.Kind)
	}
}

// bbbAPIURL builds a signed BBB API URL for the given action and query
// parameters. params must be URL-encoded and NOT include a trailing &.
func bbbAPIURL(baseURL, sharedSecret, action, params string) string {
	checksum := bbbChecksum(action, params, sharedSecret)
	base := strings.TrimRight(baseURL, "/") + "/api/" + action
	if params != "" {
		return base + "?" + params + "&checksum=" + checksum
	}
	return base + "?checksum=" + checksum
}

// bbbChecksum computes the BBB API request checksum: SHA-1(action + params + secret).
func bbbChecksum(action, params, secret string) string {
	h := sha1.New() //nolint:gosec
	_, _ = h.Write([]byte(action + params + secret))
	return hex.EncodeToString(h.Sum(nil))
}
