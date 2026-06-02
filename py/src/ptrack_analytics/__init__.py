"""
ptrack_analytics — meeting analysis library.

Typical Jupyter usage::

    from ptrack_analytics import load

    load("~/Documents/ptrack/meetings/spring-2026-*")

    from ptrack_analytics import meetings, presence, challenges, questions

See docs/QUERIES.md for the full API.
"""

from __future__ import annotations

import polars as pl

from .frames import challenges_view, meetings_view, presence_view
from .load import (
    IncompleteMeetingError,
    LoadError,
    load_meetings,
    load_questions,
)

__all__ = [
    "load",
    "LoadError",
    "IncompleteMeetingError",
    "meetings",
    "presence",
    "challenges",
    "questions",
]

meetings: pl.LazyFrame | None = None
presence: pl.LazyFrame | None = None
challenges: pl.LazyFrame | None = None
questions: pl.LazyFrame | None = None


def load(*patterns: str, validate: bool = True) -> None:
    """Load meetings matching *patterns* (paths or globs) and populate the
    module-level lazy frames. Each matched directory must contain
    events.parquet; an adjacent questions.jsonl is loaded when present.

    By default rejects meetings still in progress (no session_ended event)
    so every frame has fully-closed presence bands and a concrete end
    time. Pass validate=False to peek at a live session.
    """
    global meetings, presence, challenges, questions

    dirs, events = load_meetings(*patterns, validate=validate)
    meetings = meetings_view(events)
    presence = presence_view(events)
    challenges = challenges_view(events)
    questions = load_questions(dirs)
