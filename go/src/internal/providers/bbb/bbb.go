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

// Adapter polls the BBB getMeetingInfo API for live participant state.
type Adapter struct {
	cfg    *config.Config
	client *http.Client
	events chan providers.Event
}

// New creates a BBB poll adapter.
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

// ParseMeetingID accepts either a bare meeting ID or a BBB invite URL
// and returns the canonical meeting ID used by the API. Three shapes are
// recognised:
//
//   - a bare ID with no path separator — returned unchanged
//   - a join URL carrying ?meetingID=<id> in the query
//   - a Greenlight room URL with /b/<id> or /rooms/<id>[/join] in the path
//
// Any other URL is rejected with an explicit error so the teacher sees
// a clear message instead of an obscure API failure later.
func (a *Adapter) ParseMeetingID(input string) (string, error) { return ParseMeetingID(input) }

// ParseMeetingID is the package-level form of [Adapter.ParseMeetingID],
// exposed so callers without an adapter instance (tests, future helpers)
// can reuse the same parsing rules.
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

// Authenticate verifies that the BBB server is reachable and the shared
// secret is accepted by calling the getMeetings API endpoint.
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

// Subscribe starts polling getMeetingInfo and returns a channel of events.
// The channel is closed when the meeting ends or ctx is cancelled.
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
		active: map[string]string{}, // userID → fullName
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

// pollState carries cross-tick bookkeeping for pollLoop. observedNotRunning
// is set to true the first time a poll reports the meeting as not running
// before it goes live; that distinguishes "attached and watched the meeting
// start" from "attached while the meeting was already in progress".
type pollState struct {
	active             map[string]string
	meetingLive        bool
	observedNotRunning bool
}

// tick performs one poll cycle. Returns true when the meeting has ended and
// the caller should stop the loop.
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
