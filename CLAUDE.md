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
- **Python** — data analysis only: Polars-backed CSV reports and the
  GUI stats JSON. Invoked one-shot by Go (`ptrack_py report`, `stats`);
  never long-running. `report` takes one or more positional Parquet
  paths or glob patterns and produces a per-meeting CSV when exactly
  one file matches or an aggregate CSV when more do. Output always
  goes to stdout; callers redirect to a file when needed. Distributed as a single
  self-contained binary built with **PyInstaller** (`ptrack_py` /
  `ptrack_py.exe`). Users install the Go binary and the Python binary;
  no Python runtime or `uv` required.

  Reports are emitted as CSV only; no PDF generation in v1.

  Auto-generation (ASR + LLM) lives in Go — see "Challenge system".
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
│           ├── challenger/         # in-process auto-generation: audio buffer, ASR/LLM HTTP clients, YAML producer
│           ├── participants/       # cross-platform identity registry + pairing flow
│           ├── eventstore/         # Arrow/Parquet read+write
│           ├── session/            # meeting lifecycle, event dedup/normalization
│           ├── config/             # JSON loading, schema validation, live reload
│           └── gui/                # templ templates + net/http handlers
├── py/src/
│   ├── ptrack_analytics/           # Jupyter library: load + Polars frames (meetings, presence, challenges, questions)
│   └── ptrack_py/                  # binary-only: CLI entry, CSV reports, GUI stats JSON
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
pairing key — and, after the Parquet schema simplification, the
participant identity used end to end. `go/src/internal/participants/`
owns a persistent registry that maps each `display_name` to one Telegram
handle. Registrations are platform-agnostic.

**Registration flow:**

1. Student sends `/start` to the Telegram bot; the bot explains how to
   register.
2. Student sends `/register <display name>` (e.g. `/register John Smith`).
   Each Telegram account holds **at most one** registration at a time —
   sending `/register` again replaces the previous name (the old slot is
   freed first). `/whoami` shows the current registration;
   `/unregister` releases it.
3. The registry stores the `display_name → Telegram handle` binding
   persistently. If that name is already claimed by a different Telegram
   account, the bot rejects the request and tells the student to ask
   their teacher to remove the existing entry via the registry page.
4. When a participant whose display name is registered joins a meeting,
   the bot sends them a private message: "Did you just join [meeting
   title] on [platform]?" with **Yes / No** inline buttons. **Nothing
   is written to Parquet yet** — the `participant_joined` event is
   buffered in memory.
5. Tapping **Yes** flushes the buffered `participant_joined` with its
   original timestamp. Tapping **No** (or leaving the meeting before
   answering) discards the buffer silently — there is no Parquet trace
   of unverified joins. `participant_joined` is therefore implicitly
   "verified": the event log only ever contains verified participants.

**Collision handling.** If a second participant with the same display
name joins while the first is still pre-verification, the name is
*tainted*: the buffered join is dropped, the in-flight DM is edited to
"verification cancelled — name conflict", and every further join under
that name is ignored until every claimant has left the meeting. Once the
name is clear again, the next join is processed normally. After
verification, a colliding second join is silently ignored; the verified
participant continues.

Display name is the only participant identifier — there is no separate
internal ID. The registry's bolt primary key is the normalized display
name, and registry GUI URLs use the URL-encoded display name. The
stats view's display-name rewrite PATCH uses the same URL-encoded form
on its path segment.

Display name matching is case-sensitive and ignores leading/trailing
whitespace. The canonical name stored at registration is what gets
written to every Parquet record, so platform-side whitespace drift does
not pollute cross-meeting reports. A teacher can remove any entry
individually or clear the whole registry from the registry page in the
GUI.

If the Messenger is not initialized (no challenges configured), the bot
is never started and no registration prompts are sent.

Unregistered or unverifiable participants are still shown in the live
GUI status view so the teacher can ask them to register, but no Parquet
events are written for them.

Teachers can rename a participant after the fact by rewriting the
`display_name` column in one or more Parquet files via the stats view
(`PATCH /participants/{display_name}/display-name?file=<a>[&file=<b>…]&new=<name>`).
The rename is scoped to the files explicitly listed in the query and
never propagates to future meetings.

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
ptrack poll [--auto-submitted] [--port=<port>] [--wait] <path-to-bank.yaml>
```

which is a thin HTTP client to the running `ptrack serve` / `ptrack track`
daemon (see "Control plane" in `@docs/ARCHITECTURE.md`). The GUI's
**Trigger poll** menu and the user's terminal both dispatch through
this endpoint. The in-process auto-generator, when `auto_submit` is on,
calls the same pipeline directly (no CLI, no HTTP).

`--auto-submitted` is a boolean marker stored on every
`challenge_issued` event for the round. It is set only when the bank
reaches the pipeline without teacher review:

- Teacher's own bank file (`ptrack poll <bank>`) — flag unset.
- Auto-generated YAML the teacher reviewed and dispatched from the
  GUI — flag unset.
- Auto-generated YAML dispatched automatically by the in-process
  challenger when `auto_submit` is on — flag set.

The flag never appears inside the YAML — the YAML stays a clean,
producer-agnostic bank format.

Three result states are recorded per challenge:

- `correct` — right answer submitted within `answer_window` (default 30s)
- `incorrect` — wrong answer submitted within `answer_window`
- `unanswered` — no answer by `answer_window`; message edited or deleted

`answer_window` is the single deadline. Any answer arriving after it is
ignored.

**Questions are stored as JSON Lines files** — one `.jsonl` file per
meeting in `<questions_dir>/`, named after the Parquet basename (so
`meetings/<start>-<end>.parquet` pairs with
`questions/<start>-<end>.jsonl`). Each line is a JSON
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

When `auto_submit` is true the generated bank is dispatched in-process
and never touches disk. When `auto_submit` is false the bank is written
to a user-configurable review directory
(`challenges.auto_generation.review_dir`, defaults under
`~/Documents/ptrack/pending-banks/`); older auto-generated files in that
directory are swept on each new write so only the latest pending bank
exists. Audio for the generator is captured by the **browser** through
`navigator.mediaDevices.getUserMedia` (with a mute toggle and the
browser's native device picker) and streamed to the daemon over a
WebSocket — mobile-friendly, including Android-on-Termux. The
`internal/challenger/` package consumes frames in-process, batches them
into short segments, and calls an **OpenAI-compatible** ASR endpoint
(e.g. a local LocalAI daemon, the OpenAI Whisper API, or any compatible
gateway — ptrack ships no backend default and the teacher must configure
one). The same compat shape is used for the LLM chat-completions call
that produces the YAML. ptrack owns no model weights; the chosen
backend handles all warm-up, GPU memory, and unloading.

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

Main views:

1. **Live status view** — shown during an active meeting. Displays system
   information: warnings, errors, delivery diagnostics, scheduler events.
   No participant timeline is rendered live. Hosts the **Trigger poll**
   menu (Custom / Auto-generated), the **Audio** card with the browser's
   microphone toggle and device picker, and the **Shut down** lifecycle
   button.
2. **Stats view** — a single page served at `GET /stats?file=<a>&file=<b>…`.
   With one `file` query value it shows the per-meeting timeband list
   (one row per participant in that file). With more than one it
   switches to a cross-meeting container that shows one participant per
   page, with prev/next paging and a search bar to jump to a participant
   by display name. The Parquet → stats transformation is done in
   Python (Polars) and emitted as JSON; Go reads the JSON, caches it on
   disk, and renders templ.
3. **Config editor** — schema-driven form for the JSON config, with
   validation and live reload.

The stats view is the only statistics surface the GUI offers; there is
no in-GUI custom-analysis panel. Anything beyond it — ad-hoc
aggregations, charts, cross-cohort comparisons — happens in a Jupyter
notebook against `ptrack_analytics` (see `@docs/QUERIES.md`). Pushing
stats computation into Python keeps the Polars code as the single
source of truth for both GUI and notebook work.

Closing the browser tab does not stop the daemon — the **Shut down**
button is the only graceful exit path. After it runs, the GUI replaces
itself with a "ptrack has stopped — you can close this tab" screen.

See `@docs/GUI.md` for chart spec, marker encoding, and route map.

## Ad-hoc queries

The `ptrack_analytics` library (in `py/src/ptrack_analytics/`) is the
Jupyter-facing surface. It exposes `load()` and the pre-derived Polars
lazy frames (`meetings`, `presence`, `challenges`, `questions`) — and
nothing else; CSV reports and the GUI stats JSON live in `ptrack_py/`
and are not part of the library's public API. All frames use
notebook-friendly types (`Datetime`, `Duration`, struct columns) — the
raw event log is intentionally not exposed.

```python
from ptrack_analytics import load, presence, challenges
import polars as pl

load("~/Documents/ptrack/meetings/spring-2026-*")
presence.group_by("display_name").agg(pl.col("duration").sum())
```

Typical workflow: load the desired Parquet files, import the library, ask
an AI assistant to generate the desired charts or statistics. The library
API is documented in `@docs/QUERIES.md`.

## Cross-language contract

Go and Python never share a process. They communicate via:

1. **Parquet files** for event data — schema in `@docs/EVENT_SCHEMA.md`.
2. **One-shot subprocess invocation** for analytics — Go invokes
   `ptrack_py report <paths…>` to obtain CSV output on stdout (single
   matched file → per-meeting; multiple → aggregate), and
   `ptrack_py stats <paths…>` to obtain the GUI stats JSON on stdout.
   No Python process is kept alive in
   either case. The stats JSON is cached by Go alongside the input
   files; cache entries are invalidated when any input's mtime advances
   (e.g. after the display-name rewrite PATCH).

That is the entire Go ↔ Python surface. Auto-generation (ASR + LLM) is
an in-process Go feature that talks to external OpenAI-compatible
backends over HTTP — Python is not involved.

When the event schema changes, update `go/src/internal/eventstore/` (Go
Arrow schema), `py/src/ptrack_analytics/schema.py`, and
`@docs/EVENT_SCHEMA.md`. All three must match. Prefer adding optional
columns over changing existing ones.

## Go conventions

See `@.claude/rules/go-style.md` for details when writing Go.

Summary: `internal/` for everything; `slog` for logs; errors wrapped with
`%w`; `context.Context` first-arg everywhere I/O happens; table-driven
tests; no DI frameworks; no `doc.go` or package comments; comment only
the genuinely non-obvious (say why, not what).

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
| Run a fixture end-to-end        | `./bin/ptrack track --provider=mock --meeting=test/fixtures/lesson1.jsonl` |
| Track without GUI (headless)    | `./bin/ptrack track --provider=bbb --meeting=<id>`                       |
| Start GUI (connect via browser) | `./bin/ptrack serve --port=8080` — use the Connect form on the dashboard |
| Trigger a poll (any producer)   | `./bin/ptrack poll path/to/bank.yaml`                                    |
| Trigger a poll, wait for result | `./bin/ptrack poll --wait path/to/bank.yaml`                             |
| Reload config in running daemon | `./bin/ptrack reload`                                                    |
| Export CSV report for a meeting | `./bin/ptrack report meeting.parquet > report.csv`                       |
| Export cross-meeting CSV report | `./bin/ptrack report 'meetings/*.parquet' > semester.csv`                |
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
  `ptrack serve` (handler lives in `cmd/ptrack/main.go`), `ptrack track` /
  `ptrack serve` / `ptrack poll` / `ptrack report` CLI commands (the
  report path shells out to `ptrack_py report` directly via
  `internal/ptrackpy`). Telegram messenger and registry use the
  display-name flow (`/register`, `/unregister`, `/whoami`); the session
  coordinator buffers joins until verification, applies the collision
  rule, and writes only verified participants to Parquet.
- Python: `ptrack_analytics` library with schema, `load()`, and the
  derived lazy frames (`presence`, `challenge_results`). The binary-only
  `ptrack_py` package holds the typer CLI plus the formats Go shells out
  for: CSV reports (`reports.py`) and the GUI stats JSON (`stats.py`),
  exposed as `ptrack_py report <paths…>` (per-meeting when one file
  matches, aggregate with more) and `ptrack_py stats <paths…>`.
- GUI (`ptrack serve`) and `internal/gui/` package: HTTP server with
  all routes from the older `docs/GUI.md`, in-process session
  management, templ + htmx templates (dashboard, live status, meeting
  analysis with SVG timeline, cross-meeting participant view, config
  editor), CSS in `views/assets/`, English/Ukrainian i18n via the
  shared `internal/i18n` package and `gui/locales/*.json`, with a
  cookie-based locale selector. Telegram bot messages are translated
  through the same resolver, using shared keys in
  `internal/messengers/locales/*.json` plus Telegram-specific overrides
  in `internal/messengers/telegram/locales/*.json`; language follows
  the user's Telegram `language_code`. Parquet
  reader (`eventstore.ReadAll`) and display-name rewrite
  (`eventstore.UpdateDisplayName`) also implemented. `cmd/ptrack`
  builds the mux and mounts the shared `POST /poll` handler alongside
  GUI routes — there is one implementation of the poll endpoint,
  shared between `ptrack serve` and `ptrack track`.

**Pending against the current design (not yet in code):**

- Replace the GUI's single Trigger Poll button with the Custom /
  Auto-generated menu; add the Audio card and the Shut down control.
- Browser-side audio capture via `getUserMedia` and a WebSocket to the
  daemon.
- `internal/challenger/` package: rolling audio buffer, ASR client
  (OpenAI-compatible `/v1/audio/transcriptions`), transcript window,
  LLM client (OpenAI-compatible `/v1/chat/completions`), YAML producer,
  in-process dispatch into `challenges.Pipeline.RunPoll` when
  `auto_submit` is true.
- GUI stats backend rewrite — `ptrack_py stats` subcommand, on-disk
  JSON cache, unified `/stats?file=…` route in Go, paged
  one-participant-per-page cross-meeting container. Replaces the
  current `internal/gui/timeline.go` Go-side computation and the
  separate `/meetings/{id}` and `/participants/{display_name}` routes.

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
  Shut down control
- CSV reports (single meeting and cross-meeting)
- Polars analytics via `ptrack_analytics` library and CLI

**v1 stretch:**

- Auto-generated challenges via in-process `internal/challenger/`,
  using OpenAI-compatible ASR + LLM endpoints (e.g. a local LocalAI
  daemon, OpenAI, or any compatible gateway; no backend ships as a
  default). Both `auto_submit` modes supported.
- Browser-side audio capture via `getUserMedia` + WebSocket to the
  daemon (prerequisite for the above)
- Optional: screen-share OCR for cross-modal challenges

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
