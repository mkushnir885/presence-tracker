# Event schema

Canonical definition of the event log. Every meeting's Parquet file
written by `go/src/internal/eventstore/` conforms to this schema. The Go Arrow
schema in `go/src/internal/eventstore/schema.go` and the Polars schema in
`py/src/ptrack_analytics/schema.py` must match this document exactly.

All meeting events live in a single **`<meeting_id>.parquet`** file.
Question content is stored separately as a **`<meeting_id>.jsonl`** file
in `questions_dir` (JSON Lines, one object per question). The Parquet
schema below covers all event types; question text is never in Parquet.

`display_name` is the participant identity end to end. Every
per-participant event records the canonical registered name (the value
the student supplied via `/register`, with leading/trailing whitespace
trimmed); there is no separate participant-ID column. Events are written
only for verified participants — joins from unregistered or unverified
people are buffered in memory and discarded if verification never
arrives.

## Columns

| Column         | Type                 | Nullable | Description                                                                                          |
|----------------|----------------------|----------|------------------------------------------------------------------------------------------------------|
| `meeting_id`   | `string`             | no       | Stable ID for this meeting session.                                                                  |
| `timestamp`    | `int64`              | no       | For `session_started`: absolute Unix timestamp in ms. For all other events: ms elapsed since `session_started`. |
| `event_type`   | `string`             | no       | Event kind. See "Event types" below.                                                                 |
| `display_name` | `string`             | yes      | Canonical registered name; null for meeting-scoped events.                                           |
| `challenge_id` | `string`             | yes      | Join key threading the lifecycle of one participant's challenge; null for non-challenge events.      |
| `question_id`  | `string` (UUIDv4)    | yes      | References a record in the meeting's `<meeting_id>.jsonl` file; set on `challenge_issued`.           |
| `metadata`     | `map<string,string>` | yes      | Free-form key-value bag for event-type-specific fields.                                              |

The narrow schema (6 real columns + metadata map) makes multi-meeting
concatenation trivial — all event files share the same shape. The join
keys (`challenge_id`, `question_id`) are first-class columns so that
analytics joins benefit from Parquet column pruning, dictionary
encoding, and predicate pushdown.

## Event types

### Session lifecycle

Each Parquet file contains exactly one `session_started` and one
`session_ended` event. They bound the period during which ptrack was
attached to the meeting; the `cause` metadata field records whether
that boundary coincides with the meeting's actual boundary or with the
tracking process attaching/detaching.

| Event type        | `display_name` | Key metadata fields                       |
|-------------------|----------------|-------------------------------------------|
| `session_started` | null           | `platform`, `host_display_name`, `cause`  |
| `session_ended`   | null           | `duration_seconds`, `cause`               |

`cause` is one of:

- `"meeting"` — the boundary is the meeting's actual boundary as
  observed by the provider. For `session_started`: tracking was already
  attached when the meeting began, so the event's absolute timestamp is
  the meeting's true start time. For `session_ended`: the provider
  reported the meeting ending while tracking was still attached.
- `"tracking"` — the boundary is the tracking process's boundary.
  For `session_started`: tracking attached after the meeting was
  already in progress, so the meeting's true start time is unknown and
  not recorded. For `session_ended`: tracking detached (shutdown,
  signal, error) while the meeting was still running.

Only one start event and one end event are written per session; the
producer picks the `cause` that matches what actually bounded the
session. Providers that cannot determine whether the meeting is already
in progress at attach time (e.g. a hypothetical webhook-only adapter
that depends on receiving the meeting-start notification live) must
fail at startup rather than emit a misleading `cause`.

All other event timestamps remain relative to `session_started`
regardless of `cause`. When `cause = "tracking"` on `session_started`,
the meeting's true start time is not represented in the log at all —
this is intentional; the event log only records what ptrack actually
observed.

### Participant lifecycle

| Event type            | `display_name` | Key metadata fields                  |
|-----------------------|----------------|--------------------------------------|
| `participant_joined`  | set            | `join_method` (web/app/phone)        |
| `participant_left`    | set            | `reason` (left/disconnected/removed) |

`participant_joined` is written only after the verification DM is
confirmed (Yes), with the original join timestamp preserved. Since the
event log only contains verified participants, `participant_joined`
itself implies verification — there is no separate `participant_verified`
event. If the participant denies verification or leaves before answering,
no `participant_joined` row is written.

`participant_left` is written only when the provider observed the
participant leaving. If a participant was still present when the
session ended, **no synthetic `participant_left` is written** — the
band stays open in the log. Analytics close any open band at the
`session_ended` timestamp; the GUI uses `session_ended.cause` to label
the right-edge marker ("till the end of the meeting" when
`cause = "meeting"`, "till tracking stopped" when `cause = "tracking"`).
Fabricating a `participant_left` would erase this distinction, which is
the only signal that tells "we observed a leave" apart from "we stopped
watching."

Note: **mic, camera, screen-share, and chat activity are not tracked.**
Chat is not monitored. Participant pairing is handled entirely via the
Telegram bot outside the meeting. Verification denials, unregistered
joins, and pending-verification states stay in coordinator memory and
are never written to Parquet.

### Challenge lifecycle

`challenge_id` and `question_id` are first-class columns on challenge
events (see the column table above). The table below lists only the
event-type-specific metadata.

| Event type                     | `display_name` | `challenge_id` | `question_id` | Key metadata fields              |
|--------------------------------|----------------|----------------|---------------|----------------------------------|
| `challenge_issued`             | set            | set            | set           | `auto_submitted`, `answer_window_s` |
| `challenge_answered_correct`   | null           | set            | null          | `latency_ms`, `submitted_answer` |
| `challenge_answered_incorrect` | null           | set            | null          | `latency_ms`, `submitted_answer` |
| `challenge_unanswered`         | null           | set            | null          | —                                |
| `challenge_skipped`            | set            | set            | null          | `auto_submitted`, `reason`       |
| `challenge_generator_failed`   | null           | null           | null          | `error_class`                    |

`challenge_id` threads the lifecycle events for one participant's
challenge together. Result events (`_correct`, `_incorrect`,
`_unanswered`) carry no `display_name` — analytics join them back to
the participant via `challenge_id` from the `challenge_issued` row.
Multiple `challenge_issued` events (different participants, same poll)
may share a `question_id`.

`auto_submitted` is a boolean (`"true"` / `"false"`) that records
whether the poll was dispatched by the in-process challenger without
teacher review. The CLI's `--auto-submitted` flag and the equivalent
field on `POST /poll` set this value; the GUI's **Custom bank…** and
**Auto-generated** menu options always submit `"false"` because the
teacher selected the bank. Analytics use this flag to distinguish
unreviewed questions from the rest.

`reason` on `challenge_skipped` records why the participant did not
receive a question. Current values: `delivery_failed` (messenger send
returned an error after the challenge was assigned), `min_gap` (the
participant received a challenge within the past `min_gap_seconds`).
Future eligibility filters add their own snake_case reason strings;
analytics surface unknown values verbatim. Skipped events are excluded
from the "issued" tally (correct + incorrect + unanswered) but still
appear as markers on the per-participant timeband.

`question_id` is a UUIDv4 referencing a record in the meeting's
`<meeting_id>.jsonl` file in `questions_dir`. To retrieve the full
question text and metadata, load that file and look up by `question_id`.
The `ptrack_analytics` library does this automatically, exposing a
`questions` lazy frame that can be joined with `challenges`.

## Metadata conventions

- Keys are snake_case.
- Values are always strings (per Arrow map constraints); numeric values
  are stringified and parsed back in analytics.
- Boolean values use `"true"` / `"false"`.
- Timestamps besides the top-level `timestamp` column are ISO-8601 in UTC.

## Analytics-side derivations

`ptrack_analytics` computes these from the raw event stream. They are
**not** stored in the event log — recomputed on demand:

- `presence_intervals(display_name)` — pairs of (joined, left).
- `challenge_score_series` — time-ordered (timestamp, state) per participant.
- `presence_ratio` — total presence seconds / meeting duration.
- `challenge_accuracy` — correct / (correct + incorrect + unanswered).

## Changing the schema

1. Update this document first.
2. Update `go/src/internal/eventstore/schema.go`.
3. Update `py/src/ptrack_analytics/schema.py`.

To change the `.jsonl` question record fields, update the field table
in `@docs/CHALLENGES.md` and the writer in `go/src/internal/eventstore/questions.go`.
The `.jsonl` format has no formal schema version; additive changes
(new optional fields) are backward-compatible.
