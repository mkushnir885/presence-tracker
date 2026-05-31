"""Polars event schema; keep in sync with the Go side.
from_start_ms: ms elapsed since the meeting start (session_started is 0). The
absolute start/end instants live in session_started/session_ended metadata
under "timestamp_ms" (Unix ms).
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
