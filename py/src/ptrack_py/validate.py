"""Pre-flight validation for inputs to the report/stats subcommands.

The Go event store flushes records to Parquet as they happen, so a file
written while the meeting is still running is on disk but missing its
terminal session_ended event. Analysis over such a file would silently
treat the last observed timestamp as the meeting's end, which is
misleading. ptrack_py refuses incomplete files outright; the teacher
can retry once the daemon has finalised the session.
"""

from __future__ import annotations

import polars as pl

from ptrack_analytics.schema import EVENT_SCHEMA


class IncompleteMeetingError(Exception):
    """Raised when a Parquet file has no session_ended event."""

    def __init__(self, path: str) -> None:
        super().__init__(
            f"{path}: meeting is still in progress (no session_ended event); "
            "stop the tracking session and try again."
        )
        self.path = path


def ensure_session_ended(path: str) -> None:
    """Raise IncompleteMeetingError if *path* has no session_ended event.

    Reads only the event_type column, so the cost is negligible even
    for large files.
    """
    df: pl.DataFrame = (  # type: ignore  # ty limitation: collect() returns InProcessQuery union
        pl.scan_parquet(path, schema=pl.Schema(EVENT_SCHEMA))
        .filter(pl.col("event_type") == "session_ended")
        .select(pl.len())
        .collect()
    )
    if int(df.item()) == 0:
        raise IncompleteMeetingError(path)
