# Analytics library

`ptrack_analytics` (`py/src/ptrack_analytics/`) is the Jupyter-facing
analytics library. It loads meeting Parquet (and the matching question
JSONL) and exposes a small set of pre-derived Polars lazy frames with
notebook-friendly types — `Datetime` for instants, `Duration` for
elapsed time, struct columns for per-event details. The raw event log
is intentionally not part of the public surface; everything users need
is shaped into these frames.

CSV reports and the GUI stats JSON are *not* part of this library —
they live in the binary-only `ptrack_py/` package (`reports.py`,
`stats.py`) and build on the same internal frame helpers.

## Using in Jupyter

```python
from ptrack_analytics import load
import polars as pl

# Load one or more meeting directories. Each must contain
# events.parquet; the adjacent questions.jsonl is loaded automatically
# when present. Meetings still in progress (no session_ended event) are
# rejected.
load("~/Documents/ptrack/meetings/spring-2026-*")

from ptrack_analytics import meetings, presence, challenges, questions
```

All four are `pl.LazyFrame`. Their schemas:

### `meetings` — one row per meeting

| Column       | Type                  | Notes                                   |
| ------------ | --------------------- | --------------------------------------- |
| `meeting_id` | `Utf8`                |                                         |
| `platform`   | `Utf8`                | `bbb`, `meet`, `zoom`, `mock`           |
| `started_at` | `Datetime("ms","UTC")`|                                         |
| `ended_at`   | `Datetime("ms","UTC")`| always set (validated on load)          |
| `duration`   | `Duration("ms")`      | `ended_at - started_at`                 |
| `start_cause`| `Utf8`                | from session_started metadata           |
| `end_cause`  | `Utf8`                | from session_ended metadata             |

### `presence` — one row per (display_name, meeting_id)

| Column             | Type                        | Notes                                          |
| ------------------ | --------------------------- | ---------------------------------------------- |
| `display_name`     | `Utf8`                      |                                                |
| `meeting_id`       | `Utf8`                      | join with `meetings` for absolute times        |
| `total_duration`   | `Duration("ms")`            | sum of band durations                          |
| `ratio`            | `Float64`                   | `total_duration / meetings.duration`, in `[0, 1]` |
| `present_till_end` | `Boolean`                   | `True` if any band stayed open at session end  |
| `bands`            | `List[Struct{...}]`         | per-join bands, ordered by `joined_at`         |

Each `bands` element is a struct with:

| Field          | Type                    | Notes                                     |
| -------------- | ----------------------- | ----------------------------------------- |
| `joined_at`    | `Datetime("ms","UTC")`  |                                           |
| `left_at`      | `Datetime("ms","UTC")`  | open bands clipped to `meetings.ended_at` |
| `duration`     | `Duration("ms")`        | `left_at - joined_at`                     |
| `join_method`  | `Utf8`                  |                                           |
| `leave_reason` | `Utf8`                  | null for the open band when `present_till_end` |

Use `pl.col("bands").list.explode()` (or `explode("bands")`) to flatten back
to one row per band when you need band-level aggregations.

### `challenges` — one row per `challenge_issued` or `challenge_skipped`

| Column             | Type                                            | Notes                                  |
| ------------------ | ----------------------------------------------- | -------------------------------------- |
| `display_name`     | `Utf8`                                          |                                        |
| `meeting_id`       | `Utf8`                                          |                                        |
| `challenge_id`     | `Utf8`                                          |                                        |
| `question_id`      | `Utf8`                                          | join key to `questions`; null when `state == "skipped"` |
| `fired_at`         | `Datetime("ms","UTC")`                          | when the challenge was issued or skipped |
| `answered_at`      | `Datetime("ms","UTC")`                          | set only when `state` is `correct` or `incorrect` |
| `latency`          | `Duration("ms")`                                | same nullability as `answered_at`      |
| `state`            | `Enum{correct,incorrect,unanswered,skipped}`    |                                        |
| `submitted_answer` | `Utf8`                                          | same nullability as `answered_at`      |
| `skip_reason`      | `Utf8`                                          | set only when `state == "skipped"`     |
| `auto_submitted`   | `Boolean`                                       | poll dispatched without teacher review |

Question text is not duplicated onto every challenge row — `challenges`
carries only `question_id`. Join with `questions` to bring in the
prompt, choices, correct answer, etc.

### `questions` — one row per unique `question_id`

| Column        | Type             | Notes                                |
| ------------- | ---------------- | ------------------------------------ |
| `question_id` | `Utf8`           | join key from `challenges`           |
| `question`    | `Struct{...}`    | full record packed into one column   |

The struct's fields are `auto_submitted`, `question_type`, `prompt`,
`choices`, `correct_answer`, `match_mode`, `tolerance` — see
`py/src/ptrack_analytics/schema.py`. Records are deduped by
`question_id` across loaded meetings, so a question referenced by many
challenges still appears exactly once.

To pull a specific field, use struct access:

```python
challenges.join(questions, on="question_id").select(
    "display_name", "state", pl.col("question").struct.field("prompt")
)
```

### Example session

```python
from ptrack_analytics import load, meetings, presence, challenges, questions
import polars as pl

load("meetings/spring-2026-*")

# Average attended time per student, in minutes
(
    presence
    .group_by("display_name")
    .agg(pl.col("duration").sum())
    .with_columns((pl.col("duration").dt.total_seconds() / 60).alias("minutes"))
    .sort("minutes")
    .collect()
)

# Challenge accuracy per meeting
(
    challenges
    .group_by("meeting_id")
    .agg((pl.col("state") == "correct").mean().alias("accuracy"))
    .sort("meeting_id")
    .collect()
)

# Which questions are hardest?
(
    challenges
    .join(questions, on="question_id")
    .with_columns(pl.col("question").struct.field("prompt"))
    .group_by("question_id", "prompt")
    .agg((pl.col("state") == "correct").mean().alias("accuracy"))
    .sort("accuracy")
    .collect()
)
```

### Helper analyses

`ptrack_analytics.analysis` ships a few ready-to-call helpers built on
the four exported frames. Each takes the relevant frames as arguments
so the call site reads as documentation.

```python
from ptrack_analytics import load, meetings, presence, challenges
from ptrack_analytics.analysis import (
    challenge_accuracy,
    plot_concurrent_participants,
    plot_presence_heatmap,
)

load("meetings/spring-2026-*")

# Per-meeting step chart of concurrent participants over time.
plot_concurrent_participants(presence, meetings)

# Per-participant correct/issued ratio across the loaded meetings.
# Skipped challenges are excluded.
challenge_accuracy(challenges)

# Heatmap of presence ratio (participants × meetings); each cell
# carries its ratio label. Pass display_name to keep one row only.
plot_presence_heatmap(presence, meetings)
plot_presence_heatmap(presence, meetings, display_name="Alice")
```

The plotting helpers lazy-import `matplotlib` — install it (`uv add
matplotlib`) before calling them if it isn't already in your notebook
environment.

### CSV reports from a notebook

There is no notebook helper for CSV generation; for ad-hoc tables call
`pl.DataFrame.write_csv` on whatever lazy frame you collect. If you
want the exact CSV the GUI offers, shell out to the binary instead:

```bash
ptrack_py report meetings/2026-04-21 > reports/2026-04-21.csv
ptrack_py report meetings/spring-2026-* > reports/semester.csv
```

(One matched directory produces a per-meeting CSV; more than one
switches to the cross-meeting aggregate. CSV is always written to
stdout.)

## Relationship to the GUI

The GUI's single stats view (`GET /stats?file=<a>&file=<b>…` — see
`@docs/GUI.md`) is backed by the `ptrack_py stats` subcommand
(implemented in `py/src/ptrack_py/stats.py`). That code builds on the
same internal frame helpers under `ptrack_analytics.frames` and emits
a JSON document; Go caches the JSON on disk between requests and
invalidates an entry when any of the input files' mtime advances.

There is no "Statistics" panel and no `POST /analysis/...` endpoint —
the GUI's stats surface is fixed. Anything beyond the per-meeting and
cross-meeting timeband views — custom aggregations, ad-hoc charts,
cross-cohort comparisons — happens in a Jupyter notebook against this
library. The language boundary stays at Parquet + JSON/CSV exactly as
the cross-language contract in `@CLAUDE.md` describes.

## Changing pre-loaded frames

When you add a new derived frame:

1. Add a builder to `py/src/ptrack_analytics/frames.py` shaped like
   the existing `*_view` functions: `def foo_view(events: pl.LazyFrame)
   -> pl.LazyFrame`.
2. Wire it up in `py/src/ptrack_analytics/__init__.py` so `load()`
   populates it and add it to `__all__`.
3. Document its schema in the "Using in Jupyter" section above.
