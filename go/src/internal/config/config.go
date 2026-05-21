package config

import (
	"fmt"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"

	"presence-tracker/src/internal/paths"
)

// Config is the top-level runtime configuration loaded from config.yaml.
type Config struct {
	SchemaVersion int              `json:"schema_version,omitempty"`
	MeetingsDir   string           `json:"meetings_dir,omitempty"`
	QuestionsDir  string           `json:"questions_dir,omitempty"`
	ReportsDir    string           `json:"reports_dir,omitempty"`
	DataDir       string           `json:"data_dir,omitempty"`
	CacheDir      string           `json:"cache_dir,omitempty"`
	RetentionDays int              `json:"retention_days,omitempty"`
	Providers     ProvidersConfig  `json:"providers,omitzero"`
	Messengers    MessengersConfig `json:"messengers,omitzero"`
	Challenges    ChallengesConfig `json:"challenges,omitzero"`
	EventStore    EventStoreConfig `json:"eventstore,omitzero"`
	GUI           GUIConfig        `json:"gui,omitzero"`
	Logging       LoggingConfig    `json:"logging,omitzero"`
}

type ProvidersConfig struct {
	BBB  BBBConfig  `json:"bbb,omitzero"`
	Meet MeetConfig `json:"meet,omitzero"`
	Zoom ZoomConfig `json:"zoom,omitzero"`
}

type BBBConfig struct {
	Enabled             bool   `json:"enabled,omitempty"`
	BaseURL             string `json:"base_url,omitempty"`
	SharedSecret        string `json:"shared_secret,omitempty"`
	TLSSkipVerify       bool   `json:"tls_skip_verify,omitempty"` // disable TLS certificate verification (for self-signed certs in dev)
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty"`
}

type OAuthConfig struct {
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret is required by Google's token endpoint even for Desktop-app
	// PKCE clients. Zoom does not require it and ignores it if present.
	ClientSecret string `json:"client_secret,omitempty"`
	RedirectPort int    `json:"redirect_port,omitempty"`
}

type MeetConfig struct {
	Enabled             bool        `json:"enabled,omitempty"`
	OAuth               OAuthConfig `json:"oauth,omitzero"`
	PollIntervalSeconds int         `json:"poll_interval_seconds,omitempty"`
}

type ZoomConfig struct {
	Enabled             bool        `json:"enabled,omitempty"`
	OAuth               OAuthConfig `json:"oauth,omitzero"`
	PollIntervalSeconds int         `json:"poll_interval_seconds,omitempty"`
}

type MessengersConfig struct {
	Telegram TelegramConfig `json:"telegram,omitzero"`
}

type TelegramConfig struct {
	Enabled  bool   `json:"enabled,omitempty"`
	BotToken string `json:"bot_token,omitempty"`
}

type ChallengesConfig struct {
	Defaults ChallengeDefaults `json:"defaults,omitzero"`
	Poll     PollConfig        `json:"poll,omitzero"`
}

type ChallengeDefaults struct {
	AnswerWindowSeconds         int `json:"answer_window_seconds,omitempty"`
	MinGapBetweenChallengesSecs int `json:"min_gap_between_challenges_seconds,omitempty"`
}

type PollConfig struct {
	MaxDeliverySkewMS int `json:"max_delivery_skew_ms,omitempty"`
}

type EventStoreConfig struct {
	Compression  string `json:"compression,omitempty"`
	RowGroupSize int    `json:"row_group_size,omitempty"`
}

type GUIConfig struct {
	BindAddr           string `json:"bind_addr,omitempty"`
	Port               int    `json:"port,omitempty"`
	OpenBrowserOnStart bool   `json:"open_browser_on_start,omitempty"`
}

type LoggingConfig struct {
	Level  string `json:"level,omitempty"`
	Format string `json:"format,omitempty"`
	File   string `json:"file,omitempty"`
}

// applyConstraints sets value-range, length, and enum restrictions on the
// generated config schema. Constraints live here next to the struct
// definitions; the schemagen tool and the runtime validator both build
// their schema via Schema() and so see the same rules.
//
// at(...) panics on a missing path, so a Go field rename without a matching
// update here fails schema construction loudly instead of silently
// dropping the constraint.
func applyConstraints(root *jsonschema.Schema) {
	at(root, "schema_version").Minimum = new(1.0)
	at(root, "schema_version").Maximum = new(1.0)
	at(root, "meetings_dir").MinLength = new(1)
	at(root, "questions_dir").MinLength = new(1)
	at(root, "reports_dir").MinLength = new(1)
	at(root, "data_dir").MinLength = new(1)
	at(root, "cache_dir").MinLength = new(1)
	at(root, "retention_days").Minimum = new(0.0)

	at(root, "providers", "bbb", "poll_interval_seconds").Minimum = new(1.0)

	port(at(root, "providers", "meet", "oauth", "redirect_port"))
	at(root, "providers", "meet", "poll_interval_seconds").Minimum = new(1.0)

	port(at(root, "providers", "zoom", "oauth", "redirect_port"))
	at(root, "providers", "zoom", "poll_interval_seconds").Minimum = new(1.0)

	at(root, "challenges", "defaults", "answer_window_seconds").Minimum = new(1.0)
	at(root, "challenges", "defaults", "min_gap_between_challenges_seconds").Minimum = new(0.0)
	at(root, "challenges", "poll", "max_delivery_skew_ms").Minimum = new(0.0)

	at(root, "eventstore", "compression").Enum = []any{"zstd", "snappy", "none"}
	at(root, "eventstore", "row_group_size").Minimum = new(1.0)

	at(root, "gui", "bind_addr").MinLength = new(1)
	port(at(root, "gui", "port"))

	at(root, "logging", "level").Enum = []any{"debug", "info", "warn", "error"}
	at(root, "logging", "format").Enum = []any{"text", "json"}
}

// at descends into a schema by json-tag path and panics if any segment is
// missing. The panic is the design: if a config field was renamed without
// updating applyConstraints, schema construction should fail at
// startup/schemagen time rather than silently emit an unconstrained
// schema.
func at(s *jsonschema.Schema, path ...string) *jsonschema.Schema {
	cur := s
	for i, p := range path {
		next, ok := cur.Properties[p]
		if !ok || next == nil {
			panic(fmt.Sprintf("config: no schema property at path %v", path[:i+1]))
		}
		cur = next
	}
	return cur
}

// port applies the standard TCP port range to an integer schema property.
func port(s *jsonschema.Schema) {
	s.Minimum = new(1.0)
	s.Maximum = new(65535.0)
}

// Defaults returns a Config populated with the v1 default values. Load uses
// it as the starting point before unmarshalling user-supplied YAML on top —
// any field absent from the file keeps its default; any field present
// (including explicit zero) wins. The same defaulted value drives the JSON
// Schema's `default:` annotations via tools/schemagen.
func Defaults() Config {
	return Config{
		SchemaVersion: 1,
		MeetingsDir:   filepath.Join(paths.DataDir(), "meetings"),
		QuestionsDir:  filepath.Join(paths.DataDir(), "questions"),
		ReportsDir:    filepath.Join(paths.DataDir(), "reports"),
		DataDir:       paths.DataDir(),
		CacheDir:      paths.CacheDir(),
		RetentionDays: 180,
		Providers: ProvidersConfig{
			BBB:  BBBConfig{PollIntervalSeconds: 10},
			Meet: MeetConfig{OAuth: OAuthConfig{RedirectPort: 9126}, PollIntervalSeconds: 10},
			Zoom: ZoomConfig{OAuth: OAuthConfig{RedirectPort: 9125}, PollIntervalSeconds: 10},
		},
		Challenges: ChallengesConfig{
			Defaults: ChallengeDefaults{
				AnswerWindowSeconds:         30,
				MinGapBetweenChallengesSecs: 60,
			},
			Poll: PollConfig{MaxDeliverySkewMS: 2000},
		},
		EventStore: EventStoreConfig{
			Compression:  "zstd",
			RowGroupSize: 10000,
		},
		GUI: GUIConfig{
			BindAddr: "127.0.0.1",
			Port:     8080,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
	}
}
