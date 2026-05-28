package zoom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/providers"
	providersoauth "presence-tracker/src/internal/providers/oauth"
)

const (
	zoomAuthURL  = "https://zoom.us/oauth/authorize"
	zoomTokenURL = "https://zoom.us/oauth/token" //nolint:gosec // G101: OAuth token endpoint URL, not a credential
	zoomAPIBase  = "https://api.zoom.us/v2"
)

// dashboard_meetings:read:admin requires account-admin authorisation on a
// Zoom Pro plan or higher.
var zoomScopes = []string{"meeting:read:meeting", "dashboard_meetings:read:admin"}

// Adapter polls the Zoom Dashboard API for live participant state.
type Adapter struct {
	cfg    *config.Config
	client *http.Client
	events chan providers.Event
}

// New creates a Zoom poll adapter. OAuth tokens are persisted under
// config.DataDir().
func New(cfg *config.Config) *Adapter {
	return &Adapter{
		cfg:    cfg,
		events: make(chan providers.Event, 64),
	}
}

func (a *Adapter) Name() string { return "zoom" }

// ParseMeetingID accepts either a bare Zoom meeting ID or a Zoom join URL
// and returns the canonical numeric meeting ID used by the Dashboard API.
// Recognised shapes:
//
//   - a bare ID with optional embedded spaces (the Zoom UI displays meeting
//     numbers with visual grouping, e.g. "123 4567 890")
//   - a join URL with /j/<id>, /s/<id>, /w/<id>, or /wc/join/<id> in the path
//
// Personal meeting room URLs (/my/<vanity>) are rejected because they cannot
// be resolved to a meeting ID without an extra API call.
func (a *Adapter) ParseMeetingID(input string) (string, error) { return ParseMeetingID(input) }

// ParseMeetingID is the package-level form of [Adapter.ParseMeetingID].
func ParseMeetingID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("zoom: empty meeting input")
	}

	if !strings.ContainsAny(input, "/?:") {
		id := strings.Join(strings.Fields(input), "")
		if id == "" {
			return "", errors.New("zoom: empty meeting input")
		}
		return id, nil
	}

	u, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("zoom: parse meeting input %q: %w", input, err)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		switch p {
		case "j", "s", "w":
			if i+1 < len(parts) && parts[i+1] != "" {
				return parts[i+1], nil
			}
		case "wc":
			if i+2 < len(parts) && parts[i+1] == "join" && parts[i+2] != "" {
				return parts[i+2], nil
			}
		}
	}

	return "", fmt.Errorf("zoom: cannot extract meeting ID from %q", input)
}

// Authenticate runs the PKCE OAuth flow with the
// dashboard_meetings:read:admin scope. The authorising account must be a
// Zoom account admin.
func (a *Adapter) Authenticate(ctx context.Context) error {
	zoom := a.cfg.Get().Providers.Zoom
	oauthCfg := providersoauth.Config{
		ClientID:     zoom.OAuth.ClientID,
		AuthURL:      zoomAuthURL,
		TokenURL:     zoomTokenURL,
		Scopes:       zoomScopes,
		RedirectPort: zoom.OAuth.RedirectPort,
		TokenFile:    filepath.Join(config.DataDir(), "zoom_oauth.json"),
	}
	client, err := providersoauth.AuthorizedClient(ctx, oauthCfg)
	if err != nil {
		return fmt.Errorf("zoom: authenticate: %w", err)
	}
	a.client = client
	return nil
}

// Subscribe starts polling the Zoom Dashboard API and returns a channel of
// events. The channel is closed when the meeting ends or ctx is cancelled.
func (a *Adapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	go a.pollLoop(ctx, meetingID)
	return a.events, nil
}

type zoomParticipant struct {
	id    string // participantUUID
	name  string
	email string
}

func (a *Adapter) pollLoop(ctx context.Context, meetingID string) {
	defer close(a.events)

	interval := time.Duration(a.cfg.Get().Providers.Zoom.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	state := pollState{
		active: map[string]string{}, // platformID → displayName
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

// pollState carries cross-tick bookkeeping for pollLoop. observedNotLive is
// set the first time a poll finds the meeting not live before it goes live,
// distinguishing "watched the meeting start" from "attached mid-meeting".
type pollState struct {
	active          map[string]string
	meetingLive     bool
	observedNotLive bool
}

// tick performs one poll cycle. Returns true when the meeting has ended.
func (a *Adapter) tick(ctx context.Context, meetingID string, state *pollState) bool {
	participants, live, err := a.fetchParticipants(ctx, meetingID)
	if err != nil {
		slog.Warn("zoom: fetch participants", "err", err)
		return false
	}

	if !state.meetingLive {
		if !live {
			state.observedNotLive = true
		} else {
			state.meetingLive = true
			a.emit(providers.Event{
				Kind:              providers.EventKindMeetingStarted,
				MeetingID:         meetingID,
				Timestamp:         time.Now().UTC(),
				MeetingInProgress: !state.observedNotLive,
			})
		}
	}

	if state.meetingLive {
		current := map[string]string{}
		for _, p := range participants {
			id := p.email
			if id == "" {
				id = p.id
			}
			current[id] = p.name

			if _, seen := state.active[id]; !seen {
				a.emit(providers.Event{
					Kind:        providers.EventKindParticipantJoined,
					MeetingID:   meetingID,
					PlatformID:  id,
					DisplayName: p.name,
					Timestamp:   time.Now().UTC(),
				})
				state.active[id] = p.name
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

		if !live {
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

// fetchParticipants calls the Zoom Dashboard API for live participants.
// live is false when the meeting is not yet started or has ended; in that
// case the returned slice is empty and err is nil.
func (a *Adapter) fetchParticipants(ctx context.Context, meetingID string) ([]zoomParticipant, bool, error) {
	var all []zoomParticipant
	pageToken := ""

	for {
		u := fmt.Sprintf("%s/metrics/meetings/%s/participants?type=live&page_size=300", zoomAPIBase, meetingID)
		if pageToken != "" {
			u += "&next_page_token=" + pageToken
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, false, fmt.Errorf("build request: %w", err)
		}
		resp, err := a.client.Do(req)
		if err != nil {
			return nil, false, fmt.Errorf("request: %w", err)
		}

		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return nil, false, nil // meeting not live
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, false, fmt.Errorf("unexpected status %d", resp.StatusCode)
		}

		var body struct {
			NextPageToken string `json:"next_page_token"`
			Participants  []struct {
				ID       string `json:"id"`
				UserName string `json:"user_name"`
				Email    string `json:"email"`
			} `json:"participants"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()
			return nil, false, fmt.Errorf("decode: %w", err)
		}
		_ = resp.Body.Close()

		for _, p := range body.Participants {
			all = append(all, zoomParticipant{id: p.ID, name: p.UserName, email: p.Email})
		}

		if body.NextPageToken == "" {
			break
		}
		pageToken = body.NextPageToken
	}

	return all, true, nil
}

func (a *Adapter) emit(evt providers.Event) {
	select {
	case a.events <- evt:
	default:
		slog.Warn("zoom: event channel full, dropping event", "kind", evt.Kind)
	}
}
