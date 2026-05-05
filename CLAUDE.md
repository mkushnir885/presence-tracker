# Presence Tracker

Extension for video-conferencing platforms that tracks student presence
during online lessons. Zoom, Google Meet, and BigBlueButton are supported
behind a common `Provider` interface. Non-intrusive presence challenges
are delivered to participants privately via a messenger bot (Telegram
first, abstracted so others can be added). Outputs per-meeting and
cross-meeting statistics via CLI and a local web GUI, plus PDF reports.

This is a university diploma project. Clarity and defensible design
choices matter as much as shipping features.

## Tech stack

- **Go** — main binary, CLI (cobra), HTTP server, provider adapters,
  messenger adapters, challenge scheduler, Parquet event log (via
  `github.com/apache/arrow/go/v17`), orchestration.
- **Python** — data analysis (Polars), CSV report generation, and
  (v1 stretch / v2) AI-generated question pipeline: ASR, small-LLM
  question generation. Distributed as a single self-contained binary
  built with **PyInstaller** (`ptrack_py` / `ptrack_py.exe`). Users
  install the Go binary and the Python binary; no Python runtime or
  `uv` required.
- **templ + htmx** — server-rendered GUI with minimal client-side JS.
  Supports dark/light/system color themes and English/Ukrainian UI
  languages (easily extended by adding translation files). Opened in
  the system browser — no native desktop wrapper.
- **YAML** — config file format, validated against a JSON Schema that
  also drives the web-based config editor's form layout.
- **Parquet** — canonical data exchange format between Go and Python.
  Events are schema-defined once in `@docs/EVENT_SCHEMA.md` and read
  from both sides.

## Repository layout

```
presence-tracker/
├── go/
│   ├── go.mod
│   ├── go.sum
│   └── src/
│       ├── cmd/ptrack/             # single CLI binary: serve, track, report
│       └── internal/
│           ├── providers/          # video-conferencing adapters (Zoom, Meet, BBB)
│           ├── messengers/         # messenger adapters (Telegram first)
│           ├── challenges/         # Challenge interface, scheduler, built-in types
│           │   ├── filebased/      # teacher-prepared question files
│           │   └── aigenerated/    # AI-generated questions (delegates to Python)
│           ├── participants/       # cross-platform identity registry + pairing flow
│           ├── eventstore/         # Arrow/Parquet read+write
│           ├── session/            # meeting lifecycle, event dedup/normalization
│           ├── config/             # YAML loading, schema validation, live reload
│           ├── gui/                # templ templates + net/http handlers
│           └── reporter/           # invokes ptrack_py binary for PDF/chart output
├── py/src/
│   ├── ptrack_analytics/           # library: Polars analysis + PDF/chart generation
│   │   └── (Jupyter-compatible; also the PyInstaller entry point)
│   ├── challenger/                 # question generation from meeting context (v1 stretch)
│   └── perception/                 # (v2) ASR (Whisper), OCR
├── test/fixtures/                  # recorded event streams for replay
└── docs/                           # reference docs, loaded on demand via @docs/...
```

## The four abstractions

Four parallel, small interfaces define the extension points. Each has its
own directory under `internal/` and follows the same pattern: interface +
adapter sub-packages + tests against a mock implementation.

| Abstraction | What varies                   | Built-in impls                  |
| ----------- | ----------------------------- | ------------------------------- |
| `Provider`  | video-conferencing platform   | `zoom`, `meet`, `bbb`, `mock`   |
| `Messenger` | message-delivery channel      | `telegram`, `mock`              |
| `Challenge` | question kind + scoring logic | `filebased`, `aigenerated`      |
| `EventSink` | where events are written      | `parquet` (default); extensible |

When adding a new implementation of any of these: add a subdirectory,
register it in `go/src/cmd/ptrack/main.go`, add a fixture under `test/fixtures/`,
and document quirks in the relevant doc (`@docs/ARCHITECTURE.md` for the
interfaces, `@docs/CHALLENGES.md` for challenge types).

See `@docs/ARCHITECTURE.md` for interface signatures and rationale.

## Participant identity

A student appears with different identifiers on each platform (email on
Zoom, email-or-name on Meet, numeric user ID on Telegram). `go/src/internal/
participants/` owns a persistent registry keyed by a stable internal
`ParticipantID`.

**Registration flow — meeting-time pairing:**

1. Student sends `/start` to the Telegram bot. No data required upfront;
   the bot replies with a short one-time pairing code: `PTRACK:A3F9`.
2. The student types that code once in the meeting chat.
3. The provider adapter detects the code in chat and records which
   platform identifier (email, display name) posted it. The participant
   registry binds that platform identifier to the Telegram user who
   received the code.
4. The student is now registered for all future meetings on that platform.

This binds the Telegram identity to the meeting identity without a
teacher-maintained roster. The binding is strong: the attacker must both
control the Telegram account that received the code and be present in the
meeting to post it.

If a participant joins a meeting but is not yet paired, challenges are
skipped and a `participant_unregistered` event is logged. The teacher
sees this in the GUI and can remind the student to complete pairing.

Teachers can rename a participant by rewriting the `display_name`
column directly in the relevant Parquet file(s). All renames are
file-scoped — they never propagate to future meetings automatically:

- **Single-file rename** (`PATCH /meetings/{id}/participants/{p}/display-name`):
  rewrites `display_name` for that participant in one Parquet file.
- **Multi-file rename** (from the cross-meeting view): applies the same
  new name to all Parquet files currently shown in that view. The
  teacher selects the scope explicitly; files outside the current view
  are never touched.

Future meetings always record whatever name the platform provides. A
rename never sets a persistent override for meetings that have not yet
happened.

If a participant's display name changed mid-meeting (the platform or the
user sent different names), the Parquet file may contain several distinct
`display_name` values for the same `participant_id`. The GUI detects
this, shows all variants as **Name1 | Name2 | Name3**, and hints the
teacher to pick a canonical name with the single-file rename button.

## Challenge system

Challenges are generated, delivered, timed, and scored entirely outside
the meeting. The teacher is never interrupted by the challenge flow.

The MVP implements **file-based challenges** end-to-end. AI-generated
challenges reuse the same delivery/scoring pipeline and only add a new
generator.

Three result states are recorded per challenge:

- `correct` — right answer submitted within `answer_window` (default 30s)
- `incorrect` — wrong answer submitted within `answer_window`
- `unanswered` — no answer by `answer_window`; message edited or deleted

`answer_window` is the single deadline. Any answer arriving after it is
ignored.

**Questions are stored as JSON Lines files** — one `.jsonl` file per
meeting in `<questions_dir>/`, named by meeting ID. Each line is a JSON
object with the full question content (prompt, type, choices, correct
answer, etc.) and a UUIDv4 `question_id`. `challenge_issued` events in
the Parquet reference that UUID. `ptrack_analytics.load()` automatically
discovers the matching `.jsonl` file and exposes a `questions` lazy frame
for Jupyter analysis. Polars loads `.jsonl` natively with `read_ndjson`.

Question bank files (teacher-prepared YAML) are not stored by the system.
When a poll runs, questions are written to the meeting's `.jsonl` file;
the original bank YAML is the teacher's responsibility to keep.

Any number of challenge types may be enabled simultaneously, including
zero (tracking-only mode). Enabling zero challenges is valid.

See `@docs/CHALLENGES.md` for the interface, file format, AI pipeline,
and design rationale.

## Configuration

Single YAML config file, validated against a JSON Schema. The schema is
the source of truth for both runtime validation and the web-based config
editor's form layout. All tunables live here: platform credentials,
messenger credentials, ASR/LLM model choices, challenge schedules, answer
windows, retention policy, GUI port.

See `@docs/CONFIG.md` for the full schema, defaults, and example configs.

## GUI

The GUI is a local web app served by `ptrack serve`, opened in the
system browser. No native desktop wrapper required. Supports dark/light/
system color themes and English/Ukrainian UI languages (extensible by
adding a translation file).

Three main views:

1. **Live status view** — shown during an active meeting. Displays system
   information: warnings, errors, delivery diagnostics, scheduler events.
   No participant timeline is rendered live.
2. **Meeting analysis view** — timeline chart per participant from a
   saved meeting file. Time on X-axis, presence and challenge bands
   horizontally. Hovering a challenge marker fetches and displays the
   question text.
3. **Config editor** — schema-driven form for the YAML config, with
   validation and live reload.

For advanced data analysis beyond the built-in charts, use Jupyter
Notebooks with `ptrack_analytics` directly.

See `@docs/GUI.md` for chart spec, marker encoding, and route map.

## Ad-hoc queries

The `ptrack_analytics` library (in `py/src/ptrack_analytics/`) provides
the full analysis and PDF generation API. Advanced users import it in a
**Jupyter Notebook** for arbitrary exploration:

```python
from ptrack_analytics import load, presence, challenges, generate_pdf

load("~/Documents/ptrack/meetings/spring-2026-*.parquet")
presence.group_by("participant_id").agg(pl.col("presence_seconds").mean())
```

Typical workflow: load the desired Parquet files, import the library, ask
an AI assistant to generate the desired charts or statistics. The library
API is documented in `@docs/QUERIES.md`.

## Cross-language contract

Go and Python never share a process. They communicate via:

1. **Parquet files** for event data — schema in `@docs/EVENT_SCHEMA.md`.
2. **Subprocess invocation** — Go invokes the Python binary as
   `ptrack_py report ...` or `ptrack_py generate ...`. The binary is
   the PyInstaller-built `ptrack_py` / `ptrack_py.exe`, located next to
   the Go binary or in PATH.
3. **Localhost HTTP on a short-lived port** for the AI-generated challenge
   pipeline, where Go pushes transcript chunks and requests questions;
   the Python side owns the ASR + LLM process so models stay warm.

When the event schema changes, update `go/src/internal/eventstore/` (Go Arrow
schema), `py/src/ptrack_analytics/schema.py`, and `@docs/EVENT_SCHEMA.md`.
All three must match. Prefer adding optional columns over changing
existing ones.

## Go conventions

See `@.claude/rules/go-style.md` for details when writing Go.

Summary: `internal/` for everything; `slog` for logs; errors wrapped with
`%w`; `context.Context` first-arg everywhere I/O happens; table-driven
tests; no DI frameworks; comments only where logic is non-obvious.

## Python conventions

See `@.claude/rules/python-style.md` for details when writing Python.

Summary: Python 3.12+; `uv` for dev/test; Polars lazy API by default;
`ty check`; `typer` for CLIs; `ruff` for lint+format.
For releases: PyInstaller single-file binary (`ptrack_py`).

## Common commands

| Task                            | Command                                                                  |
| ------------------------------- | ------------------------------------------------------------------------ |
| Build Go binary                 | `cd go && just build` → `./go/bin/ptrack`                                |
| Build Python binary             | `cd py && just build` → `./py/bin/ptrack_py`                             |
| Build both                      | `just build` → `./bin/ptrack` and `./bin/ptrack_py`                      |
| Run Go tests                    | `cd go && just test`                                                     |
| Run Python tests                | `cd py && just test`                                                     |
| Run all tests                   | `just test`                                                              |
| Format                          | `just fmt`                                                               |
| Lint                            | `just lint`                                                              |
| Run a fixture end-to-end        | `./bin/ptrack track --provider=mock --fixture=test/fixtures/bbb/lesson1` |
| Track without GUI (headless)    | `./bin/ptrack track --provider=bbb --meeting=<id>`                       |
| Start GUI (connect via browser) | `./bin/ptrack serve --port=8080` — use the Connect form on the dashboard |
| Export CSV report for a meeting | `./bin/ptrack report --in meeting.parquet --out report.csv`              |
| Export cross-meeting CSV report | `./bin/ptrack report --in 'meetings/*.parquet' --out semester.csv`       |
| Ad-hoc analysis (Jupyter)       | `cd py && jupyter notebook` — import `ptrack_analytics`, call `load()`   |

## Current status

**Core implementation complete.** The following are implemented and compile
cleanly:

- Go: config loader, BBB provider (webhook), Meet provider (polling via
  REST API v2), Zoom provider (webhook, HMAC-validated), mock provider (fixture
  replay), shared OAuth 2.0 PKCE helper (`internal/providers/oauth/`), Telegram
  messenger, mock messenger, file-based challenge type + poller, BoltDB participant
  registry, Arrow/Parquet event store, session coordinator, `ptrack track` and
  `ptrack report` CLI commands, `internal/reporter/` package (subprocess invocation
  of `ptrack_py`, CSV parsing for GUI use).
  Note: Google Meet REST API does not expose chat messages, so pairing codes cannot
  be detected from Meet; participants must be pre-registered.
- Python: `ptrack_analytics` library with schema, `load()`, derived frames
  (`presence`, `challenge_results`), CSV report generation (`generate_csv`,
  `generate_aggregate_csv` in `reports.py`), and `ptrack_py report` /
  `ptrack_py aggregate` CLI commands.
- GUI (`ptrack serve`) and `internal/gui/` package: HTTP server with all
  routes from `docs/GUI.md`, in-process session management, templ + htmx
  templates (dashboard, live status, meeting analysis with SVG timeline,
  cross-meeting participant view, config editor), CSS in `views/assets/`,
  English/Ukrainian i18n via `gui/locales/*.json` and a cookie-based locale
  selector. Parquet reader (`eventstore.ReadAll`) and display-name rewrite
  (`eventstore.UpdateDisplayName`) also implemented.

**Not yet implemented (TODO stubs in code):**

- AI-generated challenges (`challenges/aigenerated/`, `py/src/challenger/`).
- Named GUI analyses (`py/src/ptrack_analytics/analyses.py`).

When adding code, confirm the module layout in this file and
`@docs/ARCHITECTURE.md` are still current, and prefer updating docs first
when a design decision differs from what is documented.

## Staging

**v1 core (must-ship):**

- Provider adapters: BBB first, then Meet, then Zoom
- Messenger: Telegram
- Challenges: file-based
- GUI: live meeting view + multi-meeting aggregate + config editor
- CSV reports (single meeting and cross-meeting)
- Polars analytics via `ptrack_analytics` library and CLI

**v1 stretch:**

- AI-generated challenges using ASR (Whisper) + small LLM (Qwen/Gemma/Phi)
- Optional: screen-share OCR for cross-modal challenges
- Optional: local-only mode with Ollama / llama.cpp

**v2 / future work:**

- Additional messengers (Discord, Signal)
- Real-time "nudge" alerts to the teacher when a pattern emerges
  (e.g., three consecutive unanswered challenges from the same student)

## Non-goals

- No face **recognition**. The project intentionally replaces face-based
  presence detection with messenger challenges.
- No cheat-proof challenges. The goal is to make cheating at least as
  much work as attending. This is stated explicitly in the thesis.
- No cloud deployment. Runs locally on the instructor's machine.
- No mobile client.
- No attendance-grading decisions made by the tool. It produces a
  presence _record_; grading is the teacher's decision.
- No user-created query expressions in the GUI. Advanced analysis uses
  Jupyter + `ptrack_analytics` directly.

## Ethics

This tool processes attendance data about real students and delivers
private messages to them via a bot. See `@docs/ETHICS.md` for the consent
flow, data retention limits, and features that require explicit teacher
acknowledgement before being enabled.

Whenever touching features that collect, store, or export participant
data: default to the least invasive option, never persist data longer
than the configured retention, and read `@docs/ETHICS.md` before
designing new data flows.
