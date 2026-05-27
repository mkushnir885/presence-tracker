// Command ptrack is the main CLI binary for the presence tracker.
// Sub-commands: track, poll, serve, report.
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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cli/browser"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"presence-tracker/src/internal/challenger"
	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/gui"
	"presence-tracker/src/internal/messengers"
	mockmessenger "presence-tracker/src/internal/messengers/mock"
	"presence-tracker/src/internal/messengers/telegram"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/providers"
	bbbprovider "presence-tracker/src/internal/providers/bbb"
	meetprovider "presence-tracker/src/internal/providers/meet"
	mockprovider "presence-tracker/src/internal/providers/mock"
	zoomprovider "presence-tracker/src/internal/providers/zoom"
	"presence-tracker/src/internal/reporter"
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
	root.AddCommand(reportCmd())
	root.AddCommand(serveCmd())
	return root
}

// trackCmd subscribes to a meeting and records events.
func trackCmd() *cobra.Command {
	var (
		cfgPath      string
		providerName string
		meetingID    string
		fixture      string
	)

	cmd := &cobra.Command{
		Use:   "track",
		Short: "Track presence for a meeting",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrack(cmd.Context(), cfgPath, providerName, meetingID, fixture)
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.yaml (default: search standard locations)")
	cmd.Flags().StringVar(&providerName, "provider", "bbb", "video-conferencing provider (bbb, meet, zoom)")
	cmd.Flags().StringVar(&meetingID, "meeting", "", "meeting ID (required when not using --fixture)")
	cmd.Flags().StringVar(&fixture, "fixture", "", "path to a recorded fixture directory for offline replay")

	return cmd
}

// pollCmd is a thin HTTP client to the running daemon.
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

	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.yaml (used only to discover the daemon port when PTRACK_PORTS is unset)")
	cmd.Flags().BoolVar(&autoSubmitted, "auto-submitted", false, "mark the poll as dispatched without teacher review")
	cmd.Flags().IntVar(&port, "port", 0, "daemon port; required when several ptrack processes are running")
	cmd.Flags().StringVar(&serverURL, "server", "", "override the daemon URL (e.g. http://127.0.0.1:8080)")

	return cmd
}

// reloadCmd asks a running daemon to re-read its config from disk.
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
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.json (used only to discover the daemon port when PTRACK_PORTS is unset)")
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
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reload rejected (HTTP %d): %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	_, _ = fmt.Fprintln(os.Stdout, "config reloaded")
	return nil
}

// reportCmd generates a CSV report from one or more meeting Parquet files.
func reportCmd() *cobra.Command {
	var (
		input  string
		output string
	)

	cmd := &cobra.Command{
		Use:   "report",
		Short: "Generate a CSV report from one or more meeting Parquet files",
		Example: `  ptrack report --in meeting.parquet --out report.csv
  ptrack report --in 'meetings/*.parquet' --out semester.csv
  ptrack report --in meeting.parquet --out -`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReport(cmd.Context(), input, output)
		},
	}

	cmd.Flags().StringVar(&input, "in", "", "Parquet file or glob pattern (e.g. 'meetings/*.parquet')")
	cmd.Flags().StringVar(&output, "out", "", "output CSV path, or - for stdout")
	_ = cmd.MarkFlagRequired("in")
	_ = cmd.MarkFlagRequired("out")

	return cmd
}

func runReport(ctx context.Context, input, output string) error {
	files, err := filepath.Glob(input)
	if err != nil {
		return fmt.Errorf("report: glob %q: %w", input, err)
	}
	if len(files) == 0 {
		// No glob match — pass the raw input through so the user sees a
		// clean error from ptrack_py instead of a silent empty result.
		files = []string{input}
	}

	csv, err := reporter.Generate(ctx, files)
	if err != nil {
		return err
	}
	if output == "-" {
		_, err = os.Stdout.Write(csv)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("report: create output directory: %w", err)
	}
	return os.WriteFile(output, csv, 0o644)
}

func runTrack(ctx context.Context, cfgPath, providerName, meetingID, fixture string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	v := cfg.Get()
	setupLogging(v.Logging)

	if fixture != "" {
		if meetingID == "" {
			meetingID = "fixture"
		}
	} else if meetingID == "" {
		return fmt.Errorf("--meeting is required (or use --fixture for offline replay)")
	}

	prov, err := buildProvider(providerName, fixture, cfg)
	if err != nil {
		return err
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

	msgr, err := buildMessenger(cfg, registry)
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

	// internalMeetingID is a time-based UUID that identifies this session in the
	// Parquet event log. It is independent of the provider meeting ID (--meeting
	// flag), which is only used for platform-side meeting lookup.
	internalMeetingID := uuid.Must(uuid.NewV7()).String()
	startTime := time.Now()

	store, err := eventstore.NewWriter(v.MeetingsDir, "", startTime, v.EventStore.Compression, v.EventStore.RowGroupSize)
	if err != nil {
		return fmt.Errorf("init event store: %w", err)
	}

	sessCfg := session.Config{
		MeetingID:                   internalMeetingID,
		PlatformMeetingID:           meetingID,
		MeetingsDir:                 v.MeetingsDir,
		QuestionsDir:                v.QuestionsDir,
		ProviderName:                prov.Name(),
		AnswerWindowSecs:            v.Challenges.Defaults.AnswerWindowSeconds,
		MinGapBetweenChallengesSecs: v.Challenges.Defaults.MinGapBetweenChallengesSecs,
		EventStoreCompression:       v.EventStore.Compression,
		RowGroupSize:                v.EventStore.RowGroupSize,
	}

	coord := session.New(sessCfg, prov, msgr, registry, store)
	router.SetHandler(coord)
	defer router.SetHandler(nil)

	var chSvc *challenger.Service
	if ag := v.Challenges.AutoGeneration; ag.Enabled {
		chSvc = challenger.New(ag, coord, coord)
		if err := chSvc.SweepReviewDir(); err != nil {
			slog.Warn("track: sweep review_dir", "err", err)
		}
	}

	mux := http.NewServeMux()
	mountPollHandler(mux, func() *session.Coordinator { return coord })
	mountReloadHandler(mux, cfg)
	mountAudioHandler(mux, func() *challenger.Service { return chSvc })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	chosen := ln.Addr().(*net.TCPAddr).Port
	if err := appendCurrentPort(chosen); err != nil {
		_ = ln.Close()
		return err
	}
	go runHTTPServer(ctx, ln, mux)

	slog.Info("tracking started", "meeting_id", internalMeetingID, "platform_meeting", meetingID, "provider", prov.Name(), "control_port", chosen)

	return coord.Run(ctx)
}

// runHTTPServer serves mux on ln until ctx is cancelled. Blocks; intended
// to be called from a goroutine when the caller needs to do other work.
func runHTTPServer(ctx context.Context, ln net.Listener, mux *http.ServeMux) {
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http: serve", "err", err)
	}
}

// pollRequest / pollResponse / pollErrorResponse are the wire types shared
// between the daemon's POST /poll endpoint and ptrack poll.
type pollRequest struct {
	AutoSubmitted bool   `json:"auto_submitted"`
	BankPath      string `json:"bank_path"`
}

type pollResponse struct {
	PollID         string `json:"poll_id"`
	ScheduledCount int    `json:"scheduled_count"`
	SkippedCount   int    `json:"skipped_count"`
}

type pollErrorResponse struct {
	Error string `json:"error"`
}

// mountPollHandler registers POST /poll on mux. coordFn returns the active
// session coordinator, or nil when no session is running yet.
func mountPollHandler(mux *http.ServeMux, coordFn func() *session.Coordinator) {
	mux.HandleFunc("POST /poll", func(w http.ResponseWriter, r *http.Request) {
		var req pollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writePollError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.BankPath == "" {
			writePollError(w, http.StatusBadRequest, "bank_path is required")
			return
		}

		coord := coordFn()
		if coord == nil {
			writePollError(w, http.StatusConflict, "no active session")
			return
		}

		result, err := coord.RunPoll(r.Context(), req.BankPath, req.AutoSubmitted)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writePollError(w, http.StatusNotFound, err.Error())
				return
			}
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

// mountReloadHandler registers POST /config/reload on mux. The handler
// calls cfg.Reload(); on success it returns 204, on validation/IO error
// it returns 422 with the error message.
func mountReloadHandler(mux *http.ServeMux, cfg *config.Config) {
	mux.HandleFunc("POST /config/reload", func(w http.ResponseWriter, _ *http.Request) {
		if err := cfg.Reload(); err != nil {
			writePollError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// audioBodyLimit caps one /audio/segment request body. 64 MB covers
// ~30 min of Opus at typical bitrates and rejects accidental raw PCM.
const audioBodyLimit = 64 << 20

// mountAudioHandler registers POST /audio/segment on mux. challengerFn
// returns the active session's challenger, or nil when none is running
// or auto-generation is disabled. The browser POSTs one Opus/WebM blob
// per poll interval; the handler runs ASR + accumulator + (optional)
// LLM in-process and returns the structured Result as JSON.
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
			writePollError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writePollJSON(w, http.StatusOK, result)
	})
}

// appendCurrentPort appends port to the PTRACK_PORTS environment variable
// (comma-separated). Child processes the daemon spawns inherit the list,
// which is how ptrack poll finds its way back to the daemon.
func appendCurrentPort(port int) error {
	existing := os.Getenv("PTRACK_PORTS")
	portStr := strconv.Itoa(port)
	if existing == "" {
		return os.Setenv("PTRACK_PORTS", portStr)
	}
	return os.Setenv("PTRACK_PORTS", existing+","+portStr)
}

// runPoll posts to the running daemon's POST /poll endpoint.
func runPoll(ctx context.Context, cfgPath, serverURL string, autoSubmitted bool, port int, bankPath string) error {
	abs, err := filepath.Abs(bankPath)
	if err != nil {
		return fmt.Errorf("resolve bank path: %w", err)
	}

	base, err := resolveDaemonURL(serverURL, cfgPath, port)
	if err != nil {
		return err
	}

	body, _ := json.Marshal(pollRequest{AutoSubmitted: autoSubmitted, BankPath: abs})
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

// resolveDaemonURL picks the daemon's base URL in priority order:
// --server flag, --port flag, PTRACK_PORTS env (when it lists exactly one
// port), config.yaml gui.port, 8080. If PTRACK_PORTS lists several ports
// and --port is not set, returns a helpful error.
func resolveDaemonURL(serverURL, cfgPath string, port int) (string, error) {
	if serverURL != "" {
		return serverURL, nil
	}
	if port != 0 {
		return fmt.Sprintf("http://127.0.0.1:%d", port), nil
	}
	if v := os.Getenv("PTRACK_PORTS"); v != "" {
		parts := strings.Split(v, ",")
		if len(parts) == 1 {
			p, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil {
				return "", fmt.Errorf("invalid PTRACK_PORTS=%q: %w", v, err)
			}
			return fmt.Sprintf("http://127.0.0.1:%d", p), nil
		}
		return "", fmt.Errorf("multiple ptrack daemons running (PTRACK_PORTS=%s); pass --port=<port>", v)
	}
	fallback := 8080
	if cfgPath != "" {
		if cfg, err := config.Load(cfgPath); err == nil && cfg.Get().GUI.Port != 0 {
			fallback = cfg.Get().GUI.Port
		}
	} else if path, ok := config.Default(); ok {
		if cfg, err := config.Load(path); err == nil && cfg.Get().GUI.Port != 0 {
			fallback = cfg.Get().GUI.Port
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d", fallback), nil
}

// serveCmd starts the GUI web server.
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
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.yaml")
	cmd.Flags().IntVar(&port, "port", 0, "override GUI port from config")
	return cmd
}

func runServe(ctx context.Context, cfgPath string, portOverride int) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if portOverride != 0 {
		if err := cfg.Apply(func(v *config.Values) { v.GUI.Port = portOverride }); err != nil {
			return fmt.Errorf("apply --port override: %w", err)
		}
	}
	v := cfg.Get()
	setupLogging(v.Logging)

	registry, err := participants.OpenBolt(config.DataDir())
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer func() { _ = registry.Close() }()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	msgr, err := buildMessenger(cfg, registry)
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
	srv.OnShutdown(cancelSrv)

	addr := fmt.Sprintf("%s:%d", v.GUI.BindAddr, v.GUI.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gui: listen %s: %w", addr, err)
	}
	chosen := ln.Addr().(*net.TCPAddr).Port
	if err := appendCurrentPort(chosen); err != nil {
		_ = ln.Close()
		return err
	}

	slog.Info("gui: server started", "addr", "http://"+addr)
	if v.GUI.OpenBrowserOnStart {
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = browser.OpenURL("http://" + addr)
		}()
	}

	runHTTPServer(srvCtx, ln, mux)
	return nil
}

func buildMessenger(cfg *config.Config, registry participants.Registry) (messengers.Messenger, error) {
	tg := cfg.Get().Messengers.Telegram
	if !tg.Enabled {
		return mockmessenger.New(), nil
	}
	m, err := telegram.New(tg.BotToken, registry)
	if err != nil {
		return nil, fmt.Errorf("init telegram: %w", err)
	}
	return m, nil
}

func buildProvider(name, fixture string, cfg *config.Config) (providers.Provider, error) {
	if fixture != "" {
		return mockprovider.New(fixture).WithSpeed(10.0), nil
	}
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

// loadConfig loads the named config file. Unlike config.Load (which
// happily uses defaults when no file exists — desired by ptrack serve),
// loadConfig fails when the file is missing. Use it for commands that
// require credentials (ptrack track).
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
