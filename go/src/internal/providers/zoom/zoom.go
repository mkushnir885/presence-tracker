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

var zoomScopes = []string{
	"dashboard:read:list_meeting_participants:admin",
}

type Adapter struct {
	cfg    *config.Config
	client *http.Client
	events chan providers.Event
}

func New(cfg *config.Config) *Adapter {
	return &Adapter{
		cfg:    cfg,
		events: make(chan providers.Event, 64),
	}
}

func (a *Adapter) Name() string { return "zoom" }

func (*Adapter) ParseMeetingID(input string) (string, error) {
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

func (a *Adapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	go a.pollLoop(ctx, meetingID)
	return a.events, nil
}

type zoomParticipant struct {
	participantUUID string
	name            string
}

const zoomStatusInWaitingRoom = "in_waiting_room"

func (a *Adapter) pollLoop(ctx context.Context, meetingID string) {
	defer close(a.events)

	interval := time.Duration(a.cfg.Get().Providers.Zoom.PollIntervalSeconds) * time.Second
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
	active          map[string]string
	meetingLive     bool
	observedNotLive bool
}

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
			id := p.participantUUID
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
			return nil, false, nil
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, false, fmt.Errorf("unexpected status %d", resp.StatusCode)
		}

		var body struct {
			NextPageToken string `json:"next_page_token"`
			Participants  []struct {
				ParticipantUUID string `json:"participant_uuid"`
				UserName        string `json:"user_name"`
				LeaveTime       string `json:"leave_time"`
				Status          string `json:"status"`
			} `json:"participants"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()
			return nil, false, fmt.Errorf("decode: %w", err)
		}
		_ = resp.Body.Close()

		// type=live returns every participant session during the meeting,
		// including ones who already left or are still in the waiting room.
		// Keep only sessions that are currently active in the meeting itself.
		for _, p := range body.Participants {
			if p.LeaveTime != "" || p.Status == zoomStatusInWaitingRoom {
				continue
			}
			all = append(all, zoomParticipant{
				participantUUID: p.ParticipantUUID,
				name:            p.UserName,
			})
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
