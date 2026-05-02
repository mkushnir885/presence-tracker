// Command ptrack is the main CLI binary for the presence tracker.
// Sub-commands: track, poll, serve, report, export.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/challenges/filebased"
	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/messengers/telegram"
	"presence-tracker/src/internal/participants"
	"presence-tracker/src/internal/paths"
	"presence-tracker/src/internal/providers"
	bbbprovider "presence-tracker/src/internal/providers/bbb"
	mockprovider "presence-tracker/src/internal/providers/mock"
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
	// TODO: add serve, report, export commands
	return root
}

// trackCmd subscribes to a meeting and records events.
func trackCmd() *cobra.Command {
	var (
		cfgPath      string
		providerName string
		meetingID    string
		fixture      string
		bankPath     string
		pollEvery    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "track",
		Short: "Track presence for a meeting",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrack(cmd.Context(), cfgPath, providerName, meetingID, fixture, bankPath, pollEvery)
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", "", "path to config.yaml (default: search standard locations)")
	cmd.Flags().StringVar(&providerName, "provider", "bbb", "video-conferencing provider (bbb)")
	cmd.Flags().StringVar(&meetingID, "meeting", "", "meeting ID (required when not using --fixture)")
	cmd.Flags().StringVar(&fixture, "fixture", "", "path to a recorded fixture directory for offline replay")
	cmd.Flags().StringVar(&bankPath, "poll-bank", "", "question-bank YAML to use for automatic polls (optional)")
	cmd.Flags().DurationVar(&pollEvery, "poll-interval", 0, "trigger a poll at this fixed interval (requires --poll-bank)")

	return cmd
}

// pollCmd triggers a challenge poll in the currently running tracker session.
func pollCmd() *cobra.Command {
	var bankPath string

	cmd := &cobra.Command{
		Use:   "poll",
		Short: "Trigger a challenge poll in the running tracker session",
		Long: `Load a question-bank YAML file and trigger an immediate challenge poll
in the ptrack track session that is currently running on this machine.

ptrack track must be running before calling this command.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPoll(cmd.Context(), bankPath)
		},
	}

	cmd.Flags().StringVar(&bankPath, "bank", "", "question-bank YAML file")
	_ = cmd.MarkFlagRequired("bank")

	return cmd
}

func runTrack(ctx context.Context, cfgPath, providerName, meetingID, fixture, bankPath string, pollEvery time.Duration) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	setupLogging(cfg.Logging)

	if fixture != "" {
		if meetingID == "" {
			meetingID = "replay-" + time.Now().Format("20060102T150405")
		}
	} else if meetingID == "" {
		return fmt.Errorf("--meeting is required (or use --fixture for offline replay)")
	}

	prov, err := buildProvider(providerName, fixture, &cfg.Providers.BBB)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := prov.Authenticate(ctx); err != nil {
		return fmt.Errorf("provider authenticate: %w", err)
	}

	ttl := time.Duration(cfg.Messengers.Telegram.PairingCodeTTLHours) * time.Hour
	registry, err := participants.OpenBolt(cfg.DataDir, ttl)
	if err != nil {
		return fmt.Errorf("open participant registry: %w", err)
	}
	defer func() {
		if err := registry.Close(); err != nil {
			slog.Error("track: close registry", "err", err)
		}
	}()

	if !cfg.Messengers.Telegram.Enabled {
		return errors.New("no messenger is enabled; set messengers.telegram.enabled: true")
	}
	generateCode := func(ctx context.Context, handle string) (string, error) {
		return registry.StartPairing(ctx, "telegram", participants.Handle(handle))
	}
	tgAdapter, err := telegram.New(cfg.Messengers.Telegram.BotToken, generateCode, session.PairingPrefix)
	if err != nil {
		return fmt.Errorf("init telegram: %w", err)
	}

	store, err := eventstore.NewWriter(cfg.MeetingsDir, meetingID, cfg.EventStore.Compression, cfg.EventStore.RowGroupSize)
	if err != nil {
		return fmt.Errorf("init event store: %w", err)
	}

	sessCfg := session.Config{
		MeetingID:                   meetingID,
		MeetingsDir:                 cfg.MeetingsDir,
		QuestionsDir:                cfg.QuestionsDir,
		ProviderName:                prov.Name(),
		MessengerName:               tgAdapter.Name(),
		AnswerWindowSecs:            cfg.Challenges.Defaults.AnswerWindowSeconds,
		MinGapBetweenChallengesSecs: cfg.Challenges.Defaults.MinGapBetweenChallengesSecs,
		EventStoreCompression:       cfg.EventStore.Compression,
		RowGroupSize:                cfg.EventStore.RowGroupSize,
	}

	coord := session.New(sessCfg, prov, tgAdapter, registry, store)

	// Always start the control socket so ptrack poll can trigger rounds.
	coord.Listen(ctx, paths.ControlSocketPath(), func(path string) (challenges.ChallengeType, error) {
		fb := &filebased.ChallengeType{}
		return fb, fb.LoadBank(path)
	})

	if bankPath != "" {
		fb := &filebased.ChallengeType{}
		if err := fb.LoadBank(bankPath); err != nil {
			return fmt.Errorf("load question bank: %w", err)
		}
		window := time.Duration(cfg.Challenges.Defaults.AnswerWindowSeconds) * time.Second
		poller := challenges.NewPoller(fb, coord, window)
		coord.SetPoller(poller)
	}

	if pollEvery > 0 && bankPath != "" {
		go func() {
			t := time.NewTicker(pollEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					slog.Info("track: triggering scheduled poll")
					if err := coord.TriggerPoll(ctx); err != nil {
						slog.Error("track: poll failed", "err", err)
					}
				}
			}
		}()
	}

	slog.Info("tracking started", "meeting", meetingID, "provider", prov.Name())
	return coord.Run(ctx)
}

// runPoll connects to the running tracker session via the control socket and
// asks it to trigger a poll using the given question-bank file.
func runPoll(ctx context.Context, bankPath string) error {
	abs, err := filepath.Abs(bankPath)
	if err != nil {
		return fmt.Errorf("resolve bank path: %w", err)
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", paths.ControlSocketPath())
	if err != nil {
		return fmt.Errorf("connect to running tracker: %w\n(is ptrack track running?)", err)
	}
	defer func() { _ = conn.Close() }()

	req := struct {
		BankPath string `json:"bank_path"`
	}{BankPath: abs}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send poll request: %w", err)
	}

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("read poll response: %w", err)
	}

	if !resp.OK {
		return fmt.Errorf("poll failed: %s", resp.Error)
	}

	slog.Info("poll triggered successfully")
	return nil
}

func buildProvider(name, fixture string, bbbCfg *config.BBBConfig) (providers.Provider, error) {
	if fixture != "" {
		return mockprovider.New(fixture).WithSpeed(10.0), nil
	}
	switch name {
	case "bbb":
		return bbbprovider.New(bbbCfg), nil
	default:
		return nil, fmt.Errorf("unknown provider %q; supported: bbb", name)
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
