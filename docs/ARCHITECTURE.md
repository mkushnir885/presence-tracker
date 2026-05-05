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
│                       │                                                 │
│      ┌────────────────┼────────────────┬─────────────────────┐          │
│      ▼                ▼                ▼                     ▼          │
│  providers.*    messengers.*     challenges.Poller        gui.Server    │
│  (zoom/meet/   (telegram/...)    + challenges.*          (templ+htmx)   │
│   bbb/mock)                      (filebased/aigenerated)  optional      │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
         │                                        │
         ▼                                        ▼
┌─────────────────────────────┐    ┌─────────────────────────────────────┐
│ ptrack_py (PyInstaller)     │    │ ptrack_py (PyInstaller)             │
│  challenger service         │    │  ptrack_analytics subcommand        │
│  (long-running, AI-gen only)│    │  (one-shot: report / generate)      │
│  faster-whisper + LLM       │    │  Polars (CSV generation)            │
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
3. For **file-based challenges**: the teacher manually triggers a poll
   from the GUI. For **AI-generated challenges**: the `Poller` fires
   automatically when the context window is ready. In both cases the
   `Challenge` implementation generates a batch of questions that are
   randomly distributed across eligible participants.
4. At poll time, one question record is appended per unique question to
   the meeting's `<meeting_id>.jsonl` file in `questions_dir`. Each
   record carries a UUIDv4 `question_id` plus the full question content
   (prompt, type, choices, correct answer, source bank if file-based).
   `challenge_issued` events in the Parquet reference that UUID.
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

## Interfaces

### Provider (`go/src/internal/providers/provider.go`)

```go
type Provider interface {
    Name() string
    Authenticate(ctx context.Context) error
    Subscribe(ctx context.Context, meetingID string) (<-chan Event, error)
    FetchPostMeeting(ctx context.Context, meetingID string) ([]Event, error)
}
```

`Subscribe` closes the channel when the meeting ends or `ctx` is
cancelled. Events emitted: `participant_joined` and `participant_left`.
Chat is not surfaced through the Provider interface. `FetchPostMeeting`
is idempotent.

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

- `RegistrationEvent` — a student sent `/register <platform> <name>`.
  The messenger adapter validates the command syntax and calls
  `Registry.Register`; the result determines the bot's reply. This is
  handled inside the adapter so registration works even when no meeting
  is active.
- `JoinConfirmationEvent` — a student tapped **Yes** or **No** on a
  join-confirmation message. The session coordinator uses this to emit
  `participant_verified` or `participant_verification_denied`.
- `AnswerEvent` — a student answered a challenge question.

### Challenge (`go/src/internal/challenges/challenge.go`)

```go
type ChallengeType interface {
    Name() string               // e.g. "filebased", "aigenerated"
    Configure(cfg ChallengeConfig) error
    // Generate produces a batch of prompts for a poll round.
    // Returns ErrNoContext when questions cannot yet be produced.
    // The returned slice may be smaller than count.
    Generate(ctx context.Context, count int) ([]ChallengePrompt, []AnswerKey, error)
    // Score decides correct/incorrect. Timing is handled by the poller.
    Score(key AnswerKey, submitted Answer) ScoreResult
}

// Poller coordinates poll rounds for a challenge type.
type Poller interface {
    TriggerPoll(ctx context.Context, session *Session) error
    Start(ctx context.Context, session *Session) error   // AI-gen only: auto loop
    Stop(ctx context.Context) error
    UpdateConfig(cfg PollConfig) error
}
```

### Participant registry (`go/src/internal/participants/`)

```go
type Registry interface {
    // Resolve looks up a participant by (platform, displayName).
    // Matching is case-insensitive with whitespace trimming.
    Resolve(platform string, displayName string) (ParticipantID, bool)

    // Register stores a (platform, displayName) → handle binding.
    // Returns ErrNameTaken if that (platform, displayName) is already
    // claimed by a different handle. A handle may overwrite its own
    // previous entry for the same (platform, displayName).
    Register(messengerName string, handle Handle, platform, displayName string) (ParticipantID, error)

    // Unregister removes the registry entry for a participant.
    Unregister(id ParticipantID) error

    // Handle returns the messenger handle for a participant.
    Handle(p ParticipantID, messengerName string) (Handle, bool)

    // List returns all entries, for display on the registry GUI page.
    List() ([]RegistryEntry, error)

    // Clear removes all entries. Called by DELETE /registry.
    // Parquet files are not affected.
    Clear() error
}

type RegistryEntry struct {
    ID             ParticipantID
    Platform       string
    DisplayName    string   // canonical casing as registered
    MessengerName  string
    Handle         Handle
    MessengerLabel string   // human-readable (e.g. Telegram @username or first name)
    RegisteredAt   time.Time
}
```

Backed by BoltDB. Persists across meetings. The messenger adapter calls
`Register` directly when it receives a `/register` command — registration
works even when no meeting is active.

### Per-file display name rewrite (`eventstore`)

A function in `go/src/internal/eventstore/` handles all rename
operations. Both the single-file and multi-file rename buttons in the
GUI call it once per file:

```go
// RenameParticipant rewrites the display_name column for all events
// belonging to participantID in the given Parquet file. It reads the
// file into memory, patches the column, and atomically replaces the file.
func RenameParticipant(ctx context.Context, path string, participantID ParticipantID, newName string) error
```

This is the backend for `PATCH /meetings/{id}/participants/{p}/display-name`.
Renames are always scoped to the files explicitly selected by the
teacher. They never create a persistent name override — future meetings
record the platform-provided name until the teacher renames them too.

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

### Go ↔ Python: challenger (long-running, AI challenges only)

Go spawns `ptrack_py challenger serve --port=<p>` at session start.
Challenger listens on localhost-only HTTP:

- `POST /context/audio` — Go pushes PCM audio chunks from the meeting.
- `POST /context/screen` — Go pushes screen-share frames at 1 fps (if
  screen OCR is enabled).
- `POST /challenges/generate` — Go requests a batch of questions keyed
  by participant IDs. Returns prompts + answer keys in the file-based
  YAML format.
- `DELETE /session` — meeting ended; models stay loaded for the next
  session.

Challenger keeps ASR and LLM loaded. Startup cost is paid once, not per
meeting.

### Why subprocess-HTTP instead of gRPC

Two languages, one machine, bounded message rate. The HTTP layer is
debuggable with `curl` and trivial to stand up. The data path (Parquet
files) is already out-of-band; HTTP only carries control messages.

## Event schema

Canonical schema in `@docs/EVENT_SCHEMA.md`. Enforced in three places
that must stay in sync:

- `go/src/internal/eventstore/schema.go` (Go Arrow schema)
- `py/src/ptrack_analytics/schema.py` (Polars schema)
- `@docs/EVENT_SCHEMA.md` (authoritative prose)

Breaking changes require updating all three and bumping `schema_version`.

## Storage layout

All paths are resolved by the `config` package using platform conventions.
The config key `data_dir` and `meetings_dir` (etc.) override defaults.

### Linux

```
~/.config/ptrack/
├── config.yaml
└── secrets.yaml            # 0600; credentials only

~/.local/share/ptrack/
└── participants.db          # registry (BoltDB)

~/Documents/ptrack/          # teacher-visible data
├── meetings/
│   ├── 2026-04-21-algebra.parquet
│   └── 2026-04-23-algebra.parquet
├── questions/
│   ├── 2026-04-21-algebra.jsonl    # question records for that meeting
│   └── 2026-04-23-algebra.jsonl
└── reports/
    └── 2026-04-21-algebra.csv

~/.cache/ptrack/
└── models/                  # ASR + LLM model weights
```

### Windows

```
%APPDATA%\ptrack\
├── config.yaml
└── secrets.yaml             # DPAPI-encrypted at rest

%LOCALAPPDATA%\ptrack\
└── participants.db

%USERPROFILE%\Documents\ptrack\
├── meetings\
│   └── 2026-04-21-algebra.parquet
├── questions\
│   └── 2026-04-21-algebra.jsonl
└── reports\
    └── 2026-04-21-algebra.csv

%LOCALAPPDATA%\ptrack\cache\
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
