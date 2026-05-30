"""Reject Parquet files still being written (no session_ended event yet):
treating the last observed timestamp as the meeting end would mislead.
"""

from __future__ import annotations

import polars as pl

from ptrack_analytics.schema import EVENT_SCHEMA


class IncompleteMeetingError(Exception):
    def __init__(self, path: str) -> None:
        super().__init__(
            f"{path}: meeting is still in progress (no session_ended event); "
            "stop the tracking session and try again."
        )
        self.path = path


def ensure_session_ended(path: str) -> None:
    """Raise IncompleteMeetingError if *path* has no session_ended event."""
    df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        pl.scan_parquet(path, schema=pl.Schema(EVENT_SCHEMA))
        .filter(pl.col("event_type") == "session_ended")
        .select(pl.len())
        .collect()
    )
    if int(df.item()) == 0:
        raise IncompleteMeetingError(path)
