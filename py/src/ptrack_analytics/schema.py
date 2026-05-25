"""
Polars schema for the event Parquet files written by the Go event store.

Must stay in sync with:
  - go/src/internal/eventstore/schema.go
  - docs/EVENT_SCHEMA.md

display_name is the participant identity end to end — there is no separate
participant_id column. Every per-participant event carries the canonical
registered name.

timestamp semantics:
  - meeting_started row: absolute Unix timestamp in milliseconds.
  - all other rows: milliseconds elapsed since the meeting_started timestamp.
"""

from __future__ import annotations

import polars as pl

# Column schema used when reading Parquet files.
# The metadata column stores a JSON-encoded map[string]string.
# challenge_id and question_id are first-class join keys; null for events
# that are not part of a challenge lifecycle.
EVENT_SCHEMA: dict[str, pl.DataType | type[pl.DataType]] = {
    "meeting_id": pl.String,
    "timestamp": pl.Int64,
    "event_type": pl.String,
    "display_name": pl.String,
    "challenge_id": pl.String,
    "question_id": pl.String,
    "metadata": pl.String,  # JSON-encoded; parse with pl.Expr.str.json_decode
}
