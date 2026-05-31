"""
ptrack_analytics — meeting analysis library.

Typical Jupyter usage::

    from ptrack_analytics import load

    load("~/Documents/ptrack/meetings/spring-2026-*")

    from ptrack_analytics import (
        data, meetings, participants, presence, challenges, questions
    )

See docs/QUERIES.md for the full API.
"""

from __future__ import annotations

import polars as pl

from .frames import challenge_results, meeting_times
from .frames import presence as _presence_fn
from .load import LoadError, load_events, load_questions, resolve_meeting_dirs

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


def load(pattern: str) -> None:
    """Load meetings matching *pattern* (a meeting-directory path or glob) and
    populate the module-level lazy frames. Each matched directory must contain
    events.parquet; an adjacent questions.jsonl is loaded when present.
    """
    global data, meetings, participants, presence, challenges, questions

    dirs = resolve_meeting_dirs(pattern)
    data = load_events(pattern)
    meetings = meeting_times(data)
    participants = (
        data.filter(pl.col("display_name").is_not_null())
        .group_by("display_name")
        .agg(pl.len().alias("event_count"))
    )
    presence = _presence_fn(data)
    challenges = challenge_results(data)
    questions = load_questions(dirs)
