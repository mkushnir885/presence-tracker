"""
Polars schema for the event Parquet files written by the Go event store.

Must stay in sync with:
  - go/src/internal/eventstore/schema.go
  - docs/EVENT_SCHEMA.md

timestamp semantics (schema_version 3):
  - meeting_started row: absolute Unix timestamp in milliseconds.
  - all other rows: milliseconds elapsed since the meeting_started timestamp.
"""

from __future__ import annotations

import polars as pl

# Column schema used when reading Parquet files.
# The metadata column stores a JSON-encoded map[string]string.
EVENT_SCHEMA: dict[str, pl.DataType | type[pl.DataType]] = {
    "event_id": pl.String,
    "meeting_id": pl.String,
    "timestamp": pl.Int64,
    "source": pl.String,
    "event_type": pl.String,
    "participant_id": pl.String,
    "platform_handle": pl.String,
    "display_name": pl.String,
    "metadata": pl.String,  # JSON-encoded; parse with pl.Expr.str.json_decode
}

SCHEMA_VERSION = 3
