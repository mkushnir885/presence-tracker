"""GUI-stats-only load helpers.

The stats payload threads the source directory back into each meeting
entry and resolves question_id references inline. Neither concern is
relevant to a notebook user, so the helpers live here instead of in
ptrack_analytics.
"""

from __future__ import annotations

import json
from collections.abc import Iterable
from pathlib import Path
from typing import Any

import polars as pl

from ptrack_analytics.load import (
    EVENTS_FILE,
    QUESTIONS_FILE,
    collect_df,
    scan_events,
)


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


def load_questions_index(
    meeting_dirs: Iterable[Path | str],
) -> dict[str, dict[str, Any]]:
    """Map question_id -> full question record across the given meetings.

    Missing files and malformed lines are skipped silently; this mirrors the
    GUI stats consumer, which treats absent records as "no question payload".
    """
    out: dict[str, dict[str, Any]] = {}
    for d in meeting_dirs:
        path = Path(d) / QUESTIONS_FILE
        if not path.exists():
            continue
        with path.open("r", encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    record = json.loads(line)
                except json.JSONDecodeError:
                    continue
                qid = record.get("question_id")
                if not qid:
                    continue
                out[qid] = record
    return out
