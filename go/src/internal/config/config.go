package config

import (
	"path/filepath"

	"presence-tracker/src/internal/paths"
)

// Config is the top-level runtime configuration loaded from config.yaml.
type Config struct {
	SchemaVersion int              `yaml:"schema_version"`
	MeetingsDir   string           `yaml:"meetings_dir"`
	QuestionsDir  string           `yaml:"questions_dir"`
	ReportsDir    string           `yaml:"reports_dir"`
	DataDir       string           `yaml:"data_dir"`
	CacheDir      string           `yaml:"cache_dir"`
	RetentionDays int              `yaml:"retention_days"`
	Providers     ProvidersConfig  `yaml:"providers"`
	Messengers    MessengersConfig `yaml:"messengers"`
	Challenges    ChallengesConfig `yaml:"challenges"`
	EventStore    EventStoreConfig `yaml:"eventstore"`
	GUI           GUIConfig        `yaml:"gui"`
	Logging       LoggingConfig    `yaml:"logging"`
}

type ProvidersConfig struct {
	BBB  BBBConfig  `yaml:"bbb"`
	Meet MeetConfig `yaml:"meet"` // TODO: Meet provider not implemented yet
	Zoom ZoomConfig `yaml:"zoom"` // TODO: Zoom provider not implemented yet
}

type BBBConfig struct {
	Enabled       bool   `yaml:"enabled"`
	BaseURL       string `yaml:"base_url"`
	SharedSecret  string `yaml:"shared_secret"`
	WebhookPort   int    `yaml:"webhook_port"`
	WebhookHost   string `yaml:"webhook_host"`    // publicly-reachable hostname/IP for the BBB callback; defaults to "localhost"
	WebhookSecret string `yaml:"webhook_secret"`  // optional extra HMAC secret for hook payloads
	TLSSkipVerify bool   `yaml:"tls_skip_verify"` // disable TLS certificate verification (for self-signed certs in dev)
}

type OAuthConfig struct {
	ClientID     string `yaml:"client_id"`
	RedirectPort int    `yaml:"redirect_port"`
}

type MeetConfig struct {
	Enabled             bool        `yaml:"enabled"`
	OAuth               OAuthConfig `yaml:"oauth"`
	PollIntervalSeconds int         `yaml:"poll_interval_seconds"`
}

type ZoomConfig struct {
	Enabled            bool        `yaml:"enabled"`
	OAuth              OAuthConfig `yaml:"oauth"`
	WebhookPort        int         `yaml:"webhook_port"`
	WebhookHost        string      `yaml:"webhook_host"`
	WebhookSecretToken string      `yaml:"webhook_secret_token"` // Zoom webhook verification token
}

type MessengersConfig struct {
	Telegram TelegramConfig `yaml:"telegram"`
}

type TelegramConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BotToken string `yaml:"bot_token"`
}

type ChallengesConfig struct {
	Defaults    ChallengeDefaults `yaml:"defaults"`
	Poll        PollConfig        `yaml:"poll"`
	FileBased   FileBasedConfig   `yaml:"filebased"`
	AIGenerated AIGeneratedConfig `yaml:"aigenerated"`
}

type ChallengeDefaults struct {
	AnswerWindowSeconds         int `yaml:"answer_window_seconds"`
	MinGapBetweenChallengesSecs int `yaml:"min_gap_between_challenges_seconds"`
}

type PollConfig struct {
	MaxDeliverySkewMS int `yaml:"max_delivery_skew_ms"`
}

type FileBasedConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BanksDir string `yaml:"banks_dir"`
}

type AIGeneratedConfig struct {
	Enabled bool `yaml:"enabled"`
	// TODO: AI-generated challenges not implemented yet.
}

type EventStoreConfig struct {
	Compression  string `yaml:"compression"`
	RowGroupSize int    `yaml:"row_group_size"`
}

type GUIConfig struct {
	BindAddr           string `yaml:"bind_addr"`
	Port               int    `yaml:"port"`
	OpenBrowserOnStart bool   `yaml:"open_browser_on_start"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	File   string `yaml:"file"`
}

// defaults fills zero-value fields with sensible values matching CONFIG.md.
func (c *Config) defaults() {
	if c.SchemaVersion == 0 {
		c.SchemaVersion = 1
	}
	if c.DataDir == "" {
		c.DataDir = paths.DataDir()
	}
	if c.MeetingsDir == "" {
		c.MeetingsDir = filepath.Join(paths.DataDir(), "meetings")
	}
	if c.QuestionsDir == "" {
		c.QuestionsDir = filepath.Join(paths.DataDir(), "questions")
	}
	if c.ReportsDir == "" {
		c.ReportsDir = filepath.Join(paths.DataDir(), "reports")
	}
	if c.CacheDir == "" {
		c.CacheDir = paths.CacheDir()
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 180
	}
	if c.Challenges.Defaults.AnswerWindowSeconds == 0 {
		c.Challenges.Defaults.AnswerWindowSeconds = 30
	}
	if c.Challenges.Defaults.MinGapBetweenChallengesSecs == 0 {
		c.Challenges.Defaults.MinGapBetweenChallengesSecs = 60
	}
	if c.Challenges.Poll.MaxDeliverySkewMS == 0 {
		c.Challenges.Poll.MaxDeliverySkewMS = 2000
	}
	if c.EventStore.Compression == "" {
		c.EventStore.Compression = "zstd"
	}
	if c.EventStore.RowGroupSize == 0 {
		c.EventStore.RowGroupSize = 10000
	}
	if c.GUI.BindAddr == "" {
		c.GUI.BindAddr = "127.0.0.1"
	}
	if c.GUI.Port == 0 {
		c.GUI.Port = 8080
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
	if c.Providers.BBB.WebhookPort == 0 {
		c.Providers.BBB.WebhookPort = 9124
	}
	if c.Providers.Zoom.WebhookPort == 0 {
		c.Providers.Zoom.WebhookPort = 9123
	}
	if c.Providers.Zoom.OAuth.RedirectPort == 0 {
		c.Providers.Zoom.OAuth.RedirectPort = 9125
	}
	if c.Providers.Meet.OAuth.RedirectPort == 0 {
		c.Providers.Meet.OAuth.RedirectPort = 9126
	}
	if c.Providers.Meet.PollIntervalSeconds == 0 {
		c.Providers.Meet.PollIntervalSeconds = 10
	}
}
