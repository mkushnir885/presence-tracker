# Presence Tracker

Extension for video-conferencing platforms that tracks student presence
during online lessons. Zoom, Google Meet, and BigBlueButton are supported
behind a common `Provider` interface. Non-intrusive presence challenges
are delivered to participants privately via a messenger bot (Telegram
first, abstracted so others can be added). Outputs per-meeting and
cross-meeting statistics via CLI and a local web GUI, plus CSV reports.

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

  Reports are emitted as CSV only; no PDF generation in v1.
- **templ + htmx** — server-rendered GUI with minimal client-side JS.
  Supports dark/light/system color themes and English/Ukrainian UI
  languages (easily extended by adding translation files). Opened in
  the system browser — no native desktop wrapper.
- **JSON** — config file format (single `config.json`, secrets inline,
  written with mode `0600`), validated against a JSON Schema that also
  drives the web-based config editor's form layout. Banks stay YAML.
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
│       ├── cmd/ptrack/             # single CLI binary: serve, track, poll, report
│       └── internal/
│           ├── providers/          # video-conferencing adapters (Zoom, Meet, BBB)
│           ├── messengers/         # messenger adapters (Telegram first)
│           ├── challenges/         # single YAML pipeline: parse, validate, fan out, score
│           ├── challenger/         # supervises the optional ptrack_py challenger subprocess
│           ├── participants/       # cross-platform identity registry + pairing flow
│           ├── eventstore/         # Arrow/Parquet read+write
│           ├── session/            # meeting lifecycle, event dedup/normalization
│           ├── config/             # JSON loading, schema validation, live reload
│           ├── gui/                # templ templates + net/http handlers
│           └── reporter/           # invokes ptrack_py binary for CSV output
├── py/src/
│   ├── ptrack_analytics/           # library: Polars analysis + CSV report generation
│   │   └── (Jupyter-compatible; also the PyInstaller entry point)
│   ├── challenger/                 # question generation from meeting context (v1 stretch)
│   └── perception/                 # (v2) ASR (Whisper), OCR
├── test/fixtures/                  # recorded event streams for replay
└── docs/                           # reference docs, loaded on demand via @docs/...
```

## The three abstractions

Three parallel, small interfaces define the extension points. Each has
its own directory under `internal/` and follows the same pattern:
interface + adapter sub-packages + tests against a mock implementation.

| Abstraction | What varies                   | Built-in impls                  |
| ----------- | ----------------------------- | ------------------------------- |
| `Provider`  | video-conferencing platform   | `zoom`, `meet`, `bbb`, `mock`   |
| `Messenger` | message-delivery channel      | `telegram`, `mock`              |
| `EventSink` | where events are written      | `parquet` (default); extensible |

The challenge layer is intentionally **not** an abstraction: there is one
pipeline, one YAML bank format, one scorer. The variability that used to
live behind a `Challenge` interface ("how do we get the questions?") was
moved outside the system entirely — see "Challenge system" below.

When adding a new implementation of any abstraction: add a subdirectory,
register it in `go/src/cmd/ptrack/main.go`, add a fixture under
`test/fixtures/`, and document quirks in `@docs/ARCHITECTURE.md`.

See `@docs/ARCHITECTURE.md` for interface signatures and rationale.

## Participant identity

A student's display name on the video-conferencing platform is the
pairing key. `go/src/internal/participants/` owns a persistent registry
that maps `(platform, display_name)` to a stable internal `ParticipantID`
and Telegram handle.

**Registration flow — display-name pairing:**

1. Student sends `/start` to the Telegram bot; the bot explains how to
   register and lists the supported platforms.
2. Student sends `/register <platform> <display name>` (e.g.
   `/register zoom John Smith`), or `/register all <display name>` to
   register for all platforms simultaneously.
3. The registry stores the `(platform, display_name)` → Telegram handle
   binding persistently. If that `(platform, display_name)` is already
   claimed by a different Telegram account, the bot rejects the request:
   "Name already registered — ask your teacher to remove the existing
   entry via the registry page, then try again." The same student may
   re-send `/register` to overwrite their own previous entry.
4. When a participant with a matching display name joins a meeting, the
   bot sends them a private message: "Did you just join [meeting title]
   on [platform]?" with **Yes / No** inline buttons.
5. Tapping **Yes** verifies the participant for that meeting session —
   challenges will be sent. Tapping **No** (or never responding) leaves
   them unverified and challenges are skipped for that session.

Registration is persistent across meetings. The per-meeting **Yes/No**
tap is the only action required during a meeting; it is comparable in
effort to answering a challenge.

Display name matching is case-insensitive and ignores leading/trailing
whitespace. A student can update their registration by sending `/register`
again; a teacher can remove any entry individually or clear the whole
registry from the registry page in the GUI.

If the Messenger is not initialized (no challenges configured), the bot
is never started and no registration prompts are sent.

If a participant joins without a matching registry entry, challenges are
skipped and a `participant_unregistered` event is logged. The teacher
sees unregistered participants in the GUI and can ask students to run
`/register`.

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

Challenges are delivered, timed, and scored entirely outside the meeting.
The teacher is never interrupted by the challenge flow.

There is exactly **one** challenge pipeline. The input to it is a YAML
"question bank" file; the output is a poll round: each eligible
participant gets one randomly assigned question over the messenger,
answers are scored within `answer_window`, and events are written to
the Parquet log. The pipeline does not care where the YAML came from.

Sending a poll always goes through the single CLI subcommand

```
ptrack poll [--type=<label>] [--port=<port>] [--wait] <path-to-bank.yaml>
```

which is a thin HTTP client to the running `ptrack serve` / `ptrack track`
daemon (see "Control plane" in `@docs/ARCHITECTURE.md`). The GUI's
**Trigger poll** menu, the user's terminal, and the Python challenger
when auto-submit is on all dispatch through this same endpoint.

`--type` is a free-form label that is stored verbatim on every
`challenge_issued` event for the round. The system does not constrain
its values. Conventions used by the built-in producers:

- `custom` — teacher's own bank file. Default for bare `ptrack poll`.
- `combined` — auto-generated YAML that the teacher reviewed/edited and
  dispatched manually from the GUI.
- `aigenerated` — auto-generated YAML dispatched automatically by the
  Python challenger.

`--type` never appears inside the YAML — the YAML stays a clean,
producer-agnostic bank format.

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
discovers the matching `.jsonl` file and exposes a `questions` lazy
frame for Jupyter analysis. Polars loads `.jsonl` natively with
`read_ndjson`.

Question bank files (teacher-prepared YAML) are not stored by the
system. When a poll runs, questions are written to the meeting's
`.jsonl` file; the original bank YAML is the teacher's responsibility
to keep.

Auto-generated banks land in a short-lived pending directory
(`/tmp/ptrack/` on Linux, `%TEMP%\ptrack\` on Windows) and are removed
once dispatched or once a newer file replaces them. Audio for the
generator is captured by the **browser** through
`navigator.mediaDevices.getUserMedia` (with a mute toggle and the
browser's native device picker) and streamed to the daemon over a
WebSocket — mobile-friendly, including Android-on-Termux.

Polls are optional. A session with no polls at all is valid
(tracking-only mode).

See `@docs/CHALLENGES.md` for the YAML format, the auto-generation
pipeline, the model warm-start lifecycle, and design rationale.

## Configuration

Single JSON config file (`config.json`), validated against a JSON
Schema. The schema is the source of truth for both runtime validation
and the web-based config editor's form layout. Secrets (bot tokens,
OAuth credentials, BBB shared secret) live inline in the same file,
which is written with mode `0600` on Unix. All other tunables live
here too: platform credentials, ASR/LLM model choices, challenge
schedules, answer windows, retention policy, GUI port.

The runtime holds an atomic snapshot of resolved values; readers call
`cfg.Get()` per use (per poll tick, per meeting start) so a `ptrack
reload` or GUI save takes effect on natural boundaries without
restart. Saves prune default-equal fields and rewrite the file
canonically with a `$schema` reference to a sibling
`config.schema.json` that editors auto-discover.

See `@docs/CONFIG.md` for the full schema, defaults, save/reload
semantics, and example configs.

## GUI

The GUI is a local web app served by `ptrack serve`, opened in the
system browser. No native desktop wrapper required. Supports dark/light/
system color themes and English/Ukrainian UI languages (extensible by
adding a translation file).

Three main views:

1. **Live status view** — shown during an active meeting. Displays system
   information: warnings, errors, delivery diagnostics, scheduler events.
   No participant timeline is rendered live. Hosts the **Trigger poll**
   menu (Custom / Auto-generated), the **Audio** card with the browser's
   microphone toggle and device picker, and the **Free models** and
   **Shut down** lifecycle buttons.
2. **Meeting analysis view** — timeline chart per participant from a
   saved meeting file. Time on X-axis, presence and challenge bands
   horizontally. Hovering a challenge marker fetches and displays the
   question text.
3. **Config editor** — schema-driven form for the YAML config, with
   validation and live reload.

Closing the browser tab does not stop the daemon — the **Shut down**
button is the only graceful exit path. After it runs, the GUI replaces
itself with a "ptrack has stopped — you can close this tab" screen.

For advanced data analysis beyond the built-in charts, use Jupyter
Notebooks with `ptrack_analytics` directly.

See `@docs/GUI.md` for chart spec, marker encoding, and route map.

## Ad-hoc queries

The `ptrack_analytics` library (in `py/src/ptrack_analytics/`) provides
the full analysis and CSV report API. Advanced users import it in a
**Jupyter Notebook** for arbitrary exploration:

```python
from ptrack_analytics import load, presence, challenges, generate_csv

load("~/Documents/ptrack/meetings/spring-2026-*.parquet")
presence.group_by("participant_id").agg(pl.col("presence_seconds").mean())
```

Typical workflow: load the desired Parquet files, import the library, ask
an AI assistant to generate the desired charts or statistics. The library
API is documented in `@docs/QUERIES.md`.

## Cross-language contract

Go and Python never share a process. They communicate via:

1. **Parquet files** for event data — schema in `@docs/EVENT_SCHEMA.md`.
2. **One-shot subprocess invocation** for analytics — Go invokes
   `ptrack_py report ...` / `ptrack_py aggregate ...` to obtain CSV
   output on stdout, no Python process kept alive.
3. **Long-running subprocess + YAML files + CLI re-entry** for the
   optional auto-generated challenges. Go spawns `ptrack_py challenger
   run` once per session; the Python side writes generated YAML banks
   to the pending directory, and (when `auto_submit` is on) re-enters
   the system through `ptrack poll --type=aigenerated <path>` like any
   other producer.
4. **Localhost HTTP** for the audio path only: the browser's WebSocket
   stream lands on the Go daemon and is relayed to the challenger over
   a small `POST /context/audio` endpoint (or its stdin), so the ASR
   process can ingest it. No `POST /challenges/generate` exists — Go
   never asks Python for a batch.

When the event schema changes, update `go/src/internal/eventstore/` (Go
Arrow schema), `py/src/ptrack_analytics/schema.py`, and
`@docs/EVENT_SCHEMA.md`. All three must match. Prefer adding optional
columns over changing existing ones.

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
| Trigger a poll (any producer)   | `./bin/ptrack poll --type=custom path/to/bank.yaml`                      |
| Trigger a poll, wait for result | `./bin/ptrack poll --wait path/to/bank.yaml`                             |
| Reload config in running daemon | `./bin/ptrack reload`                                                    |
| Export CSV report for a meeting | `./bin/ptrack report --in meeting.parquet --out report.csv`              |
| Export cross-meeting CSV report | `./bin/ptrack report --in 'meetings/*.parquet' --out semester.csv`       |
| Ad-hoc analysis (Jupyter)       | `cd py && jupyter notebook` — import `ptrack_analytics`, call `load()`   |

## Current status

**Core implementation, MVP-era.** The challenge pipeline, control plane,
and `ptrack poll` CLI now match the current design. Auto-generation,
audio capture, and the BBB/Zoom polling rewrite are still pending.

- Go: config loader, BBB provider (polling `getMeetingInfo`), Meet
  provider (polling via REST API v2), Zoom provider (polling the
  Dashboard API), mock provider (fixture replay), shared OAuth 2.0 PKCE
  helper (`internal/providers/oauth/`), Telegram messenger, mock
  messenger, single `internal/challenges/` pipeline (load, score,
  per-session `Pipeline` with `RunPoll`/`HandleAnswer`), BoltDB
  participant registry, Arrow/Parquet event store, session coordinator,
  `POST /poll` HTTP endpoint mounted by both `ptrack track` and
  `ptrack serve` (handler lives in `cmd/ptrack/main.go`), `PTRACK_PORTS`
  env var that lists every running daemon's port, `ptrack track` /
  `ptrack serve` / `ptrack poll` / `ptrack report` CLI commands,
  `internal/reporter/` package.
  Note: the Telegram messenger and participant registry still implement
  the old pairing-code flow and need to be updated to the display-name
  flow described above.
- Python: `ptrack_analytics` library with schema, `load()`, derived
  frames (`presence`, `challenge_results`), CSV report generation
  (`generate_csv`, `generate_aggregate_csv` in `reports.py`), and
  `ptrack_py report` / `ptrack_py aggregate` CLI commands.
- GUI (`ptrack serve`) and `internal/gui/` package: HTTP server with
  all routes from the older `docs/GUI.md`, in-process session
  management, templ + htmx templates (dashboard, live status, meeting
  analysis with SVG timeline, cross-meeting participant view, config
  editor), CSS in `views/assets/`, English/Ukrainian i18n via
  `gui/locales/*.json` and a cookie-based locale selector. Parquet
  reader (`eventstore.ReadAll`) and display-name rewrite
  (`eventstore.UpdateDisplayName`) also implemented. `cmd/ptrack`
  builds the mux and mounts the shared `POST /poll` handler alongside
  GUI routes — there is one implementation of the poll endpoint,
  shared between `ptrack serve` and `ptrack track`.

**Pending against the current design (not yet in code):**

- Replace the GUI's single Trigger Poll button with the Custom /
  Auto-generated menu; add the Audio card, Free models, and Shut down
  controls.
- Browser-side audio capture via `getUserMedia` and a WebSocket to the
  daemon; relay path Go → Python.
- `internal/challenger/` package to supervise the optional
  `ptrack_py challenger run` subprocess and forward audio.
- Auto-generation pipeline on the Python side: rolling transcript,
  YAML producer, optional `ptrack poll` re-entry.
- Named GUI analyses (`py/src/ptrack_analytics/analyses.py`).

When adding code, confirm the module layout in this file and
`@docs/ARCHITECTURE.md` are still current, and prefer updating docs
first when a design decision differs from what is documented.

## Staging

**v1 core (must-ship):**

- Provider adapters: BBB first, then Meet, then Zoom
- Messenger: Telegram
- Challenges: single YAML pipeline + `ptrack poll` CLI + GUI Trigger
  poll menu (Custom path only — auto-generation is stretch)
- GUI: live meeting view + multi-meeting aggregate + config editor +
  system controls (Free models, Shut down)
- CSV reports (single meeting and cross-meeting)
- Polars analytics via `ptrack_analytics` library and CLI

**v1 stretch:**

- Auto-generated challenges via the optional `ptrack_py challenger`
  subprocess (ASR with Whisper + small LLM Qwen/Gemma/Phi), with both
  `auto_submit` modes wired through `ptrack poll`
- Browser-side audio capture via `getUserMedia` + WebSocket relay to
  the daemon (prerequisite for the above)
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
