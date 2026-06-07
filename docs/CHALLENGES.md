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

| Producer            | `--auto-submitted` | Notes                                                                            |
|---------------------|--------------------|----------------------------------------------------------------------------------|
| Teacher, by hand    | not set (false)    | The teacher prepares a YAML bank in their editor and selects it from the GUI or shell. |
| Auto-generator      | set (true)         | The in-process Go challenger dispatches the generated bank through the challenge pipeline directly, in memory (when `auto_submit` is on). Nothing is written to disk. |
| Reviewed auto-gen   | not set (false)    | The Go challenger writes the generated YAML to `challenges.auto_generation.review_dir` and stops; the teacher reviews/edits it and triggers the poll via the GUI menu (when `auto_submit` is off). Once dispatched manually it counts as teacher-reviewed. |
| User scripts        | caller's choice    | Any tool that produces a valid YAML bank can call `ptrack poll [--auto-submitted] <path>`. Set the flag only when no human reviewed the bank before dispatch. |

The `--auto-submitted` flag is a CLI/transport-side marker. It is
**never** written inside the YAML file itself — the YAML stays a clean,
producer-agnostic bank format.

## The poll trigger: `ptrack poll`

```
ptrack poll [--auto-submitted] [--port=<port>] [--wait] <path-to-bank.yaml>
```

`ptrack poll` is a **thin client** to the running `ptrack` daemon. It
contains no challenge logic of its own: it resolves the daemon URL,
opens a localhost HTTP connection, and posts the poll request.

- `--auto-submitted` marks the poll as dispatched without teacher
  review. Defaults to false; pass it only from automated producers that
  bypass the teacher.
- `--port` selects a daemon when several `ptrack` processes are running
  in parallel (one meeting per process). Optional when the daemon binds
  the configured `gui.port` and that value lives in the local config.
- `--wait` keeps the CLI attached until the poll's `answer_window`
  elapses, then prints `delivered N, correct K, incorrect M, unanswered U`
  to stdout and uses the exit code accordingly. Without `--wait`, the
  CLI exits immediately on the 200 OK that confirms the poll was
  scheduled — useful for fire-and-forget callers like the Python
  generator.

The same code path serves the GUI's **Trigger poll** menu (see
`@docs/GUI.md`). Both invocations end up calling `POST /poll` on the
running daemon's HTTP API.

### Listener port

`ptrack serve` and `ptrack track` both bind to `gui.port` from
`config.json` (or the `--port` flag override). The flag overrides only
for the current run — it never writes to `config.json`. If the chosen
port is already taken, the daemon refuses to start with a hint to pass
`--port=<free port>` instead of falling back to a random port.

`ptrack poll` (and `ptrack reload`) find the daemon via, in order:
`--server=URL`, `--port=<port>`, the local config's `gui.port`. When
running multiple daemons in parallel, the user picks the target by
passing `--port` explicitly.

### Endpoint shape

```
POST /poll
```

Request body:

```json
{ "auto_submitted": false, "bank_path": "/abs/path/to/bank.yaml" }
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

## Review file

The review file is only used when
`challenges.auto_generation.auto_submit = false`. It holds the latest
pending auto-generated YAML that the teacher will inspect and dispatch
manually.

- **Configured by** `challenges.auto_generation.review_dir` and
  `challenges.auto_generation.bank_basename`. The path is
  `<review_dir>/<bank_basename>.yaml`. Defaults:
  `~/Documents/ptrack/generated.yaml` on Unix,
  `%USERPROFILE%\Documents\ptrack\generated.yaml` on Windows.
- The directory is created on first write if it does not exist.
- Every regeneration overwrites the same file in place — only the
  latest pending bank ever exists; no per-run filenames, no sweep step.

The GUI's **Trigger poll** menu enables the **Auto-generated** option
only when the file is present. On selection the menu dispatches the
bank with `auto_submitted=false` (the teacher acted).

When `auto_submit = true`, the challenger never writes to disk at all:
the generated bank is passed in-memory directly to the challenge
pipeline.

## Question bank format (YAML)

One file per bank. The bank has no top-level `title` or `id` fields —
the filename is the identifier and question IDs are assigned at issue
time.

```yaml
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

When a poll runs, every issued question is appended to
`questions.jsonl` inside the meeting's directory, sitting next to that
meeting's `events.parquet` (a meeting directory therefore contains
exactly `events.parquet` and, if any polls ran, `questions.jsonl`).
Each line is one JSON object with the full question content plus a
UUIDv4 `question_id`.
`challenge_issued` events in the Parquet file reference the same
`question_id`. The `auto_submitted` marker is **not** written here —
it lives on the event row in Parquet (`auto_submitted` metadata key).

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

The poll's issue time is not stored here; the `challenge_issued` event in
Parquet carries it as a `from_start_ms` offset.

Example file:

```jsonl
{"question_id":"3f2a...","question_type":"multiple_choice","prompt":"Which is prime?","choices":["21","23","27","51"],"correct_answer":["23"]}
{"question_id":"9c1b...","question_type":"numeric","prompt":"What is 7!?","correct_answer":5040,"tolerance":0}
{"question_id":"7d4e...","question_type":"short_text","prompt":"Name a property of isosceles triangles.","correct_answer":["two equal sides","two equal angles"],"match_mode":"substring_ci"}
```

The `ptrack_analytics` library discovers `questions.jsonl` next to the
loaded meeting's `events.parquet` and exposes a `questions` lazy
frame. Joining
`challenges` with `questions` on `question_id` gives full question
context for any challenge event.

## Auto-generation (optional)

Auto-generation is an in-process producer of YAML banks built into the
Go daemon. When `challenges.auto_generation.enabled` is true, the
`go/src/internal/challenger/` package runs alongside the session: it
consumes audio frames forwarded from the browser, maintains a rolling
in-memory transcript window, and on its own schedule asks an LLM for a
fresh batch of questions.

Both inference paths — ASR and LLM — go out over **OpenAI-compatible
HTTP**. The challenger holds no model weights itself; it speaks to a
configured backend that does. ptrack does not ship a default backend —
the teacher configures base URL, API key, and model per side. A local
**LocalAI** daemon is a convenient self-hosted choice (it exposes both
`/v1/chat/completions` and `/v1/audio/transcriptions` from one process);
hosted backends (OpenAI, Gemini, any self-hosted gateway) are
interchangeable.

The challenger:

1. Consumes Opus/WebM segments that the browser POSTs to
   `POST /audio/segment` (one HTTP request per `MediaRecorder` chunk;
   no WebSocket) and forwards each segment body to the configured ASR
   endpoint. The returned transcripts are appended to a rolling buffer
   sized per `transcript_window_minutes`.
2. When the configured `poll_interval_seconds` elapses (or an
   early-regen condition fires), it builds a prompt from the current
   transcript window, calls the LLM's chat-completions endpoint asking
   for up to `max_questions_per_poll` questions in YAML bank format,
   and validates the response.
3. If `challenges.auto_generation.auto_submit` is true, it dispatches
   the bank directly through the in-process challenge pipeline with
   `auto_submitted=true`. **Nothing is written to disk.**
4. If `auto_submit` is false, it writes the YAML to the configured
   `review_dir` (see "Review directory" above) and stops. The GUI
   surfaces the file in the **Auto-generated** option of the Trigger
   poll menu; the teacher submits it (with `auto_submitted=false`)
   after optionally reviewing or editing it.

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

The GUI batches captured audio into short Opus/WebM segments with
`MediaRecorder` and uploads each segment to the daemon as a regular
`POST /audio/segment` request — no WebSocket, no streaming framing.
`internal/challenger/` receives each segment in-process and forwards it
to the ASR endpoint as a standard OpenAI-compatible
`/audio/transcriptions` request. Segments are never written to disk by
Go or the browser; the ASR backend's local handling of the request body
is its own concern.

The browser exposes a mute toggle next to the meeting status panel,
which simply stops `MediaRecorder` and suppresses uploads for as long
as it stays engaged.

### Generation prompt

A stable system prompt instructs the LLM to produce questions as YAML
conforming to the bank format. Prompts live in
`go/src/internal/challenger/prompts.go` — treat this file as part of
the system's public contract.

If the model emits JSON or mis-shaped YAML, the challenger tolerates
both (loads with a permissive parser, normalizes before writing).
Invalid questions are dropped silently.

### Lesson language

`challenges.auto_generation.language` declares the spoken lesson
language. Accepted values:

- A BCP-47 / ISO 639-1 tag, e.g. `"en"`, `"uk"`, `"uk-UA"`.
- The sentinel `"auto"` to opt out of all hinting and rely on
  backend-side language detection.

Default: `"auto"`. When a concrete tag is set the challenger does two
things:

- Forwards it as the `language` parameter on every ASR request, which
  Whisper-class models use as a hard hint. Without the hint accented
  or non-English speech routinely transcribes into hallucinated
  English; with it accuracy improves noticeably.
- Injects it into the LLM's user prompt as a
  "write every prompt and answer in this language" instruction so
  generated questions match the audience regardless of how the
  transcript looks (helpful when the lecturer code-switches into
  English for technical terms).

Region subtags (`uk-UA`) are stripped for the ASR request because
Whisper only accepts the primary ISO 639-1 subtag; the LLM prompt
keeps the full tag verbatim. `"auto"` and the empty string both
disable the hint on both sides.

### Model choices

Both backends are OpenAI-compatible HTTP endpoints. The teacher
configures a base URL, API key (optional for local), and model name
per side; the LLM and ASR endpoints are independent and do not have to
share a host.

| Use case           | Self-hosted example                 | Hosted alternatives                    |
|--------------------|-------------------------------------|----------------------------------------|
| Question generator | LocalAI running a small chat model  | OpenAI, Gemini, any OAI-compat gateway |
| ASR                | LocalAI running a Whisper model     | OpenAI Whisper API                     |

ptrack does not ship default base URLs or model names. The teacher
picks the backend and fills both fields in config. If the chosen LLM
backend does not expose `/v1/audio/transcriptions`, point `asr.base_url`
at one that does (e.g. a separate Whisper-only sidecar).

See `@docs/CONFIG.md` for configuration keys.

### Resource lifecycle

There are no resident models inside `ptrack`. The chosen backend
(LocalAI, hosted API, ...) owns model weights, GPU memory, and warm-up
— the challenger is just an HTTP client. Practical consequences:

- **No "Free models" button.** Memory management belongs to the chosen
  backend (e.g. LocalAI's single-active-backend mode for swap-on-demand,
  or the backend's own unload command).
- **No preload step inside `ptrack`.** The first generation pays
  whatever cold-start the backend has; subsequent ones reuse whatever
  it kept warm.
- **Shutdown is just the Go daemon stopping.** The GUI's **Shut down**
  button stops the active session, closes all listeners, and exits the
  process. The external ASR/LLM backend keeps running independently.

Sessions that don't need auto-generation simply leave
`challenges.auto_generation.enabled` at `false` and incur no audio,
ASR, or LLM traffic at all.

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

Ineligible participants produce `challenge_skipped` events (with a
`reason` metadata key — `min_gap` for the eligibility filter above,
`delivery_failed` when the messenger send fails after assignment). A poll
round with zero eligible participants is skipped and emits
`challenge_poll_skipped_no_participants`.

## Result states

| State        | Condition                                                          |
|--------------|--------------------------------------------------------------------|
| `correct`    | Right answer submitted within `answer_window`.                     |
| `incorrect`  | Wrong answer submitted within `answer_window`.                     |
| `unanswered` | No answer received by `answer_window`. Message edited or deleted.  |

Additional diagnostic events (not counted in challenge stats):

- `challenge_skipped` — the participant was excluded from this poll
  round. `reason` metadata distinguishes `delivery_failed` from
  eligibility skips (`min_gap`, …). Rendered as a distinct marker on
  the timeband.
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
write a valid YAML bank and invoke `ptrack poll` is a producer. Pass
`--auto-submitted` only when the bank reaches the pipeline without a
human reviewing it; that is the single bit analytics use to set apart
unreviewed questions from everything else.
