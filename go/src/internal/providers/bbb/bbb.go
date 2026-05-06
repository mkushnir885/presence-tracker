package bbb

import (
	"context"
	"crypto/sha1" //nolint:gosec // BBB API uses SHA-1 by specification
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/providers"
)

// Adapter is the BigBlueButton provider.
type Adapter struct {
	cfg        *config.BBBConfig
	httpClient *http.Client
	httpServer *http.Server
	events     chan providers.Event
	cancel     context.CancelFunc
}

// New creates a BBB adapter from the given configuration.
// If cfg.Mode is "poll", it returns a polling adapter that requires no public address.
// Otherwise it returns a webhook adapter (default).
func New(cfg *config.BBBConfig) providers.Provider {
	if cfg.Mode == "poll" {
		return newPollAdapter(cfg)
	}
	return newWebhookAdapter(cfg)
}

func newWebhookAdapter(cfg *config.BBBConfig) *Adapter {
	return &Adapter{
		cfg:        cfg,
		httpClient: newHTTPClient(cfg),
		events:     make(chan providers.Event, 64),
	}
}

// newHTTPClient builds an HTTP client respecting the TLSSkipVerify setting.
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
	u := a.apiURL("getMeetings", "")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("bbb: authenticate: %w", err)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("bbb: authenticate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bbb: authenticate: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// Subscribe registers a BBB webhook for meetingID, starts the local HTTP
// listener on the configured webhook_port, and returns a channel of events.
// The channel is closed when the meeting ends or ctx is cancelled.
func (a *Adapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	ctx, cancel := context.WithCancel(ctx)
	a.cancel = cancel

	addr := fmt.Sprintf(":%d", a.cfg.WebhookPort)
	ln, err := net.Listen("tcp", addr) //nolint:noctx // net.Listen is a synchronous bind; context cancellation is handled via the shutdown goroutine below
	if err != nil {
		cancel()
		return nil, fmt.Errorf("bbb: listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/bbb/webhook", a.handleWebhook)
	a.httpServer = &http.Server{Handler: mux}

	go func() {
		if err := a.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("bbb: webhook server error", "err", err)
		}
	}()

	// Shut down the HTTP server when ctx is cancelled.
	go func() { //nolint:gosec // G118: goroutine outlives the request context intentionally for graceful shutdown
		<-ctx.Done()
		// ctx is cancelled here; use a fresh timeout for graceful shutdown.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = a.httpServer.Shutdown(shutCtx) //nolint:contextcheck
		close(a.events)
	}()

	// Register the webhook with BBB.
	host := a.cfg.WebhookHost
	if host == "" {
		host = "localhost"
	}
	callbackURL := fmt.Sprintf("http://%s:%d/bbb/webhook", host, a.cfg.WebhookPort)
	if err := a.registerHook(ctx, meetingID, callbackURL); err != nil {
		// Non-fatal: log and continue. Events from API polling could be added as fallback.
		slog.Warn("bbb: hook registration failed — no real-time events will arrive", "err", err)
	}

	// If the meeting is already running, emit a MeetingStarted event carrying the
	// actual creation time so the coordinator can correct the file name and log timestamp.
	if t, err := a.getMeetingCreateTime(ctx, meetingID); err != nil {
		slog.Warn("bbb: could not fetch meeting start time", "err", err)
	} else if !t.IsZero() {
		slog.Info("bbb: meeting already running", "started_at", t)
		select {
		case a.events <- providers.Event{
			Kind:      providers.EventKindMeetingStarted,
			MeetingID: meetingID,
			Timestamp: t,
		}:
		default:
		}
	}

	return a.events, nil
}

// FetchPostMeeting returns events collected via the BBB recordings API.
// TODO: implement recordings-based event retrieval for crash recovery.
func (a *Adapter) FetchPostMeeting(_ context.Context, _ string) ([]providers.Event, error) {
	return nil, nil
}

// handleWebhook processes incoming BBB webhook POSTs.
func (a *Adapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// TODO: validate BBB webhook checksum for production deployments.

	rawEvent := r.FormValue("event")
	if rawEvent == "" {
		// Some BBB versions send JSON body instead of form-encoded.
		var body struct {
			Event string `json:"event"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			rawEvent = body.Event
		}
	}

	var eventList []bbbEventWrapper
	if err := json.Unmarshal([]byte(rawEvent), &eventList); err != nil {
		slog.Warn("bbb: could not parse webhook payload", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	for _, wrapper := range eventList {
		if evt, ok := a.convertEvent(wrapper); ok {
			select {
			case a.events <- evt:
			default:
				slog.Warn("bbb: event channel full, dropping event", "type", wrapper.Data.ID)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// BBB webhook JSON structures.
type bbbEventWrapper struct {
	Data bbbEventData `json:"data"`
}

type bbbEventData struct {
	ID         string        `json:"id"`
	Attributes bbbAttributes `json:"attributes"`
	Event      bbbEventMeta  `json:"event"`
}

type bbbAttributes struct {
	Meeting bbbMeeting `json:"meeting"`
	User    bbbUser    `json:"user"`
	Message bbbMessage `json:"message"`
}

type bbbMessage struct {
	Message struct {
		Text string `json:"text"`
	} `json:"message"`
}

type bbbMeeting struct {
	InternalMeetingID string `json:"internal-meeting-id"`
	ExternalMeetingID string `json:"external-meeting-id"`
}

type bbbUser struct {
	InternalUserID string `json:"internal-user-id"`
	Name           string `json:"name"`
	Role           string `json:"role"`
}

type bbbEventMeta struct {
	TS int64 `json:"ts"` // Unix milliseconds
}

func (a *Adapter) convertEvent(w bbbEventWrapper) (providers.Event, bool) {
	ts := time.UnixMilli(w.Data.Event.TS).UTC()
	meetingID := w.Data.Attributes.Meeting.ExternalMeetingID
	if meetingID == "" {
		meetingID = w.Data.Attributes.Meeting.InternalMeetingID
	}
	user := w.Data.Attributes.User

	switch w.Data.ID {
	case "user-joined":
		return providers.Event{
			Kind:        providers.EventKindParticipantJoined,
			MeetingID:   meetingID,
			PlatformID:  user.InternalUserID,
			DisplayName: user.Name,
			Timestamp:   ts,
			Extra:       map[string]string{"role": user.Role},
		}, true
	case "user-left":
		return providers.Event{
			Kind:       providers.EventKindParticipantLeft,
			MeetingID:  meetingID,
			PlatformID: user.InternalUserID,
			Timestamp:  ts,
		}, true
	case "meeting-ended":
		return providers.Event{
			Kind:      providers.EventKindMeetingEnded,
			MeetingID: meetingID,
			Timestamp: ts,
		}, true
	case "meeting-created":
		return providers.Event{
			Kind:      providers.EventKindMeetingStarted,
			MeetingID: meetingID,
			Timestamp: ts,
		}, true
	}
	return providers.Event{}, false
}

// registerHook calls the BBB hooks/create API endpoint.
func (a *Adapter) registerHook(ctx context.Context, meetingID, callbackURL string) error {
	params := "callbackURL=" + url.QueryEscape(callbackURL) +
		"&meetingID=" + url.QueryEscape(meetingID)
	apiURL := a.apiURL("hooks/create", params)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("bbb: hooks/create request: %w", err)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("bbb: hooks/create: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bbb: hooks/create: status %d", resp.StatusCode)
	}
	slog.Info("bbb: webhook registered", "meeting", meetingID, "callback", callbackURL)
	return nil
}

func (a *Adapter) apiURL(action, params string) string {
	return bbbAPIURL(a.cfg.BaseURL, a.cfg.SharedSecret, action, params)
}

// bbbAPIURL builds a signed BBB API URL for the given action and query parameters.
// params must be URL-encoded and NOT include a trailing &.
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

// getMeetingCreateTime calls the BBB getMeetingInfo API and returns the meeting's
// creation time. Returns a zero time (and nil error) if the meeting does not exist yet.
func (a *Adapter) getMeetingCreateTime(ctx context.Context, meetingID string) (time.Time, error) {
	params := "meetingID=" + url.QueryEscape(meetingID)
	apiURL := a.apiURL("getMeetingInfo", params)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("bbb: getMeetingInfo request: %w", err)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("bbb: getMeetingInfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		ReturnCode string `xml:"returncode"`
		CreateTime int64  `xml:"createTime"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		return time.Time{}, fmt.Errorf("bbb: getMeetingInfo parse: %w", err)
	}
	if result.ReturnCode != "SUCCESS" {
		return time.Time{}, nil // meeting not found or not yet started
	}
	return time.UnixMilli(result.CreateTime).UTC(), nil
}
