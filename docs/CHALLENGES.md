# Challenge system

Challenges verify that a participant is actually engaged with the lesson,
without cameras and without interrupting the teacher.

## Design principles

1. **The teacher is never interrupted.** Delivery, timing, and scoring
   happen automatically. For file-based challenges, the teacher triggers
   the poll but does not manage individual deliveries.
2. **Delivery is out-of-band.** Challenges go to Telegram, not the
   meeting chat. Individual responses are private.
3. **Synchronized delivery.** All eligible participants receive their
   question at the same moment. A student cannot pass their answer to a
   classmate because the classmate is simultaneously busy with their own
   question inside the same `answer_window`.
4. **Graceful degradation.** A missing messenger registration, a failed
   challenge generation, or a network hiccup must not crash the session
   or produce a false "unanswered" mark.
5. **Honest limits.** No challenge is cheat-proof. The goal is to make
   cheating at least as much work as attending.
6. **Zero challenges is valid.** Any number of challenge types may be
   enabled, including zero (tracking-only mode).

## Question storage

Questions are stored as **JSON Lines (`.jsonl`) files**, one per meeting,
in the configured `questions_dir`:

```
~/Documents/ptrack/questions/
├── 2026-04-21-algebra.jsonl
└── 2026-04-23-algebra.jsonl
```

The filename matches the meeting ID. Each line is one JSON object. At
poll time, one object is appended per unique question in the poll round.
`challenge_issued` events in the Parquet reference the same `question_id`.

Every question gets a **UUIDv4** assigned at poll time. There is no
cross-poll or cross-meeting ID linking; the `.jsonl` record is a
verbatim snapshot of what was asked.

Question bank files (teacher-prepared YAML) are **not** stored by the
system. When a poll runs, the issued questions are written to the `.jsonl`
file. The original bank YAML is the teacher's own responsibility.

JSON Lines is chosen over YAML because Polars loads it natively
(`pl.read_ndjson()`), variable fields per question type work naturally
as absent keys, and it is compact and append-friendly.

### Question record fields

| Field           | Always present | Description                                                  |
|-----------------|----------------|--------------------------------------------------------------|
| `question_id`   | yes            | UUIDv4; referenced by `challenge_issued` events in Parquet   |
| `challenge_type`| yes            | `filebased` or `aigenerated`                                 |
| `question_type` | yes            | `multiple_choice`, `numeric`, `short_text`                   |
| `prompt`        | yes            | Question text shown to the student                           |
| `choices`       | MCQ only       | Array of choice strings                                      |
| `correct_answer`| yes            | Sorted list for MCQ/short_text; number for numeric           |
| `match_mode`    | short_text only| `exact`, `substring_ci`, or `regex`                          |
| `tolerance`     | numeric only   | Allowed tolerance (±)                                        |
| `issued_at`     | yes            | ISO-8601 UTC timestamp of the poll that issued this question |

Example file:

```jsonl
{"question_id":"3f2a...","challenge_type":"filebased","question_type":"multiple_choice","prompt":"Which is prime?","choices":["21","23","27","51"],"correct_answer":["23"],"issued_at":"2026-04-21T10:15:00Z"}
{"question_id":"9c1b...","challenge_type":"filebased","question_type":"numeric","prompt":"What is 7!?","correct_answer":5040,"tolerance":0,"issued_at":"2026-04-21T10:15:00Z"}
{"question_id":"7d4e...","challenge_type":"aigenerated","question_type":"short_text","prompt":"Name a property of isosceles triangles.","correct_answer":["two equal sides","two equal angles"],"match_mode":"substring_ci","issued_at":"2026-04-21T10:32:00Z"}
```

The `ptrack_analytics` library discovers the matching `.jsonl` file when
loading a meeting and exposes a `questions` lazy frame. Joining
`challenges` with `questions` on `question_id` gives full question
context for any challenge event.

## Types

### File-based (v1 MVP)

Teacher prepares question-bank YAML files before the semester (or before
each meeting).

**Poll flow:**

1. Teacher selects a question-bank file in the GUI and presses
   **Start Poll**.
2. The system takes all questions from the file, shuffles them, and
   assigns one question per eligible participant (cycling through the
   shuffled list if the class is larger than the bank).
3. A UUIDv4 is assigned to each question and the questions are appended
   to the meeting's `.jsonl` file in `questions_dir`.
4. Questions are delivered simultaneously to all eligible participants
   via the messenger.
5. After `answer_window` elapses, results are saved.

#### File format

YAML, one file per question bank. The bank has no top-level `title` or
`id` fields — the filename is the identifier and question IDs are derived
automatically from content.

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

If a teacher edits a question in the bank file, the change only affects
future polls — it has no impact on historical data. The `.jsonl` records
for past meetings are verbatim snapshots of what was asked.

Validation rules in `go/src/internal/challenges/filebased/validate.go`. The
teacher sees validation errors in the file picker before they can load
an invalid file.

#### Answer matching

- `multiple_choice` — the student's selected set must equal the `answer`
  set exactly (same elements, order-insensitive). No partial credit.
  - If `len(answer) == 1`, each button is a one-click commit.
  - If `len(answer) > 1`, buttons toggle on/off and a Submit button
    finalizes.
- `numeric` — parse answer as a number; pass if within `±tolerance`.
  Rejects non-numeric input with a hint.
- `short_text` — matched per the `match` mode. Default is
  `substring_ci`.

### AI-generated (v1 stretch / v2)

A local (or hosted) small LLM generates questions from the meeting
context. Polls fire automatically when a configured amount of new
context has accumulated.

**Poll flow:**

1. The `challenger` service accumulates meeting context (ASR transcript,
   optionally screen-share OCR).
2. When the context window reaches the configured threshold, Go calls
   `POST /challenges/generate` with the requested question count.
3. The Python challenger returns a batch of questions in the file-based
   YAML format. Malformed questions are discarded (logged, not raised).
4. Each valid question is assigned a UUIDv4 and appended to the
   meeting's `.jsonl` file in `questions_dir`.
5. Questions are shuffled and delivered simultaneously via the messenger.
6. After `answer_window` elapses, results are saved.

**The generator output is the file-based YAML format, byte-for-byte.**
The same parser, validator, and messenger rendering logic consume both
sources. This unification is a hard requirement.

**Dynamic configuration during a meeting:** `questions_per_poll` and
`poll_interval_seconds` can be changed from the live status view at any
time without restarting the `challenger` service.

#### Context inputs

- **Audio transcript** (always on when AI-gen is enabled). Rolling
  window, size configurable (default: last 20 minutes of speech).
- **Screen-share OCR** (off by default). Capture one frame per second
  and OCR text regions.

#### Question language

Configured via `challenges.aigenerated.question_language` (ISO code,
e.g. `uk`, `en`). Default: `uk`. Independent of the UI language.

#### Generation prompt

A stable system prompt instructs the model to produce questions as YAML
conforming to the file-based format. Prompts live in
`py/src/challenger/prompts.py` — treat this file as part of the system's
public contract.

If the model emits JSON or mis-shaped YAML, the Python challenger
tolerates both (loads with a permissive parser, normalizes before
returning). Invalid questions are dropped silently at debug log level.

#### Model choices

| Use case           | Default local model          | Alt (smaller)  | Alt (hosted)       |
|--------------------|------------------------------|----------------|--------------------|
| Question generator | Qwen 2.5 3B (Q4_K_M)         | SmolLM2 1.7B   | OpenAI / Gemini    |
| ASR                | faster-whisper `small` int8  | `base` int8    | OpenAI Whisper API |

See `@docs/CONFIG.md` for configuration keys.

## Poll coordination

Config keys that apply to all poll types:

- `answer_window_seconds` — deadline for an answer (default 30s). After
  this time the message is edited (inline keyboard removed) or deleted.
- `max_delivery_skew_ms` — warn if class-wide fanout takes longer than
  this. Default 2000 ms.

### File-based poll config

No automatic interval — the teacher triggers each poll manually from the GUI.

### AI-generated poll config (runtime-adjustable)

- `poll_interval_seconds` — minimum time between polls. Default 1200 s.
- `questions_per_poll` — questions requested per poll. Default 15.
- `early_regen_on_context_ready` — fire early if significant new context
  arrived. Default true.
- `question_language` — ISO language code for generated questions.
  Default `uk`.

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

- `challenge_skipped_unregistered` — participant has no messenger handle.
- `challenge_skipped_offline` — messenger delivery failed.
- `challenge_generator_failed` — AI-gen only; no questions produced.

## Copy-paste defenses

1. **Synchronous delivery.** All participants receive their question at
   the same moment. Relaying an answer requires the relay to complete
   within `answer_window` — impractical at 30 s.
2. **Short answer window.** 30 s is enough for an attentive student and
   hard for a relay through a third party.
3. **Random distribution** of questions reduces the value of sharing.

## Adding a new challenge type

1. Create `go/src/internal/challenges/<n>/` with an implementation of `ChallengeType`.
2. Register in `go/src/cmd/ptrack/main.go`.
3. Document the type in this file under "Types".
4. Add tests that exercise correct/incorrect/unanswered on the mock messenger.
