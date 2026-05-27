package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/google/jsonschema-go/jsonschema"
)

// Values is the full resolved configuration tree handed to runtime code.
// Every field is always populated: either from the user's overrides file
// or from defaults().
type Values struct {
	MeetingsDir   string           `json:"meetings_dir,omitempty"`
	QuestionsDir  string           `json:"questions_dir,omitempty"`
	ReportsDir    string           `json:"reports_dir,omitempty"`
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
	TLSSkipVerify       bool   `json:"tls_skip_verify,omitempty"`
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty"`
}

type OAuthConfig struct {
	ClientID     string `json:"client_id,omitempty"`
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
	Defaults       ChallengeDefaults    `json:"defaults,omitzero"`
	Poll           PollConfig           `json:"poll,omitzero"`
	AutoGeneration AutoGenerationConfig `json:"auto_generation,omitzero"`
}

type ChallengeDefaults struct {
	AnswerWindowSeconds         int `json:"answer_window_seconds,omitempty"`
	MinGapBetweenChallengesSecs int `json:"min_gap_between_challenges_seconds,omitempty"`
}

type PollConfig struct {
	MaxDeliverySkewMS int `json:"max_delivery_skew_ms,omitempty"`
}

type AutoGenerationConfig struct {
	Enabled             bool   `json:"enabled,omitempty"`
	AutoSubmit          bool   `json:"auto_submit,omitempty"`
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty"`
	MinWordsPerQuestion int    `json:"min_words_per_question,omitempty"`
	MaxQuestionsPerPoll int    `json:"max_questions_per_poll,omitempty"`
	ReviewDir           string `json:"review_dir,omitempty"`
	// Language is the spoken lesson language as a BCP-47 / ISO 639-1
	// short tag (e.g. "en", "uk"). Drives both ASR accuracy (Whisper's
	// language hint) and the LLM prompt's output-language instruction.
	// The sentinel "auto" disables both hints and lets the ASR backend
	// detect the language while the LLM matches the transcript.
	Language string          `json:"language,omitempty"`
	ASR      AIBackendConfig `json:"asr,omitzero"`
	LLM      AIBackendConfig `json:"llm,omitzero"`
}

// AIBackendConfig is the connection target for one OpenAI-compatible
// HTTP backend (used for ASR and the question-generation LLM). ptrack
// holds no model weights — the backend (Ollama, OpenAI, any compatible
// gateway) does.
type AIBackendConfig struct {
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
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

// defaults returns the built-in default Values. Treated as immutable —
// callers must not mutate the returned struct.
func defaults() Values {
	return Values{
		MeetingsDir:   expandPath("~/Documents/ptrack/meetings"),
		QuestionsDir:  expandPath("~/Documents/ptrack/questions"),
		ReportsDir:    expandPath("~/Documents/ptrack/reports"),
		RetentionDays: 180,
		Providers: ProvidersConfig{
			BBB:  BBBConfig{PollIntervalSeconds: 10},
			Meet: MeetConfig{OAuth: OAuthConfig{RedirectPort: 9126}, PollIntervalSeconds: 10},
			Zoom: ZoomConfig{OAuth: OAuthConfig{RedirectPort: 9125}, PollIntervalSeconds: 10},
		},
		Challenges: ChallengesConfig{
			Defaults: ChallengeDefaults{AnswerWindowSeconds: 30, MinGapBetweenChallengesSecs: 60},
			Poll:     PollConfig{MaxDeliverySkewMS: 2000},
			AutoGeneration: AutoGenerationConfig{
				PollIntervalSeconds: 300,
				MinWordsPerQuestion: 30,
				MaxQuestionsPerPoll: 5,
				ReviewDir:           expandPath("~/Documents/ptrack/pending-banks"),
				Language:            "auto",
				ASR:                 AIBackendConfig{BaseURL: "http://127.0.0.1:11434", Model: "whisper"},
				LLM:                 AIBackendConfig{BaseURL: "http://127.0.0.1:11434", Model: "qwen2.5:3b"},
			},
		},
		EventStore: EventStoreConfig{Compression: "zstd", RowGroupSize: 10000},
		GUI:        GUIConfig{BindAddr: "127.0.0.1", Port: 8080},
		Logging:    LoggingConfig{Level: "info", Format: "text"},
	}
}

// Defaults returns a copy of the built-in defaults. Public for schemagen
// and other tools that need the canonical default tree.
func Defaults() Values { return defaults() }

// Config holds the resolved Values plus the on-disk source path. Reads
// via Get are lock-free (atomic.Pointer); writes (Apply, Reload) take a
// mutex so concurrent saves do not interleave file I/O.
type Config struct {
	mu        sync.Mutex
	path      string
	defaults  Values
	schemaRef json.RawMessage
	current   atomic.Pointer[Values]
}

// Get returns a snapshot of the current resolved Values. Callers that
// want live-reload behaviour should call Get afresh at each use (per
// poll tick, per meeting start, etc.).
func (c *Config) Get() Values { return *c.current.Load() }

// Path returns the on-disk file path Config reads from and writes to.
// The path is set even when the file does not yet exist on disk.
func (c *Config) Path() string { return c.path }

// Reload re-reads the override file (file is authoritative), validates,
// prunes default-equal fields, rewrites the canonical file, and atomically
// replaces the in-memory snapshot.
func (c *Config) Reload() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, err := c.readFromFile()
	if err != nil {
		return err
	}
	return c.commit(v)
}

// Apply mutates a snapshot of the current Values through mutator, then
// runs the same commit pipeline as Reload (validate → prune → save →
// replace current).
func (c *Config) Apply(mutator func(*Values)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.Get()
	mutator(&v)
	return c.commit(v)
}

// readFromFile reads c.path and overlays it onto defaults. An absent
// file yields the defaults unchanged. A top-level "$schema" key, if
// present, is captured into c.schemaRef so it round-trips on the next
// write; the rest of the file is decoded strictly (unknown fields are
// rejected).
func (c *Config) readFromFile() (Values, error) {
	v := c.defaults
	c.schemaRef = nil
	data, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return v, nil
		}
		return v, fmt.Errorf("config: read %s: %w", c.path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return v, fmt.Errorf("config: parse %s: %w", c.path, err)
	}
	if s, ok := raw["$schema"]; ok {
		c.schemaRef = s
		delete(raw, "$schema")
	}
	rest, err := json.Marshal(raw)
	if err != nil {
		return v, fmt.Errorf("config: re-marshal %s: %w", c.path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(rest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("config: parse %s: %w", c.path, err)
	}
	return v, nil
}

// commit is the shared end of every entry point: validate the minimal
// overrides derived from v, write canonical JSON to disk, and store v
// as the new current snapshot.
func (c *Config) commit(v Values) error {
	normalisePaths(&v)
	overrides := diffToMap(v, c.defaults)
	if err := validateOverrides(overrides); err != nil {
		return err
	}
	if len(c.schemaRef) > 0 {
		overrides["$schema"] = c.schemaRef
	}
	if err := writeConfigFile(c.path, overrides); err != nil {
		return err
	}
	c.current.Store(&v)
	return nil
}

// validateOverrides validates the minimal-overrides map against the
// embedded JSON Schema. Defaults are trusted; only user-introduced
// values are checked.
func validateOverrides(overrides map[string]any) error {
	schema, err := ResolvedSchema()
	if err != nil {
		return err
	}
	if err := schema.Validate(overrides); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// diffToMap returns a nested map[string]any containing only fields where
// v differs from base. Unexported fields and fields without a json tag
// are ignored. Recurses into nested structs; non-struct leaves compare
// by reflect.DeepEqual.
func diffToMap(v, base any) map[string]any {
	out := map[string]any{}
	vv := reflect.Indirect(reflect.ValueOf(v))
	bv := reflect.Indirect(reflect.ValueOf(base))
	t := vv.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name := jsonName(field)
		if name == "" || name == "-" {
			continue
		}
		vf := vv.Field(i)
		bf := bv.Field(i)
		if vf.Kind() == reflect.Struct {
			sub := diffToMap(vf.Interface(), bf.Interface())
			if len(sub) > 0 {
				out[name] = sub
			}
			continue
		}
		if !reflect.DeepEqual(vf.Interface(), bf.Interface()) {
			out[name] = vf.Interface()
		}
	}
	return out
}

// normalisePaths resolves "~" and forward-slash separators in every
// user-settable path field so the resolved Values always carries
// OS-native absolute paths, regardless of how the JSON was authored.
func normalisePaths(v *Values) {
	v.MeetingsDir = expandPath(v.MeetingsDir)
	v.QuestionsDir = expandPath(v.QuestionsDir)
	v.ReportsDir = expandPath(v.ReportsDir)
	v.Challenges.AutoGeneration.ReviewDir = expandPath(v.Challenges.AutoGeneration.ReviewDir)
}

// applyConstraints declares value-range, length, and enum restrictions
// for the generated schema. at(...) panics on a missing path, so a Go
// field rename without a matching update here fails schema construction
// loudly instead of silently dropping the constraint.
func applyConstraints(root *jsonschema.Schema) {
	at(root, "meetings_dir").MinLength = new(1)
	at(root, "questions_dir").MinLength = new(1)
	at(root, "reports_dir").MinLength = new(1)
	at(root, "retention_days").Minimum = new(0.0)

	at(root, "providers", "bbb", "poll_interval_seconds").Minimum = new(1.0)
	at(root, "providers", "bbb", "shared_secret").WriteOnly = true

	portRange(at(root, "providers", "meet", "oauth", "redirect_port"))
	at(root, "providers", "meet", "oauth", "client_secret").WriteOnly = true
	at(root, "providers", "meet", "poll_interval_seconds").Minimum = new(1.0)

	portRange(at(root, "providers", "zoom", "oauth", "redirect_port"))
	at(root, "providers", "zoom", "oauth", "client_secret").WriteOnly = true
	at(root, "providers", "zoom", "poll_interval_seconds").Minimum = new(1.0)

	at(root, "messengers", "telegram", "bot_token").WriteOnly = true

	at(root, "challenges", "defaults", "answer_window_seconds").Minimum = new(1.0)
	at(root, "challenges", "defaults", "min_gap_between_challenges_seconds").Minimum = new(0.0)
	at(root, "challenges", "poll", "max_delivery_skew_ms").Minimum = new(0.0)

	at(root, "challenges", "auto_generation", "poll_interval_seconds").Minimum = new(30.0)
	at(root, "challenges", "auto_generation", "min_words_per_question").Minimum = new(5.0)
	maxq := at(root, "challenges", "auto_generation", "max_questions_per_poll")
	maxq.Minimum = new(1.0)
	maxq.Maximum = new(20.0)
	at(root, "challenges", "auto_generation", "review_dir").MinLength = new(1)
	at(root, "challenges", "auto_generation", "language").Pattern = `^(auto|[a-zA-Z]{2,3}(-[a-zA-Z]{2,4})?)$`
	at(root, "challenges", "auto_generation", "asr", "base_url").MinLength = new(1)
	at(root, "challenges", "auto_generation", "asr", "api_key").WriteOnly = true
	at(root, "challenges", "auto_generation", "asr", "model").MinLength = new(1)
	at(root, "challenges", "auto_generation", "llm", "base_url").MinLength = new(1)
	at(root, "challenges", "auto_generation", "llm", "api_key").WriteOnly = true
	at(root, "challenges", "auto_generation", "llm", "model").MinLength = new(1)

	at(root, "eventstore", "compression").Enum = []any{"zstd", "snappy", "none"}
	at(root, "eventstore", "row_group_size").Minimum = new(1.0)

	at(root, "gui", "bind_addr").MinLength = new(1)
	portRange(at(root, "gui", "port"))

	at(root, "logging", "level").Enum = []any{"debug", "info", "warn", "error"}
	at(root, "logging", "format").Enum = []any{"text", "json"}
}

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

func portRange(s *jsonschema.Schema) {
	s.Minimum = new(1.0)
	s.Maximum = new(65535.0)
}
