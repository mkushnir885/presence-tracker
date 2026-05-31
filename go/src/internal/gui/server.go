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

type Server struct {
	cfg      *config.Config
	registry participants.Registry
	router   *messengers.Router
	stats    *stats.Loader

	mu     sync.RWMutex
	active *activeSession

	shutdownFn func() // stops the daemon; set by cmd/ptrack via OnShutdown

	// stopCh is closed by SignalShutdown so long-lived SSE handlers exit
	// promptly instead of holding up the graceful shutdown.
	stopOnce sync.Once
	stopCh   chan struct{}
}

func (s *Server) OnShutdown(fn func()) { s.shutdownFn = fn }

// activeSession is the currently running tracking session; nil when idle.
type activeSession struct {
	meetingID    string
	providerName string
	coord        *session.Coordinator
	challenger   *challenger.Service
	cancel       context.CancelFunc
	startedAt    time.Time
	done         chan struct{}
	buf          *logBuffer // captures slog output for the live status log
}

func New(cfg *config.Config, registry participants.Registry, router *messengers.Router) *Server {
	return &Server{
		cfg:      cfg,
		registry: registry,
		router:   router,
		stats:    stats.New(filepath.Join(config.CacheDir(), "stats")),
		stopCh:   make(chan struct{}),
	}
}

func (s *Server) SignalShutdown() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

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

func (s *Server) Coord() *session.Coordinator {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil
	}
	return s.active.coord
}

func (s *Server) Challenger() *challenger.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil
	}
	return s.active.challenger
}

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

func (s *Server) handleMeetings(w http.ResponseWriter, r *http.Request) {
	meetingsDir := s.cfg.Get().MeetingsDir
	entries, err := os.ReadDir(meetingsDir)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, "Failed to list meetings: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var meetings []views.Meeting
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(meetingsDir, e.Name())
		if _, err := os.Stat(filepath.Join(path, eventstore.EventsFile)); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		meetings = append(meetings, views.Meeting{
			ID:        e.Name(),
			CreatedAt: fileCreatedAt(path, info.ModTime()),
		})
	}

	sort.Slice(meetings, func(i, j int) bool {
		return meetings[i].CreatedAt.After(meetings[j].CreatedAt)
	})

	locale := localeFromRequest(r)
	_ = views.Meetings(views.MeetingsData{Meetings: meetings}, locale).Render(r.Context(), w)
}

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) { //nolint:contextcheck // the tracking session outlives the request, so it runs on a detached context
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}

	providerName := r.FormValue("provider")
	meetingID := r.FormValue("meeting_id")
	dirName := strings.TrimSpace(r.FormValue("dir_name"))

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

	store, err := eventstore.NewWriter(s.cfg.Get().MeetingsDir, dirName, startTime)
	if err != nil {
		status := http.StatusInternalServerError
		if dirName != "" {
			status = http.StatusBadRequest
		}
		http.Error(w, "event store error: "+err.Error(), status)
		return
	}

	sessCfg := session.Config{
		MeetingID:                   internalMeetingID,
		PlatformMeetingID:           meetingID,
		MeetingsDir:                 s.cfg.Get().MeetingsDir,
		ProviderName:                prov.Name(),
		AnswerWindowSecs:            s.cfg.Get().Challenges.Defaults.AnswerWindowSeconds,
		MinGapBetweenChallengesSecs: s.cfg.Get().Challenges.Defaults.MinGapBetweenChallengesSecs,
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

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	data, ok := s.statusData()
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	locale := localeFromRequest(r)
	_ = views.Status(data, locale).Render(r.Context(), w)
}

const statusStreamTick = 2 * time.Second

// handleStatusStream is the live-status SSE stream. Each region (started
// row, body, rosters, log, pending-bank button) is re-rendered on a tick and
// re-sent only when it differs from the last value, so an idle session emits
// nothing but keep-alives.
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

	// Seed each region with the initial render so the first tick only emits
	// real changes (the page already holds this HTML).
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

func writeSSEEvent(w io.Writer, event, payload string) {
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	for line := range strings.SplitSeq(payload, "\n") {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = io.WriteString(w, "\n")
}

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

const pollFileMaxBytes = 1 << 20

// writeJSONError sends {"error": msg} as JSON. Unlike http.Error it sets an
// application/json content type and escapes msg, so error text containing
// quotes or backslashes still produces valid JSON.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errchkjson // map[string]string cannot fail to marshal
}

func (s *Server) handlePollFile(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()
	if act == nil {
		writeJSONError(w, http.StatusConflict, "no active session")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, pollFileMaxBytes)
	if err := r.ParseMultipartForm(pollFileMaxBytes); err != nil {
		writeJSONError(w, http.StatusBadRequest, "upload too large or malformed")
		return
	}
	file, header, err := r.FormFile("bank")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing bank file")
		return
	}
	defer func() { _ = file.Close() }()

	tmp, err := os.CreateTemp("", "ptrack-bank-*.yaml")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "temp file: "+err.Error())
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(tmp, file); err != nil {
		_ = tmp.Close()
		writeJSONError(w, http.StatusInternalServerError, "write bank: "+err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "close bank: "+err.Error())
		return
	}

	result, err := act.coord.RunPoll(r.Context(), tmpPath, false)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeJSONError(w, status, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errchkjson // response fields are JSON-safe scalars
		"poll_id":         result.PollID,
		"scheduled_count": result.ScheduledCount,
		"skipped_count":   result.SkippedCount,
		"file_name":       header.Filename,
	})
}

// handleEvents is the daemon-liveness SSE stream every page holds open. When
// it drops (shutdown, Ctrl-C, kill) the browser's EventSource fires onerror
// and the layout flips to the "stopped" screen.
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
	// HX-Trigger fires ptrack-shutdown so this path and SIGINT paint the same
	// "stopped" screen; the short delay lets the response reach the browser
	// before the listener closes.
	w.Header().Set("HX-Trigger", "ptrack-shutdown")
	w.WriteHeader(http.StatusNoContent)
	if s.shutdownFn != nil {
		go func() {
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

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	dirs, names, err := s.collectMeetingDirs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	doc, err := s.stats.Load(r.Context(), dirs)
	if err != nil {
		if errors.Is(err, ptrackpy.ErrIncompleteMeeting) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "stats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := views.StatsData{Dirs: names, Doc: doc}
	locale := localeFromRequest(r)
	_ = views.Stats(data, locale).Render(r.Context(), w)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	dirs, names, err := s.collectMeetingDirs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	csv, err := ptrackpy.Run(r.Context(), append([]string{"report"}, dirs...)...)
	if err != nil {
		if errors.Is(err, ptrackpy.ErrIncompleteMeeting) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "report: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+reportFilename(names)+`"`)
	_, _ = w.Write(csv)
}

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

	dirs, _, err := s.collectMeetingDirs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for _, dir := range dirs {
		if err := eventstore.UpdateDisplayName(dir, oldName, newName); err != nil { //nolint:contextcheck // synchronous Parquet rewrite; not cancellable mid-write
			http.Error(w, "rename failed for "+filepath.Base(dir)+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// collectMeetingDirs reads ?dir=… query values, verifies each exists under
// the configured meetings dir, and returns absolute paths plus the bare
// directory names (for display).
func (s *Server) collectMeetingDirs(r *http.Request) (paths, names []string, err error) {
	raw := r.URL.Query()["dir"]
	if len(raw) == 0 {
		return nil, nil, errors.New("at least one dir= query value is required")
	}

	base := s.cfg.Get().MeetingsDir
	for _, name := range raw {
		if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			return nil, nil, fmt.Errorf("invalid dir value %q", name)
		}
		full := filepath.Join(base, name)
		if _, statErr := os.Stat(filepath.Join(full, eventstore.EventsFile)); statErr != nil { //nolint:gosec // name is validated against path separators and ".." just above
			return nil, nil, fmt.Errorf("meeting dir %q not found", name)
		}
		paths = append(paths, full)
		names = append(names, name)
	}
	return paths, names, nil
}

func reportFilename(names []string) string {
	if len(names) == 1 {
		return names[0] + ".csv"
	}
	return "report.csv"
}

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

	visible := total
	if len(req.DisplayNames) == 0 {
		visible, err = s.registry.Find(r.Context(), filter)
		if err != nil {
			http.Error(w, "find: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
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

func (s *Server) handleQuestion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	q, err := eventstore.ReadQuestion(s.cfg.Get().MeetingsDir, id)
	if err != nil {
		http.Error(w, "read question failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if q == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(q) //nolint:errchkjson // question record is JSON-safe
}

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

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	mutator := func(v *config.Values) {
		v.MeetingsDir = form.Get("meetings_dir")
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
		v.Challenges.AutoGeneration.ExtraRules = formStringList(form, "challenges.auto_generation.extra_rules")

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

func formStringList(form url.Values, key string) []string {
	raw := form[key]
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func formSecret(form url.Values, key string, current string) string {
	v := form.Get(key)
	if v == "" {
		return current
	}
	return v
}

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
