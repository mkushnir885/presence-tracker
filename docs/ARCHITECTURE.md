# Architecture

## Overview

The system ships as two binaries: the Go binary (`ptrack` / `ptrack.exe`)
and the Python binary (`ptrack_py` / `ptrack_py.exe`, built with
PyInstaller). The teacher places both in the same directory; Go discovers
the Python binary by looking next to itself, then in PATH.

`ptrack` runs as a CLI tool. `ptrack serve` optionally starts an HTTP
server for the browser-based GUI; `ptrack track` runs headlessly without
it.

```
┌──────────────────────────────── ptrack (Go) ────────────────────────────┐
│                                                                         │
│   cobra CLI ── session.Coordinator ── eventstore (Parquet)              │
│   (track/serve/poll)        │                                           │
│        ┌──────────┬─────────┴─────┬──────────────┬─────────────┐        │
│        ▼          ▼               ▼              ▼             ▼        │
│   providers.*  messengers.*   challenges     challenger    gui.Server   │
│   (zoom/meet/  (telegram/    (YAML           (in-process   + control    │
│    bbb/mock)    ...)          pipeline)       audio→ASR→   plane HTTP)  │
│                                               LLM→YAML)                 │
└────────────────────────────────────────┬──────────────────────┬─────────┘
                                         │ OpenAI-compatible    │ ptrack poll
                                         ▼ HTTP                 │ (thin client)
                              External ASR + LLM         ┌──────────────────────┐
                              (Ollama / OpenAI /         │ ptrack_py            │
                               Gemini / ...)             │  (one-shot: report / │
                                                         │   stats)             │
                                                         │  Polars (CSV + JSON) │
                                                         └──────────────────────┘
```

## Data flow

1. Teacher starts the meeting on one of the supported platforms and
   launches `ptrack track --meeting=<id>` (or clicks "Start tracking"
   in the GUI, if running).
2. The selected `Provider` adapter delivers meeting events (join/leave)
   into the session coordinator. Chat is not monitored — participant
   pairing is handled entirely via the Telegram bot (see "Participant
   identity" in `CLAUDE.md`).
3. A poll is dispatched by calling `ptrack poll [--auto-submitted] <bank>`
   (from the GUI's Trigger poll menu, from the user's terminal, or from
   any external script). The in-process auto-generator, when
   `auto_submit` is on, calls the same pipeline directly without going
   through the CLI. The poll endpoint loads the bank, calls
   `Provider.FetchPresence` for an up-to-the-second view of who is
   actually in the meeting right now, picks eligible participants from
   that snapshot, and randomly assigns one question per participant.
4. At poll time, one question record is appended per unique question to
   the meeting's JSONL file in `questions_dir` (basename matches the
   Parquet file so loaders can pair them by filename). Each
   record carries a UUIDv4 `question_id` plus the full question content
   (prompt, type, choices, correct answer). `challenge_issued` events in
   the Parquet reference that UUID. The `--type` label travels with the
   event row, not with the `.jsonl` record.
5. The `Messenger` adapter delivers each question to the assigned
   participant through the appropriate channel (Telegram DM). Delivery
   is fanned out as close to simultaneously as rate limits allow.
6. The messenger adapter listens for answers. An answer within
   `answer_window` produces a `challenge_answered_correct` or
   `challenge_answered_incorrect` event. If the window elapses with no
   answer, the messenger edits or deletes the message and a
   `challenge_unanswered` event is emitted.
7. All events are written to `<meetings_dir>/<meeting_id>.parquet` by
   `eventstore`.
8. For CSV report generation, Go invokes
   `ptrack_py report meeting.parquet` and reads the CSV off stdout.
   The same CSV is offered as a download. Advanced users can import
   `ptrack_analytics` directly in Jupyter.

The teacher remains in control of the meeting's flow: the only act
that interrupts ordinary teaching is the brief moment of triggering or
approving a poll. Everything that follows — fan-out, timing, scoring,
event writing — happens in the background.

## Interfaces

### Provider (`go/src/internal/providers/provider.go`)

```go
type Provider interface {
    Name() string
    Authenticate(ctx context.Context) error
    Subscribe(ctx context.Context, meetingID string) (<-chan Event, error)
    FetchPresence(ctx context.Context, meetingID string) ([]Participant, error)
}
```

`Subscribe` closes the channel when the meeting ends or `ctx` is
cancelled. Events emitted: `participant_joined`, `participant_left`,
and the session boundary signals `meeting_started` / `meeting_ended`
(see "Session boundaries" below). Chat is not surfaced through the
Provider interface.

#### Session boundaries

The session coordinator writes exactly one `session_started` and one
`session_ended` event per Parquet file. Which `cause` they carry
depends on the relative ordering of tracking attach/detach and the
provider's view of the meeting:

- At attach time the provider must answer "is the meeting already in
  progress?" — by emitting `meeting_started` immediately (with the
  meeting's true start timestamp) if tracking attached *before* the
  meeting began and the start was later observed, or by reporting
  "already running" if tracking attached after the meeting had begun.
  The coordinator emits `session_started` with `cause = "meeting"` in
  the first case, `cause = "tracking"` in the second.
- At detach time the coordinator emits `session_ended` with
  `cause = "meeting"` if the provider reported `meeting_ended` first,
  or `cause = "tracking"` if the daemon is shutting down while the
  provider still considers the meeting active.

A provider that cannot answer the "already in progress?" question at
attach time (e.g. a hypothetical webhook-only adapter that needs to
receive the meeting-start notification live) must fail
`Subscribe`/`Start` rather than emit a misleading boundary. Current
polling-based providers answer it trivially from the first poll
response.

Participants still in the meeting at `session_ended` are intentionally
left as open bands — no synthetic `participant_left` is emitted. See
`@docs/EVENT_SCHEMA.md` for the analytics-side close rule and the GUI
marker semantics.

`FetchPresence` is a synchronous "who is in the meeting right now"
query. The challenge pipeline invokes it immediately before fanning
out a poll round so dispatch decisions are made on a fresh snapshot
rather than on the (potentially stale) state accumulated from the
background `Subscribe` stream — see "Why polling, not webhook" below.

#### Operating mode per platform

All three providers use **polling only**. The webhook surfaces that BBB
and Zoom expose are not consumed by ptrack — see "Why polling, not
webhook" below.

| Platform | How events are obtained                                                                                                                                                                                                                                                                            |
|----------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **BBB**  | Polls `getMeetingInfo` on a timer (`poll_interval_seconds`). No reachability requirement; works with every BBB installation, including ones reachable only over the campus VPN.                                                                                                                    |
| **Zoom** | Polls the Zoom Dashboard API (`dashboard_meetings:read:admin`). Requires **Zoom Pro plan or higher** and OAuth authorisation by an account admin. No public address required.                                                                                                                      |
| **Meet** | Polls the conference records REST API v2 (`conferenceRecords.participants` / `participantSessions`). Requires a Google Workspace account (personal Gmail does not expose conference data through this API). No public address required.                                                            |

#### Why polling, not webhook

1. **Network reachability is a non-starter for a local app.** Webhook
   requires the platform's servers to reach the teacher's machine —
   Zoom needs a public HTTPS address, BBB at minimum needs LAN/VPN
   routing. ptrack is positioned as a tool the teacher launches on
   their own laptop with no infrastructure to set up.
2. **Zoom webhooks dropped events in testing.** Empirically, the Zoom
   Webhook Events stream missed a non-trivial fraction of
   join/leave notifications for participants whose activity *was*
   visible through the Dashboard API. The harder-to-configure channel
   turned out to be the less reliable one.
3. **Meet has no webhook anyway.** A polling implementation has to
   exist for Meet, so a webhook code path would be an *additional*
   surface — extra adapter branch, inbound HTTP listener, HMAC
   validation, replay/idempotency handling — used by only two
   providers, for marginal benefit.
4. **The latency win does not matter here.** Challenge dispatch
   accuracy does not depend on the background polling cadence:
   immediately before fanning out a poll round, the session
   coordinator issues a fresh `Provider` presence fetch and filters
   eligible recipients against that just-in-time snapshot. A single
   HTTP call (sub-second in practice) yields the same accuracy as a
   webhook channel, without any of webhook's infrastructure
   requirements. Background polling cadence (`poll_interval_seconds`,
   typically 30–60 s) is therefore tuned for provider rate-limit
   friendliness and GUI/log freshness, not for dispatch correctness.

`New()` in each provider package returns the polling adapter behind
the `Provider` interface.

### Messenger (`go/src/internal/messengers/messenger.go`)

```go
type Messenger interface {
    Name() string
    // Start begins the bot loop. The returned channel emits RegistrationEvent,
    // JoinConfirmationEvent, and AnswerEvent values. Closed when ctx is done.
    Start(ctx context.Context) (<-chan Event, error)
    Stop(ctx context.Context) error

    // SendJoinConfirmation sends a "Did you just join [meeting] on [platform]?"
    // DM with Yes/No inline buttons. The response arrives as a JoinConfirmationEvent.
    SendJoinConfirmation(ctx context.Context, handle Handle, lang, meetingID, platform string) (MessageRef, error)

    SendChallenge(ctx context.Context, handle Handle, lang string, c ChallengePrompt) (MessageRef, error)
    EditMessage(ctx context.Context, ref MessageRef, newText string) error
    DeleteMessage(ctx context.Context, ref MessageRef) error
}
```

`Handle` is the messenger-specific persistent ID (for Telegram, the
`chat_id`).

**Recipient language is supplied by the caller, not re-resolved by the
adapter.** Every adapter method the session coordinator drives
(`SendJoinConfirmation`, `SendChallenge`, the result `Notify`/
`SendNotification` follow-ups) takes the recipient's catalog `lang`. The
coordinator already holds each participant's registered language — it
resolved the registry entry to find where to send — so it passes that
language down rather than making the adapter hit the registry once per
delivered message. The adapter only consults the registry for its own
*incoming* command and callback replies (`/start`, `/register`,
`/whoami`, `/language`, MCQ-keyboard taps), where no caller-supplied
language exists. An empty `lang` falls back to English.

Three event kinds flow out of the channel:

- `RegistrationEvent` — a student sent `/register <name>`. The
  messenger adapter validates the command syntax and calls
  `Registry.Register`; the result determines the bot's reply. The
  adapter also handles `/unregister` (releases the caller's
  registration) and `/whoami` (shows the current one). Each messenger
  account holds at most one registration; sending `/register` again
  replaces the previous name. Registration is handled inside the
  adapter so it works even when no meeting is active.
- `JoinConfirmationEvent` — a student tapped **Yes** or **No** on a
  join-confirmation message. On **Yes** the session coordinator flushes
  the buffered `participant_joined` event (the event log only contains
  verified participants, so the joined row is itself the verification
  record). On **No** the buffer is discarded; no events are written.
- `AnswerEvent` — a student answered a challenge question.

The Messenger runs for the whole daemon lifetime, not per-session.
`messengers.Router` owns the bot loop and forwards events to whichever
session coordinator is currently active (installed via `SetHandler`).
Registrations therefore work before the first meeting is started and
between meetings; join confirmations and answers outside an active
session are dropped.

### Challenge pipeline (`go/src/internal/challenges/`)

There is **no `ChallengeType` interface and no plug-in producer registry**.
The challenge package is a single concrete pipeline:

- `Load(path string) (Bank, error)` — parse and validate a YAML bank
  (`validate.go`).
- `Score(q Question, submitted Answer) ScoreResult` — score one answer
  against one question.
- `RunPoll(ctx, session, bank, autoSubmitted)` — pick eligible
  participants, assign questions, append `.jsonl` records, dispatch
  through the `Messenger`, listen for answers, emit events. Called by
  the daemon's `POST /poll` HTTP endpoint.

The `autoSubmitted` boolean is attached to every `challenge_issued`
event for this poll round. The system never inspects it; it exists so
analytics can separate unreviewed challenger output from
teacher-driven polls.

How a YAML reaches `RunPoll` is *not* the challenge package's concern —
that is the control plane's concern. See "Control plane" below.

### Participant registry (`go/src/internal/participants/`)

```go
type Registry interface {
    // Resolve looks up a participant by displayName.
    // Matching is case-insensitive with whitespace trimming. The
    // returned entry carries the canonical registered casing.
    Resolve(displayName string) (RegistryEntry, bool)

    // Register stores a displayName → handle binding.
    // Returns ErrNameTaken if the name is already claimed by a different
    // handle. If the handle already has a registration under a different
    // name, the previous entry is replaced atomically.
    Register(messengerName string, handle Handle, messengerLabel, displayName string) error

    // UnregisterByName removes the entry for the given display name.
    UnregisterByName(displayName string) (bool, error)

    // UnregisterByHandle removes the entry owned by (messengerName, handle).
    UnregisterByHandle(messengerName string, handle Handle) (bool, error)

    // HandleForName returns the messenger handle bound to displayName
    // under messengerName (used by the coordinator to send the verification DM).
    HandleForName(displayName, messengerName string) (Handle, bool)

    // LookupByHandle returns the entry owned by (messengerName, handle), if any.
    LookupByHandle(messengerName string, handle Handle) (RegistryEntry, bool)

    // List returns all entries, for display on the registry GUI page.
    List() ([]RegistryEntry, error)

    // Clear removes all entries. Called by DELETE /registry.
    // Parquet files are not affected.
    Clear() error
}

type RegistryEntry struct {
    DisplayName    string   // canonical casing as registered
    MessengerName  string
    Handle         Handle
    MessengerLabel string   // human-readable (e.g. Telegram @username or first name)
    RegisteredAt   time.Time
}
```

Backed by BoltDB. Persists across meetings. Display names are
platform-agnostic — a single registration matches the participant on
any provider. Each messenger account holds at most one registration at
a time. Display name is the identity end to end: it is the bolt primary
key, the URL identifier on the registry page, and the value written to
every per-participant Parquet event. There is no internal opaque ID.
The messenger adapter calls `Register` directly when it receives a
`/register` command — registration works even when no meeting is active.

### Per-file display name rewrite (`eventstore`)

A helper in `go/src/internal/eventstore/` rewrites display names in a
single Parquet file:

```go
// UpdateDisplayName rewrites every row whose display_name equals oldName,
// replacing it with newName. Reads the file into memory, patches the
// column, atomically replaces the file.
func UpdateDisplayName(parquetPath, oldName, newName string) error
```

This is the backend for
`PATCH /participants/{name}/display-name?file=<a>[&file=<b>…]&new=<new>`,
where `{name}` is the current (URL-encoded) display name. The handler
applies `UpdateDisplayName` to every file listed in the query. Renames
are scoped to the files explicitly requested and never create a
persistent override — future meetings record the canonical name from
whatever registration is active at the time.

## Control plane

`ptrack serve` and `ptrack track` both bind an HTTP server on
`127.0.0.1` that exposes two surfaces:

- The **HTML routes** (rendered by `internal/gui/`) — only enabled in
  `serve` mode.
- The **JSON control plane** — always on. The same routes serve the
  GUI's htmx form submissions and CLI thin clients.

### Listener port

Both `ptrack serve` and `ptrack track` bind to `gui.port` from
`config.json` (or the `--port` flag override; the flag does not persist
to the config file). There is no random-port fallback: if the chosen
port is already in use the daemon refuses to start with a hint to pass
`--port=<free port>`. The user is responsible for assigning distinct
ports when running multiple daemons in parallel.

### `ptrack poll` CLI (thin client)

```
ptrack poll [--auto-submitted] [--port=<port>] [--wait] <path-to-bank.yaml>
```

`ptrack poll` contains no challenge logic — it resolves the daemon URL,
POSTs to `http://127.0.0.1:<port>/poll`, and exits. `--auto-submitted`
defaults to false; pass it only from automated producers that dispatch
without teacher review. See `@docs/CHALLENGES.md` for the endpoint body
and the error codes.

Port resolution priority: `--server=URL` flag, `--port=<port>` flag,
config `gui.port`. When more than one daemon is running, the user
passes `--port=<port>` to pick which one — there is no env-based
auto-discovery.

### Lifecycle endpoints

- `POST /system/shutdown` — stops the active session, closes the audio
  WebSocket, drains the in-process challenger, closes all listeners,
  and returns 200 once drainage is complete. Backed by the GUI's
  **Shut down** button; the browser then renders a "you can close this
  tab" page.

There is no `/system/unload-models` endpoint: ptrack holds no model
weights of its own, so there is nothing in-process to free. Memory used
by the external ASR/LLM backend (e.g. Ollama) is the operator's
concern.

Closing the browser tab without using **Shut down** leaves the daemon
running. A new tab reconnects to the same session at the same
`http://127.0.0.1:<port>` address.

## Cross-process model

### Go ↔ Python: ptrack_py (one-shot subprocess)

The `ptrack_py` binary runs when Go needs CSV reports or GUI stats:

```
ptrack_py report <parquet> [<p2> …]
ptrack_py stats  <parquet> [<p2> …]
```

Both commands take positional Parquet paths or glob patterns and
write their result (CSV or JSON) to stdout. `report` produces a
per-meeting CSV when exactly one Parquet matches and the cross-meeting
aggregate when more do.

Go reads the result off stdout directly. Exit code + stderr is the
contract.

The CSV / JSON producers (`reports.py`, `stats.py`) live in
`py/src/ptrack_py/` — the binary-only package — and build on top of
`py/src/ptrack_analytics/`, the Jupyter-facing library
(`from ptrack_analytics import load, presence, challenges`). Notebook
users depend on `ptrack_analytics` directly and never on `ptrack_py`.

### Go ↔ external inference (OpenAI-compatible HTTP)

Auto-generation, when enabled, sends audio to an ASR endpoint and
prompts to an LLM endpoint over plain OpenAI-compatible HTTP. The
default is a local Ollama daemon; any compatible gateway works (base
URL, API key, and model name are config-driven —
`challenges.auto_generation.asr.*` and `challenges.auto_generation.llm.*`).

ptrack owns no model weights, no warm-up state, and no resident memory
for inference. Lifecycle of the chosen backend (start, stop, free) is
the operator's responsibility — `ollama serve` for local, account
quota for hosted. There is no `/challenges/generate` endpoint anywhere:
generation is autonomous inside `internal/challenger/`, which watches
its own schedule, writes YAML, and (when `auto_submit` is on)
dispatches in-process through the challenge pipeline.

### Audio capture path

Audio is captured **by the browser**, not by ptrack itself:

```
Browser (getUserMedia) ──WebSocket──► Go control plane ──HTTP──► ASR backend
   ▲                                          │
   │ mic picker, mute toggle                  ▼
   │ permission dialog            rolling transcript in memory
                                              │
                                              ▼  LLM /chat/completions
                                       internal/challenger
                                       (writes YAML, optionally dispatches)
```

Choosing browser-side capture buys three things:

- the browser's native device picker and microphone permission UX, with
  no extra UI code on our side;
- a working path on mobile, including Android-via-Termux: the browser
  on the phone captures audio and pipes it to the daemon over the
  loopback WebSocket;
- a mute control that lives where the teacher is already looking, with
  no audio captured before they explicitly start streaming.

The audio bytes are never written to disk by Go or the browser; the
browser's `MediaRecorder`/`AudioWorklet` buffers stay in memory only.
Go batches incoming frames into short segments (a few seconds each)
and POSTs each one to the ASR endpoint. The returned transcripts
accumulate into a rolling window of `transcript_window_minutes`
minutes; older content is evicted in memory, never flushed to disk.

### Why subprocess for ptrack_py instead of in-process RPC

The Python work that remains — Polars-backed CSV reports and the GUI
stats JSON — is naturally one-shot: load Parquet, run Polars
expressions, emit a few KB of output. A subprocess invocation with
stdout-as-result is debuggable from a shell (`ptrack_py report meeting.parquet`),
has no IPC state to maintain, and recycles all memory between runs. Go
reads the output and caches it next to the inputs.

## Event schema

Canonical schema in `@docs/EVENT_SCHEMA.md`. Enforced in three places
that must stay in sync:

- `go/src/internal/eventstore/schema.go` (Go Arrow schema)
- `py/src/ptrack_analytics/schema.py` (Polars schema)
- `@docs/EVENT_SCHEMA.md` (authoritative prose)

Breaking changes require updating all three and bumping `schema_version`.

## Storage layout

Internal directories (config, app data, cache) are fixed at platform
defaults — not user-settable. Users who want them elsewhere create a
symlink to the default location. The user-facing `meetings_dir` is
settable from `config.json`; reports are streamed to stdout, not stored.

### Linux

```
~/.config/ptrack/                       # configDir() — fixed
├── config.json                         # 0600; secrets inline
└── config.schema.json                  # 0644; written on every save

~/.local/share/ptrack/                  # config.DataDir() — fixed
├── participants.db                     # registry (BoltDB)
├── meet_oauth.json                     # OAuth tokens
└── zoom_oauth.json

~/Documents/ptrack/                     # user-facing — settable
├── meetings/                           # meetings_dir
│   ├── 2026-04-21-algebra.parquet
│   └── 2026-04-23-algebra.parquet
├── questions/                          # questions_dir
│   ├── 2026-04-21-algebra.jsonl
│   └── 2026-04-23-algebra.jsonl
├── pending-banks/                      # challenges.auto_generation.review_dir (only used when auto_submit = false)
│   └── auto-2026-04-21T10-15.yaml
```

### Windows

```
%APPDATA%\ptrack\                       # configDir() — fixed
├── config.json
└── config.schema.json

%LOCALAPPDATA%\ptrack\                  # config.DataDir() — fixed
├── participants.db
├── meet_oauth.json
└── zoom_oauth.json

%USERPROFILE%\Documents\ptrack\         # user-facing — settable
├── meetings\
├── questions\
├── pending-banks\                      # challenges.auto_generation.review_dir (only used when auto_submit = false)
└── 2026-04-21-algebra.csv
```

## GUI

The web server binds to `127.0.0.1` by default (loopback-only). The
teacher opens `http://127.0.0.1:8080` in any browser. `ptrack serve`
optionally opens the browser automatically on start.

The GUI supports dark/light/system color themes and English/Ukrainian
UI languages. Theme preference and language are stored in localStorage.

Translation lookup is provided by the small `internal/i18n` package: a
`Catalog` merges JSON namespaces (one per subsystem) and hands out
`Locale` values bound to a language. The GUI loads
`go/src/internal/gui/locales/<lang>.json`; messenger adapters load
`internal/messengers/locales/<lang>.json` (shared keys) plus their own
`internal/messengers/<adapter>/locales/<lang>.json` (adapter-specific
keys). Keys use dotted prefixes (`messenger.*`, `messenger.telegram.*`,
…); unknown keys fall through to the key string itself so untranslated
copy is visible rather than blank.

## Security and privacy

- All credentials live inline in `config.json` with mode `0600` on
  Unix; on Windows the user is responsible for ACLs.
- The daemon's HTTP listener binds to `127.0.0.1` only. Never 0.0.0.0.
- Audio frames and transcripts are in-memory only — Go never writes
  either to disk. ASR is performed by an external HTTP backend, which
  receives audio over the network; choosing where to send it (local
  Ollama vs. a hosted API) is the teacher's privacy decision.
- Display name collision is rejected at registration time — the bot
  returns an error if a name is already claimed by a different handle,
  so only one Telegram account can own a given `(platform, name)` pair.
- Event log data is kept per the configured retention (default: 180 days).

See `@docs/ETHICS.md` for the consent and retention rationale.
