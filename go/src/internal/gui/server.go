package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/gui/views"
	"presence-tracker/src/internal/messengers"
	mockmessenger "presence-tracker/src/internal/messengers/mock"
	"presence-tracker/src/internal/messengers/telegram"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/providers"
	bbbprovider "presence-tracker/src/internal/providers/bbb"
	"presence-tracker/src/internal/reporter"
	"presence-tracker/src/internal/session"
)

// Server is the GUI HTTP server for ptrack serve.
type Server struct {
	cfg      *config.Config
	registry participants.Registry

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

// New creates a Server.
func New(cfg *config.Config, registry participants.Registry) *Server {
	return &Server{cfg: cfg, registry: registry}
}

// RegisterRoutes attaches the GUI's HTML and htmx routes to mux. The HTTP
// server lifecycle is owned by the caller (cmd/ptrack).
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(views.Assets, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(sub)))

	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("POST /session", s.handleStartSession)
	mux.HandleFunc("DELETE /session", s.handleStopSession)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /status/unregistered", s.handleUnregisteredFragment)
	mux.HandleFunc("GET /meetings/{id}", s.handleMeeting)
	mux.HandleFunc("GET /meetings/{id}/export.csv", s.handleMeetingCSV)
	mux.HandleFunc("PATCH /meetings/{id}/participants/{p}/display-name", s.handleRenameParticipant)
	mux.HandleFunc("GET /participants/export.csv", s.handleAllParticipantsCSV)
	mux.HandleFunc("GET /participants/{p}", s.handleParticipant)
	mux.HandleFunc("GET /participants/{p}/export.csv", s.handleParticipantCSV)
	mux.HandleFunc("GET /registry", s.handleRegistry)
	mux.HandleFunc("DELETE /registry/{name}", s.handleDeleteRegistryEntry)
	mux.HandleFunc("DELETE /registry", s.handleClearRegistry)
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

	// Sort newest first.
	sort.Slice(meetings, func(i, j int) bool {
		return meetings[i].ModTime.After(meetings[j].ModTime)
	})

	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()

	data := views.DashboardData{Meetings: meetings}
	if act != nil {
		data.ActiveSession = true
		data.ActiveMeetingID = act.meetingID
	}

	locale := localeFromRequest(r)
	_ = views.Dashboard(data, locale).Render(r.Context(), w)
}

// handleStartSession starts a new tracking session.
func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}

	providerName := r.FormValue("provider")
	meetingID := r.FormValue("meeting_id")

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

	var msgr messengers.Messenger
	if s.cfg.Get().Messengers.Telegram.Enabled {
		tgAdapter, err := telegram.New(s.cfg.Get().Messengers.Telegram.BotToken, s.registry)
		if err != nil {
			http.Error(w, "telegram init error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		msgr = tgAdapter
	} else {
		msgr = mockmessenger.New()
	}

	internalMeetingID := uuid.Must(uuid.NewV7()).String()
	startTime := time.Now()

	store, err := eventstore.NewWriter(s.cfg.Get().MeetingsDir, startTime, s.cfg.Get().EventStore.Compression, s.cfg.Get().EventStore.RowGroupSize)
	if err != nil {
		http.Error(w, "event store error: "+err.Error(), http.StatusInternalServerError)
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

	go func() {
		defer close(done)
		defer func() {
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

// handleMeeting renders the meeting analysis page.
func (s *Server) handleMeeting(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	parquetPath := filepath.Join(s.cfg.Get().MeetingsDir, id+".parquet")

	records, err := eventstore.ReadAll(r.Context(), parquetPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "failed to read meeting: "+err.Error(), http.StatusInternalServerError)
		return
	}

	csvStr, genErr := reporter.Generate(r.Context(), parquetPath)
	var csvRows []reporter.Row
	if genErr == nil {
		csvRows, _ = reporter.Parse(csvStr)
	}

	info := ComputeMeetingInfo(id, records, csvRows)

	data := views.MeetingData{
		MeetingID: id,
		Info:      info,
	}

	locale := localeFromRequest(r)
	_ = views.MeetingAnalysis(data, locale).Render(r.Context(), w)
}

// handleMeetingCSV serves the CSV report for a meeting as a download.
func (s *Server) handleMeetingCSV(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	parquetPath := filepath.Join(s.cfg.Get().MeetingsDir, id+".parquet")

	csvStr, err := reporter.Generate(r.Context(), parquetPath)
	if err != nil {
		http.Error(w, "report generation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.csv"`)
	_, _ = w.Write([]byte(csvStr))
}

// handleRenameParticipant renames a participant in a single Parquet file.
// The {p} path parameter is the current display name (URL-decoded by
// net/http); the request body / HX-Prompt header carries the new name.
func (s *Server) handleRenameParticipant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	oldName := r.PathValue("p")
	parquetPath := filepath.Join(s.cfg.Get().MeetingsDir, id+".parquet")

	var newName string

	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var body struct {
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad JSON body", http.StatusBadRequest)
			return
		}
		newName = body.DisplayName
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form data", http.StatusBadRequest)
			return
		}
		newName = r.Header.Get("HX-Prompt")
		if newName == "" {
			newName = r.FormValue("display_name")
		}
	}

	if newName == "" {
		http.Error(w, "display_name is required", http.StatusBadRequest)
		return
	}

	if err := eventstore.UpdateDisplayName(parquetPath, oldName, newName); err != nil {
		http.Error(w, "rename failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleParticipant renders the cross-meeting participant view.
// The {p} path parameter is the display name (URL-decoded by net/http).
func (s *Server) handleParticipant(w http.ResponseWriter, r *http.Request) {
	displayName := r.PathValue("p")

	pattern := filepath.Join(s.cfg.Get().MeetingsDir, "*.parquet")
	files, err := filepath.Glob(pattern)
	if err != nil {
		http.Error(w, "glob error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var meetingRows []views.ParticipantMeetingRow

	for _, path := range files {
		meetingID := strings.TrimSuffix(filepath.Base(path), ".parquet")
		records, err := eventstore.ReadAll(r.Context(), path)
		if err != nil {
			continue
		}

		info := ComputeMeetingInfo(meetingID, records, nil)

		for _, row := range info.Rows {
			if row.DisplayName == displayName {
				meetingRows = append(meetingRows, views.ParticipantMeetingRow{
					MeetingID:         meetingID,
					StartTime:         info.StartTime,
					EndTime:           info.EndTime,
					PresenceRatio:     row.PresenceRatio,
					ChallengesIssued:  row.ChallengesIssued,
					ChallengesCorrect: row.ChallengesCorrect,
					Absent:            false,
					Segments:          row.Segments,
					MeetingDuration:   info.Duration,
				})
				break
			}
		}
	}

	sort.Slice(meetingRows, func(i, j int) bool {
		return meetingRows[i].StartTime.Before(meetingRows[j].StartTime)
	})

	data := views.ParticipantData{
		DisplayName: displayName,
		Meetings:    meetingRows,
	}

	locale := localeFromRequest(r)
	_ = views.ParticipantView(data, locale).Render(r.Context(), w)
}

// handleParticipantCSV serves CSV for all meetings of one participant.
func (s *Server) handleParticipantCSV(w http.ResponseWriter, r *http.Request) {
	displayName := r.PathValue("p")
	pattern := filepath.Join(s.cfg.Get().MeetingsDir, "*.parquet")

	csvStr, err := reporter.Generate(r.Context(), pattern)
	if err != nil {
		http.Error(w, "report failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// TODO: filter rows to just `displayName` instead of returning the full
	// aggregate. Today's reporter has no name filter.
	_ = displayName

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="participant-`+displayName+`.csv"`)
	_, _ = w.Write([]byte(csvStr))
}

// handleAllParticipantsCSV serves an aggregate CSV for all participants.
func (s *Server) handleAllParticipantsCSV(w http.ResponseWriter, r *http.Request) {
	pattern := filepath.Join(s.cfg.Get().MeetingsDir, "*.parquet")

	csvStr, err := reporter.Generate(r.Context(), pattern)
	if err != nil {
		http.Error(w, "report failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="participants.csv"`)
	_, _ = w.Write([]byte(csvStr))
}

// handleRegistry renders the participant registry page.
func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	entries, err := s.registry.List(r.Context())
	if err != nil {
		http.Error(w, "list registry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	locale := localeFromRequest(r)
	_ = views.Registry(entries, locale).Render(r.Context(), w)
}

// handleDeleteRegistryEntry removes one registry entry by display name
// (URL-decoded by net/http).
func (s *Server) handleDeleteRegistryEntry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.registry.UnregisterByName(r.Context(), name); err != nil {
		http.Error(w, "unregister: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleClearRegistry removes all registered participants.
func (s *Server) handleClearRegistry(w http.ResponseWriter, r *http.Request) {
	if err := s.registry.ClearAll(r.Context()); err != nil {
		http.Error(w, "clear failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
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
	data := views.ConfigData{
		MeetingsDir:                s.cfg.Get().MeetingsDir,
		QuestionsDir:               s.cfg.Get().QuestionsDir,
		ReportsDir:                 s.cfg.Get().ReportsDir,
		DataDir:                    config.DataDir(),
		RetentionDays:              s.cfg.Get().RetentionDays,
		GUIBindAddr:                s.cfg.Get().GUI.BindAddr,
		GUIPort:                    s.cfg.Get().GUI.Port,
		GUIOpenBrowserOnStart:      s.cfg.Get().GUI.OpenBrowserOnStart,
		LogLevel:                   s.cfg.Get().Logging.Level,
		LogFormat:                  s.cfg.Get().Logging.Format,
		BBBEnabled:                 s.cfg.Get().Providers.BBB.Enabled,
		BBBBaseURL:                 s.cfg.Get().Providers.BBB.BaseURL,
		TelegramEnabled:            s.cfg.Get().Messengers.Telegram.Enabled,
		TelegramBotToken:           s.cfg.Get().Messengers.Telegram.BotToken,
		AnswerWindowSeconds:        s.cfg.Get().Challenges.Defaults.AnswerWindowSeconds,
		MinGapBetweenChallengesSec: s.cfg.Get().Challenges.Defaults.MinGapBetweenChallengesSecs,
		EventStoreCompression:      s.cfg.Get().EventStore.Compression,
		EventStoreRowGroupSize:     s.cfg.Get().EventStore.RowGroupSize,
	}

	locale := localeFromRequest(r)
	_ = views.ConfigEditor(data, locale).Render(r.Context(), w)
}

// handleSaveConfig is a stub pending the GUI rewrite around the new
// config.Config API (Apply with writeOnly handling, etc.). Returns 501
// so the existing form does not silently no-op.
func (s *Server) handleSaveConfig(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "config save not yet wired to the new config API", http.StatusNotImplemented)
}

// buildServeProvider creates a provider for the serve context (no fixture support).
func buildServeProvider(name string, cfg *config.Config) (providers.Provider, error) {
	switch name {
	case "bbb":
		return bbbprovider.New(cfg), nil
	default:
		return nil, fmt.Errorf("unknown provider %q; supported: bbb", name)
	}
}
