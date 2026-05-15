# Event schema

Canonical definition of the event log. Every meeting's Parquet file
written by `go/src/internal/eventstore/` conforms to this schema. The Go Arrow
schema in `go/src/internal/eventstore/schema.go` and the Polars schema in
`py/src/ptrack_analytics/schema.py` must match this document exactly.

All meeting events live in a single **`<meeting_id>.parquet`** file.
Question content is stored separately as a **`<meeting_id>.jsonl`** file
in `questions_dir` (JSON Lines, one object per question). The Parquet
schema below covers all event types; question text is never in Parquet.

## Schema version

Current: **3**. Stored as a Parquet file-level metadata key
`schema_version`. Readers refuse files with a higher major version.
New nullable columns may be added in a minor bump; removals and renames
are major.

## Columns

| Column            | Type                 | Nullable | Description                                              |
|-------------------|----------------------|----------|----------------------------------------------------------|
| `event_id`        | `string` (UUIDv7)    | no       | Unique per event. UUIDv7 sorts by time.                  |
| `meeting_id`      | `string`             | no       | Stable ID for this meeting session.                      |
| `timestamp`       | `int64`              | no       | For `meeting_started`: absolute Unix timestamp in ms. For all other events: ms elapsed since `meeting_started`. |
| `source`          | `string`             | no       | Origin of the event. See "Sources" below.                |
| `event_type`      | `string`             | no       | Event kind. See "Event types" below.                     |
| `participant_id`  | `string`             | yes      | Internal stable participant ID; null for meeting-scoped events. |
| `platform_handle` | `string`             | yes      | Raw platform-side identifier (email, display name, etc). |
| `display_name`    | `string`             | yes      | Display name at event time; custom override applied if set. |
| `metadata`        | `map<string,string>` | yes      | Free-form key-value bag for event-type-specific fields.  |

The narrow schema (8 real columns + metadata map) makes multi-meeting
concatenation trivial — all event files share the same shape.

## Sources

- `provider:zoom` / `provider:meet` / `provider:bbb` / `provider:mock`
- `messenger:telegram` / `messenger:mock`
- `scheduler` — challenge scheduler lifecycle events
- `system` — the tool itself (start/stop, config reload)

## Event types

### Meeting lifecycle

| Event type        | `participant_id` | Key metadata fields                 |
|-------------------|------------------|-------------------------------------|
| `meeting_started` | null             | `platform`, `host_display_name`     |
| `meeting_ended`   | null             | `duration_seconds`, `reason`        |

### Participant lifecycle

| Event type              | `participant_id` | Key metadata fields              |
|-------------------------|------------------|----------------------------------|
| `participant_joined`    | set              | `join_method` (web/app/phone)    |
| `participant_left`      | set              | `reason` (left/disconnected/removed) |

Note: **mic, camera, screen-share, and chat activity are not tracked.**
Chat is not monitored. Participant pairing is handled entirely via the
Telegram bot outside the meeting.

### Participant pairing

| Event type                        | `participant_id` | Key metadata fields                  |
|-----------------------------------|------------------|--------------------------------------|
| `participant_verification_sent`   | set              | `messenger`, `platform`              |
| `participant_verified`            | set              | `messenger`, `platform`, `latency_ms`|
| `participant_verification_denied` | set              | `messenger`, `platform`              |
| `participant_unregistered`        | set              |                                      |

`participant_verification_sent` is emitted when the session coordinator
finds a registry entry for the joining participant's display name and
sends a "Did you just join?" DM.

`participant_verified` is emitted when the participant taps **Yes**.
`latency_ms` is the time between `participant_joined` and the
confirmation tap.

`participant_verification_denied` is emitted when the participant taps
**No**. (Ignoring the message is not recorded as a distinct event; the
participant simply stays unverified for the session.)

`participant_unregistered` is emitted when a participant joins and no
registry entry matches their `(platform, display_name)`.

### Challenge lifecycle

| Event type                       | `participant_id` | Key metadata fields                                                |
|----------------------------------|------------------|--------------------------------------------------------------------|
| `challenge_issued`               | set              | `challenge_id`, `challenge_type`, `question_id`, `answer_window_s` |
| `challenge_answered_correct`     | set              | `challenge_id`, `latency_ms`                                       |
| `challenge_answered_incorrect`   | set              | `challenge_id`, `latency_ms`, `submitted_hash`                     |
| `challenge_unanswered`           | set              | `challenge_id`                                                     |
| `challenge_skipped_unregistered` | set              | `challenge_id`, `challenge_type`                                   |
| `challenge_skipped_offline`      | set              | `challenge_id`, `challenge_type`, `delivery_error`                 |
| `challenge_generator_failed`     | null             | `challenge_type`, `error_class`                                    |

`challenge_id` threads the lifecycle events for one participant's
challenge together. Multiple `challenge_issued` events (different
participants, same poll) may share a `question_id`.

`challenge_type` is a free-form label set by the producer of the poll —
the `--type=<label>` value passed to `ptrack poll`. The system does not
constrain its values. Conventions used by the built-in producers are
`custom` (teacher's own bank, the default for `ptrack poll`),
`combined` (auto-generated YAML submitted manually after review), and
`aigenerated` (auto-generated YAML submitted automatically by the
challenger). User scripts may use any other label. The same label
appears on the related `challenge_skipped_*` and
`challenge_generator_failed` events so analytics can filter the whole
poll round by producer.

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

- `presence_intervals(participant_id)` — pairs of (joined, left).
- `challenge_score_series` — time-ordered (timestamp, state) per participant.
- `presence_ratio` — total presence seconds / meeting duration.
- `challenge_accuracy` — correct / (correct + incorrect + unanswered).

## Changing the schema

1. Update this document first.
2. Update `go/src/internal/eventstore/schema.go`.
3. Update `py/src/ptrack_analytics/schema.py`.
4. Bump `schema_version`:
   - Adding a nullable column: minor bump.
   - Removing / renaming / changing a column type: major bump.
5. Add a migration note in `docs/EVENT_SCHEMA_CHANGELOG.md` (create if
   missing).

To change the `.jsonl` question record fields, update the field table
in `@docs/CHALLENGES.md` and the writer in `go/src/internal/eventstore/questions.go`.
The `.jsonl` format has no formal schema version; additive changes
(new optional fields) are backward-compatible.
