# Analytics library

`ptrack_analytics` is a regular Python library (`py/src/ptrack_analytics/`)
that provides meeting analysis and CSV report generation. It is the same
code whether called from the Go CLI, used internally by the GUI server, or
used interactively in a Jupyter Notebook.

## Using in Jupyter

```python
from ptrack_analytics import load, presence, challenges, questions
import polars as pl

# Load meeting Parquet files. For each meeting found, the matching
# .jsonl file from questions_dir is also loaded automatically.
load("~/Documents/ptrack/meetings/spring-2026-*.parquet")

# Top-level frames (all lazy)
from ptrack_analytics import data, meetings, participants

data: pl.LazyFrame         # all events from loaded files, concatenated
meetings: pl.LazyFrame     # one row per meeting (id, start, end, duration)
participants: pl.LazyFrame # one row per display_name seen across the loaded files

# Derived frames (also lazy; shared code path with CSV reports)
from ptrack_analytics import presence, challenges, questions

presence: pl.LazyFrame     # (display_name, meeting_id, presence_seconds, ...)
challenges: pl.LazyFrame   # (display_name, meeting_id, challenge_id, challenge_type, state, latency_ms)
questions: pl.LazyFrame    # loaded from .jsonl files: (question_id, prompt, question_type, choices, ...)
```

`questions` is loaded from the `.jsonl` files in `questions_dir` that
correspond to the loaded meetings. Polars reads them with `read_ndjson`;
absent fields for irrelevant question types become nulls. Join with the
`challenges` frame on `question_id` to access question text alongside
challenge results.

`challenge_type` on the `challenges` frame is the free-form producer
label captured at poll time (`custom`, `combined`, `aigenerated`, or any
user-defined value — see `@docs/CHALLENGES.md` and
`@docs/EVENT_SCHEMA.md`). It is useful for filtering or grouping
challenges by where the questions came from.

### Example session

```python
from ptrack_analytics import load, presence, challenges, questions
import polars as pl

load("meetings/spring-2026-*.parquet")

# Who attends the least?
presence.group_by("display_name") \
    .agg(pl.col("presence_seconds").mean().alias("avg_s")) \
    .sort("avg_s") \
    .collect()

# Challenge accuracy per meeting
challenges.group_by("meeting_id") \
    .agg((pl.col("state").eq("correct").sum() / pl.len()).alias("accuracy")) \
    .sort("meeting_id") \
    .collect()

# Which questions are hardest? (join challenges + questions)
(
    challenges
    .join(questions.select(["question_id", "prompt"]), on="question_id")
    .group_by("question_id", "prompt")
    .agg((pl.col("state").eq("correct").sum() / pl.len()).alias("accuracy"))
    .sort("accuracy")
    .collect()
)
```

### CSV generation from a notebook

```python
from ptrack_analytics import generate_csv, generate_aggregate_csv

# Single meeting CSV
generate_csv("meetings/2026-04-21.parquet", "reports/2026-04-21.csv")

# Cross-meeting CSV for all meetings matching a glob
generate_aggregate_csv("meetings/spring-2026-*.parquet", "reports/semester.csv")
```

`generate_csv` produces one row per participant with columns:
`display_name`, `presence_ratio`, `challenges_issued`, `challenges_correct`.

`generate_aggregate_csv` produces one row per (participant, meeting) with
the same statistics columns plus `meeting` (ISO datetime of start), sorted
by `display_name` then `meeting`.

Both functions return the DataFrame before writing so the result can be
inspected or piped further in a notebook cell.

## Named analyses (GUI)

The GUI's Statistics panel shows a fixed set of named analyses defined
as functions in `ptrack_analytics.analyses`. Each function takes the
pre-loaded frames and returns a Polars DataFrame or a matplotlib Figure.
New analyses are added by writing a new function and registering it with
the `@analysis` decorator — no YAML, no user-editable expressions.

The GUI fetches analysis results via `POST /analysis/{name}` and renders:

- `pl.DataFrame` → HTML table with sticky header, CSV
  download button.
- `matplotlib.Figure` → inline PNG.
- Scalar → formatted value, large.

## Adding a new analysis

1. Add a function in `py/src/ptrack_analytics/analyses.py`:

```python
@analysis(name="avg_presence", title="Average presence per student")
def avg_presence(presence: pl.LazyFrame, **_) -> pl.DataFrame:
    return (
        presence.group_by("display_name")
        .agg(pl.col("presence_seconds").mean().alias("avg_s"))
        .sort("avg_s", descending=True)
        .collect()
    )
```

2. The function is automatically available in the GUI and importable
   from the library.

## Changing pre-loaded frames

When you add a new derived frame:

1. Add its construction to `py/src/ptrack_analytics/analysis.py` as a
   pure function `derive_<n>(data: pl.LazyFrame) -> pl.LazyFrame`.
2. Export it from `py/src/ptrack_analytics/__init__.py`.
3. Document it in the "Using in Jupyter" section above.
