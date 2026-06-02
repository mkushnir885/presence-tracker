"""Polars schemas for the event log and the question bank. Mirrored on
the Go side — both ends must agree.
"""

from __future__ import annotations

import polars as pl

EVENT_SCHEMA: dict[str, pl.DataType | type[pl.DataType]] = {
    "meeting_id": pl.String,
    "from_start_ms": pl.Int64,
    "event_type": pl.String,
    "display_name": pl.String,
    "challenge_id": pl.String,
    "question_id": pl.String,
    "metadata": pl.String,
}

QUESTIONS_SCHEMA: dict[str, pl.DataType | type[pl.DataType]] = {
    "question_id": pl.String,
    "auto_submitted": pl.Boolean,
    "question_type": pl.String,
    "prompt": pl.String,
    "choices": pl.List(pl.String),
    "correct_answer": pl.String,
    "match_mode": pl.String,
    "tolerance": pl.Float64,
}
