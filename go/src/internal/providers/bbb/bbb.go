package bbb

import (
	"context"
	"crypto/sha1" //nolint:gosec // BBB API uses SHA-1 by specification
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/providers"
)

type Adapter struct {
	cfg    *config.Config
	client *http.Client
	events chan providers.Event
}

func New(cfg *config.Config) *Adapter {
	return &Adapter{
		cfg:    cfg,
		client: newHTTPClient(cfg.Get().Providers.BBB.TLSSkipVerify),
		events: make(chan providers.Event, 64),
	}
}

func newHTTPClient(insecure bool) *http.Client {
	if insecure {
		return &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed cert in dev; controlled by explicit config flag
			},
		}
	}
	return &http.Client{}
}

func (a *Adapter) Name() string { return "bbb" }

func (a *Adapter) ParseMeetingID(input string) (string, error) { return ParseMeetingID(input) }

func ParseMeetingID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("bbb: empty meeting input")
	}

	if !strings.ContainsAny(input, "/?:") {
		return input, nil
	}

	u, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("bbb: parse meeting input %q: %w", input, err)
	}

	if id := strings.TrimSpace(u.Query().Get("meetingID")); id != "" {
		return id, nil
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if (p == "b" || p == "rooms") && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], nil
		}
	}

	return "", fmt.Errorf("bbb: cannot extract meeting ID from %q", input)
}

func (a *Adapter) Authenticate(ctx context.Context) error {
	bbb := a.cfg.Get().Providers.BBB
	apiURL := bbbAPIURL(bbb.BaseURL, bbb.SharedSecret, "getMeetings", "")
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

func (a *Adapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	go a.pollLoop(ctx, meetingID)
	return a.events, nil
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

	interval := time.Duration(a.cfg.Get().Providers.BBB.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	state := pollState{
		active: map[string]string{},
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if done := a.tick(ctx, meetingID, &state); done {
				return
			}
		}
	}
}

type pollState struct {
	active             map[string]string
	meetingLive        bool
	observedNotRunning bool
}

func (a *Adapter) tick(ctx context.Context, meetingID string, state *pollState) bool {
	info, err := a.fetchMeetingInfo(ctx, meetingID)
	if err != nil {
		slog.Warn("bbb: fetchMeetingInfo", "err", err)
		return false
	}

	isRunning := info.ReturnCode == "SUCCESS" && info.Running == "true"

	if !state.meetingLive {
		if !isRunning {
			state.observedNotRunning = true
		} else {
			state.meetingLive = true
			// Seeing "running" without ever having seen "not running" means we
			// attached after the meeting had already started.
			midMeeting := !state.observedNotRunning
			ts := time.Now().UTC()
			if !midMeeting && info.CreateTime > 0 {
				ts = time.UnixMilli(info.CreateTime).UTC()
			}
			a.emit(providers.Event{
				Kind:              providers.EventKindMeetingStarted,
				MeetingID:         meetingID,
				Timestamp:         ts,
				MeetingInProgress: midMeeting,
			})
		}
	}

	if state.meetingLive {
		current := map[string]string{}
		for _, att := range info.Attendees.List {
			current[att.UserID] = att.FullName
		}

		for _, att := range info.Attendees.List {
			if _, seen := state.active[att.UserID]; !seen {
				a.emit(providers.Event{
					Kind:        providers.EventKindParticipantJoined,
					MeetingID:   meetingID,
					PlatformID:  att.UserID,
					DisplayName: att.FullName,
					Timestamp:   time.Now().UTC(),
					Extra:       map[string]string{"role": att.Role},
				})
				state.active[att.UserID] = att.FullName
			}
		}

		for id := range state.active {
			if _, ok := current[id]; !ok {
				a.emit(providers.Event{
					Kind:       providers.EventKindParticipantLeft,
					MeetingID:  meetingID,
					PlatformID: id,
					Timestamp:  time.Now().UTC(),
				})
				delete(state.active, id)
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
	bbb := a.cfg.Get().Providers.BBB
	params := "meetingID=" + url.QueryEscape(meetingID)
	apiURL := bbbAPIURL(bbb.BaseURL, bbb.SharedSecret, "getMeetingInfo", params)

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

func bbbAPIURL(baseURL, sharedSecret, action, params string) string {
	checksum := bbbChecksum(action, params, sharedSecret)
	base := strings.TrimRight(baseURL, "/") + "/api/" + action
	if params != "" {
		return base + "?" + params + "&checksum=" + checksum
	}
	return base + "?checksum=" + checksum
}

// bbbChecksum is the per-call signature the BBB API requires: SHA-1 of the
// action name, the query string, and the shared secret.
func bbbChecksum(action, params, secret string) string {
	h := sha1.New() //nolint:gosec
	_, _ = h.Write([]byte(action + params + secret))
	return hex.EncodeToString(h.Sum(nil))
}
