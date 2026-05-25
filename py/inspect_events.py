#!/usr/bin/env python3
"""Print events from a Parquet meeting file in a human-readable format."""

from __future__ import annotations

import json
import sys
from datetime import UTC, datetime, timedelta

import polars as pl


def fmt_elapsed(ms: int) -> str:
    td = timedelta(milliseconds=ms)
    total_s = int(td.total_seconds())
    h, remainder = divmod(total_s, 3600)
    m, s = divmod(remainder, 60)
    frac = ms % 1000
    return f"{h:02d}:{m:02d}:{s:02d}.{frac:03d}"


def main() -> None:
    if len(sys.argv) < 2:
        print(
            f"usage: {sys.argv[0]} <meeting.parquet> [event_type_filter]",
            file=sys.stderr,
        )
        sys.exit(1)

    path = sys.argv[1]
    event_filter = sys.argv[2] if len(sys.argv) > 2 else None

    df = pl.read_parquet(path)

    if event_filter:
        df = df.filter(pl.col("event_type").str.contains(event_filter))

    # Find the absolute start time from meeting_started row.
    start_rows = df.filter(pl.col("event_type") == "meeting_started")
    start_ts_ms: int | None = None
    if not start_rows.is_empty():
        start_ts_ms = start_rows["timestamp"][0]
        start_dt = datetime.fromtimestamp(start_ts_ms / 1000, tz=UTC)
        print(f"Meeting start: {start_dt.isoformat()}  ({path})")
    else:
        print(f"(no meeting_started event found)  ({path})")

    print(f"{'TIME':>15}  {'EVENT TYPE':<35}  {'PARTICIPANT':<20}  METADATA")
    print("-" * 110)

    for row in df.iter_rows(named=True):
        ts: int = row["timestamp"]
        etype: str = row["event_type"]

        if etype == "meeting_started" and start_ts_ms is not None:
            time_str = datetime.fromtimestamp(ts / 1000, tz=UTC).strftime(
                "%H:%M:%S.000"
            )
        else:
            time_str = fmt_elapsed(ts)

        participant = row.get("display_name") or ""

        raw_meta = row.get("metadata") or ""
        if raw_meta:
            try:
                meta = json.loads(raw_meta)
                meta_str = "  ".join(f"{k}={v}" for k, v in meta.items())
            except (json.JSONDecodeError, AttributeError):
                meta_str = raw_meta
        else:
            meta_str = ""

        print(f"{time_str:>15}  {etype:<35}  {participant:<20}  {meta_str}")


if __name__ == "__main__":
    main()
