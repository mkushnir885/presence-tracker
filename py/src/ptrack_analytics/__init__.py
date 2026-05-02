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
from typing import TYPE_CHECKING

import polars as pl

from .frames import challenge_results
from .frames import presence as _presence_fn
from .load import LoadError, load_events, load_questions

if TYPE_CHECKING:
    pass

__all__ = [
    "load",
    "LoadError",
    "data",
    "meetings",
    "participants",
    "presence",
    "challenges",
    "questions",
    "generate_pdf",
    "generate_aggregate_pdf",
]

# Module-level lazy frames, populated by load().
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
    """
    Load meeting Parquet files matching *pattern* and populate the module-level
    lazy frames (data, meetings, participants, presence, challenges, questions).

    *questions_dir* defaults to the same directory as the matched Parquet files
    with the basename replaced by "questions/".
    """
    global data, meetings, participants, presence, challenges, questions, _questions_dir

    data = load_events(pattern)

    # Derive meetings frame: one row per meeting.
    meetings = (
        data.group_by("meeting_id")
        .agg(
            pl.col("timestamp").min().alias("started_at"),
            pl.col("timestamp").max().alias("ended_at"),
        )
        .with_columns(
            (pl.col("ended_at") - pl.col("started_at"))
            .dt.total_seconds()
            .alias("duration_seconds")
        )
    )

    # Derive participants frame: one row per participant.
    participants = (
        data.filter(pl.col("participant_id").is_not_null())
        .group_by("participant_id")
        .agg(pl.col("display_name").drop_nulls().last().alias("display_name"))
    )

    presence = _presence_fn(data)
    challenges = challenge_results(data)

    # Infer questions directory if not provided.
    import glob as _glob

    first_path = sorted(_glob.glob(str(Path(pattern).expanduser())))[0]
    _questions_dir = questions_dir or str(Path(first_path).parent.parent / "questions")

    _meeting_ids = data.select("meeting_id").unique().collect()["meeting_id"].to_list()  # type: ignore  # ty limitation: collect() on LazyFrame returns DataFrame which supports []
    questions = load_questions(_questions_dir, _meeting_ids)


def generate_pdf(parquet_path: str, output_path: str) -> None:
    """Generate a per-meeting PDF report.

    TODO: PDF generation not implemented yet. Delegate to ptrack_py report subcommand.
    """
    raise NotImplementedError("PDF generation not yet implemented")


def generate_aggregate_pdf(pattern: str, output_path: str) -> None:
    """Generate an aggregate PDF report over multiple meetings.

    TODO: PDF generation not implemented yet.
    """
    raise NotImplementedError("PDF generation not yet implemented")
