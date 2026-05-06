package meet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"presence-tracker/src/internal/config"
	providersoauth "presence-tracker/src/internal/providers/oauth"

	"presence-tracker/src/internal/providers"
)

const (
	meetAPIBase = "https://meet.googleapis.com/v2"
	authURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenURL    = "https://oauth2.googleapis.com/token"
)

var meetScopes = []string{
	"https://www.googleapis.com/auth/meetings.space.readonly",
}

// Adapter is the Google Meet provider.
type Adapter struct {
	cfg     *config.MeetConfig
	dataDir string
	client  *http.Client
	events  chan providers.Event
}

// New creates a Meet adapter. dataDir is used for OAuth token persistence.
func New(cfg *config.MeetConfig, dataDir string) *Adapter {
	return &Adapter{
		cfg:     cfg,
		dataDir: dataDir,
		events:  make(chan providers.Event, 64),
	}
}

func (a *Adapter) Name() string { return "meet" }

// Authenticate runs the PKCE OAuth flow if no valid token is stored, then
// verifies API access by listing spaces.
func (a *Adapter) Authenticate(ctx context.Context) error {
	oauthCfg := providersoauth.Config{
		ClientID:     a.cfg.OAuth.ClientID,
		ClientSecret: a.cfg.OAuth.ClientSecret,
		AuthURL:      authURL,
		TokenURL:     tokenURL,
		Scopes:       meetScopes,
		RedirectPort: a.cfg.OAuth.RedirectPort,
		TokenFile:    filepath.Join(a.dataDir, "meet_oauth.json"),
	}
	client, err := providersoauth.AuthorizedClient(ctx, oauthCfg)
	if err != nil {
		return fmt.Errorf("meet: authenticate: %w", err)
	}
	a.client = client

	// Verify access with a lightweight API call.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		meetAPIBase+"/spaces", nil)
	if err != nil {
		return fmt.Errorf("meet: verify access: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("meet: verify access: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("meet: API access denied (status %d); check OAuth scopes and client_id", resp.StatusCode)
	}
	return nil
}

// Subscribe begins polling the Meet API for the meeting identified by meetingID.
// meetingID may be a meeting code (e.g. "abc-defg-hij") or a space name
// (e.g. "spaces/jQCFfuBOdN5z"). The returned channel is closed when the meeting
// ends or ctx is cancelled.
func (a *Adapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	spaceName, err := a.resolveSpace(ctx, meetingID)
	if err != nil {
		return nil, fmt.Errorf("meet: resolve space %q: %w", meetingID, err)
	}
	slog.Info("meet: resolved meeting space", "space", spaceName)

	go a.pollLoop(ctx, spaceName)
	return a.events, nil
}

// FetchPostMeeting is not implemented for Meet.
func (a *Adapter) FetchPostMeeting(_ context.Context, _ string) ([]providers.Event, error) {
	return nil, nil
}

// resolveSpace turns a meeting code or space alias into a canonical space name.
func (a *Adapter) resolveSpace(ctx context.Context, meetingID string) (string, error) {
	if strings.HasPrefix(meetingID, "spaces/") {
		return meetingID, nil
	}
	// Look up the space by alias (the meeting code).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		meetAPIBase+"/spaces/"+meetingID, nil)
	if err != nil {
		return "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("space not found; check the meeting code")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var space struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&space); err != nil {
		return "", fmt.Errorf("parse space: %w", err)
	}
	return space.Name, nil
}

func (a *Adapter) pollLoop(ctx context.Context, spaceName string) {
	defer close(a.events)

	interval := time.Duration(a.cfg.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// activeParticipants tracks participants currently in the meeting.
	// key = participant resource name, value = display name at join time.
	activeParticipants := map[string]string{}
	var currentRecord string // conferenceRecord resource name

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Find the active conference record for the space.
		if currentRecord == "" {
			record, err := a.findActiveRecord(ctx, spaceName)
			if err != nil {
				slog.Warn("meet: poll: find active record", "err", err)
				continue
			}
			if record == "" {
				continue // meeting not started yet
			}
			currentRecord = record
			slog.Info("meet: meeting started", "record", currentRecord)
			a.send(ctx, providers.Event{
				Kind:      providers.EventKindMeetingStarted,
				MeetingID: spaceName,
				Timestamp: time.Now(),
			})
		}

		// Poll participants in the current record.
		participants, err := a.listParticipants(ctx, currentRecord)
		if err != nil {
			slog.Warn("meet: poll: list participants", "err", err)
			continue
		}

		currentIDs := map[string]struct{}{}
		for _, p := range participants {
			currentIDs[p.name] = struct{}{}
			if _, seen := activeParticipants[p.name]; !seen {
				// New participant.
				activeParticipants[p.name] = p.displayName
				a.send(ctx, providers.Event{
					Kind:        providers.EventKindParticipantJoined,
					MeetingID:   spaceName,
					PlatformID:  p.platformID,
					DisplayName: p.displayName,
					Timestamp:   p.joinTime,
				})
			}
		}

		// Detect participants who left (present before, absent now).
		for id, name := range activeParticipants {
			if _, present := currentIDs[id]; !present {
				delete(activeParticipants, id)
				a.send(ctx, providers.Event{
					Kind:        providers.EventKindParticipantLeft,
					MeetingID:   spaceName,
					DisplayName: name,
					Timestamp:   time.Now(),
				})
			}
		}

		// Check if the meeting has ended.
		ended, err := a.isRecordEnded(ctx, currentRecord)
		if err != nil {
			slog.Warn("meet: poll: check record end", "err", err)
			continue
		}
		if ended {
			slog.Info("meet: meeting ended", "record", currentRecord)
			a.send(ctx, providers.Event{
				Kind:      providers.EventKindMeetingEnded,
				MeetingID: spaceName,
				Timestamp: time.Now(),
			})
			return
		}
	}
}

func (a *Adapter) send(ctx context.Context, evt providers.Event) {
	select {
	case a.events <- evt:
	case <-ctx.Done():
	default:
		slog.Warn("meet: event channel full, dropping event", "kind", evt.Kind)
	}
}

type participantInfo struct {
	name        string // conferenceRecords/.../participants/... resource name
	platformID  string // user resource name or anonymous ID
	displayName string
	joinTime    time.Time
}

// findActiveRecord returns the name of the active conferenceRecord for the space,
// or "" if the meeting has not started.
func (a *Adapter) findActiveRecord(ctx context.Context, spaceName string) (string, error) {
	filter := fmt.Sprintf("space.name=\"%s\" AND end_time IS NULL", spaceName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		meetAPIBase+"/conferenceRecords?filter="+encodeFilter(filter), nil)
	if err != nil {
		return "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var result struct {
		ConferenceRecords []struct {
			Name string `json:"name"`
		} `json:"conferenceRecords"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("parse conference records: %w", err)
	}
	if len(result.ConferenceRecords) == 0 {
		return "", nil
	}
	return result.ConferenceRecords[0].Name, nil
}

// listParticipants returns participants currently in the conference record
// (those without a latestEndTime).
func (a *Adapter) listParticipants(ctx context.Context, recordName string) ([]participantInfo, error) {
	// Filter to participants still present (no end time yet).
	filter := "latest_end_time IS NULL"
	url := meetAPIBase + "/" + recordName + "/participants?filter=" + encodeFilter(filter)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Participants []struct {
			Name             string `json:"name"`
			EarliestStartTime string `json:"earliestStartTime"`
			User             struct {
				SignedInUser *struct {
					User        string `json:"user"`
					DisplayName string `json:"displayName"`
				} `json:"signedinUser"`
				AnonymousUser *struct {
					DisplayName string `json:"displayName"`
				} `json:"anonymousUser"`
			} `json:"user"`
		} `json:"participants"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse participants: %w", err)
	}

	out := make([]participantInfo, 0, len(result.Participants))
	for _, p := range result.Participants {
		info := participantInfo{name: p.Name}
		joinTime, _ := time.Parse(time.RFC3339, p.EarliestStartTime)
		if joinTime.IsZero() {
			joinTime = time.Now()
		}
		info.joinTime = joinTime

		if su := p.User.SignedInUser; su != nil {
			info.platformID = su.User
			info.displayName = su.DisplayName
		} else if au := p.User.AnonymousUser; au != nil {
			info.platformID = p.Name // use resource name as stable ID for anonymous users
			info.displayName = au.DisplayName
		}
		out = append(out, info)
	}
	return out, nil
}

// isRecordEnded reports whether the conference record has an end_time set.
func (a *Adapter) isRecordEnded(ctx context.Context, recordName string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		meetAPIBase+"/"+recordName, nil)
	if err != nil {
		return false, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status %d", resp.StatusCode)
	}
	var record struct {
		EndTime string `json:"endTime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return false, fmt.Errorf("parse record: %w", err)
	}
	return record.EndTime != "", nil
}

// encodeFilter percent-encodes the filter string for use in a query parameter.
func encodeFilter(filter string) string {
	return strings.ReplaceAll(strings.ReplaceAll(filter, " ", "%20"), "\"", "%22")
}
