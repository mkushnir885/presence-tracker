"""Reject Parquet files still being written (no session_ended event yet):
treating the last observed timestamp as the meeting end would mislead.
"""

from __future__ import annotations

import polars as pl

from .load import collect_df, scan_events


class IncompleteMeetingError(Exception):
    def __init__(self, path: str) -> None:
        super().__init__(
            f"{path}: meeting is still in progress (no session_ended event); "
            "stop the tracking session and try again."
        )
        self.path = path


def ensure_session_ended(path: str) -> None:
    """Raise IncompleteMeetingError if *path* has no session_ended event."""
    df = collect_df(
        scan_events(path)
        .filter(pl.col("event_type") == "session_ended")
        .select(pl.len())
    )
    if int(df.item()) == 0:
        raise IncompleteMeetingError(path)
