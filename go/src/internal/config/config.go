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

type Values struct {
	MeetingsDir       string           `json:"meetings_dir,omitempty"`
	MeetingsDirFormat string           `json:"meetings_dir_format,omitempty"`
	RetentionDays     int              `json:"retention_days,omitempty"`
	Providers         ProvidersConfig  `json:"providers,omitzero"`
	Messengers        MessengersConfig `json:"messengers,omitzero"`
	Challenges        ChallengesConfig `json:"challenges,omitzero"`
	GUI               GUIConfig        `json:"gui,omitzero"`
	Logging           LoggingConfig    `json:"logging,omitzero"`
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
	Enabled             bool            `json:"enabled,omitempty"`
	AutoSubmit          bool            `json:"auto_submit,omitempty"`
	PollIntervalSeconds int             `json:"poll_interval_seconds,omitempty"`
	MinWordsPerQuestion int             `json:"min_words_per_question,omitempty"`
	MaxQuestionsPerPoll int             `json:"max_questions_per_poll,omitempty"`
	ReviewDir           string          `json:"review_dir,omitempty"`
	BankBasename        string          `json:"bank_basename,omitempty"`
	Language            string          `json:"language,omitempty"`
	ASR                 AIBackendConfig `json:"asr,omitzero"`
	LLM                 AIBackendConfig `json:"llm,omitzero"`
	ExtraRules          []string        `json:"extra_rules,omitempty"`
}

type AIBackendConfig struct {
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
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

func defaults() Values {
	return Values{
		MeetingsDir:       expandPath("~/Documents/ptrack/meetings"),
		MeetingsDirFormat: "{start:%y%m%d-%H%M}_{end:%y%m%d-%H%M}",
		RetentionDays:     180,
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
				ReviewDir:           expandPath("~/Documents/ptrack"),
				BankBasename:        "generated",
				Language:            "auto",
				ASR:                 AIBackendConfig{BaseURL: "http://127.0.0.1:8080/v1", Model: "whisper-large-turbo-q5_O"},
				LLM:                 AIBackendConfig{BaseURL: "http://127.0.0.1:8080/v1", Model: "qwen3-4b"},
				ExtraRules: []string{
					"Questions must be answerable purely from the transcript. Do not invent facts. Avoid trivia unrelated to the transcript topic.",
					"Prefer questions tied to what was specifically said in this transcript (e.g. \"Which of these terms did the teacher mention?\") over general subject-knowledge questions answerable without this transcript.",
				},
			},
		},
		GUI:     GUIConfig{BindAddr: "127.0.0.1", Port: 8080},
		Logging: LoggingConfig{Level: "info", Format: "text"},
	}
}

// Config holds the resolved config. Reads via Get are lock-free (atomic
// snapshot); writes (Apply, Reload) take mu so saves don't interleave.
type Config struct {
	mu        sync.Mutex
	path      string
	defaults  Values
	schemaRef json.RawMessage
	current   atomic.Pointer[Values]
}

// Get returns the current snapshot; callers read it per use so a Reload or
// Apply takes effect on the next natural boundary without a restart.
func (c *Config) Get() Values { return *c.current.Load() }

func (c *Config) Path() string { return c.path }

func (c *Config) Reload() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, err := c.readFromFile()
	if err != nil {
		return err
	}
	return c.commit(v)
}

func (c *Config) Apply(mutator func(*Values)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.Get()
	mutator(&v)
	return c.commit(v)
}

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

// commit prunes default-equal fields, validates the remaining overrides
// against the schema, writes the file canonically, then swaps the snapshot.
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

func normalisePaths(v *Values) {
	v.MeetingsDir = expandPath(v.MeetingsDir)
	v.Challenges.AutoGeneration.ReviewDir = expandPath(v.Challenges.AutoGeneration.ReviewDir)
}

// applyConstraints adds min/max/minLength bounds the reflected schema can't
// infer from the struct.
func applyConstraints(root *jsonschema.Schema) {
	at(root, "meetings_dir").MinLength = new(1)
	at(root, "meetings_dir_format").MinLength = new(1)
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
	at(root, "challenges", "auto_generation", "bank_basename").MinLength = new(1)
	at(root, "challenges", "auto_generation", "bank_basename").Pattern = `^[^/\\\s][^/\\]*$`
	at(root, "challenges", "auto_generation", "language").Pattern = `^(auto|[a-zA-Z]{2,3}(-[a-zA-Z]{2,4})?)$`
	at(root, "challenges", "auto_generation", "asr", "api_key").WriteOnly = true
	at(root, "challenges", "auto_generation", "llm", "api_key").WriteOnly = true

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
