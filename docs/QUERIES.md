# Analytics library

`ptrack_analytics` (`py/src/ptrack_analytics/`) is the Jupyter-facing
analytics library. It loads meeting Parquet (and the matching question
JSONL) and exposes a small set of pre-derived Polars lazy frames; from
there everything is a Polars expression.

CSV reports and the GUI stats JSON are *not* part of this library —
they live in the binary-only `ptrack_py/` package (`reports.py`,
`stats.py`) so the library surface stays small and notebook-relevant.
Both consumers build on top of the same lazy frames documented below.

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

### CSV reports from a notebook

There is no notebook helper for CSV generation; for ad-hoc tables call
`pl.DataFrame.write_csv` on whatever lazy frame you collect. If you
want the exact CSV the GUI offers, shell out to the binary instead:

```bash
ptrack_py report    --in meetings/2026-04-21.parquet     --out reports/2026-04-21.csv
ptrack_py aggregate --in 'meetings/spring-2026-*.parquet' --out reports/semester.csv
```

(`ptrack_py` lives in the sibling `ptrack_py/` package; see
"Relationship to the GUI" below.)

## Relationship to the GUI

The GUI's single stats view (`GET /stats?file=<a>&file=<b>…` — see
`@docs/GUI.md`) is backed by the `ptrack_py stats` subcommand
(implemented in `py/src/ptrack_py/stats.py`). That code builds on top
of this library's `presence` / `challenges` / `questions` lazy frames,
collects them into a JSON document for the requested files, and prints
it to stdout. With one input the JSON describes a per-meeting timeband
list; with more than one it describes the cross-meeting dataset for
every participant in those files.

Go caches the JSON on disk between requests and invalidates an entry
when any of the input files' mtime advances. The expected callers of
`ptrack_py stats` are therefore the GUI server and CLI / scripts;
notebooks have no reason to invoke it, since they can call the lazy
frames directly with full Polars expressiveness.

There is no "Statistics" panel and no `POST /analysis/...` endpoint —
the GUI's stats surface is fixed. Anything beyond the per-meeting and
cross-meeting timeband views — custom aggregations, ad-hoc charts,
cross-cohort comparisons — happens in a Jupyter notebook against this
library. The language boundary stays at Parquet + JSON/CSV exactly as
the cross-language contract in `@CLAUDE.md` describes.

## Changing pre-loaded frames

When you add a new derived frame:

1. Add its construction to `py/src/ptrack_analytics/frames.py` as a
   pure function `derive_<n>(data: pl.LazyFrame) -> pl.LazyFrame`.
2. Wire it up in `py/src/ptrack_analytics/__init__.py` so `load()`
   populates it and add it to `__all__`.
3. Document it in the "Using in Jupyter" section above.
