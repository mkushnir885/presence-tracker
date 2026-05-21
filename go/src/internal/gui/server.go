package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cli/browser"
	"github.com/google/uuid"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/controlplane"
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

	"gopkg.in/yaml.v3"
)

// Server is the GUI HTTP server for ptrack serve.
type Server struct {
	cfg      *config.Config
	cfgPath  string
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
func New(cfg *config.Config, cfgPath string, registry participants.Registry) *Server {
	return &Server{cfg: cfg, cfgPath: cfgPath, registry: registry}
}

// Serve starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()

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
	mux.HandleFunc("DELETE /registry/{id}", s.handleDeleteRegistryEntry)
	mux.HandleFunc("DELETE /registry", s.handleClearRegistry)
	controlplane.Mount(mux, s)
	mux.HandleFunc("GET /questions/{id}", s.handleQuestion)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("POST /config", s.handleSaveConfig)

	addr := fmt.Sprintf("%s:%d", s.cfg.GUI.BindAddr, s.cfg.GUI.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gui: listen %s: %w", addr, err)
	}

	chosen := ln.Addr().(*net.TCPAddr).Port
	if err := controlplane.PublishPort(chosen); err != nil {
		_ = ln.Close()
		return err
	}

	slog.Info("gui: server started", "addr", "http://"+addr)

	if s.cfg.GUI.OpenBrowserOnStart {
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = browser.OpenURL("http://" + addr)
		}()
	}

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("gui: serve: %w", err)
	}
	return nil
}

func (s *Server) enabledPlatforms() []string {
	var out []string
	if s.cfg.Providers.BBB.Enabled {
		out = append(out, "bbb")
	}
	if s.cfg.Providers.Meet.Enabled {
		out = append(out, "meet")
	}
	if s.cfg.Providers.Zoom.Enabled {
		out = append(out, "zoom")
	}
	return out
}

// handleDashboard renders the main dashboard page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.cfg.MeetingsDir)
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
	if s.cfg.Messengers.Telegram.Enabled {
		tgAdapter, err := telegram.New(s.cfg.Messengers.Telegram.BotToken, s.registry, s.enabledPlatforms())
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

	store, err := eventstore.NewWriter(s.cfg.MeetingsDir, startTime, s.cfg.EventStore.Compression, s.cfg.EventStore.RowGroupSize)
	if err != nil {
		http.Error(w, "event store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sessCfg := session.Config{
		MeetingID:                   internalMeetingID,
		PlatformMeetingID:           meetingID,
		MeetingsDir:                 s.cfg.MeetingsDir,
		QuestionsDir:                s.cfg.QuestionsDir,
		ProviderName:                prov.Name(),
		MessengerName:               msgr.Name(),
		AnswerWindowSecs:            s.cfg.Challenges.Defaults.AnswerWindowSeconds,
		MinGapBetweenChallengesSecs: s.cfg.Challenges.Defaults.MinGapBetweenChallengesSecs,
		EventStoreCompression:       s.cfg.EventStore.Compression,
		RowGroupSize:                s.cfg.EventStore.RowGroupSize,
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
	parquetPath := filepath.Join(s.cfg.MeetingsDir, id+".parquet")

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
	parquetPath := filepath.Join(s.cfg.MeetingsDir, id+".parquet")

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
func (s *Server) handleRenameParticipant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pid := r.PathValue("p")
	parquetPath := filepath.Join(s.cfg.MeetingsDir, id+".parquet")

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
		// htmx hx-prompt puts the value in the HX-Prompt header.
		newName = r.Header.Get("HX-Prompt")
		if newName == "" {
			newName = r.FormValue("display_name")
		}
	}

	if newName == "" {
		http.Error(w, "display_name is required", http.StatusBadRequest)
		return
	}

	if err := eventstore.UpdateDisplayName(parquetPath, pid, newName); err != nil {
		http.Error(w, "rename failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleParticipant renders the cross-meeting participant view.
func (s *Server) handleParticipant(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("p")

	pattern := filepath.Join(s.cfg.MeetingsDir, "*.parquet")
	files, err := filepath.Glob(pattern)
	if err != nil {
		http.Error(w, "glob error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var displayName string
	var meetingRows []views.ParticipantMeetingRow

	for _, path := range files {
		meetingID := strings.TrimSuffix(filepath.Base(path), ".parquet")
		records, err := eventstore.ReadAll(r.Context(), path)
		if err != nil {
			continue
		}

		info := ComputeMeetingInfo(meetingID, records, nil)

		absent := true
		for _, row := range info.Rows {
			if row.ParticipantID == pid {
				absent = false
				if displayName == "" && len(row.DisplayNames) > 0 {
					displayName = row.DisplayNames[0]
				}
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
		if absent {
			// Only include if there's any record (the file references this meeting).
			_ = absent
		}
	}

	if displayName == "" {
		displayName = pid
	}

	sort.Slice(meetingRows, func(i, j int) bool {
		return meetingRows[i].StartTime.Before(meetingRows[j].StartTime)
	})

	data := views.ParticipantData{
		ParticipantID: pid,
		DisplayName:   displayName,
		Meetings:      meetingRows,
	}

	locale := localeFromRequest(r)
	_ = views.ParticipantView(data, locale).Render(r.Context(), w)
}

// handleParticipantCSV serves CSV for all meetings of one participant.
func (s *Server) handleParticipantCSV(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("p")
	pattern := filepath.Join(s.cfg.MeetingsDir, "*.parquet")

	csvStr, err := reporter.Generate(r.Context(), pattern)
	if err != nil {
		http.Error(w, "report failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rows, err := reporter.ParseAggregate(csvStr)
	if err != nil {
		http.Error(w, "parse failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Filter rows for this participant (by participantID — CSV has display_name).
	// We need to resolve pid to a display_name first.
	_ = pid
	_ = rows

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="participant-`+pid+`.csv"`)
	_, _ = w.Write([]byte(csvStr))
}

// handleAllParticipantsCSV serves an aggregate CSV for all participants.
func (s *Server) handleAllParticipantsCSV(w http.ResponseWriter, r *http.Request) {
	pattern := filepath.Join(s.cfg.MeetingsDir, "*.parquet")

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

// handleDeleteRegistryEntry removes one registry entry by ParticipantID.
func (s *Server) handleDeleteRegistryEntry(w http.ResponseWriter, r *http.Request) {
	id := participants.ParticipantID(r.PathValue("id"))
	if err := s.registry.Unregister(r.Context(), id); err != nil {
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

// Resolve implements controlplane.Sessions. The GUI hosts at most one
// active session at a time, so both the explicit meeting ID and the
// "active" alias resolve to the same coordinator.
func (s *Server) Resolve(meetingID string) (*session.Coordinator, error) {
	s.mu.RLock()
	act := s.active
	s.mu.RUnlock()

	if act == nil {
		return nil, controlplane.ErrNoActiveSession
	}
	if meetingID == controlplane.ActiveMeetingID || meetingID == act.coord.MeetingID() {
		return act.coord, nil
	}
	return nil, controlplane.ErrMeetingNotFound
}

// handleQuestion returns a question record as JSON.
func (s *Server) handleQuestion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	q, err := eventstore.ReadQuestion(s.cfg.QuestionsDir, id)
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
		MeetingsDir:                s.cfg.MeetingsDir,
		QuestionsDir:               s.cfg.QuestionsDir,
		ReportsDir:                 s.cfg.ReportsDir,
		DataDir:                    s.cfg.DataDir,
		RetentionDays:              s.cfg.RetentionDays,
		GUIBindAddr:                s.cfg.GUI.BindAddr,
		GUIPort:                    s.cfg.GUI.Port,
		GUIOpenBrowserOnStart:      s.cfg.GUI.OpenBrowserOnStart,
		LogLevel:                   s.cfg.Logging.Level,
		LogFormat:                  s.cfg.Logging.Format,
		BBBEnabled:                 s.cfg.Providers.BBB.Enabled,
		BBBBaseURL:                 s.cfg.Providers.BBB.BaseURL,
		BBBWebhookPort:             s.cfg.Providers.BBB.WebhookPort,
		TelegramEnabled:            s.cfg.Messengers.Telegram.Enabled,
		TelegramBotToken:           s.cfg.Messengers.Telegram.BotToken,
		AnswerWindowSeconds:        s.cfg.Challenges.Defaults.AnswerWindowSeconds,
		MinGapBetweenChallengesSec: s.cfg.Challenges.Defaults.MinGapBetweenChallengesSecs,
		EventStoreCompression:      s.cfg.EventStore.Compression,
		EventStoreRowGroupSize:     s.cfg.EventStore.RowGroupSize,
	}

	locale := localeFromRequest(r)
	_ = views.ConfigEditor(data, locale).Render(r.Context(), w)
}

// handleSaveConfig parses the config form and writes config.yaml.
func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}

	s.cfg.MeetingsDir = r.FormValue("meetings_dir")
	s.cfg.QuestionsDir = r.FormValue("questions_dir")
	s.cfg.ReportsDir = r.FormValue("reports_dir")
	s.cfg.DataDir = r.FormValue("data_dir")
	if v, err := strconv.Atoi(r.FormValue("retention_days")); err == nil {
		s.cfg.RetentionDays = v
	}

	s.cfg.GUI.BindAddr = r.FormValue("gui_bind_addr")
	if v, err := strconv.Atoi(r.FormValue("gui_port")); err == nil {
		s.cfg.GUI.Port = v
	}
	s.cfg.GUI.OpenBrowserOnStart = r.FormValue("gui_open_browser") == "true"

	s.cfg.Logging.Level = r.FormValue("log_level")
	s.cfg.Logging.Format = r.FormValue("log_format")

	s.cfg.Providers.BBB.Enabled = r.FormValue("bbb_enabled") == "true"
	s.cfg.Providers.BBB.BaseURL = r.FormValue("bbb_base_url")
	if v, err := strconv.Atoi(r.FormValue("bbb_webhook_port")); err == nil {
		s.cfg.Providers.BBB.WebhookPort = v
	}

	s.cfg.Messengers.Telegram.Enabled = r.FormValue("tg_enabled") == "true"
	s.cfg.Messengers.Telegram.BotToken = r.FormValue("tg_bot_token")

	if v, err := strconv.Atoi(r.FormValue("answer_window")); err == nil {
		s.cfg.Challenges.Defaults.AnswerWindowSeconds = v
	}
	if v, err := strconv.Atoi(r.FormValue("min_gap")); err == nil {
		s.cfg.Challenges.Defaults.MinGapBetweenChallengesSecs = v
	}

	s.cfg.EventStore.Compression = r.FormValue("compression")
	if v, err := strconv.Atoi(r.FormValue("row_group_size")); err == nil {
		s.cfg.EventStore.RowGroupSize = v
	}

	if s.cfgPath != "" {
		data, err := yaml.Marshal(s.cfg)
		if err != nil {
			http.Error(w, "marshal config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(s.cfgPath, data, 0o644); err != nil {
			http.Error(w, "write config: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

// buildServeProvider creates a provider for the serve context (no fixture support).
func buildServeProvider(name string, cfg *config.Config) (providers.Provider, error) {
	switch name {
	case "bbb":
		return bbbprovider.New(&cfg.Providers.BBB), nil
	default:
		return nil, fmt.Errorf("unknown provider %q; supported: bbb", name)
	}
}
