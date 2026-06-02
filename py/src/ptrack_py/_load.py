"""GUI-stats-only load helpers.

The stats payload threads the source directory back into each meeting
entry — a concern that is not relevant to a notebook user, so it lives
here instead of in ptrack_analytics.
"""

from __future__ import annotations

from collections.abc import Iterable
from pathlib import Path

import polars as pl

from ptrack_analytics.load import EVENTS_FILE, collect_df, scan_events


def meeting_source_dirs(meeting_dirs: Iterable[Path | str]) -> dict[str, str]:
    """Map each directory's meeting_id to its source-directory path.

    Scans the first row of each events.parquet for the meeting_id. Used by the
    GUI stats loader to thread source paths back into the rendered payload.
    """
    out: dict[str, str] = {}
    for d in meeting_dirs:
        dir_path = Path(d)
        df = collect_df(
            scan_events(dir_path / EVENTS_FILE).select(
                pl.col("meeting_id").first().alias("meeting_id")
            )
        )
        if df.height == 0:
            continue
        mid = df.row(0)[0]
        if isinstance(mid, str) and mid:
            out[mid] = str(dir_path)
    return out
