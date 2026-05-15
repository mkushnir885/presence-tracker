# Configuration

Single YAML config file, validated against a JSON Schema. The schema is
the source of truth for both runtime validation (in `go/src/internal/config/`)
and the web-based config editor's form layout (in `go/src/internal/gui/`).

## File locations

Config file resolved in this order (first match wins):

1. `--config <path>` command-line flag.
2. **Linux:** `$XDG_CONFIG_HOME/ptrack/config.yaml`
   (default `~/.config/ptrack/config.yaml`)
3. **Windows:** `%APPDATA%\ptrack\config.yaml`
4. `./config.yaml` in the current directory.

Secrets (OAuth tokens, bot tokens, API keys) live in `secrets.yaml` in
the same directory as `config.yaml`. On Linux: `chmod 0600`. On Windows:
the file is encrypted at rest with DPAPI by the Go binary on first write.

The main config references secrets by name, not value:

```yaml
# config.yaml
messengers:
  telegram:
    bot_token: ${secrets.telegram_bot_token}
```

```yaml
# secrets.yaml  (protected; never shown in the GUI editor)
telegram_bot_token: "123456:ABC-DEF..."
zoom_oauth_client_id: "..."
meet_oauth_client_id: "..."
bbb_shared_secret: "..."
```

Note: Zoom uses **PKCE** with no `client_secret`. Google Meet uses PKCE
too, but Google's token endpoint requires `client_secret` even for
Desktop-app clients — include it alongside `client_id`.

## Full schema (v1)

```yaml
schema_version: 1

# --- Data directories --------------------------------------------------------
# All paths support ~ expansion and environment variables.
# Defaults follow platform conventions (see @docs/ARCHITECTURE.md#storage-layout).
meetings_dir: "~/Documents/ptrack/meetings"
questions_dir: "~/Documents/ptrack/questions"  # per-meeting .jsonl question files
reports_dir: "~/Documents/ptrack/reports"
data_dir: ""           # internal data (participant registry); platform default
cache_dir: ""          # model weights cache; platform default
retention_days: 180    # purge meeting Parquet + question files older than this

# --- Video-conferencing providers --------------------------------------------
providers:
  zoom:
    enabled: false
    # ptrack polls the Zoom Dashboard API on a timer. No public address required, but:
    #   • Requires Zoom Pro plan or higher (Dashboard API is not available on free accounts).
    #   • OAuth authorisation must be performed by an account admin
    #     (the dashboard_meetings:read:admin scope requires admin consent).
    # PKCE flow: only client_id required; no client_secret.
    # On first use, ptrack opens a browser window for the OAuth consent screen.
    # The access + refresh tokens are stored in secrets.yaml after consent.
    oauth:
      client_id: ${secrets.zoom_oauth_client_id}
      redirect_port: 9125      # localhost redirect URI: http://127.0.0.1:9125/callback
    poll_interval_seconds: 10

  meet:
    enabled: false
    # Google Meet uses polling only — no public address required.
    # PKCE flow, but Google's token endpoint also requires client_secret even
    # for Desktop-app clients. Find it in Cloud Console → Credentials → your app.
    oauth:
      client_id: ${secrets.meet_oauth_client_id}
      client_secret: ${secrets.meet_oauth_client_secret}
      redirect_port: 9126
    poll_interval_seconds: 10  # how often to query the Meet API for participant changes

  bbb:
    enabled: true
    base_url: "https://bbb.example.edu/bigbluebutton/"
    shared_secret: ${secrets.bbb_shared_secret}
    # ptrack polls getMeetingInfo on a timer. No reachability requirement;
    # works with every BBB installation, at no extra cost.
    poll_interval_seconds: 10

# --- Messengers --------------------------------------------------------------
messengers:
  telegram:
    enabled: true
    bot_token: ${secrets.telegram_bot_token}
    # Pairing code TTL. Codes expire if the student never types them in a meeting.
    pairing_code_ttl_hours: 1

# --- Challenges --------------------------------------------------------------
challenges:
  # One pipeline, many producers. See @docs/CHALLENGES.md.
  # A session with no polls at all is valid (tracking-only mode).

  # Default poll behaviour, applied to every ptrack poll invocation
  # regardless of producer.
  defaults:
    answer_window_seconds: 30
    min_gap_between_challenges_seconds: 60

  poll:
    max_delivery_skew_ms: 2000

  # Teacher-prepared YAML banks live here. ptrack reads but never writes
  # to this directory.
  banks_dir: "~/Documents/ptrack/question-banks"

  # Optional autonomous YAML producer. When enabled, ptrack_py runs as a
  # long-lived child process of the Go daemon for the duration of every
  # session and writes generated banks to pending_dir.
  auto_generation:
    enabled: false

    # If true, the challenger invokes `ptrack poll --type=aigenerated`
    # immediately after writing a YAML. If false, the YAML is left in
    # pending_dir and surfaced in the GUI's Trigger poll menu so the
    # teacher can review/edit and submit it manually (as --type=combined).
    auto_submit: true

    # Where generated YAML banks land. Only the most recent file is kept.
    # Files are removed on submission or when superseded by a new one.
    # Platform default is /tmp/ptrack on Linux, %TEMP%\ptrack on Windows.
    pending_dir: ""

    # If true (default), block session start until ASR + LLM are loaded.
    # If false, models load lazily on first generation — faster session
    # start, slower first poll.
    preload_models: true

    # Auto-release ASR + LLM after this many seconds of idle inside the
    # challenger. 0 disables auto-unload; the teacher releases models
    # explicitly via the GUI's "Free models" button. Default: 0.
    idle_unload_after_seconds: 0

    poll_interval_seconds: 1200
    questions_per_poll: 15
    early_poll_on_context_ready: true
    question_language: "uk"        # ISO code for generated question text
    context:
      audio_transcript:
        enabled: true
        window_minutes: 20
      screen_ocr:
        enabled: false
        sample_fps: 1
    asr:
      backend: "faster-whisper"    # faster-whisper | whisper-api
      model: "small"               # tiny | base | small | medium | large-v3
      quantization: "int8"
      language: "uk"               # ISO code or "auto"
      cpu_threads: 4
    generator:
      backend: "llama-cpp"         # llama-cpp | openai | gemini
      model_path: ""               # resolved relative to cache_dir if relative
      context_tokens: 4096
      # For hosted backends:
      # api_key: ${secrets.openai_api_key}
      # model_name: "gpt-4o-mini"

# --- Event store -------------------------------------------------------------
eventstore:
  compression: "zstd"              # zstd | snappy | none
  row_group_size: 10000

# --- GUI ---------------------------------------------------------------------
gui:
  bind_addr: "127.0.0.1"
  port: 8080
  open_browser_on_start: true

# --- Logging -----------------------------------------------------------------
logging:
  level: "info"                    # debug | info | warn | error
  format: "text"                   # text | json
  file: ""                         # empty = stderr only
```

## OAuth / PKCE flow (Meet and Zoom)

Since this tool is for personal use on the teacher's own machine, the
recommended OAuth approach is **Authorization Code + PKCE**:

1. On first `ptrack track` (or from the config editor "Authorize" button),
   the Go binary generates a PKCE code verifier + challenge, then opens
   the platform's OAuth consent URL in the system browser.
2. After the teacher approves, the platform redirects to
   `http://127.0.0.1:<redirect_port>/callback`. The Go binary's temporary
   HTTP listener captures the authorization code.
3. Go exchanges the code + verifier for access and refresh tokens.
4. Tokens are stored in `secrets.yaml` (protected file).
5. Subsequent runs use the stored refresh token; re-authorization is only
   needed if the token is revoked.

This requires registering a "Desktop" or "Native" OAuth app in each
platform's developer console and setting the redirect URI to
`http://127.0.0.1:<redirect_port>/callback`. No client secret is involved.

## Config editor in the GUI

The `/config` route renders a form generated from the JSON Schema.
Edits are validated in-browser and server-side on save. On save, config
is written atomically (temp file + rename) and applicable sections
reload without restarting the process.

Secret fields are never shown. The editor shows "••• (set)" or "not set".
A separate "Edit secrets" flow writes `secrets.yaml` directly.

## Live reload

| Section                                                       | Reload behavior                                  |
|---------------------------------------------------------------|--------------------------------------------------|
| `challenges.defaults`                                         | Next poll uses new values                        |
| `challenges.poll`                                             | Next poll uses new delivery skew value           |
| `challenges.banks_dir`                                        | Next file picker refresh                         |
| `challenges.auto_generation.poll_interval_seconds`            | Immediately via API; next poll                   |
| `challenges.auto_generation.questions_per_poll`               | Immediately via API; next poll                   |
| `challenges.auto_generation.auto_submit`                      | Immediately via API; next generation             |
| `challenges.auto_generation` (other keys)                     | Requires `challenger` restart; done at next meeting |
| `providers.*`                                                 | Next meeting only                                |
| `messengers.*`                                                | Next meeting only                                |
| `gui.*`                                                       | Requires full restart                            |
| `logging.*`                                                   | Takes effect immediately                         |

## Validation

The schema enforces:

- Every enabled provider has credentials referenced from secrets.
- Exactly one `messengers.*` is enabled (in v1).
- Paths exist (`challenges.banks_dir`, ASR/LLM model paths where applicable).
- Port numbers are in valid range and don't collide between `gui.port`,
  `providers.*.oauth.redirect_port`, and the challenger's auto-assigned port.

Note: **zero challenge types enabled is explicitly allowed.**

Validation errors include a JSON Pointer into the config so the GUI can
highlight the exact offending field.
