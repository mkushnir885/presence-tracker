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
│                                                                          │
│   cobra CLI ── session.Coordinator ── eventstore (Parquet)               │
│                       │                                                  │
│      ┌────────────────┼────────────────┬─────────────────────┐          │
│      ▼                ▼                ▼                     ▼          │
│  providers.*    messengers.*     challenges.Poller        gui.Server     │
│  (zoom/meet/   (telegram/...)    + challenges.*          (templ+htmx)   │
│   bbb/mock)                      (filebased/aigenerated)  optional      │
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
         │                                        │
         ▼                                        ▼
┌─────────────────────────────┐    ┌─────────────────────────────────────┐
│ ptrack_py (PyInstaller)     │    │ ptrack_py (PyInstaller)             │
│  challenger service         │    │  ptrack_analytics subcommand        │
│  (long-running, AI-gen only)│    │  (one-shot: report / generate)      │
│  faster-whisper + LLM       │    │  Polars + matplotlib + fpdf2        │
└─────────────────────────────┘    └─────────────────────────────────────┘
```

## Data flow

1. Teacher starts the meeting on one of the supported platforms and
   launches `ptrack track --meeting=<id>` (or clicks "Start tracking"
   in the GUI, if running).
2. The selected `Provider` adapter delivers meeting events (join/leave,
   chat) into the session coordinator. **Chat is monitored solely to
   detect pairing codes** (`PTRACK:<code>`) — chat content is not stored.
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
8. For PDF and chart generation, Go invokes
   `ptrack_py report --in meeting.parquet --out report.pdf`.
   Advanced users can import `ptrack_analytics` directly in Jupyter.

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
cancelled. Events emitted include `participant_joined`, `participant_left`,
and `chat_message` (used internally for pairing code detection; not
persisted unless the code matches). `FetchPostMeeting` is idempotent.

### Messenger (`go/src/internal/messengers/messenger.go`)

```go
type Messenger interface {
    Name() string
    Start(ctx context.Context) (<-chan Event, error)   // registration + answer events
    Stop(ctx context.Context) error

    SendChallenge(ctx context.Context, handle Handle, c ChallengePrompt) (MessageRef, error)
    EditMessage(ctx context.Context, ref MessageRef, newText string) error
    DeleteMessage(ctx context.Context, ref MessageRef) error
}
```

`Handle` is the messenger-specific persistent ID (for Telegram, the
`chat_id`). The messenger emits two kinds of events on its channel:
pairing events (a student sent `/start` and received a code) and answer
events (a student answered a previously-delivered challenge).

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
    // Resolve maps a platform identifier to an internal ParticipantID.
    Resolve(platform string, platformID string) (ParticipantID, bool)

    // StartPairing is called when a new Telegram /start arrives.
    // Returns a short one-time code to be typed by the student in the meeting chat.
    StartPairing(messengerName string, handle Handle) (pairingCode string, err error)

    // CompletePairing is called when the provider adapter detects a
    // PTRACK:<code> in meeting chat. Binds the platform identifier to
    // the Telegram handle that initiated pairing.
    CompletePairing(platform string, platformID string, code string) (ParticipantID, error)

    // SetDisplayName stores a teacher-defined override name for a participant.
    // This name takes precedence over the platform-provided display_name in
    // all analytics and reports.
    SetDisplayName(p ParticipantID, name string) error

    Handle(p ParticipantID, messengerName string) (Handle, bool)
    All() []Participant
}
```

Backed by a small on-disk store (BoltDB or JSON file). Persists across
meetings so pairing is one-time per platform.

## Cross-process model

### Go ↔ Python: ptrack_analytics (one-shot subprocess)

The analytics binary runs only when a PDF or chart is requested:

```
ptrack_py report   --in <parquet> --out <pdf>
ptrack_py aggregate --in '<glob>'  --out <pdf>
```

Exit code + stderr is the contract. The same library code is importable
in Jupyter Notebooks (`from ptrack_analytics import load, presence,
challenges`). PDF and chart generation use matplotlib + fpdf2 in both
contexts.

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
    └── 2026-04-21-algebra.pdf

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
- Pairing codes are one-time and short-lived (configurable TTL, default
  1 hour). A code that is never used expires silently.
- Event log data is kept per the configured retention (default: 180 days).

See `@docs/ETHICS.md` for the consent and retention rationale.
