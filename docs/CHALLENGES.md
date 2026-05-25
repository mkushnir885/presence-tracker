# Challenge system

Challenges verify that a participant is actually engaged with the lesson,
without cameras and without interrupting the teacher.

## Design principles

1. **The teacher is never interrupted.** Delivery, timing, and scoring
   happen automatically once a poll has been triggered.
2. **Delivery is out-of-band.** Challenges go to the participant's
   messenger (Telegram in v1), not the meeting chat. Individual responses
   are private.
3. **Synchronized delivery.** All eligible participants receive their
   question at the same moment. A student cannot pass their answer to a
   classmate because the classmate is simultaneously busy with their own
   question inside the same `answer_window`.
4. **Graceful degradation.** A missing messenger registration, a failed
   challenge generation, or a network hiccup must not crash the session
   or produce a false "unanswered" mark.
5. **Honest limits.** No challenge is cheat-proof. The goal is to make
   cheating at least as much work as attending.
6. **Zero challenges is valid.** A session may have no polls at all
   (tracking-only mode) and still produce useful presence data.

## One pipeline, many producers

There is exactly **one** challenge pipeline inside the system:

1. A YAML "challenge bank" file is handed to the system.
2. The system validates it, calls `Provider.FetchPresence` for a
   just-in-time snapshot of who is in the meeting right now, picks
   eligible participants from that snapshot, randomly assigns one
   question per participant, appends every issued question to the
   meeting's `.jsonl` file with a fresh UUIDv4, fans out the questions
   simultaneously via the configured messenger, and scores answers as
   they arrive (or marks them `unanswered` when `answer_window` elapses).

The just-in-time presence fetch decouples dispatch accuracy from the
background `Provider` polling cadence: a long polling interval (tens
of seconds) is fine because the moment that matters — choosing the
recipients of this round — is backed by its own fresh API call.

The interesting variability sits **outside** the system: how the YAML
file was produced. Three producer roles are recognized:

| Producer            | Convention `--type=` label | Notes                                                                            |
|---------------------|----------------------------|----------------------------------------------------------------------------------|
| Teacher, by hand    | `custom` (default)         | The teacher prepares a YAML bank in their editor and selects it from the GUI or shell. |
| Auto-generator      | `aigenerated`              | The Python challenger process writes a YAML to the pending directory and immediately invokes `ptrack poll` itself (when `auto_submit` is on). |
| Reviewed auto-gen   | `combined`                 | The Python challenger writes a YAML to the pending directory and stops; the teacher reviews/edits it and triggers the poll via the GUI menu (when `auto_submit` is off). |
| User scripts        | any other label            | Any tool that produces a valid YAML bank can call `ptrack poll --type=<anything> <path>`. The label is free-form; the system stores it verbatim on every `challenge_issued` event. |

The `--type` label is purely a CLI/transport-side tag. It is **never**
written inside the YAML file itself — the YAML stays a clean,
producer-agnostic bank format.

## The poll trigger: `ptrack poll`

```
ptrack poll [--type=<label>] [--port=<port>] [--wait] <path-to-bank.yaml>
```

`ptrack poll` is a **thin client** to the running `ptrack` daemon. It
contains no challenge logic of its own: it resolves the daemon URL,
opens a localhost HTTP connection, and posts the poll request.

- `--type` defaults to `custom`. Any string is accepted; the conventions
  above are recommendations, not constraints.
- `--port` selects a daemon when several `ptrack` processes are running
  in parallel (one meeting per process). When exactly one daemon is
  reachable via `PTRACK_PORTS`, `--port` is optional.
- `--wait` keeps the CLI attached until the poll's `answer_window`
  elapses, then prints `delivered N, correct K, incorrect M, unanswered U`
  to stdout and uses the exit code accordingly. Without `--wait`, the
  CLI exits immediately on the 200 OK that confirms the poll was
  scheduled — useful for fire-and-forget callers like the Python
  generator.

The same code path serves the GUI's **Trigger poll** menu (see
`@docs/GUI.md`). Both invocations end up calling `POST /poll` on the
running daemon's HTTP API.

### Listener port discovery

On startup, `ptrack serve` and `ptrack track` append the port they
bound to onto the `PTRACK_PORTS` environment variable (comma-separated):

- `ptrack serve` — the configured `gui.bind_addr` port (default 8080).
- `ptrack track` (headless) — a random free loopback port.

Every child process the daemon spawns inherits this variable, which is
how the Python challenger and any `ptrack poll` invocation it makes
find their way back to the same daemon — no on-disk descriptor, no
cleanup on shutdown.

When the teacher runs `ptrack poll` directly from a fresh shell
(`PTRACK_PORTS` unset), the CLI falls back to the port from
`config.yaml`, then to `8080`. The CLI's `--server=URL` flag overrides
everything else. When `PTRACK_PORTS` lists more than one port and
`--port` is not supplied, the CLI errors with a helpful message.

### Endpoint shape

```
POST /poll
```

Request body:

```json
{ "type": "custom", "bank_path": "/abs/path/to/bank.yaml" }
```

Response (immediate, 200 OK):

```json
{ "poll_id": "...", "scheduled_count": 8, "skipped_count": 1 }
```

Error codes used by the poll endpoint:

| Code | Meaning                                                       |
|------|---------------------------------------------------------------|
| 404  | `bank_path` does not exist or is unreadable                   |
| 409  | No active session                                             |
| 422  | YAML is invalid (response body lists errors with JSON pointers) |
| 503  | Messenger is currently unavailable                            |

## Pending directory

Auto-generated YAMLs land in a known temp directory:

| OS      | Path                                                       |
|---------|------------------------------------------------------------|
| Linux   | `/tmp/ptrack/`                                             |
| Windows | `%TEMP%\ptrack\`                                           |

Files in this directory are short-lived:

- The GUI's **Trigger poll** menu enables the **Auto-generated** option
  only when at least one YAML is present here. On selection, the menu
  calls `ptrack poll --type=combined <path>`.
- When `auto_submit` is on, the Python challenger calls
  `ptrack poll --type=aigenerated <path>` itself immediately after
  writing the file.
- A YAML in this directory is removed in either of two events:
  1. It has been submitted (whether via GUI or auto-submit).
  2. A new YAML has been generated to replace it (only the most recent
     pending file is ever kept).

The directory is local-only and never contains data older than the
current session.

## Question bank format (YAML)

One file per bank. The bank has no top-level `title` or `id` fields —
the filename is the identifier and question IDs are assigned at issue
time.

```yaml
version: 1
questions:
  - prompt: "Which of the following is a prime number?"
    type: multiple_choice
    choices: ["21", "23", "27", "51"]
    # `answer` is always a list. Single-answer questions have one entry;
    # multi-answer questions have two or more.
    answer: ["23"]
  - prompt: "Which of these are even numbers? Select all that apply."
    type: multiple_choice
    choices: ["2", "3", "4", "5", "6"]
    answer: ["2", "4", "6"]
  - prompt: "What is 7 factorial?"
    type: numeric
    answer: 5040
    tolerance: 0              # allow ±tolerance, default 0
  - prompt: "Name one property of an isosceles triangle."
    type: short_text
    answer: ["two equal sides", "two equal angles"]   # any match counts
    match: substring_ci       # exact | substring_ci | regex
```

Validation rules in `go/src/internal/challenges/validate.go`. Validation
errors are surfaced through the `ptrack poll` exit code and through the
GUI file picker before submission.

### Answer matching

- `multiple_choice` — the student's selected set must equal the `answer`
  set exactly (same elements, order-insensitive). No partial credit.
  - If `len(answer) == 1`, each button is a one-click commit.
  - If `len(answer) > 1`, buttons toggle on/off and a Submit button
    finalizes.
- `numeric` — parse answer as a number; pass if within `±tolerance`.
  Rejects non-numeric input with a hint.
- `short_text` — matched per the `match` mode. Default is
  `substring_ci`.

## Question records (`.jsonl`)

When a poll runs, every issued question is appended to the meeting's
`<meeting_id>.jsonl` file in `questions_dir`. Each line is one JSON
object with the full question content plus a UUIDv4 `question_id`.
`challenge_issued` events in the Parquet file reference the same
`question_id`. The `--type` label of the poll is **not** written here —
it lives on the event row in Parquet (`challenge_type` column).

JSON Lines is chosen over YAML because Polars loads it natively
(`pl.read_ndjson()`), variable fields per question type work naturally
as absent keys, and it is compact and append-friendly.

### Question record fields

| Field           | Always present | Description                                                  |
|-----------------|----------------|--------------------------------------------------------------|
| `question_id`   | yes            | UUIDv4; referenced by `challenge_issued` events in Parquet   |
| `question_type` | yes            | `multiple_choice`, `numeric`, `short_text`                   |
| `prompt`        | yes            | Question text shown to the student                           |
| `choices`       | MCQ only       | Array of choice strings                                      |
| `correct_answer`| yes            | Sorted list for MCQ/short_text; number for numeric           |
| `match_mode`    | short_text only| `exact`, `substring_ci`, or `regex`                          |
| `tolerance`     | numeric only   | Allowed tolerance (±)                                        |
| `issued_at`     | yes            | ISO-8601 UTC timestamp of the poll that issued this question |

Example file:

```jsonl
{"question_id":"3f2a...","question_type":"multiple_choice","prompt":"Which is prime?","choices":["21","23","27","51"],"correct_answer":["23"],"issued_at":"2026-04-21T10:15:00Z"}
{"question_id":"9c1b...","question_type":"numeric","prompt":"What is 7!?","correct_answer":5040,"tolerance":0,"issued_at":"2026-04-21T10:15:00Z"}
{"question_id":"7d4e...","question_type":"short_text","prompt":"Name a property of isosceles triangles.","correct_answer":["two equal sides","two equal angles"],"match_mode":"substring_ci","issued_at":"2026-04-21T10:32:00Z"}
```

The `ptrack_analytics` library discovers the matching `.jsonl` file when
loading a meeting and exposes a `questions` lazy frame. Joining
`challenges` with `questions` on `question_id` gives full question
context for any challenge event.

## Auto-generation (optional)

Auto-generation is an out-of-band producer of YAML banks. The Python
binary `ptrack_py` runs as a long-lived child process of the Go daemon
for the duration of a session whenever
`challenges.auto_generation.enabled` is true.

The Python process:

1. Receives audio captured by the browser GUI and relayed by Go (see
   "Audio capture path" below). It maintains a rolling in-memory
   transcript window sized per config.
2. When the configured interval elapses (or the early-regen condition
   fires), it asks the local or hosted LLM for a batch of questions in
   the YAML bank format, validates the result, and writes the file to
   the pending directory.
3. If `challenges.auto_generation.auto_submit` is true, it then exec's
   `ptrack poll --type=aigenerated <path>` to dispatch the poll
   immediately.
4. If `auto_submit` is false, it stops there. The GUI surfaces the file
   in the **Auto-generated** option of the Trigger poll menu; the
   teacher submits it (with `--type=combined`) after optionally
   reviewing or editing it.

Malformed generator output is dropped silently at debug log level. A
failed generation emits a `challenge_generator_failed` event so the
teacher sees it in the GUI's system log.

### Audio capture path

Audio capture is browser-side, not OS-side. The browser captures
microphone input through `navigator.mediaDevices.getUserMedia`, which:

- shows the platform's native microphone permission dialog;
- exposes a built-in device picker so the teacher can choose between
  headset, laptop mic, or any other input;
- works on mobile browsers, which is how Android-on-Termux use is
  supported.

The GUI streams PCM (or Opus) audio frames over a WebSocket to the Go
daemon's control plane. Go forwards these to the Python challenger over
stdin (or its localhost HTTP audio endpoint, depending on the
implementation choice in `internal/challenger/`). The data is consumed
by faster-whisper inside Python and never written to disk by any party.

The browser exposes a mute toggle next to the meeting status panel,
which simply pauses the WebSocket stream and synthesizes a silence
marker on resume so faster-whisper does not mis-segment around the gap.

### Generation prompt

A stable system prompt instructs the LLM to produce questions as YAML
conforming to the bank format. Prompts live in
`py/src/challenger/prompts.py` — treat this file as part of the system's
public contract.

If the model emits JSON or mis-shaped YAML, the Python challenger
tolerates both (loads with a permissive parser, normalizes before
writing). Invalid questions are dropped silently.

### Model choices

| Use case           | Default local model          | Alt (smaller)  | Alt (hosted)       |
|--------------------|------------------------------|----------------|--------------------|
| Question generator | Qwen 2.5 3B (Q4_K_M)         | SmolLM2 1.7B   | OpenAI / Gemini    |
| ASR                | faster-whisper `small` int8  | `base` int8    | OpenAI Whisper API |

See `@docs/CONFIG.md` for configuration keys.

### Model lifecycle (warm start)

The Python challenger loads its ASR and LLM models once at session
start and keeps them resident for the rest of the session. Each
subsequent generation reuses the warm models; load cost is paid once,
not per poll.

Two notable departures from the obvious lifecycle:

- **Models are not unloaded on `meeting_ended`.** They stay resident.
  The teacher releases them explicitly via the GUI's **Free models**
  button (see `@docs/GUI.md`). Rationale: back-to-back meetings should
  not pay the load cost twice.
- **Process termination is explicit.** The full shutdown path
  (Python process exits, Go daemon stops, all listeners closed) is
  triggered by the GUI's **Shut down** button, which then renders a
  "you can close this browser tab" page. Closing the browser tab by
  itself does not stop anything — the daemon survives, models stay
  warm, and a new tab reconnects to the same session.

Two config-driven optimizations:

- `challenges.auto_generation.preload_models: true|false` —
  if true (default), Python blocks startup until models are loaded and
  reports `loaded faster-whisper small in 4.2s, 480 MB; loaded
  llama-cpp Qwen2.5-3B in 7.1s, 2.4 GB` over its stderr (Go relays this
  to the GUI's system log). If false, models load lazily on the first
  generation, trading a faster session start for a slower first poll.
- `challenges.auto_generation.idle_unload_after_seconds` —
  if non-zero, the challenger auto-unloads after that many seconds of
  inactivity. Default 0 (never auto-unload). Useful on memory-tight
  machines.

Hosted backends (`generator.backend: openai | gemini`,
`asr.backend: whisper-api`) skip the load step entirely; the warm-start
machinery still holds the transcript window and API client state, just
without the multi-gigabyte memory footprint.

## Poll coordination

Config keys that apply to every poll regardless of producer:

- `answer_window_seconds` — deadline for an answer (default 30s). After
  this time the message is edited (inline keyboard removed) or deleted.
- `max_delivery_skew_ms` — warn if class-wide fanout takes longer than
  this. Default 2000 ms.

### Per-participant eligibility

A participant is eligible for a given poll iff:

- They are currently in the meeting.
- They are paired with at least one enabled messenger.
- They haven't received a challenge within the past `min_gap_seconds`
  (default 60).

Ineligible participants produce `challenge_skipped_*` events. A poll
round with zero eligible participants is skipped and emits
`challenge_poll_skipped_no_participants`.

## Result states

| State        | Condition                                                          |
|--------------|--------------------------------------------------------------------|
| `correct`    | Right answer submitted within `answer_window`.                     |
| `incorrect`  | Wrong answer submitted within `answer_window`.                     |
| `unanswered` | No answer received by `answer_window`. Message edited or deleted.  |

Additional diagnostic events (not counted in challenge stats):

- `challenge_skipped_offline` — messenger delivery failed.
- `challenge_generator_failed` — auto-generation produced no usable
  questions for a scheduled batch.

## Copy-paste defenses

1. **Synchronous delivery.** All participants receive their question at
   the same moment. Relaying an answer requires the relay to complete
   within `answer_window` — impractical at 30 s.
2. **Short answer window.** 30 s is enough for an attentive student and
   hard for a relay through a third party.
3. **Random distribution** of questions reduces the value of sharing.

## Adding a new producer

Producers do not need to be registered anywhere. Anything that can
write a valid YAML bank and invoke `ptrack poll` is a producer. To
distinguish a new producer's output in analytics, pick a stable
`--type=<label>` and document it in the team's own notes — the system
records the label verbatim and does not constrain its value.
