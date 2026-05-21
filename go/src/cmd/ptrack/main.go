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
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/controlplane"
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

// pollCmd is a thin HTTP client to the running daemon's control plane.
func pollCmd() *cobra.Command {
	var (
		cfgPath   string
		typeLabel string
		meetingID string
		serverURL string
	)

	cmd := &cobra.Command{
		Use:   "poll <path-to-bank.yaml>",
		Short: "Trigger a challenge poll on the running ptrack daemon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPoll(cmd.Context(), cfgPath, serverURL, typeLabel, meetingID, args[0])
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.yaml (used only to discover the daemon port when PTRACK_PORT is not set)")
	cmd.Flags().StringVar(&typeLabel, "type", "custom", "free-form producer label stored on every challenge_issued event")
	cmd.Flags().StringVar(&meetingID, "meeting", "", "meeting ID; defaults to the single active session")
	cmd.Flags().StringVar(&serverURL, "server", "", "override the daemon URL (e.g. http://127.0.0.1:8080)")

	return cmd
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
	csv, err := reporter.Generate(ctx, input)
	if err != nil {
		return err
	}
	if output == "-" {
		_, err = fmt.Fprint(os.Stdout, csv)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("report: create output directory: %w", err)
	}
	return os.WriteFile(output, []byte(csv), 0o644)
}

func runTrack(ctx context.Context, cfgPath, providerName, meetingID, fixture string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	setupLogging(cfg.Logging)

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

	registry, err := participants.OpenBolt(cfg.DataDir)
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

	// internalMeetingID is a time-based UUID that identifies this session in the
	// Parquet event log. It is independent of the provider meeting ID (--meeting
	// flag), which is only used for platform-side meeting lookup.
	internalMeetingID := uuid.Must(uuid.NewV7()).String()
	startTime := time.Now()

	store, err := eventstore.NewWriter(cfg.MeetingsDir, startTime, cfg.EventStore.Compression, cfg.EventStore.RowGroupSize)
	if err != nil {
		return fmt.Errorf("init event store: %w", err)
	}

	sessCfg := session.Config{
		MeetingID:                   internalMeetingID,
		PlatformMeetingID:           meetingID,
		MeetingsDir:                 cfg.MeetingsDir,
		QuestionsDir:                cfg.QuestionsDir,
		ProviderName:                prov.Name(),
		MessengerName:               msgr.Name(),
		AnswerWindowSecs:            cfg.Challenges.Defaults.AnswerWindowSeconds,
		MinGapBetweenChallengesSecs: cfg.Challenges.Defaults.MinGapBetweenChallengesSecs,
		EventStoreCompression:       cfg.EventStore.Compression,
		RowGroupSize:                cfg.EventStore.RowGroupSize,
	}

	coord := session.New(sessCfg, prov, msgr, registry, store)

	port, err := startControlPlane(ctx, &controlplane.SingleSession{Coord: coord}, 0)
	if err != nil {
		return err
	}
	slog.Info("tracking started", "meeting_id", internalMeetingID, "platform_meeting", meetingID, "provider", prov.Name(), "control_port", port)

	return coord.Run(ctx)
}

// startControlPlane binds a loopback TCP listener (port 0 → random free),
// mounts the controlplane routes, publishes PTRACK_PORT, and serves until
// ctx is cancelled. Returns the chosen port.
func startControlPlane(ctx context.Context, sessions controlplane.Sessions, port int) (int, error) {
	mux := http.NewServeMux()
	controlplane.Mount(mux, sessions)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("controlplane: listen %s: %w", addr, err)
	}
	chosen := ln.Addr().(*net.TCPAddr).Port
	if err := controlplane.PublishPort(chosen); err != nil {
		_ = ln.Close()
		return 0, err
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("controlplane: serve", "err", err)
		}
	}()
	return chosen, nil
}

// runPoll posts to the running daemon's POST /meetings/{id}/polls endpoint.
func runPoll(ctx context.Context, cfgPath, serverURL, typeLabel, meetingID, bankPath string) error {
	abs, err := filepath.Abs(bankPath)
	if err != nil {
		return fmt.Errorf("resolve bank path: %w", err)
	}

	base, err := resolveDaemonURL(serverURL, cfgPath)
	if err != nil {
		return err
	}

	id := meetingID
	if id == "" {
		id = controlplane.ActiveMeetingID
	}

	body, _ := json.Marshal(map[string]string{"type": typeLabel, "bank_path": abs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/meetings/"+id+"/polls", bytes.NewReader(body))
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
// --server flag, PTRACK_PORT env, config.yaml gui.port, 8080.
func resolveDaemonURL(serverURL, cfgPath string) (string, error) {
	if serverURL != "" {
		return serverURL, nil
	}
	if v := os.Getenv("PTRACK_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("invalid PTRACK_PORT=%q: %w", v, err)
		}
		return fmt.Sprintf("http://127.0.0.1:%d", port), nil
	}
	port := 8080
	if cfgPath != "" {
		if cfg, err := config.Load(cfgPath); err == nil && cfg.GUI.Port != 0 {
			port = cfg.GUI.Port
		}
	} else if path, ok := config.Default(); ok {
		if cfg, err := config.Load(path); err == nil && cfg.GUI.Port != 0 {
			port = cfg.GUI.Port
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port), nil
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
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	setupLogging(cfg.Logging)
	if portOverride != 0 {
		cfg.GUI.Port = portOverride
	}

	registry, err := participants.OpenBolt(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer func() { _ = registry.Close() }()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := gui.New(cfg, cfgPath, registry)
	return srv.Serve(ctx)
}

func enabledPlatforms(cfg *config.Config) []string {
	var out []string
	if cfg.Providers.BBB.Enabled {
		out = append(out, "bbb")
	}
	if cfg.Providers.Meet.Enabled {
		out = append(out, "meet")
	}
	if cfg.Providers.Zoom.Enabled {
		out = append(out, "zoom")
	}
	return out
}

func buildMessenger(cfg *config.Config, registry participants.Registry) (messengers.Messenger, error) {
	if !cfg.Messengers.Telegram.Enabled {
		return mockmessenger.New(), nil
	}
	tg, err := telegram.New(cfg.Messengers.Telegram.BotToken, registry, enabledPlatforms(cfg))
	if err != nil {
		return nil, fmt.Errorf("init telegram: %w", err)
	}
	return tg, nil
}

func buildProvider(name, fixture string, cfg *config.Config) (providers.Provider, error) {
	if fixture != "" {
		return mockprovider.New(fixture).WithSpeed(10.0), nil
	}
	switch name {
	case "bbb":
		return bbbprovider.New(&cfg.Providers.BBB), nil
	case "meet":
		return meetprovider.New(&cfg.Providers.Meet, cfg.DataDir), nil
	case "zoom":
		return zoomprovider.New(&cfg.Providers.Zoom, cfg.DataDir), nil
	default:
		return nil, fmt.Errorf("unknown provider %q; supported: bbb, meet, zoom", name)
	}
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		var ok bool
		path, ok = config.Default()
		if !ok {
			return nil, errors.New("no config file found; create config.yaml in the OS config directory or pass --config")
		}
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
