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
from .load import load_events, load_questions, resolve_meetings

__all__ = [
    "load",
    "meetings",
    "presence",
    "challenges",
    "questions",
]

meetings: pl.LazyFrame | None = None
presence: pl.LazyFrame | None = None
challenges: pl.LazyFrame | None = None
questions: pl.LazyFrame | None = None


def load(*patterns: str) -> None:
    """Load meetings matching *patterns* (paths or globs) and populate the
    module-level lazy frames. Each matched directory must contain
    events.parquet; an adjacent questions.jsonl is loaded when present.

    Rejects meetings still in progress (no session_ended event) so every
    frame has fully-closed presence bands and a concrete end time.
    """
    global meetings, presence, challenges, questions

    dirs = resolve_meetings(*patterns)
    events = load_events(dirs)
    meetings = meetings_view(events)
    presence = presence_view(events)
    challenges = challenges_view(events)
    questions = load_questions(dirs)
