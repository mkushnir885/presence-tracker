"""
ptrack_analytics — meeting analysis library.

Typical Jupyter usage::

    from ptrack_analytics import load

    load("~/Documents/ptrack/meetings/spring-2026-*.parquet")

    from ptrack_analytics import (
        data, meetings, participants, presence, challenges, questions
    )

See docs/QUERIES.md for the full API.
"""

from __future__ import annotations

from pathlib import Path

import polars as pl

from .frames import challenge_results
from .frames import presence as _presence_fn
from .load import LoadError, load_events, load_questions

__all__ = [
    "load",
    "LoadError",
    "data",
    "meetings",
    "participants",
    "presence",
    "challenges",
    "questions",
]

data: pl.LazyFrame | None = None
meetings: pl.LazyFrame | None = None
participants: pl.LazyFrame | None = None
presence: pl.LazyFrame | None = None
challenges: pl.LazyFrame | None = None
questions: pl.LazyFrame | None = None

_questions_dir: str | None = None


def load(
    pattern: str,
    questions_dir: str | None = None,
) -> None:
    """Load Parquet files matching *pattern* and populate the module-level
    lazy frames. *questions_dir* defaults to a sibling "questions/" dir.
    """
    global data, meetings, participants, presence, challenges, questions, _questions_dir

    data = load_events(pattern)

    meeting_starts = (
        data.filter(pl.col("event_type") == "session_started")
        .group_by("meeting_id")
        .agg(
            pl.from_epoch(pl.col("timestamp").first(), time_unit="ms").alias(
                "started_at"
            )
        )
    )
    # duration_ms is just the largest timestamp: non-start rows store ms
    # offsets from the session start, so the max offset is the meeting length.
    meetings = (
        data.group_by("meeting_id")
        .agg(pl.col("timestamp").max().alias("duration_ms"))
        .join(meeting_starts, on="meeting_id", how="left")
        .with_columns(
            (pl.col("duration_ms") / 1_000.0).alias("duration_seconds"),
        )
    )

    participants = (
        data.filter(pl.col("display_name").is_not_null())
        .group_by("display_name")
        .agg(pl.len().alias("event_count"))
    )

    presence = _presence_fn(data)
    challenges = challenge_results(data)

    import glob as _glob

    # Meetings live in <base>/meetings/*.parquet and questions in
    # <base>/questions/*.jsonl, so default to a sibling questions/ dir.
    first_path = sorted(_glob.glob(str(Path(pattern).expanduser())))[0]
    _questions_dir = questions_dir or str(Path(first_path).parent.parent / "questions")

    _meeting_ids = data.select("meeting_id").unique().collect()["meeting_id"].to_list()  # type: ignore  # ty: collect() return is typed as a union
    questions = load_questions(_questions_dir, _meeting_ids)
