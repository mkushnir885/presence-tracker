package zoom

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/providers"
	providersoauth "presence-tracker/src/internal/providers/oauth"
)

const zoomAPIBase = "https://api.zoom.us/v2"

// zoomPollScopes includes dashboard_meetings:read:admin, which requires account-admin
// authorisation on a Zoom Pro plan or higher.
var zoomPollScopes = []string{"meeting:read", "dashboard_meetings:read:admin"}

// pollAdapter implements Provider by polling the Zoom Dashboard API for live
// participant data. It requires no public address but does require a Pro (or
// higher) Zoom plan and an OAuth authorisation by an account admin.
type pollAdapter struct {
	cfg     *config.ZoomConfig
	dataDir string
	client  *http.Client
	events  chan providers.Event
}

func newPollAdapter(cfg *config.ZoomConfig, dataDir string) *pollAdapter {
	return &pollAdapter{
		cfg:     cfg,
		dataDir: dataDir,
		events:  make(chan providers.Event, 64),
	}
}

func (a *pollAdapter) Name() string { return "zoom" }

// Authenticate runs the PKCE OAuth flow with the additional
// dashboard_meetings:read:admin scope. The authorising account must be a Zoom
// account admin. Tokens are stored separately from the webhook adapter's tokens
// because the scope set differs.
func (a *pollAdapter) Authenticate(ctx context.Context) error {
	oauthCfg := providersoauth.Config{
		ClientID:     a.cfg.OAuth.ClientID,
		AuthURL:      zoomAuthURL,
		TokenURL:     zoomTokenURL,
		Scopes:       zoomPollScopes,
		RedirectPort: a.cfg.OAuth.RedirectPort,
		TokenFile:    filepath.Join(a.dataDir, "zoom_poll_oauth.json"),
	}
	client, err := providersoauth.AuthorizedClient(ctx, oauthCfg)
	if err != nil {
		return fmt.Errorf("zoom poll: authenticate: %w", err)
	}
	a.client = client
	return nil
}

// Subscribe starts polling the Zoom Dashboard API and returns a channel of events.
// The channel is closed when the meeting ends or ctx is cancelled.
func (a *pollAdapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	go a.pollLoop(ctx, meetingID)
	return a.events, nil
}

// FetchPostMeeting is not implemented for the Zoom poll adapter.
func (a *pollAdapter) FetchPostMeeting(_ context.Context, _ string) ([]providers.Event, error) {
	return nil, nil
}

type pollParticipant struct {
	id    string // participantUUID
	name  string
	email string
}

func (a *pollAdapter) pollLoop(ctx context.Context, meetingID string) {
	defer close(a.events)

	interval := time.Duration(a.cfg.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	active := map[string]string{} // platformID → displayName
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

// tick performs one poll cycle. Returns true when the meeting has ended.
func (a *pollAdapter) tick(ctx context.Context, meetingID string, active map[string]string, meetingLive *bool) bool {
	participants, live, err := a.fetchParticipants(ctx, meetingID)
	if err != nil {
		slog.Warn("zoom poll: fetch participants", "err", err)
		return false
	}

	if !*meetingLive && live {
		*meetingLive = true
		a.emit(providers.Event{
			Kind:      providers.EventKindMeetingStarted,
			MeetingID: meetingID,
			Timestamp: time.Now().UTC(),
		})
	}

	if *meetingLive {
		current := map[string]string{}
		for _, p := range participants {
			id := p.email
			if id == "" {
				id = p.id
			}
			current[id] = p.name

			if _, seen := active[id]; !seen {
				a.emit(providers.Event{
					Kind:        providers.EventKindParticipantJoined,
					MeetingID:   meetingID,
					PlatformID:  id,
					DisplayName: p.name,
					Timestamp:   time.Now().UTC(),
				})
				active[id] = p.name
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
func (a *pollAdapter) fetchParticipants(ctx context.Context, meetingID string) ([]pollParticipant, bool, error) {
	var all []pollParticipant
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
			all = append(all, pollParticipant{id: p.ID, name: p.UserName, email: p.Email})
		}

		if body.NextPageToken == "" {
			break
		}
		pageToken = body.NextPageToken
	}

	return all, true, nil
}

func (a *pollAdapter) emit(evt providers.Event) {
	select {
	case a.events <- evt:
	default:
		slog.Warn("zoom poll: event channel full, dropping event", "kind", evt.Kind)
	}
}
