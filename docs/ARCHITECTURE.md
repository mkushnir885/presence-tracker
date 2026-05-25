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
│        ┌────────────────────┼────────────────┬──────────────────┐       │
│        ▼                    ▼                ▼                  ▼       │
│   providers.*         messengers.*       challenges          gui.Server │
│   (zoom/meet/        (telegram/...)   (YAML pipeline:        + control  │
│    bbb/mock)                          load, fan-out, score)  plane HTTP │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
         │                            │                       ▲
         │                            │                       │ ptrack poll
         ▼                            ▼                       │ (thin client)
┌─────────────────────────────┐    ┌─────────────────────────────────────┐
│ ptrack_py challenger        │    │ ptrack_py (PyInstaller)             │
│  (long-running, AI-gen)     │    │  ptrack_analytics subcommand        │
│  faster-whisper + LLM       │    │  (one-shot: report / aggregate)     │
│  writes YAML to pending dir │    │  Polars (CSV generation)            │
│  optional: exec ptrack poll │    │                                     │
└─────────────────────────────┘    └─────────────────────────────────────┘
```

## Data flow

1. Teacher starts the meeting on one of the supported platforms and
   launches `ptrack track --meeting=<id>` (or clicks "Start tracking"
   in the GUI, if running).
2. The selected `Provider` adapter delivers meeting events (join/leave)
   into the session coordinator. Chat is not monitored — participant
   pairing is handled entirely via the Telegram bot (see "Participant
   identity" in `CLAUDE.md`).
3. A poll is dispatched by calling `ptrack poll --type=<label> <bank>`
   (from the GUI's Trigger poll menu, from the Python challenger when
   `auto_submit` is on, from the user's terminal, or from any external
   script). The poll endpoint loads the bank, calls
   `Provider.FetchPresence` for an up-to-the-second view of who is
   actually in the meeting right now, picks eligible participants from
   that snapshot, and randomly assigns one question per participant.
4. At poll time, one question record is appended per unique question to
   the meeting's `<meeting_id>.jsonl` file in `questions_dir`. Each
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
   `ptrack_py report --in meeting.parquet --format csv --out -` (stdout)
   and caches the result for the GUI stats columns. The same CSV is also
   offered as a download. Advanced users can import `ptrack_analytics`
   directly in Jupyter.

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
cancelled. Events emitted: `participant_joined` and `participant_left`.
Chat is not surfaced through the Provider interface.

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
    SendJoinConfirmation(ctx context.Context, handle Handle, meetingID, platform string) (MessageRef, error)

    SendChallenge(ctx context.Context, handle Handle, c ChallengePrompt) (MessageRef, error)
    EditMessage(ctx context.Context, ref MessageRef, newText string) error
    DeleteMessage(ctx context.Context, ref MessageRef) error
}
```

`Handle` is the messenger-specific persistent ID (for Telegram, the
`chat_id`). Three event kinds flow out of the channel:

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
- `RunPoll(ctx, session, bank, typeLabel)` — pick eligible participants,
  assign questions, append `.jsonl` records, dispatch through the
  `Messenger`, listen for answers, emit events. Called by the daemon's
  `POST /poll` HTTP endpoint.

The `typeLabel` string is a free-form tag attached to every
`challenge_issued` event for this poll round. The system never inspects
it; it exists for analytics and human-readable distinction between
producers.

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

### Listener port discovery

Every running daemon appends its loopback HTTP port to the
`PTRACK_PORTS` environment variable (comma-separated). `ptrack serve`
uses the configured `gui.bind_addr` port (default 8080); `ptrack track`
(headless) binds a random free loopback port. The exported list is
inherited by every child the daemon spawns — the Python challenger,
and any `ptrack poll` invocation re-entered from that child — so they
find their way back without an on-disk descriptor.

Because the daemon supervises exactly one meeting at a time, running
multiple `ptrack` processes in parallel is the supported way to track
multiple meetings. `ptrack poll` then uses `--port=<port>` to pick the
right daemon.

### `ptrack poll` CLI (thin client)

```
ptrack poll [--type=<label>] [--port=<port>] [--wait] <path-to-bank.yaml>
```

`ptrack poll` contains no challenge logic — it resolves the daemon URL,
POSTs to `http://127.0.0.1:<port>/poll`, and exits. Default `--type` is
`custom`. See `@docs/CHALLENGES.md` for the endpoint body and the
error codes.

Port resolution priority: `--server=URL` flag, `--port=<port>` flag,
single entry in `PTRACK_PORTS`, config.yaml `gui.port`, `8080`. If
`PTRACK_PORTS` lists more than one port and `--port` is not set, the
CLI errors and asks the user to disambiguate.

### Lifecycle endpoints

- `POST /system/unload-models` — releases the Python challenger's
  resident ASR + LLM models. Backed by the GUI's **Free models**
  button. The challenger process keeps running; the next generation
  reloads on demand.
- `POST /system/shutdown` — stops the active session, terminates the
  Python challenger, closes all listeners, and returns 200 once
  drainage is complete. Backed by the GUI's **Shut down** button; the
  browser then renders a "you can close this tab" page.

Closing the browser tab without using **Shut down** leaves the daemon
running and the Python challenger warm. A new tab reconnects to the
same session at the same `http://127.0.0.1:<port>` address.

## Cross-process model

### Go ↔ Python: ptrack_analytics (one-shot subprocess)

The analytics binary runs when a CSV report is requested or when the
GUI needs statistics for a meeting page:

```
ptrack_py report    --in <parquet> --format csv --out <csv-or->
ptrack_py aggregate --in '<glob>'  --format csv --out <csv-or->
```

Passing `--out -` writes the CSV to stdout, which Go reads directly to
populate GUI stats columns without a temporary file. Exit code + stderr
is the contract. The same library code is importable in Jupyter Notebooks
(`from ptrack_analytics import load, presence, challenges`).

### Go ↔ Python: challenger (long-running, optional)

Go spawns `ptrack_py challenger run` once per session, when
`challenges.auto_generation.enabled` is true. The contract is built
out of three primitives, in increasing order of intimacy:

1. **Files.** Python writes generated YAML banks to the pending
   directory (`/tmp/ptrack/` on Linux). That is the *output* contract.
2. **CLI re-entry.** When `auto_submit` is on, Python invokes
   `ptrack poll --type=aigenerated <path>` itself. This re-enters the
   same control plane the GUI uses; no extra surface to maintain.
3. **HTTP for audio.** Audio frames captured by the browser GUI are
   streamed over a WebSocket to the Go daemon and forwarded to the
   Python challenger over a small localhost HTTP endpoint (or its
   stdin). This is the only in-process link from Go into Python.

Endpoints exposed by the challenger (localhost only):

- `POST /context/audio` — Go relays browser-captured PCM/Opus frames.
- `POST /context/screen` — optional OCR frames, 1 fps, when screen OCR
  is enabled.
- `POST /control/unload-models` — release ASR + LLM memory.
- `POST /control/shutdown` — stop and exit.

Note that there is **no** `POST /challenges/generate` endpoint anymore.
Generation is autonomous inside the challenger: it watches its own
schedule, writes YAML, and optionally re-enters via `ptrack poll`. Go
never asks for a batch synchronously.

### Audio capture path

Audio is captured **by the browser**, not by either binary:

```
Browser (getUserMedia) ──WebSocket──► Go control plane ──HTTP──► Python challenger
   ▲                                                                 │
   │ mic picker, mute toggle                                         ▼
   │ permission dialog                                             ASR rolling window
```

Choosing browser-side capture buys three things:

- the browser's native device picker and microphone permission UX, with
  no extra UI code on our side;
- a working path on mobile, including Android-via-Termux: the browser
  on the phone captures audio and pipes it to the daemon over the
  loopback WebSocket;
- a mute control that lives where the teacher is already looking, with
  no audio captured before they explicitly start streaming.

The audio bytes are never written to disk by Go, Python, or the GUI.
The browser's `MediaRecorder`/`AudioWorklet` buffers stay in memory
only. faster-whisper consumes the frames into a rolling transcript
window of `context.audio_transcript.window_minutes` minutes; older
audio is dropped, not flushed to disk.

### Why subprocess + HTTP instead of gRPC

Two languages, one machine, bounded message rate. The HTTP layer is
debuggable with `curl` and trivial to stand up. The data path (Parquet
files and generated YAML banks) is already out-of-band; HTTP only
carries control messages and the audio stream.

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
symlink to the default location. The user-facing directories
(`meetings_dir`, `questions_dir`, `reports_dir`) are settable from
`config.json`.

### Linux

```
~/.config/ptrack/                       # configDir() — fixed
├── config.json                         # 0600; secrets inline
└── config.schema.json                  # 0644; written on every save

~/.local/share/ptrack/                  # config.DataDir() — fixed
├── participants.db                     # registry (BoltDB)
├── meet_oauth.json                     # OAuth tokens
└── zoom_oauth.json

/tmp/ptrack/                            # pending auto-generated YAML banks
└── auto-2026-04-21T10-15.yaml

~/Documents/ptrack/                     # user-facing — settable
├── meetings/                           # meetings_dir
│   ├── 2026-04-21-algebra.parquet
│   └── 2026-04-23-algebra.parquet
├── questions/                          # questions_dir
│   ├── 2026-04-21-algebra.jsonl
│   └── 2026-04-23-algebra.jsonl
└── 2026-04-21-algebra.csv              # reports_dir (root of ~/Documents/ptrack)

~/.cache/ptrack/                        # config.CacheDir() — fixed
└── models/                             # ASR + LLM model weights
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

%TEMP%\ptrack\                          # pending auto-generated YAML banks

%USERPROFILE%\Documents\ptrack\         # user-facing — settable
├── meetings\
├── questions\
└── 2026-04-21-algebra.csv

%LOCALAPPDATA%\ptrack\cache\            # config.CacheDir() — fixed
└── models\
```

## GUI

The web server binds to `127.0.0.1` by default (loopback-only). The
teacher opens `http://127.0.0.1:8080` in any browser. `ptrack serve`
optionally opens the browser automatically on start.

The GUI supports dark/light/system color themes and English/Ukrainian
UI languages. Theme preference and language are stored in localStorage.
Translation files live in `go/src/internal/gui/locales/<lang>.json`.

## Security and privacy

- All credentials stored in `secrets.yaml` with 0600 permissions (Linux)
  or DPAPI-encrypted (Windows). Never in `config.yaml`.
- The challenger service binds to `127.0.0.1` only. Never 0.0.0.0.
- Transcript data is in-memory only — never written to disk by
  `challenger`. This is a hard requirement.
- Display name collision is rejected at registration time — the bot
  returns an error if a name is already claimed by a different handle,
  so only one Telegram account can own a given `(platform, name)` pair.
- Event log data is kept per the configured retention (default: 180 days).

See `@docs/ETHICS.md` for the consent and retention rationale.
