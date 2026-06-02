package meet

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
	"presence-tracker/src/internal/providers/polling"
)

const (
	meetAPIBase = "https://meet.googleapis.com/v2"
	authURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenURL    = "https://oauth2.googleapis.com/token" //nolint:gosec // G101: OAuth token endpoint URL, not a credential
)

const (
	Name        = "meet"
	DisplayName = "Google Meet"
)

func init() { providers.Register(Name, DisplayName) }

var meetScopes = []string{
	"https://www.googleapis.com/auth/meetings.space.readonly",
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

func (a *Adapter) Name() string        { return Name }
func (a *Adapter) DisplayName() string { return DisplayName }

func (*Adapter) ParseMeetingID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("meet: empty meeting input")
	}

	if strings.HasPrefix(input, "spaces/") {
		return input, nil
	}

	if !strings.ContainsAny(input, "/?:") {
		return input, nil
	}

	u, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("meet: parse meeting input %q: %w", input, err)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("meet: cannot extract meeting code from %q", input)
	}
	code := parts[0]
	switch code {
	case "lookup", "new", "landing", "_meet":
		return "", fmt.Errorf("meet: cannot extract meeting code from %q", input)
	}
	return code, nil
}

func (a *Adapter) Authenticate(ctx context.Context) error {
	meet := a.cfg.Get().Providers.Meet
	oauthCfg := providersoauth.Config{
		ClientID:     meet.OAuth.ClientID,
		ClientSecret: meet.OAuth.ClientSecret,
		AuthURL:      authURL,
		TokenURL:     tokenURL,
		Scopes:       meetScopes,
		RedirectPort: meet.OAuth.RedirectPort,
		TokenFile:    filepath.Join(config.DataDir(), "meet_oauth.json"),
	}
	client, err := providersoauth.AuthorizedClient(ctx, oauthCfg)
	if err != nil {
		return fmt.Errorf("meet: authenticate: %w", err)
	}
	a.client = client

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

func (a *Adapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	spaceName, err := a.resolveSpace(ctx, meetingID)
	if err != nil {
		return nil, fmt.Errorf("meet: resolve space %q: %w", meetingID, err)
	}
	slog.Info("meet: resolved meeting space", "space", spaceName)

	loop := &polling.Loop{
		Name:     "meet",
		Interval: time.Duration(a.cfg.Get().Providers.Meet.PollIntervalSeconds) * time.Second,
		Fetch:    a.newFetcher(spaceName),
		Events:   a.events,
	}
	go loop.Run(ctx)
	return a.events, nil
}

func (a *Adapter) resolveSpace(ctx context.Context, meetingID string) (string, error) {
	if strings.HasPrefix(meetingID, "spaces/") {
		return meetingID, nil
	}
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

func (a *Adapter) newFetcher(spaceName string) polling.Fetcher {
	var currentRecord string
	var recordStart time.Time
	return func(ctx context.Context) (polling.Snapshot, error) {
		if currentRecord == "" {
			record, startTime, err := a.findActiveRecord(ctx, spaceName)
			if err != nil {
				return polling.Snapshot{}, fmt.Errorf("find active record: %w", err)
			}
			if record == "" {
				return polling.Snapshot{}, nil
			}
			currentRecord = record
			recordStart = startTime
			slog.Info("meet: meeting started", "record", currentRecord, "start_time", startTime)
		}

		participants, err := a.listParticipants(ctx, currentRecord)
		if err != nil {
			return polling.Snapshot{}, fmt.Errorf("list participants: %w", err)
		}
		endTime, ended, err := a.recordEndTime(ctx, currentRecord)
		if err != nil {
			return polling.Snapshot{}, fmt.Errorf("check record end: %w", err)
		}

		snap := polling.Snapshot{
			Live:             !ended,
			MeetingStartedAt: recordStart,
		}
		for _, p := range participants {
			snap.Participants = append(snap.Participants, polling.Participant{
				ID:          p.name,
				DisplayName: p.displayName,
				JoinedAt:    p.joinTime,
			})
		}
		if ended {
			snap.MeetingEndedAt = endTime
			slog.Info("meet: meeting ended", "record", currentRecord, "end_time", endTime)
		}
		return snap, nil
	}
}

type participantInfo struct {
	name        string
	displayName string
	joinTime    time.Time
}

// Meet models a live meeting as a "conferenceRecord" on the space; the active
// one is the record with no end_time. There is no participant API until one
// exists, so polling starts here.
func (a *Adapter) findActiveRecord(ctx context.Context, spaceName string) (string, time.Time, error) {
	filter := fmt.Sprintf("space.name=\"%s\" AND end_time IS NULL", spaceName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		meetAPIBase+"/conferenceRecords?filter="+encodeFilter(filter), nil)
	if err != nil {
		return "", time.Time{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var result struct {
		ConferenceRecords []struct {
			Name      string `json:"name"`
			StartTime string `json:"startTime"`
		} `json:"conferenceRecords"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("parse conference records: %w", err)
	}
	if len(result.ConferenceRecords) == 0 {
		return "", time.Time{}, nil
	}
	rec := result.ConferenceRecords[0]
	startTime, _ := time.Parse(time.RFC3339, rec.StartTime)
	return rec.Name, startTime.UTC(), nil
}

func (a *Adapter) listParticipants(ctx context.Context, recordName string) ([]participantInfo, error) {
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
			Name              string `json:"name"`
			EarliestStartTime string `json:"earliestStartTime"`
			SignedInUser      *struct {
				User        string `json:"user"`
				DisplayName string `json:"displayName"`
			} `json:"signedInUser"`
			AnonymousUser *struct {
				DisplayName string `json:"displayName"`
			} `json:"anonymousUser"`
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
		info.joinTime = joinTime.UTC()

		if su := p.SignedInUser; su != nil {
			info.displayName = su.DisplayName
		} else if au := p.AnonymousUser; au != nil {
			info.displayName = au.DisplayName
		}
		out = append(out, info)
	}
	return out, nil
}

func (a *Adapter) recordEndTime(ctx context.Context, recordName string) (time.Time, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		meetAPIBase+"/"+recordName, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	// A vanished record (404) means the meeting is over.
	if resp.StatusCode == http.StatusNotFound {
		return time.Time{}, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, false, fmt.Errorf("status %d", resp.StatusCode)
	}
	var record struct {
		EndTime string `json:"endTime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return time.Time{}, false, fmt.Errorf("parse record: %w", err)
	}
	if record.EndTime == "" {
		return time.Time{}, false, nil
	}
	endTime, _ := time.Parse(time.RFC3339, record.EndTime)
	return endTime.UTC(), true, nil
}

func encodeFilter(filter string) string {
	return strings.ReplaceAll(strings.ReplaceAll(filter, " ", "%20"), "\"", "%22")
}
