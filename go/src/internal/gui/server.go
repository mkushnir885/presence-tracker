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
	"presence-tracker/src/internal/challenges"
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
)

type Server struct {
	cfg      *config.Config
	registry participants.Registry
	router   *messengers.Router

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
	meetingID       string
	providerName    string
	providerDisplay string
	coord           *session.Coordinator
	challenger      *challenger.Service
	cancel          context.CancelFunc
	startedAt       time.Time
	done            chan struct{}
	buf             *logBuffer // captures slog output for the live status log
}

func New(cfg *config.Config, registry participants.Registry, router *messengers.Router) *Server {
	return &Server{
		cfg:      cfg,
		registry: registry,
		router:   router,
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
	mux.HandleFunc("GET /stats/rows", s.handleStatsRows)
	mux.HandleFunc("GET /report", s.handleReport)
	mux.HandleFunc("PATCH /participants/{p}/display-name", s.handleRenameParticipant)
	mux.HandleFunc("GET /registry", s.handleRegistry)
	mux.HandleFunc("POST /registry/filter", s.handleFilterRegistry)
	mux.HandleFunc("POST /registry/delete", s.handleDeleteRegistry)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("POST /config", s.handleSaveConfig)
	mux.HandleFunc("POST /system/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /poll/pending", s.handlePendingPoll)
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
	dirFormat := strings.TrimSpace(r.FormValue("dir_format"))

	if providerName == "" || meetingID == "" {
		http.Error(w, "provider and meeting_id are required", http.StatusBadRequest)
		return
	}

	cfg := s.cfg.Get()
	if dirFormat == "" {
		dirFormat = cfg.MeetingsDirFormat
	}
	tmpl, err := eventstore.ParseDirTemplate(dirFormat)
	if err != nil {
		http.Error(w, "dir_format: "+err.Error(), http.StatusBadRequest)
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

	meetingID, err = prov.ParseMeetingID(meetingID)
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

	store, err := eventstore.NewWriter(cfg.MeetingsDir, internalMeetingID, tmpl, startTime)
	if err != nil {
		http.Error(w, "event store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sessCfg := session.Config{
		MeetingID:         internalMeetingID,
		PlatformMeetingID: meetingID,
		ProviderName:      prov.Name(),
	}

	coord := session.New(sessCfg, s.cfg, prov, msgr, s.registry, store)

	chSvc := challenger.New(s.cfg, coord, coord)

	sessCtx, cancel := context.WithCancel(context.Background())
	buf := newLogBuffer(200, slog.Default().Handler())
	newHandler := slog.New(buf)
	prevDefault := slog.Default()
	slog.SetDefault(newHandler)

	done := make(chan struct{})

	act := &activeSession{
		meetingID:       meetingID,
		providerName:    providerName,
		providerDisplay: prov.DisplayName(),
		coord:           coord,
		challenger:      chSvc,
		cancel:          cancel,
		startedAt:       time.Now(),
		done:            done,
		buf:             buf,
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
		if data.AutoGenEnabled {
			return "live-autogen"
		}
		return "live"
	}

	// Seed each region with the initial render so the first tick only emits
	// real changes (the page already holds this HTML).
	var lastStarted, lastRoster, lastLog, lastPhase string
	if data, ok := s.statusData(); ok {
		lastPhase = phaseOf(data)
		lastStarted = render(views.StatusStartedRow(data, locale))
		lastRoster = render(views.StatusRosters(data, locale))
		lastLog = render(views.StatusLog(data, locale))
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
			flusher.Flush()
			return true
		}

		if phase != "waiting" {
			roster := render(views.StatusRosters(data, locale))
			if roster != lastRoster {
				writeSSEEvent(w, "roster", roster)
				lastRoster = roster
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
		ProviderName:      act.providerDisplay,
		StartedAt:         act.startedAt,
		MeetingStartedAt:  status.MeetingStartedAt,
		MeetingInProgress: status.MeetingInProgress,
		Present:           status.Present,
		Unregistered:      status.Unregistered,
		LogEntries:        vEntries,
		AutoGenEnabled:    ag.Enabled,
		AutoGenAutoSubmit: ag.AutoSubmit,
		AutoGenIntervalS:  ag.PollIntervalSeconds,
	}, true
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

func (s *Server) handlePendingPoll(w http.ResponseWriter, r *http.Request) {
	svc, coord := s.Challenger(), s.Coord()
	if svc == nil || coord == nil {
		http.Error(w, "no active session", http.StatusConflict)
		return
	}
	path := svc.PendingBankPath()
	if path == "" {
		http.Error(w, "no pending bank", http.StatusConflict)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "read pending bank: "+err.Error(), http.StatusInternalServerError)
		return
	}
	bank, err := challenges.Parse(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	result, err := coord.RunPollBank(r.Context(), bank, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errchkjson // map with string/any values cannot fail
		"poll_id":         result.PollID,
		"scheduled_count": result.ScheduledCount,
		"skipped_count":   result.SkippedCount,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	_, names, err := s.collectMeetingDirs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	locale := localeFromRequest(r)
	_ = views.Stats(names, locale).Render(r.Context(), w)
}

func (s *Server) handleStatsRows(w http.ResponseWriter, r *http.Request) {
	dirs, names, err := s.collectMeetingDirs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	out, err := ptrackpy.Run(r.Context(), append([]string{"stats"}, dirs...)...)
	if err != nil {
		if errors.Is(err, ptrackpy.ErrInvalidData) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "stats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := views.StatsData{Dirs: names}
	if err := json.Unmarshal(out, &data); err != nil {
		http.Error(w, "stats: parse JSON: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range data.Meetings {
		data.Meetings[i].Platform = providers.DisplayName(data.Meetings[i].Platform)
	}

	locale := localeFromRequest(r)
	_ = views.StatsRows(data, locale).Render(r.Context(), w)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	dirs, names, err := s.collectMeetingDirs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	csv, err := ptrackpy.Run(r.Context(), append([]string{"report"}, dirs...)...)
	if err != nil {
		if errors.Is(err, ptrackpy.ErrInvalidData) {
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

	args := append([]string{"rename", "--from", oldName, "--to", newName}, dirs...)
	if _, err := ptrackpy.Run(r.Context(), args...); err != nil {
		http.Error(w, "rename: "+err.Error(), http.StatusInternalServerError)
		return
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
		v.MeetingsDirFormat = form.Get("meetings_dir_format")
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
		v.Challenges.AutoGeneration.BankBasename = form.Get("challenges.auto_generation.bank_basename")
		v.Challenges.AutoGeneration.Language = form.Get("challenges.auto_generation.language")
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
	// Template syntax can't be expressed in JSON Schema; check up front so an
	// invalid value never reaches disk.
	candidate := s.cfg.Get()
	mutator(&candidate)
	var applyErr error
	if candidate.MeetingsDirFormat != "" {
		if _, err := eventstore.ParseDirTemplate(candidate.MeetingsDirFormat); err != nil {
			applyErr = fmt.Errorf("meetings_dir_format: %w", err)
		}
	}
	if applyErr == nil {
		applyErr = s.cfg.Apply(mutator)
	}
	if applyErr != nil {
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
			ConfigPath: s.cfg.Path(),
			Error:      applyErr.Error(),
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
		out = append(out, views.ProviderOption{Name: bbbprovider.Name, Label: bbbprovider.DisplayName})
	}
	if p.Meet.Enabled {
		out = append(out, views.ProviderOption{Name: meetprovider.Name, Label: meetprovider.DisplayName})
	}
	if p.Zoom.Enabled {
		out = append(out, views.ProviderOption{Name: zoomprovider.Name, Label: zoomprovider.DisplayName})
	}
	return out
}
