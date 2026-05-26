package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/google/uuid"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/gui/views"
	"presence-tracker/src/internal/messengers"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/providers"
	bbbprovider "presence-tracker/src/internal/providers/bbb"
	meetprovider "presence-tracker/src/internal/providers/meet"
	zoomprovider "presence-tracker/src/internal/providers/zoom"
	"presence-tracker/src/internal/reporter"
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
}

// activeSession holds state for the currently running tracking session.
type activeSession struct {
	meetingID    string
	providerName string
	coord        *session.Coordinator
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
	}
}

// RegisterRoutes attaches the GUI's HTML and htmx routes to mux. The HTTP
// server lifecycle is owned by the caller (cmd/ptrack).
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(views.Assets, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(sub)))

	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /meetings", s.handleMeetings)
	mux.HandleFunc("POST /session", s.handleStartSession)
	mux.HandleFunc("DELETE /session", s.handleStopSession)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /status/unregistered", s.handleUnregisteredFragment)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /report", s.handleReport)
	mux.HandleFunc("PATCH /participants/{p}/display-name", s.handleRenameParticipant)
	mux.HandleFunc("GET /registry", s.handleRegistry)
	mux.HandleFunc("POST /registry/filter", s.handleFilterRegistry)
	mux.HandleFunc("POST /registry/delete", s.handleDeleteRegistry)
	mux.HandleFunc("GET /questions/{id}", s.handleQuestion)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("POST /config", s.handleSaveConfig)
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

// handleDashboard renders the main dashboard page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()

	data := views.DashboardData{
		EnabledProviders: enabledProviderOptions(s.cfg.Get().Providers),
	}
	if act != nil {
		data.ActiveSession = true
		data.ActiveMeetingID = act.meetingID
	}

	locale := localeFromRequest(r)
	_ = views.Dashboard(data, locale).Render(r.Context(), w)
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

// handleStatus renders the live status page.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()

	if act == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	status := act.coord.Status()
	logEntries := act.buf.Entries()

	// Convert gui.LogEntry to views.LogEntry.
	vEntries := make([]views.LogEntry, len(logEntries))
	for i, e := range logEntries {
		vEntries[i] = views.LogEntry{
			Time:    e.Time,
			Level:   e.Level,
			Message: e.Message,
			Attrs:   e.Attrs,
		}
	}

	data := views.StatusData{
		MeetingID:    act.meetingID,
		ProviderName: act.providerName,
		StartedAt:    act.startedAt,
		Present:      status.Present,
		Unregistered: status.Unregistered,
		LogEntries:   vEntries,
	}

	locale := localeFromRequest(r)
	_ = views.Status(data, locale).Render(r.Context(), w)
}

// handleUnregisteredFragment returns the unregistered participants HTML fragment.
func (s *Server) handleUnregisteredFragment(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()

	if act == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	status := act.coord.Status()
	locale := localeFromRequest(r)
	_ = views.UnregisteredFragment(status.Unregistered, locale).Render(r.Context(), w)
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
		http.Error(w, "stats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := views.StatsData{Files: basenames, Doc: doc}
	locale := localeFromRequest(r)
	_ = views.Stats(data, locale).Render(r.Context(), w)
}

// handleReport serves the CSV equivalent of the same file query that
// /stats reads. With one file it returns the per-meeting CSV; with
// more, the cross-meeting aggregate.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	files, basenames, err := s.collectStatsFiles(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	csv, err := reporter.Generate(r.Context(), files)
	if err != nil {
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
