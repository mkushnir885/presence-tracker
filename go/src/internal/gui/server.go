package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/google/uuid"

	"presence-tracker/src/internal/challenger"
	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/gui/views"
	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/providers"
	bbbprovider "presence-tracker/src/internal/providers/bbb"
	meetprovider "presence-tracker/src/internal/providers/meet"
	zoomprovider "presence-tracker/src/internal/providers/zoom"
	"presence-tracker/src/internal/ptrackpy"
	"presence-tracker/src/internal/session"
	"presence-tracker/src/internal/stats"
)

// Server is the GUI HTTP server for ptrack serve.
type Server struct {
	cfg      *config.Config
	registry participants.Registry
	router   *messengers.Router
	stats    *stats.Loader

	mu     sync.RWMutex
	active *activeSession

	// shutdownFn, when non-nil, is invoked by POST /system/shutdown to
	// stop the daemon process. cmd/ptrack wires it to the http.Server
	// listener-shutdown path.
	shutdownFn func()

	// stopCh is closed by SignalShutdown so the long-lived /events SSE
	// handlers can exit promptly instead of blocking http.Server.Shutdown
	// for the full grace period. stopOnce makes the close idempotent.
	stopOnce sync.Once
	stopCh   chan struct{}
}

// OnShutdown registers the callback POST /system/shutdown invokes after
// it has drained the active session. cmd/ptrack uses it to stop the
// http.Server and release the listener.
func (s *Server) OnShutdown(fn func()) { s.shutdownFn = fn }

// activeSession holds state for the currently running tracking session.
type activeSession struct {
	meetingID    string
	providerName string
	coord        *session.Coordinator
	challenger   *challenger.Service
	cancel       context.CancelFunc
	startedAt    time.Time
	done         chan struct{}
	buf          *logBuffer
}

// New creates a Server. The router owns the long-running messenger
// that handles registrations; the Server installs the active session's
// coordinator as the router's event handler while a session is running.
func New(cfg *config.Config, registry participants.Registry, router *messengers.Router) *Server {
	return &Server{
		cfg:      cfg,
		registry: registry,
		router:   router,
		stats:    stats.New(filepath.Join(config.CacheDir(), "stats")),
		stopCh:   make(chan struct{}),
	}
}

// SignalShutdown closes stopCh so any open /events SSE handlers
// return immediately. Safe to call multiple times; cmd/ptrack invokes
// it both from the shutdown button path and from the SIGINT/SIGTERM
// path so http.Server.Shutdown isn't held up by the long-poll
// handlers for its full grace period.
func (s *Server) SignalShutdown() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// RegisterRoutes attaches the GUI's HTML and htmx routes to mux. The HTTP
// server lifecycle is owned by the caller (cmd/ptrack).
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(views.Assets, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(sub)))

	mux.HandleFunc("GET /", s.handleHome)
	mux.HandleFunc("GET /meetings", s.handleMeetings)
	mux.HandleFunc("POST /session", s.handleStartSession)
	mux.HandleFunc("DELETE /session", s.handleStopSession)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /status/stream", s.handleStatusStream)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /report", s.handleReport)
	mux.HandleFunc("PATCH /participants/{p}/display-name", s.handleRenameParticipant)
	mux.HandleFunc("GET /registry", s.handleRegistry)
	mux.HandleFunc("POST /registry/filter", s.handleFilterRegistry)
	mux.HandleFunc("POST /registry/delete", s.handleDeleteRegistry)
	mux.HandleFunc("GET /questions/{id}", s.handleQuestion)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("POST /config", s.handleSaveConfig)
	mux.HandleFunc("GET /poll/pending/preview", s.handlePollPendingPreview)
	mux.HandleFunc("POST /poll/file", s.handlePollFile)
	mux.HandleFunc("POST /system/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /events", s.handleEvents)
}

// Coord returns the coordinator of the currently active session, or nil
// when no session is running. Used by cmd/ptrack to mount the poll
// handler with lazy access to the GUI-managed session.
func (s *Server) Coord() *session.Coordinator {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil
	}
	return s.active.coord
}

// Challenger returns the auto-generator of the currently active session,
// or nil when no session is running or auto-generation is disabled.
// Used by cmd/ptrack to mount the audio-segment handler.
func (s *Server) Challenger() *challenger.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil
	}
	return s.active.challenger
}

// handleHome renders the home page (Connect to a meeting form). When a
// session is already active it redirects to /status — there is no
// meaningful home action while tracking, and the live view is what the
// user wants.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()

	if act != nil {
		http.Redirect(w, r, "/status", http.StatusSeeOther)
		return
	}

	data := views.HomeData{
		EnabledProviders: enabledProviderOptions(s.cfg.Get().Providers),
	}
	locale := localeFromRequest(r)
	_ = views.Home(data, locale).Render(r.Context(), w)
}

// handleMeetings renders the Meeting files page, listing every Parquet
// file in the meetings directory. Sorting and filtering are done
// client-side; the server hands rows back sorted newest first.
func (s *Server) handleMeetings(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.cfg.Get().MeetingsDir)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, "Failed to list meetings: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var meetings []views.MeetingFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".parquet") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".parquet")
		meetings = append(meetings, views.MeetingFile{
			ID:      id,
			ModTime: info.ModTime(),
			SizeKB:  info.Size() / 1024,
		})
	}

	sort.Slice(meetings, func(i, j int) bool {
		return meetings[i].ModTime.After(meetings[j].ModTime)
	})

	locale := localeFromRequest(r)
	_ = views.Meetings(views.MeetingsData{Meetings: meetings}, locale).Render(r.Context(), w)
}

// handleStartSession starts a new tracking session.
func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}

	providerName := r.FormValue("provider")
	meetingID := r.FormValue("meeting_id")
	fileName := strings.TrimSpace(r.FormValue("file_name"))

	if providerName == "" || meetingID == "" {
		http.Error(w, "provider and meeting_id are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		http.Error(w, "session already active", http.StatusConflict)
		return
	}
	s.mu.Unlock()

	prov, err := buildServeProvider(providerName, s.cfg)
	if err != nil {
		http.Error(w, "provider error: "+err.Error(), http.StatusBadRequest)
		return
	}

	meetingID, err = providers.ParseMeetingID(prov, meetingID)
	if err != nil {
		http.Error(w, "meeting input: "+err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if err := prov.Authenticate(ctx); err != nil {
		http.Error(w, "provider auth failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	msgr := s.router.Messenger()

	internalMeetingID := uuid.Must(uuid.NewV7()).String()
	startTime := time.Now()

	store, err := eventstore.NewWriter(s.cfg.Get().MeetingsDir, fileName, startTime, s.cfg.Get().EventStore.Compression, s.cfg.Get().EventStore.RowGroupSize)
	if err != nil {
		status := http.StatusInternalServerError
		if fileName != "" {
			status = http.StatusBadRequest
		}
		http.Error(w, "event store error: "+err.Error(), status)
		return
	}

	sessCfg := session.Config{
		MeetingID:                   internalMeetingID,
		PlatformMeetingID:           meetingID,
		MeetingsDir:                 s.cfg.Get().MeetingsDir,
		QuestionsDir:                s.cfg.Get().QuestionsDir,
		ProviderName:                prov.Name(),
		AnswerWindowSecs:            s.cfg.Get().Challenges.Defaults.AnswerWindowSeconds,
		MinGapBetweenChallengesSecs: s.cfg.Get().Challenges.Defaults.MinGapBetweenChallengesSecs,
		EventStoreCompression:       s.cfg.Get().EventStore.Compression,
		RowGroupSize:                s.cfg.Get().EventStore.RowGroupSize,
	}

	coord := session.New(sessCfg, prov, msgr, s.registry, store)

	var chSvc *challenger.Service
	if ag := s.cfg.Get().Challenges.AutoGeneration; ag.Enabled {
		chSvc = challenger.New(ag, coord, coord)
		if err := chSvc.SweepReviewDir(); err != nil {
			slog.Warn("serve: sweep review_dir", "err", err)
		}
	}

	sessCtx, cancel := context.WithCancel(context.Background())
	buf := newLogBuffer(200, slog.Default().Handler())
	newHandler := slog.New(buf)
	prevDefault := slog.Default()
	slog.SetDefault(newHandler)

	done := make(chan struct{})

	act := &activeSession{
		meetingID:    meetingID,
		providerName: providerName,
		coord:        coord,
		challenger:   chSvc,
		cancel:       cancel,
		startedAt:    time.Now(),
		done:         done,
		buf:          buf,
	}

	s.mu.Lock()
	s.active = act
	s.mu.Unlock()

	s.router.SetHandler(coord)

	go func() {
		defer close(done)
		defer func() {
			s.router.SetHandler(nil)
			slog.SetDefault(prevDefault)
			s.mu.Lock()
			s.active = nil
			s.mu.Unlock()
		}()
		if err := coord.Run(sessCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("session ended with error", "err", err)
		}
	}()

	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

// handleStopSession cancels the active session.
func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	act := s.active
	s.mu.Unlock()

	if act == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	act.cancel()
	select {
	case <-act.done:
	case <-time.After(10 * time.Second):
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleStatus renders the live status page shell.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	data, ok := s.statusData()
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	locale := localeFromRequest(r)
	_ = views.Status(data, locale).Render(r.Context(), w)
}

// statusStreamTick is how often the SSE handler re-renders each
// region and compares it to the last sent payload. A 2 s tick keeps
// the perceived lag low for log lines and roster joins while leaving
// the network idle when nothing has changed (a render that matches
// the previous send is suppressed).
const statusStreamTick = 2 * time.Second

// handleStatusStream is the live-status SSE stream. The page renders
// itself once on GET /status; this stream then pushes finer-grained
// fragment updates as the underlying state changes:
//
//	started → the "Tracking/Meeting started at …" line in the header
//	body    → the entire #status-body (sent on the waiting↔live phase
//	          transition; rebuilds the controls and roster wrappers)
//	roster  → the two roster cards' contents
//	log     → the system log entries
//	pending → the auto-generated bank "Generate now" button state
//
// Each fragment is only emitted when it differs from the last value
// sent on this connection, so an idle session produces no events
// beyond the periodic SSE keep-alive comment. The handler returns when
// the request is cancelled, the daemon is shutting down, or the
// session has stopped (the session-ended event tells the client to
// navigate back to /).
func (s *Server) handleStatusStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	locale := localeFromRequest(r)
	ctx := r.Context()

	if _, err := io.WriteString(w, ": hello\n\n"); err != nil {
		return
	}
	flusher.Flush()

	render := func(c templ.Component) string {
		var buf strings.Builder
		if err := c.Render(ctx, &buf); err != nil {
			return ""
		}
		return buf.String()
	}

	phaseOf := func(data views.StatusData) string {
		if data.MeetingStartedAt.IsZero() {
			return "waiting"
		}
		return "live"
	}

	// Seed every region with the page's initial render so we only emit
	// on real changes — the first connect carries the same HTML that's
	// already in the DOM, so re-sending it would just churn the client.
	// lastPhase is tracked separately from the body fragment because
	// the body event is gated solely on the waiting↔live transition,
	// and sub-region events are responsible for everything else.
	var lastStarted, lastRoster, lastLog, lastPending, lastPhase string
	if data, ok := s.statusData(); ok {
		lastPhase = phaseOf(data)
		lastStarted = render(views.StatusStartedRow(data, locale))
		lastRoster = render(views.StatusRosters(data, locale))
		lastLog = render(views.StatusLog(data, locale))
		if data.AutoGenEnabled {
			lastPending = render(views.GenerateNowButton(data.PendingBank, locale))
		}
	}

	push := func() bool {
		data, ok := s.statusData()
		if !ok {
			writeSSEEvent(w, "session-ended", "ok")
			flusher.Flush()
			return false
		}

		phase := phaseOf(data)

		started := render(views.StatusStartedRow(data, locale))
		if started != lastStarted {
			writeSSEEvent(w, "started", started)
			lastStarted = started
		}

		if phase != lastPhase {
			body := render(views.StatusBodyInner(data, locale))
			writeSSEEvent(w, "body", body)
			lastPhase = phase
			// The new body carries fresh roster/log/pending content;
			// seed the per-region caches so the next tick only emits
			// on subsequent changes.
			lastRoster = render(views.StatusRosters(data, locale))
			lastLog = render(views.StatusLog(data, locale))
			if data.AutoGenEnabled {
				lastPending = render(views.GenerateNowButton(data.PendingBank, locale))
			}
			flusher.Flush()
			return true
		}

		if phase == "live" {
			roster := render(views.StatusRosters(data, locale))
			if roster != lastRoster {
				writeSSEEvent(w, "roster", roster)
				lastRoster = roster
			}
			if data.AutoGenEnabled {
				pending := render(views.GenerateNowButton(data.PendingBank, locale))
				if pending != lastPending {
					writeSSEEvent(w, "pending", pending)
					lastPending = pending
				}
			}
		}

		log := render(views.StatusLog(data, locale))
		if log != lastLog {
			writeSSEEvent(w, "log", log)
			lastLog = log
		}

		flusher.Flush()
		return true
	}

	tick := time.NewTicker(statusStreamTick)
	defer tick.Stop()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-tick.C:
			if !push() {
				return
			}
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes one SSE message. Multi-line payloads (templ
// renders HTML with embedded newlines) get one data: line per
// physical line, per the SSE wire format.
func writeSSEEvent(w io.Writer, event, payload string) {
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(payload, "\n") {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = io.WriteString(w, "\n")
}

// statusData captures the current session state into a StatusData
// view model. Returns ok=false when no session is active.
func (s *Server) statusData() (views.StatusData, bool) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()
	if act == nil {
		return views.StatusData{}, false
	}

	status := act.coord.Status()
	logEntries := act.buf.Entries()

	vEntries := make([]views.LogEntry, len(logEntries))
	for i, e := range logEntries {
		vEntries[i] = views.LogEntry{
			Time:    e.Time,
			Level:   e.Level,
			Message: e.Message,
			Attrs:   e.Attrs,
		}
	}

	ag := s.cfg.Get().Challenges.AutoGeneration
	return views.StatusData{
		MeetingID:         act.meetingID,
		ProviderName:      act.providerName,
		StartedAt:         act.startedAt,
		MeetingStartedAt:  status.MeetingStartedAt,
		MeetingInProgress: status.MeetingInProgress,
		Present:           status.Present,
		Unregistered:      status.Unregistered,
		LogEntries:        vEntries,
		AutoGenEnabled:    ag.Enabled && act.challenger != nil,
		AutoGenAutoSubmit: ag.AutoSubmit,
		AutoGenIntervalS:  ag.PollIntervalSeconds,
		PendingBank:       latestPendingBank(act.challenger),
	}, true
}

// latestPendingBank returns the freshest auto-*.yaml in the review dir,
// or nil when the challenger is offline, in auto_submit mode, or the
// dir is empty.
func latestPendingBank(svc *challenger.Service) *views.PendingBank {
	if svc == nil {
		return nil
	}
	dir := svc.ReviewDirPath()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var newest os.FileInfo
	var newestName string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "auto-") || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == nil || info.ModTime().After(newest.ModTime()) {
			newest = info
			newestName = e.Name()
		}
	}
	if newest == nil {
		return nil
	}
	return &views.PendingBank{
		Path:    filepath.Join(dir, newestName),
		Name:    newestName,
		ModTime: newest.ModTime(),
	}
}

// handlePollPendingPreview returns the pending YAML's raw text wrapped
// in a <pre> for inline display. Keeps it simple — full edit is the
// teacher's text editor, not the GUI.
func (s *Server) handlePollPendingPreview(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()
	if act == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	pending := latestPendingBank(act.challenger)
	if pending == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := os.ReadFile(pending.Path)
	if err != nil {
		http.Error(w, "read pending: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<pre style="white-space:pre-wrap;max-height:20rem;overflow:auto;">`))
	_, _ = w.Write([]byte(htmlEscape(string(body))))
	_, _ = w.Write([]byte(`</pre>`))
}

// pollFileMaxBytes caps the multipart upload from the GUI's "Trigger
// poll" card. Question banks are plain YAML of a few KB even in the
// worst case; 1 MiB rejects pasted binaries without truncating any
// realistic bank.
const pollFileMaxBytes = 1 << 20

// handlePollFile accepts a YAML question bank as multipart/form-data
// from the live status page, writes it to a temp file the active
// session can read, dispatches it through the same RunPoll path as
// ptrack poll, and removes the temp file afterwards. Replaces the
// previous "type a server-side path" input on the GUI — the teacher's
// browser is the source of truth for the file.
func (s *Server) handlePollFile(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()
	if act == nil {
		http.Error(w, `{"error":"no active session"}`, http.StatusConflict)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, pollFileMaxBytes)
	if err := r.ParseMultipartForm(pollFileMaxBytes); err != nil {
		http.Error(w, `{"error":"upload too large or malformed"}`, http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("bank")
	if err != nil {
		http.Error(w, `{"error":"missing bank file"}`, http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	tmp, err := os.CreateTemp("", "ptrack-bank-*.yaml")
	if err != nil {
		http.Error(w, `{"error":"temp file: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(tmp, file); err != nil {
		_ = tmp.Close()
		http.Error(w, `{"error":"write bank: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	if err := tmp.Close(); err != nil {
		http.Error(w, `{"error":"close bank: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	result, err := act.coord.RunPoll(r.Context(), tmpPath, false)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"poll_id":         result.PollID,
		"scheduled_count": result.ScheduledCount,
		"skipped_count":   result.SkippedCount,
		"file_name":       header.Filename,
	})
}

// handleEvents is the daemon-liveness SSE stream that every page opens
// in its layout. The connection is held open for the lifetime of the
// daemon; the browser's EventSource fires onerror the moment the
// stream drops (Ctrl-C, OS kill, graceful shutdown) and the layout's
// detector flips the body to the stopped template. We send periodic
// comment-only keep-alives so intermediate proxies and the browser's
// idle-connection heuristics don't cut us off prematurely.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	// Send something immediately so the client's EventSource fires
	// onopen rather than sitting in CONNECTING.
	if _, err := io.WriteString(w, "event: hello\ndata: ok\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleShutdown stops the active session and triggers the daemon-
// level shutdown callback (if registered). The response carries no
// body — instead an HX-Trigger header fires `ptrack-shutdown` on the
// client, which routes through the same showStopped() detector used
// for SIGINT/Ctrl-C, so both shutdown paths produce identical UI.
// The trigger payload carries attemptClose so the client can try
// window.close() when the tab was opened by ptrack.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	act := s.active
	s.mu.Unlock()
	if act != nil {
		act.cancel()
		select {
		case <-act.done:
		case <-time.After(10 * time.Second):
		}
	}
	trigger := map[string]map[string]bool{
		"ptrack-shutdown": {"attemptClose": s.cfg.Get().GUI.OpenBrowserOnStart},
	}
	if b, err := json.Marshal(trigger); err == nil {
		w.Header().Set("HX-Trigger", string(b))
	}
	w.WriteHeader(http.StatusNoContent)
	if s.shutdownFn != nil {
		go func() {
			// Brief delay so the HX-Trigger response reaches the
			// browser and showStopped() paints the screen before the
			// listener goes away.
			time.Sleep(200 * time.Millisecond)
			s.SignalShutdown()
			s.shutdownFn()
		}()
	}
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// handleStats renders the unified per-meeting / cross-meeting stats
// page. The file= query carries one or more meeting-basename values;
// stats.Loader fetches the JSON (from cache when fresh) and the templ
// view picks the right layout based on file count.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	files, basenames, err := s.collectStatsFiles(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	doc, err := s.stats.Load(r.Context(), files)
	if err != nil {
		if errors.Is(err, ptrackpy.ErrIncompleteMeeting) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "stats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := views.StatsData{Files: basenames, Doc: doc}
	locale := localeFromRequest(r)
	_ = views.Stats(data, locale).Render(r.Context(), w)
}

// handleReport serves the CSV equivalent of the same file query that
// /stats reads. The Python report command auto-aggregates when more
// than one --in is supplied.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	files, basenames, err := s.collectStatsFiles(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	csv, err := ptrackpy.Run(r.Context(), append([]string{"report"}, files...)...)
	if err != nil {
		if errors.Is(err, ptrackpy.ErrIncompleteMeeting) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "report: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+reportFilename(basenames)+`"`)
	_, _ = w.Write(csv)
}

// handleRenameParticipant rewrites every file= listed in the query so
// that rows previously matching the path's {p} display name carry the
// new= value instead. Files outside the request are not touched.
func (s *Server) handleRenameParticipant(w http.ResponseWriter, r *http.Request) {
	oldName := r.PathValue("p")
	newName := r.URL.Query().Get("new")
	if newName == "" {
		newName = r.Header.Get("HX-Prompt")
	}
	if newName == "" && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			newName = body.DisplayName
		}
	}
	if newName == "" {
		http.Error(w, "new= (or display_name) is required", http.StatusBadRequest)
		return
	}

	files, _, err := s.collectStatsFiles(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for _, path := range files {
		if err := eventstore.UpdateDisplayName(path, oldName, newName); err != nil {
			http.Error(w, "rename failed for "+filepath.Base(path)+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// collectStatsFiles parses the file= query into validated absolute
// paths under MeetingsDir. The basename list returned alongside is
// the form templates use for self-referential links (it preserves the
// caller's order).
func (s *Server) collectStatsFiles(r *http.Request) (paths, basenames []string, err error) {
	raw := r.URL.Query()["file"]
	if len(raw) == 0 {
		return nil, nil, errors.New("at least one file= query value is required")
	}

	dir := s.cfg.Get().MeetingsDir
	for _, name := range raw {
		if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			return nil, nil, fmt.Errorf("invalid file value %q", name)
		}
		if !strings.HasSuffix(name, ".parquet") {
			name += ".parquet"
		}
		full := filepath.Join(dir, name)
		if _, statErr := os.Stat(full); statErr != nil {
			return nil, nil, fmt.Errorf("file %q not found in meetings dir", name)
		}
		paths = append(paths, full)
		basenames = append(basenames, name)
	}
	return paths, basenames, nil
}

func reportFilename(basenames []string) string {
	if len(basenames) == 1 {
		return strings.TrimSuffix(basenames[0], ".parquet") + ".csv"
	}
	return "report.csv"
}

// handleRegistry renders the registry page in its initial state — the
// filter form is empty and every entry is listed. Subsequent filtering
// and deletion happen over POST endpoints that carry the form in the
// request body and return only the fragment that needs to swap.
func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	all, err := s.registry.Find(r.Context(), participants.Filter{})
	if err != nil {
		http.Error(w, "list registry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := views.RegistryData{
		Entries:    all,
		Messengers: messengers.Names(),
		HasAny:     len(all) > 0,
	}
	locale := localeFromRequest(r)
	_ = views.Registry(data, locale).Render(r.Context(), w)
}

// handleFilterRegistry executes the user's filter against the registry
// and returns just the results fragment (htmx swaps it into the
// #registry-results container) plus an OOB swap that refreshes the
// match-count line. On validation errors only the count line is
// updated — retargeted to #registry-info so the participants table is
// left untouched.
func (s *Server) handleFilterRegistry(w http.ResponseWriter, r *http.Request) {
	req, err := parseRegistryRequest(r)
	if err != nil {
		http.Error(w, "bad form body: "+err.Error(), http.StatusBadRequest)
		return
	}
	locale := localeFromRequest(r)
	filter, vErrs := validateInputs(req.Filter)
	if len(vErrs) > 0 {
		w.Header().Set("HX-Retarget", "#registry-info")
		w.Header().Set("HX-Reswap", "outerHTML")
		_ = views.RegistryInfo(0, vErrs, locale, false).Render(r.Context(), w)
		return
	}
	entries, err := s.registry.Find(r.Context(), filter)
	if err != nil {
		http.Error(w, "find: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = views.RegistryResults(entries, locale).Render(r.Context(), w)
	_ = views.RegistryInfo(len(entries), nil, locale, true).Render(r.Context(), w)
}

// handleDeleteRegistry removes registry entries. The body decides the
// scope:
//
//   - If display_name values are present, exactly those entries are
//     removed. Used by both the per-row trash icon (one name) and the
//     header's bulk-selection trash (many names). The filter form is
//     not consulted in this branch.
//   - Otherwise the filter form drives the delete: the filter inputs
//     are validated and every matching entry is removed. An empty
//     filter clears every registration.
//
// In either branch, when the registry becomes empty as a result the
// response carries HX-Refresh so the GET handler can rerender the
// empty-state layout (no filter form, no table).
func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	req, err := parseRegistryRequest(r)
	if err != nil {
		http.Error(w, "bad form body: "+err.Error(), http.StatusBadRequest)
		return
	}
	locale := localeFromRequest(r)

	var filter participants.Filter
	if len(req.DisplayNames) > 0 {
		filter = participants.Filter{DisplayNames: req.DisplayNames}
	} else {
		f, vErrs := validateInputs(req.Filter)
		if len(vErrs) > 0 {
			w.Header().Set("HX-Retarget", "#registry-info")
			w.Header().Set("HX-Reswap", "outerHTML")
			_ = views.RegistryInfo(0, vErrs, locale, false).Render(r.Context(), w)
			return
		}
		filter = f
	}
	if _, err := s.registry.Delete(r.Context(), filter); err != nil {
		http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
		return
	}

	total, err := s.registry.Find(r.Context(), participants.Filter{})
	if err != nil {
		http.Error(w, "find: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(total) == 0 {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Re-apply the current filter so the participants list reflects the
	// post-deletion state under whatever narrowing the user had active.
	visible := total
	if len(req.DisplayNames) == 0 {
		visible, err = s.registry.Find(r.Context(), filter)
		if err != nil {
			http.Error(w, "find: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		// Bulk-by-name delete: respect the filter form values that came
		// along on the request so the user's current view stays applied.
		f, vErrs := validateInputs(req.Filter)
		if len(vErrs) == 0 {
			visible, err = s.registry.Find(r.Context(), f)
			if err != nil {
				http.Error(w, "find: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	_ = views.RegistryResults(visible, locale).Render(r.Context(), w)
	_ = views.RegistryInfo(len(visible), nil, locale, true).Render(r.Context(), w)
}

// handleQuestion returns a question record as JSON.
func (s *Server) handleQuestion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	q, err := eventstore.ReadQuestion(s.cfg.Get().QuestionsDir, id)
	if err != nil {
		http.Error(w, "read question failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if q == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(q)
}

// handleConfig renders the config editor.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	schema, err := config.Schema()
	if err != nil {
		http.Error(w, "config schema: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := views.ConfigData{
		V:          s.cfg.Get(),
		Schema:     schema,
		DataDir:    config.DataDir(),
		CacheDir:   config.CacheDir(),
		ConfigPath: s.cfg.Path(),
	}

	locale := localeFromRequest(r)
	_ = views.ConfigEditor(data, locale).Render(r.Context(), w)
}

// handleSaveConfig applies the posted form to the current Values via
// cfg.Apply, which runs the shared validate → prune → write pipeline.
// Secrets marked writeOnly in the schema keep their existing value when
// the corresponding form field is empty (the form never echoes them).
// On validation/write failure the editor is re-rendered with the
// submitted values preserved and an inline error next to the Save button.
func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	mutator := func(v *config.Values) {
		v.MeetingsDir = form.Get("meetings_dir")
		v.QuestionsDir = form.Get("questions_dir")
		v.ReportsDir = form.Get("reports_dir")
		v.RetentionDays = formInt(form, "retention_days", v.RetentionDays)

		v.Providers.BBB.Enabled = formBool(form, "providers.bbb.enabled")
		v.Providers.BBB.BaseURL = form.Get("providers.bbb.base_url")
		v.Providers.BBB.SharedSecret = formSecret(form, "providers.bbb.shared_secret", v.Providers.BBB.SharedSecret)
		v.Providers.BBB.TLSSkipVerify = formBool(form, "providers.bbb.tls_skip_verify")
		v.Providers.BBB.PollIntervalSeconds = formInt(form, "providers.bbb.poll_interval_seconds", v.Providers.BBB.PollIntervalSeconds)

		v.Providers.Meet.Enabled = formBool(form, "providers.meet.enabled")
		v.Providers.Meet.OAuth.ClientID = form.Get("providers.meet.oauth.client_id")
		v.Providers.Meet.OAuth.ClientSecret = formSecret(form, "providers.meet.oauth.client_secret", v.Providers.Meet.OAuth.ClientSecret)
		v.Providers.Meet.OAuth.RedirectPort = formInt(form, "providers.meet.oauth.redirect_port", v.Providers.Meet.OAuth.RedirectPort)
		v.Providers.Meet.PollIntervalSeconds = formInt(form, "providers.meet.poll_interval_seconds", v.Providers.Meet.PollIntervalSeconds)

		v.Providers.Zoom.Enabled = formBool(form, "providers.zoom.enabled")
		v.Providers.Zoom.OAuth.ClientID = form.Get("providers.zoom.oauth.client_id")
		v.Providers.Zoom.OAuth.ClientSecret = formSecret(form, "providers.zoom.oauth.client_secret", v.Providers.Zoom.OAuth.ClientSecret)
		v.Providers.Zoom.OAuth.RedirectPort = formInt(form, "providers.zoom.oauth.redirect_port", v.Providers.Zoom.OAuth.RedirectPort)
		v.Providers.Zoom.PollIntervalSeconds = formInt(form, "providers.zoom.poll_interval_seconds", v.Providers.Zoom.PollIntervalSeconds)

		v.Messengers.Telegram.Enabled = formBool(form, "messengers.telegram.enabled")
		v.Messengers.Telegram.BotToken = formSecret(form, "messengers.telegram.bot_token", v.Messengers.Telegram.BotToken)

		v.Challenges.Defaults.AnswerWindowSeconds = formInt(form, "challenges.defaults.answer_window_seconds", v.Challenges.Defaults.AnswerWindowSeconds)
		v.Challenges.Defaults.MinGapBetweenChallengesSecs = formInt(form, "challenges.defaults.min_gap_between_challenges_seconds", v.Challenges.Defaults.MinGapBetweenChallengesSecs)
		v.Challenges.Poll.MaxDeliverySkewMS = formInt(form, "challenges.poll.max_delivery_skew_ms", v.Challenges.Poll.MaxDeliverySkewMS)

		v.Challenges.AutoGeneration.Enabled = formBool(form, "challenges.auto_generation.enabled")
		v.Challenges.AutoGeneration.AutoSubmit = formBool(form, "challenges.auto_generation.auto_submit")
		v.Challenges.AutoGeneration.PollIntervalSeconds = formInt(form, "challenges.auto_generation.poll_interval_seconds", v.Challenges.AutoGeneration.PollIntervalSeconds)
		v.Challenges.AutoGeneration.MinWordsPerQuestion = formInt(form, "challenges.auto_generation.min_words_per_question", v.Challenges.AutoGeneration.MinWordsPerQuestion)
		v.Challenges.AutoGeneration.MaxQuestionsPerPoll = formInt(form, "challenges.auto_generation.max_questions_per_poll", v.Challenges.AutoGeneration.MaxQuestionsPerPoll)
		v.Challenges.AutoGeneration.ReviewDir = form.Get("challenges.auto_generation.review_dir")
		v.Challenges.AutoGeneration.ASR.BaseURL = form.Get("challenges.auto_generation.asr.base_url")
		v.Challenges.AutoGeneration.ASR.APIKey = formSecret(form, "challenges.auto_generation.asr.api_key", v.Challenges.AutoGeneration.ASR.APIKey)
		v.Challenges.AutoGeneration.ASR.Model = form.Get("challenges.auto_generation.asr.model")
		v.Challenges.AutoGeneration.LLM.BaseURL = form.Get("challenges.auto_generation.llm.base_url")
		v.Challenges.AutoGeneration.LLM.APIKey = formSecret(form, "challenges.auto_generation.llm.api_key", v.Challenges.AutoGeneration.LLM.APIKey)
		v.Challenges.AutoGeneration.LLM.Model = form.Get("challenges.auto_generation.llm.model")

		v.EventStore.Compression = form.Get("eventstore.compression")
		v.EventStore.RowGroupSize = formInt(form, "eventstore.row_group_size", v.EventStore.RowGroupSize)

		v.GUI.BindAddr = form.Get("gui.bind_addr")
		v.GUI.Port = formInt(form, "gui.port", v.GUI.Port)
		v.GUI.OpenBrowserOnStart = formBool(form, "gui.open_browser_on_start")

		v.Logging.Level = form.Get("logging.level")
		v.Logging.Format = form.Get("logging.format")
		v.Logging.File = form.Get("logging.file")
	}
	if err := s.cfg.Apply(mutator); err != nil {
		schema, sErr := config.Schema()
		if sErr != nil {
			http.Error(w, "config schema: "+sErr.Error(), http.StatusInternalServerError)
			return
		}
		submitted := s.cfg.Get()
		mutator(&submitted)
		data := views.ConfigData{
			V:          submitted,
			Schema:     schema,
			DataDir:    config.DataDir(),
			CacheDir:   config.CacheDir(),
			ConfigPath: s.cfg.Path(),
			Error:      err.Error(),
		}
		locale := localeFromRequest(r)
		w.WriteHeader(http.StatusBadRequest)
		_ = views.ConfigEditor(data, locale).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

func formInt(form url.Values, key string, fallback int) int {
	s := strings.TrimSpace(form.Get(key))
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func formBool(form url.Values, key string) bool {
	return form.Get(key) != ""
}

// formSecret returns the submitted value when non-empty, otherwise the
// existing one. The form intentionally never round-trips writeOnly
// secrets, so a blank field means "keep what's on disk".
func formSecret(form url.Values, key string, current string) string {
	v := form.Get(key)
	if v == "" {
		return current
	}
	return v
}

// buildServeProvider creates a provider for the serve context (no fixture support).
func buildServeProvider(name string, cfg *config.Config) (providers.Provider, error) {
	switch name {
	case "bbb":
		return bbbprovider.New(cfg), nil
	case "meet":
		return meetprovider.New(cfg), nil
	case "zoom":
		return zoomprovider.New(cfg), nil
	default:
		return nil, fmt.Errorf("unknown provider %q; supported: bbb, meet, zoom", name)
	}
}

// enabledProviderOptions returns the list of providers the teacher has
// enabled in config, in a fixed display order. The Connect form on the
// dashboard renders these as the provider dropdown; an empty result
// triggers the "configure a provider first" hint instead.
func enabledProviderOptions(p config.ProvidersConfig) []views.ProviderOption {
	var out []views.ProviderOption
	if p.BBB.Enabled {
		out = append(out, views.ProviderOption{Name: "bbb", Label: "BigBlueButton"})
	}
	if p.Meet.Enabled {
		out = append(out, views.ProviderOption{Name: "meet", Label: "Google Meet"})
	}
	if p.Zoom.Enabled {
		out = append(out, views.ProviderOption{Name: "zoom", Label: "Zoom"})
	}
	return out
}
