package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cli/browser"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"presence-tracker/src/internal/challenger"
	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/gui"
	"presence-tracker/src/internal/messengers"
	mockmessenger "presence-tracker/src/internal/messengers/mock"
	"presence-tracker/src/internal/messengers/telegram"
	"presence-tracker/src/internal/mockfixture"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/providers"
	bbbprovider "presence-tracker/src/internal/providers/bbb"
	meetprovider "presence-tracker/src/internal/providers/meet"
	mockprovider "presence-tracker/src/internal/providers/mock"
	zoomprovider "presence-tracker/src/internal/providers/zoom"
	"presence-tracker/src/internal/ptrackpy"
	"presence-tracker/src/internal/session"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ptrack",
		Short: "Presence tracker for online lessons",
	}
	root.AddCommand(trackCmd())
	root.AddCommand(pollCmd())
	root.AddCommand(reloadCmd())
	root.AddCommand(renameCmd())
	root.AddCommand(reportCmd())
	root.AddCommand(serveCmd())
	return root
}

func renameCmd() *cobra.Command {
	var fromName, toName string
	cmd := &cobra.Command{
		Use:   "rename --from <old> --to <new> <meeting-dirs...>",
		Short: "Rewrite display_name across one or more meetings",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRename(cmd.Context(), fromName, toName, args)
		},
	}
	cmd.Flags().StringVar(&fromName, "from", "", "display name to replace")
	cmd.Flags().StringVar(&toName, "to", "", "new display name")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func runRename(ctx context.Context, fromName, toName string, inputs []string) error {
	if fromName == toName {
		return nil
	}
	dirs, err := eventstore.ResolveMeetingDirs(inputs)
	if err != nil {
		return err
	}
	args := append([]string{"rename", "--from", fromName, "--to", toName}, dirs...)
	if _, err := ptrackpy.Run(ctx, args...); err != nil {
		return err
	}
	return nil
}

func trackCmd() *cobra.Command {
	var (
		cfgPath      string
		providerName string
		meetingID    string
		port         int
	)

	cmd := &cobra.Command{
		Use:   "track",
		Short: "Track presence for a meeting",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrack(cmd.Context(), cfgPath, providerName, meetingID, port)
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.json (default: search standard locations)")
	cmd.Flags().StringVar(&providerName, "provider", "bbb", "video-conferencing provider (bbb, meet, zoom)")
	cmd.Flags().StringVar(&meetingID, "meeting", "", "meeting ID")
	cmd.Flags().IntVar(&port, "port", 0, "control-plane port; overrides gui.port from config")

	return cmd
}

func pollCmd() *cobra.Command {
	var (
		cfgPath       string
		autoSubmitted bool
		port          int
		serverURL     string
	)

	cmd := &cobra.Command{
		Use:   "poll <path-to-bank.yaml>",
		Short: "Trigger a challenge poll on the running ptrack daemon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPoll(cmd.Context(), cfgPath, serverURL, autoSubmitted, port, args[0])
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.json (used only to discover the daemon's gui.port)")
	cmd.Flags().BoolVar(&autoSubmitted, "auto-submitted", false, "mark the poll as dispatched without teacher review")
	cmd.Flags().IntVar(&port, "port", 0, "daemon port; required when several ptrack processes are running")
	cmd.Flags().StringVar(&serverURL, "server", "", "override the daemon URL (e.g. http://127.0.0.1:8080)")

	return cmd
}

func reloadCmd() *cobra.Command {
	var (
		cfgPath   string
		port      int
		serverURL string
	)
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Ask the running ptrack daemon to re-read its config from disk",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReload(cmd.Context(), cfgPath, serverURL, port)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.json (used only to discover the daemon's gui.port)")
	cmd.Flags().IntVar(&port, "port", 0, "daemon port; required when several ptrack processes are running")
	cmd.Flags().StringVar(&serverURL, "server", "", "override the daemon URL (e.g. http://127.0.0.1:8080)")
	return cmd
}

func runReload(ctx context.Context, cfgPath, serverURL string, port int) error {
	base, err := resolveDaemonURL(serverURL, cfgPath, port)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/config/reload", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact ptrack daemon at %s: %w\n(is ptrack track or ptrack serve running?)", base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("reload rejected (HTTP %d): %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	_, _ = fmt.Fprintln(os.Stdout, "config reloaded")
	return nil
}

func reportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report <meeting-dirs...>",
		Short: "Generate a CSV report from one or more meeting directories",
		Long: "Pass one or more meeting-directory paths or glob patterns. With " +
			"a single matched directory the output is a per-meeting CSV; with " +
			"more it switches to the cross-meeting aggregate. CSV is " +
			"written to stdout — redirect to a file when needed.",
		Example: `  ptrack report meetings/270526_1900-270526_2030 > report.csv
  ptrack report 'meetings/*' > semester.csv
  ptrack report meetings/jan meetings/feb > q1.csv`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReport(cmd.Context(), args)
		},
	}
	return cmd
}

func runReport(ctx context.Context, inputs []string) error {
	dirs, err := eventstore.ResolveMeetingDirs(inputs)
	if err != nil {
		return err
	}
	args := append([]string{"report"}, dirs...)
	csv, err := ptrackpy.Run(ctx, args...)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(csv)
	return err
}

func runTrack(ctx context.Context, cfgPath, providerName, meetingID string, portOverride int) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	v := cfg.Get()
	setupLogging(v.Logging)
	bindPort := v.GUI.Port
	if portOverride != 0 {
		bindPort = portOverride
	}

	if meetingID == "" {
		return fmt.Errorf("--meeting is required")
	}

	usingMock := providerName == mockprovider.Name
	var loadedFixture *mockfixture.Fixture
	if usingMock {
		loadedFixture, err = mockfixture.Load(meetingID)
		if err != nil {
			return err
		}

		speed := 10.0
		if env, ok := os.LookupEnv("FIXTURE_SPEED"); ok {
			if x, err := strconv.ParseFloat(env, 64); err == nil {
				speed = x
			}
		}
		loadedFixture.WithSpeed(speed)
	}

	prov, err := buildProvider(providerName, loadedFixture, cfg)
	if err != nil {
		return err
	}

	if usingMock {
		meetingID = "fixture"
	} else {
		meetingID, err = prov.ParseMeetingID(meetingID)
		if err != nil {
			return fmt.Errorf("meeting input: %w", err)
		}
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := prov.Authenticate(ctx); err != nil {
		return fmt.Errorf("provider authenticate: %w", err)
	}

	registry, err := participants.OpenBolt(config.DataDir())
	if err != nil {
		return fmt.Errorf("open participant registry: %w", err)
	}
	defer func() {
		if err := registry.Close(); err != nil {
			slog.Error("track: close registry", "err", err)
		}
	}()

	msgr, err := buildMessenger(cfg, registry, loadedFixture)
	if err != nil {
		return err
	}
	router := messengers.NewRouter(msgr)
	if err := router.Start(ctx); err != nil {
		return fmt.Errorf("start messenger: %w", err)
	}
	defer func() { //nolint:contextcheck // ctx is cancelled by the time this runs; shutdown uses a fresh context
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := router.Stop(stopCtx); err != nil {
			slog.Error("track: stop messenger", "err", err)
		}
	}()

	// Identifies the session in the event log, independent of the
	// provider-side --meeting ID used only for platform lookup.
	internalMeetingID := uuid.Must(uuid.NewV7()).String()
	startTime := time.Now()

	tmpl, err := eventstore.ParseDirTemplate(v.MeetingsDirFormat)
	if err != nil {
		return fmt.Errorf("meetings_dir_format: %w", err)
	}
	store, err := eventstore.NewWriter(v.MeetingsDir, internalMeetingID, tmpl, startTime)
	if err != nil {
		return fmt.Errorf("init event store: %w", err)
	}

	sessCfg := session.Config{
		MeetingID:         internalMeetingID,
		PlatformMeetingID: meetingID,
		ProviderName:      prov.Name(),
	}

	coord := session.New(sessCfg, cfg, prov, msgr, registry, store)
	router.SetHandler(coord)
	defer router.SetHandler(nil)

	chSvc := challenger.New(cfg, coord, coord)

	mux := http.NewServeMux()
	mountPollHandler(mux, func() *session.Coordinator { return coord })
	mountReloadHandler(mux, cfg)
	mountAudioHandler(mux, func() *challenger.Service { return chSvc })

	addr := fmt.Sprintf("127.0.0.1:%d", bindPort)
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return wrapPortBindError(addr, err)
	}
	if usingMock {
		loadedFixture.SetDaemonAddr("http://" + ln.Addr().String())
	}
	go runHTTPServer(ctx, ln, mux, nil)

	slog.Info("tracking started", "meeting_id", internalMeetingID, "platform_meeting", meetingID, "provider", prov.Name(), "control_port", bindPort)

	if err := coord.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// runHTTPServer serves mux until ctx is cancelled. beforeShutdown, if set,
// runs before the graceful shutdown begins — the GUI uses it to release its
// long-lived SSE handlers so shutdown isn't held up.
func runHTTPServer(ctx context.Context, ln net.Listener, mux *http.ServeMux, beforeShutdown func()) {
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		if beforeShutdown != nil {
			beforeShutdown()
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx) //nolint:contextcheck // graceful shutdown runs after the parent ctx is already cancelled
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http: serve", "err", err)
	}
}

// pollRequest/pollResponse are the POST /poll wire types, shared between the
// daemon and the ptrack poll client.
type pollRequest struct {
	AutoSubmitted bool   `json:"auto_submitted"`
	BankContent   string `json:"bank"`
}

type pollResponse struct {
	PollID         string `json:"poll_id"`
	ScheduledCount int    `json:"scheduled_count"`
	SkippedCount   int    `json:"skipped_count"`
}

type pollErrorResponse struct {
	Error string `json:"error"`
}

// mountPollHandler mounts POST /poll. coordFn returns the active session's
// coordinator, or nil when no session is running.
func mountPollHandler(mux *http.ServeMux, coordFn func() *session.Coordinator) {
	mux.HandleFunc("POST /poll", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, pollBodyLimit)
		var req pollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writePollError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.BankContent == "" {
			writePollError(w, http.StatusBadRequest, "bank is required")
			return
		}

		coord := coordFn()
		if coord == nil {
			writePollError(w, http.StatusConflict, "no active session")
			return
		}

		bank, err := challenges.Parse([]byte(req.BankContent))
		if err != nil {
			writePollError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		result, err := coord.RunPollBank(r.Context(), bank, req.AutoSubmitted)
		if err != nil {
			writePollError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		writePollJSON(w, http.StatusOK, pollResponse{
			PollID:         result.PollID,
			ScheduledCount: result.ScheduledCount,
			SkippedCount:   result.SkippedCount,
		})
	})
}

func writePollJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) //nolint:errchkjson // response bodies are simple structs
}

func writePollError(w http.ResponseWriter, status int, msg string) {
	writePollJSON(w, status, pollErrorResponse{Error: msg})
}

func mountReloadHandler(mux *http.ServeMux, cfg *config.Config) {
	mux.HandleFunc("POST /config/reload", func(w http.ResponseWriter, _ *http.Request) {
		if err := cfg.Reload(); err != nil {
			writePollError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// pollBodyLimit caps one POST /poll request body (~1 MB is generous for any YAML bank).
const pollBodyLimit = 1 << 20

// audioBodyLimit caps one /audio/segment upload (~30 min of Opus).
const audioBodyLimit = 64 << 20

func mountAudioHandler(mux *http.ServeMux, challengerFn func() *challenger.Service) {
	mux.HandleFunc("POST /audio/segment", func(w http.ResponseWriter, r *http.Request) {
		svc := challengerFn()
		if svc == nil {
			writePollError(w, http.StatusConflict, "no active session or auto-generation disabled")
			return
		}
		mime := r.Header.Get("Content-Type")
		if mime == "" {
			mime = "audio/webm"
		}
		body := http.MaxBytesReader(w, r.Body, audioBodyLimit)
		defer func() { _ = body.Close() }()

		result, err := svc.Generate(r.Context(), body, mime)
		if err != nil {
			slog.Warn("audio: segment processing failed", "err", err)
			writePollError(w, http.StatusInternalServerError, err.Error())
			return
		}
		switch result.Status {
		case challenger.StatusGenerated:
			slog.Info("audio: bank generated",
				"questions", result.Questions, "auto_submit", result.AutoSubmit)
		case challenger.StatusSkipped:
			slog.Info("audio: segment skipped",
				"reason", result.Reason, "words", result.Words, "needed", result.Needed)
		case challenger.StatusFailed:
			slog.Warn("audio: generation failed", "reason", result.Reason)
		}
		writePollJSON(w, http.StatusOK, result)
	})
}

func wrapPortBindError(addr string, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("listen %s: address already in use — another ptrack daemon is likely running on this port; pass --port=<free port> or stop the other daemon", addr)
	}
	return fmt.Errorf("listen %s: %w", addr, err)
}

func runPoll(ctx context.Context, cfgPath, serverURL string, autoSubmitted bool, port int, bankPath string) error {
	content, err := os.ReadFile(bankPath)
	if err != nil {
		return fmt.Errorf("read bank: %w", err)
	}

	base, err := resolveDaemonURL(serverURL, cfgPath, port)
	if err != nil {
		return err
	}

	body, _ := json.Marshal(pollRequest{AutoSubmitted: autoSubmitted, BankContent: string(content)}) //nolint:errchkjson // plain bool+string struct cannot fail to marshal
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/poll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact ptrack daemon at %s: %w\n(is ptrack track or ptrack serve running?)", base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("poll rejected (HTTP %d): %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	_, _ = fmt.Fprintln(os.Stdout, string(bytes.TrimSpace(respBody)))
	return nil
}

func resolveDaemonURL(serverURL, cfgPath string, port int) (string, error) {
	if serverURL != "" {
		return serverURL, nil
	}
	if port != 0 {
		return fmt.Sprintf("http://127.0.0.1:%d", port), nil
	}
	if cfgPath != "" {
		if cfg, err := config.Load(cfgPath); err == nil && cfg.Get().GUI.Port != 0 {
			return fmt.Sprintf("http://127.0.0.1:%d", cfg.Get().GUI.Port), nil
		}
	} else if path, ok := config.Default(); ok {
		if cfg, err := config.Load(path); err == nil && cfg.Get().GUI.Port != 0 {
			return fmt.Sprintf("http://127.0.0.1:%d", cfg.Get().GUI.Port), nil
		}
	}
	return "", fmt.Errorf("cannot determine daemon URL: pass --port=<port> (or --server=<url>), or set gui.port in config")
}

func serveCmd() *cobra.Command {
	var (
		cfgPath string
		port    int
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the GUI web server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), cfgPath, port)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.json")
	cmd.Flags().IntVar(&port, "port", 0, "override GUI port from config")
	return cmd
}

func runServe(ctx context.Context, cfgPath string, portOverride int) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	v := cfg.Get()
	setupLogging(v.Logging)
	bindPort := v.GUI.Port
	if portOverride != 0 {
		bindPort = portOverride
	}

	registry, err := participants.OpenBolt(config.DataDir())
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer func() { _ = registry.Close() }()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	msgr, err := buildMessenger(cfg, registry, nil)
	if err != nil {
		return err
	}
	router := messengers.NewRouter(msgr)
	if err := router.Start(ctx); err != nil {
		return fmt.Errorf("start messenger: %w", err)
	}
	defer func() { //nolint:contextcheck // ctx is cancelled by the time this runs; shutdown uses a fresh context
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := router.Stop(stopCtx); err != nil {
			slog.Error("serve: stop messenger", "err", err)
		}
	}()

	srv := gui.New(cfg, registry, router)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	mountPollHandler(mux, srv.Coord)
	mountReloadHandler(mux, cfg)
	mountAudioHandler(mux, srv.Challenger)

	srvCtx, cancelSrv := context.WithCancel(ctx)
	defer cancelSrv()
	// The shutdown button cancels the signal context (not just srvCtx) so
	// the messenger router's forwarding goroutine unblocks and the
	// deferred router.Stop() can return instead of hanging.
	srv.OnShutdown(stop)

	addr := fmt.Sprintf("%s:%d", v.GUI.BindAddr, bindPort)
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return wrapPortBindError(addr, err)
	}

	slog.Info("gui: server started", "addr", "http://"+addr)
	if v.GUI.OpenBrowserOnStart {
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = browser.OpenURL("http://" + addr)
		}()
	}

	runHTTPServer(srvCtx, ln, mux, srv.SignalShutdown)
	return nil
}

func buildMessenger(cfg *config.Config, registry participants.Registry, fixture *mockfixture.Fixture) (messengers.Messenger, error) {
	if fixture != nil {
		return mockmessenger.New(fixture, registry), nil
	}
	tg := cfg.Get().Messengers.Telegram
	m, err := telegram.New(tg.BotToken, registry)
	if err != nil {
		return nil, fmt.Errorf("init telegram: %w", err)
	}
	return m, nil
}

func buildProvider(name string, fixture *mockfixture.Fixture, cfg *config.Config) (providers.Provider, error) {
	switch name {
	case mockprovider.Name:
		return mockprovider.New(fixture), nil
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

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		var ok bool
		path, ok = config.Default()
		if !ok {
			return nil, errors.New("no config file found; create config.json in the OS config directory or pass --config")
		}
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("config: %s not found: %w", path, err)
	}
	return config.Load(path)
}

func setupLogging(cfg config.LoggingConfig) {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}
