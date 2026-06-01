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
	client := &http.Client{}
	if cfg.Get().Providers.BBB.TLSSkipVerify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed cert in dev; controlled by explicit config flag
		}
	}
	return &Adapter{
		cfg:    cfg,
		client: client,
		events: make(chan providers.Event, 64),
	}
}

func (a *Adapter) Name() string { return "bbb" }

func (*Adapter) ParseMeetingID(input string) (string, error) {
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
	Running    bool   `xml:"running"`
	StartTime  int64  `xml:"startTime"`
	EndTime    int64  `xml:"endTime"`
	Attendees  struct {
		List []struct {
			UserID   string `xml:"userID"`
			FullName string `xml:"fullName"`
			Role     string `xml:"role"`
		} `xml:"attendee"`
	} `xml:"attendees"`
}

type bbbMetadataItem struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type bbbMeetingsResponse struct {
	ReturnCode string `xml:"returncode"`
	Meetings   struct {
		List []struct {
			MeetingID string `xml:"meetingID"`
			Metadata  struct {
				Items []bbbMetadataItem `xml:",any"`
			} `xml:"metadata"`
		} `xml:"meeting"`
	} `xml:"meetings"`
}

func (a *Adapter) pollLoop(ctx context.Context, meetingID string) {
	defer close(a.events)

	interval := time.Duration(a.cfg.Get().Providers.BBB.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	state := pollState{
		active:   map[string]string{},
		input:    meetingID,
		actualID: meetingID,
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if done := a.tick(ctx, &state); done {
				return
			}
		}
	}
}

type pollState struct {
	// input is the original token; actualID is swapped to the resolved BBB
	// meetingID on the first notFound that matches a Greenlight slug.
	input    string
	actualID string

	active             map[string]string
	meetingLive        bool
	observedNotRunning bool
}

func (a *Adapter) tick(ctx context.Context, state *pollState) bool {
	info, err := a.fetchMeetingInfo(ctx, state.actualID)
	if err != nil {
		slog.Warn("bbb: fetchMeetingInfo", "err", err)
		return false
	}

	if info.ReturnCode != "SUCCESS" && !state.meetingLive {
		resolved, rerr := a.resolveSlug(ctx, state.input)
		if rerr != nil {
			slog.Warn("bbb: resolve greenlight slug", "input", state.input, "err", rerr)
			return false
		}
		if resolved == "" || resolved == state.actualID {
			state.observedNotRunning = true
			return false
		}
		slog.Info("bbb: resolved greenlight slug", "slug", state.input, "meeting_id", resolved)
		state.actualID = resolved
		info, err = a.fetchMeetingInfo(ctx, state.actualID)
		if err != nil {
			slog.Warn("bbb: fetchMeetingInfo", "err", err)
			return false
		}
	}

	meetingID := state.actualID
	isRunning := info.ReturnCode == "SUCCESS" && info.Running

	if !state.meetingLive {
		if !isRunning {
			state.observedNotRunning = true
		} else {
			state.meetingLive = true
			// Seeing "running" without ever having seen "not running" means we
			// attached after the meeting had already started.
			midMeeting := !state.observedNotRunning
			ts := time.Now().UTC()
			if !midMeeting && info.StartTime > 0 {
				ts = time.UnixMilli(info.StartTime).UTC()
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
			ts := time.Now().UTC()
			if info.EndTime > 0 {
				ts = time.UnixMilli(info.EndTime).UTC()
			}
			a.emit(providers.Event{
				Kind:      providers.EventKindMeetingEnded,
				MeetingID: meetingID,
				Timestamp: ts,
			})
			return true
		}
	}

	return false
}

// resolveSlug returns "" with no error when no live meeting matches yet.
// The "gl-" prefix covers Greenlight v2; bbb-context-id covers v3 where
// meetingID is a SecureRandom string unrelated to the friendly_id.
func (a *Adapter) resolveSlug(ctx context.Context, slug string) (string, error) {
	meetings, err := a.fetchMeetings(ctx)
	if err != nil {
		return "", err
	}
	prefixed := "gl-" + slug
	for _, m := range meetings.Meetings.List {
		if m.MeetingID == slug || m.MeetingID == prefixed {
			return m.MeetingID, nil
		}
		for _, md := range m.Metadata.Items {
			if md.XMLName.Local == "bbb-context-id" && strings.TrimSpace(md.Value) == slug {
				return m.MeetingID, nil
			}
		}
	}
	return "", nil
}

func (a *Adapter) fetchMeetings(ctx context.Context) (bbbMeetingsResponse, error) {
	bbb := a.cfg.Get().Providers.BBB
	apiURL := bbbAPIURL(bbb.BaseURL, bbb.SharedSecret, "getMeetings", "")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return bbbMeetingsResponse{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return bbbMeetingsResponse{}, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var meetings bbbMeetingsResponse
	if err := xml.NewDecoder(resp.Body).Decode(&meetings); err != nil {
		return bbbMeetingsResponse{}, fmt.Errorf("decode XML: %w", err)
	}
	if meetings.ReturnCode != "SUCCESS" {
		return bbbMeetingsResponse{}, fmt.Errorf("getMeetings returncode %q", meetings.ReturnCode)
	}
	return meetings, nil
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
	base := strings.TrimRight(baseURL, "/") + "/bigbluebutton/api/" + action
	checksum := bbbChecksum(action, params, sharedSecret)
	if params != "" {
		return base + "?" + params + "&checksum=" + checksum
	}
	return base + "?checksum=" + checksum
}

func bbbChecksum(action, params, secret string) string {
	h := sha1.New() //nolint:gosec // BBB API uses SHA-1 by specification
	_, _ = h.Write([]byte(action + params + secret))
	return hex.EncodeToString(h.Sum(nil))
}
