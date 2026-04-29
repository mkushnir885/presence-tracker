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

Note: Google Meet and Zoom use **PKCE** (no `client_secret`). Only
`client_id` and a localhost redirect URI are needed.

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
    # PKCE flow: only client_id required; no client_secret.
    # On first use, ptrack opens a browser window for the OAuth consent screen.
    # The access + refresh tokens are stored in secrets.yaml after consent.
    oauth:
      client_id: ${secrets.zoom_oauth_client_id}
      redirect_port: 9125      # localhost redirect URI: http://127.0.0.1:9125/callback
    webhook_port: 9123

  meet:
    enabled: false
    # PKCE flow: same as Zoom above.
    oauth:
      client_id: ${secrets.meet_oauth_client_id}
      redirect_port: 9126
    # Google Meet webhook / polling interval config (if applicable)

  bbb:
    enabled: true
    base_url: "https://bbb.example.edu/bigbluebutton/"
    shared_secret: ${secrets.bbb_shared_secret}
    webhook_port: 9124

# --- Messengers --------------------------------------------------------------
messengers:
  telegram:
    enabled: true
    bot_token: ${secrets.telegram_bot_token}
    # Pairing code TTL. Codes expire if the student never types them in a meeting.
    pairing_code_ttl_hours: 1

# --- Challenges --------------------------------------------------------------
challenges:
  # Zero challenge types enabled is valid (tracking-only mode).
  defaults:
    answer_window_seconds: 30
    min_gap_between_challenges_seconds: 60

  poll:
    max_delivery_skew_ms: 2000

  filebased:
    enabled: true
    # Path to the teacher's own question-bank YAML files.
    # The system reads from this directory but never writes to it.
    # The teacher is responsible for managing their bank files here.
    banks_dir: "~/Documents/ptrack/question-banks"

  aigenerated:
    enabled: false
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
| `challenges.defaults`                                         | Next challenge uses new values                   |
| `challenges.poll`                                             | Next poll uses new delivery skew value           |
| `challenges.aigenerated.poll_interval_seconds`                | Immediately via API; next poll                   |
| `challenges.aigenerated.questions_per_poll`                   | Immediately via API; next poll                   |
| `challenges.aigenerated` (other keys)                         | Requires `challenger` restart; done at next meeting |
| `providers.*`                                                 | Next meeting only                                |
| `messengers.*`                                                | Next meeting only                                |
| `gui.*`                                                       | Requires full restart                            |
| `logging.*`                                                   | Takes effect immediately                         |

## Validation

The schema enforces:

- Every enabled provider has credentials referenced from secrets.
- Exactly one `messengers.*` is enabled (in v1).
- Paths exist (`banks_dir`, ASR/LLM model paths where applicable).
- Port numbers are in valid range and don't collide between `gui.port`,
  `providers.*.webhook_port`, and the challenger's auto-assigned port.

Note: **zero challenge types enabled is explicitly allowed.**

Validation errors include a JSON Pointer into the config so the GUI can
highlight the exact offending field.
