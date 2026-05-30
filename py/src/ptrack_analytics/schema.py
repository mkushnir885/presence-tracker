"""Polars event schema; keep in sync with the Go side.
timestamp: session_started holds absolute Unix ms, every other row a ms offset from it.
"""

from __future__ import annotations

import polars as pl

EVENT_SCHEMA: dict[str, pl.DataType | type[pl.DataType]] = {
    "meeting_id": pl.String,
    "timestamp": pl.Int64,
    "event_type": pl.String,
    "display_name": pl.String,
    "challenge_id": pl.String,
    "question_id": pl.String,
    "metadata": pl.String,
}
