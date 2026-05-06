package zoom

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/providers"
	providersoauth "presence-tracker/src/internal/providers/oauth"
)

const (
	zoomAuthURL  = "https://zoom.us/oauth/authorize"
	zoomTokenURL = "https://zoom.us/oauth/token"
)

var zoomScopes = []string{"meeting:read:meeting", "meeting:read:participant"}

// Adapter is the Zoom provider.
type Adapter struct {
	cfg        *config.ZoomConfig
	dataDir    string
	httpServer *http.Server
	events     chan providers.Event
	cancel     context.CancelFunc
}

// New creates a Zoom adapter. dataDir is used for OAuth token persistence.
// If cfg.Mode is "poll", it returns a polling adapter that requires no public address
// but needs a Zoom Pro plan and an account-admin OAuth authorisation.
// Otherwise it returns a webhook adapter (default).
func New(cfg *config.ZoomConfig, dataDir string) providers.Provider {
	if cfg.Mode == "poll" {
		return newPollAdapter(cfg, dataDir)
	}
	return newWebhookAdapter(cfg, dataDir)
}

func newWebhookAdapter(cfg *config.ZoomConfig, dataDir string) *Adapter {
	return &Adapter{
		cfg:     cfg,
		dataDir: dataDir,
		events:  make(chan providers.Event, 64),
	}
}

func (a *Adapter) Name() string { return "zoom" }

// Authenticate runs the PKCE OAuth flow if no valid token is stored.
func (a *Adapter) Authenticate(ctx context.Context) error {
	oauthCfg := providersoauth.Config{
		ClientID:     a.cfg.OAuth.ClientID,
		AuthURL:      zoomAuthURL,
		TokenURL:     zoomTokenURL,
		Scopes:       zoomScopes,
		RedirectPort: a.cfg.OAuth.RedirectPort,
		TokenFile:    filepath.Join(a.dataDir, "zoom_oauth.json"),
	}
	_, err := providersoauth.EnsureToken(ctx, oauthCfg)
	if err != nil {
		return fmt.Errorf("zoom: authenticate: %w", err)
	}
	return nil
}

// Subscribe starts the local webhook HTTP server and returns a channel of events.
// The channel is closed when the meeting ends or ctx is cancelled.
//
// meetingID is the Zoom meeting number (e.g. "123456789"). It is used to filter
// incoming webhook events so that events from other meetings are ignored.
func (a *Adapter) Subscribe(ctx context.Context, meetingID string) (<-chan providers.Event, error) {
	ctx, cancel := context.WithCancel(ctx)
	a.cancel = cancel

	addr := fmt.Sprintf(":%d", a.cfg.WebhookPort)
	ln, err := net.Listen("tcp", addr) //nolint:noctx
	if err != nil {
		cancel()
		return nil, fmt.Errorf("zoom: listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/zoom/webhook", func(w http.ResponseWriter, r *http.Request) {
		a.handleWebhook(w, r, meetingID)
	})
	a.httpServer = &http.Server{Handler: mux}

	go func() {
		if err := a.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("zoom: webhook server error", "err", err)
		}
	}()

	go func() { //nolint:gosec
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = a.httpServer.Shutdown(shutCtx) //nolint:contextcheck
		close(a.events)
	}()

	host := a.cfg.WebhookHost
	if host == "" {
		host = "localhost"
	}
	// When webhook_host is a public domain (e.g. via Cloudflare Tunnel), Zoom
	// requires HTTPS on port 443 — the tunnel handles TLS termination. For
	// localhost the plain HTTP address is what Zoom actually calls.
	var publicURL string
	if host == "localhost" || host == "127.0.0.1" {
		publicURL = fmt.Sprintf("http://%s:%d/zoom/webhook", host, a.cfg.WebhookPort)
	} else {
		publicURL = fmt.Sprintf("https://%s/zoom/webhook", host)
	}
	slog.Info("zoom: webhook server listening",
		"addr", addr,
		"public_url", publicURL,
		"note", "register this URL in your Zoom app Event Subscriptions")

	return a.events, nil
}

// FetchPostMeeting is not implemented for Zoom.
func (a *Adapter) FetchPostMeeting(_ context.Context, _ string) ([]providers.Event, error) {
	return nil, nil
}

// handleWebhook processes incoming Zoom webhook POST requests.
func (a *Adapter) handleWebhook(w http.ResponseWriter, r *http.Request, meetingID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Zoom challenge-response handshake (sent when the endpoint is first registered).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if _, ok := raw["payload"]; !ok {
		// Could be a URL validation challenge.
		var challenge struct {
			Event   string `json:"event"`
			Payload struct {
				PlainToken string `json:"plainToken"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(body, &challenge); err == nil && challenge.Payload.PlainToken != "" {
			a.respondChallenge(w, challenge.Payload.PlainToken)
			return
		}
	}

	// Validate HMAC signature if a secret token is configured.
	if a.cfg.WebhookSecretToken != "" {
		if !a.validateSignature(r, body) {
			slog.Warn("zoom: webhook signature validation failed")
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	var payload zoomWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		slog.Warn("zoom: could not parse webhook payload", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Filter events not belonging to the tracked meeting.
	if payload.Payload.Object.ID != meetingID {
		w.WriteHeader(http.StatusOK)
		return
	}

	if evt, ok := a.convertEvent(payload); ok {
		select {
		case a.events <- evt:
		default:
			slog.Warn("zoom: event channel full, dropping event", "type", payload.Event)
		}

		// If meeting ended, stop the server after delivering the event.
		if evt.Kind == providers.EventKindMeetingEnded && a.cancel != nil {
			a.cancel()
		}
	}

	w.WriteHeader(http.StatusOK)
}

// respondChallenge handles Zoom's endpoint verification handshake.
func (a *Adapter) respondChallenge(w http.ResponseWriter, plainToken string) {
	mac := hmac.New(sha256.New, []byte(a.cfg.WebhookSecretToken))
	mac.Write([]byte(plainToken))
	encryptedToken := hex.EncodeToString(mac.Sum(nil))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"plainToken":     plainToken,
		"encryptedToken": encryptedToken,
	})
}

// validateSignature checks the x-zm-signature header using HMAC-SHA256.
func (a *Adapter) validateSignature(r *http.Request, body []byte) bool {
	sig := r.Header.Get("x-zm-signature")
	ts := r.Header.Get("x-zm-request-timestamp")
	if sig == "" || ts == "" {
		return false
	}
	// Zoom signature: "v0=" + HMAC-SHA256("v0:{timestamp}:{body}")
	msg := "v0:" + ts + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(a.cfg.WebhookSecretToken))
	mac.Write([]byte(msg))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

// Zoom webhook payload structures.
type zoomWebhookPayload struct {
	Event   string `json:"event"`
	EventTS int64  `json:"event_ts"` // Unix milliseconds
	Payload struct {
		AccountID string `json:"account_id"`
		Object    struct {
			ID       string `json:"id"`   // meeting number
			UUID     string `json:"uuid"` // meeting instance UUID
			Topic    string `json:"topic"`
			Duration int    `json:"duration"`
			// Participant is set for participant_joined/left events.
			Participant *zoomParticipant `json:"participant"`
		} `json:"object"`
	} `json:"payload"`
}

type zoomParticipant struct {
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	Email     string `json:"email"`
	JoinTime  string `json:"join_time"`
	LeaveTime string `json:"leave_time"`
	JoinReason string `json:"join_reason"`
}


func (a *Adapter) convertEvent(p zoomWebhookPayload) (providers.Event, bool) {
	ts := time.UnixMilli(p.EventTS).UTC()
	meetingID := p.Payload.Object.ID

	switch p.Event {
	case "meeting.started":
		return providers.Event{
			Kind:      providers.EventKindMeetingStarted,
			MeetingID: meetingID,
			Timestamp: ts,
			Extra:     map[string]string{"topic": p.Payload.Object.Topic},
		}, true

	case "meeting.ended":
		return providers.Event{
			Kind:      providers.EventKindMeetingEnded,
			MeetingID: meetingID,
			Timestamp: ts,
			Extra: map[string]string{
				"duration_seconds": fmt.Sprintf("%d", p.Payload.Object.Duration*60),
			},
		}, true

	case "meeting.participant_joined":
		if p.Payload.Object.Participant == nil {
			return providers.Event{}, false
		}
		par := p.Payload.Object.Participant
		platformID := par.Email
		if platformID == "" {
			platformID = par.UserID
		}
		joinTime, _ := time.Parse(time.RFC3339, par.JoinTime)
		if joinTime.IsZero() {
			joinTime = ts
		}
		return providers.Event{
			Kind:        providers.EventKindParticipantJoined,
			MeetingID:   meetingID,
			PlatformID:  platformID,
			DisplayName: par.UserName,
			Timestamp:   joinTime,
		}, true

	case "meeting.participant_left":
		if p.Payload.Object.Participant == nil {
			return providers.Event{}, false
		}
		par := p.Payload.Object.Participant
		platformID := par.Email
		if platformID == "" {
			platformID = par.UserID
		}
		leaveTime, _ := time.Parse(time.RFC3339, par.LeaveTime)
		if leaveTime.IsZero() {
			leaveTime = ts
		}
		return providers.Event{
			Kind:        providers.EventKindParticipantLeft,
			MeetingID:   meetingID,
			PlatformID:  platformID,
			DisplayName: par.UserName,
			Timestamp:   leaveTime,
		}, true

	}
	return providers.Event{}, false
}
