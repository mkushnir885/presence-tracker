"""
JSON stats payload for the GUI's /stats view.

The Go GUI calls `ptrack_py stats --in <a> [--in <b> ...]` and renders the
returned JSON. With one input the response describes a single meeting; with
more than one it describes every (participant, meeting) cell across the
requested files so the cross-meeting paged container can navigate locally
without further subprocess calls.

The JSON shape is intentionally uniform across both modes — only the
`mode` field and the cardinality of `meetings` / `rows` change. See
docs/GUI.md for the consumer contract.
"""

from __future__ import annotations

from typing import Any

import polars as pl

from ptrack_analytics.frames import challenge_results as _challenge_frame
from ptrack_analytics.frames import presence as _presence_frame


def generate_stats(events: pl.LazyFrame, mode: str) -> dict[str, Any]:
    """
    Build the JSON-serialisable stats document for *events*.

    *mode* is "meeting" when *events* covers exactly one Parquet file and
    "cross_meeting" otherwise; the caller (CLI) decides based on the
    number of --in arguments. Single-meeting mode omits absent-participant
    placeholder rows; cross-meeting mode includes one row per (participant,
    meeting) cell with `absent: true` when the participant did not appear.
    """
    meetings = _collect_meetings(events)
    summary = _collect_summary(events, meetings)
    segments = _collect_segments(events, meetings)
    markers = _collect_markers(events, meetings)

    return {
        "mode": mode,
        "meetings": [
            {
                "meeting_id": m["meeting_id"],
                "started_at": m["started_at_iso"],
                "duration_seconds": m["duration_seconds"],
            }
            for m in meetings
        ],
        "participants": _assemble_participants(
            meetings, summary, segments, markers, include_absent=mode == "cross_meeting"
        ),
    }


# ── collection helpers ──────────────────────────────────────────────────────


def _collect_meetings(events: pl.LazyFrame) -> list[dict[str, Any]]:
    start = (
        events.filter(pl.col("event_type") == "meeting_started")
        .group_by("meeting_id")
        .agg(
            pl.from_epoch(pl.col("timestamp").first(), time_unit="ms").alias(
                "started_at"
            )
        )
    )
    # meeting_started's timestamp is the absolute Unix anchor; every other
    # row's timestamp is an offset from it, so the duration is the max offset
    # excluding meeting_started.
    duration = (
        events.filter(pl.col("event_type") != "meeting_started")
        .group_by("meeting_id")
        .agg(pl.col("timestamp").max().alias("duration_ms"))
    )
    df: pl.DataFrame = (  # type: ignore  # ty limitation: collect returns InProcessQuery union
        start.join(duration, on="meeting_id", how="left")
        .with_columns(
            pl.col("duration_ms").fill_null(0),
        )
        .with_columns(
            pl.when(pl.col("duration_ms") > 0)
            .then(pl.col("duration_ms") / 1_000.0)
            .otherwise(pl.lit(1.0))
            .alias("duration_seconds"),
            pl.col("started_at")
            .dt.strftime("%Y-%m-%dT%H:%M:%SZ")
            .alias("started_at_iso"),
        )
        .sort("started_at")
        .collect()
    )
    return df.to_dicts()


def _collect_summary(
    events: pl.LazyFrame, meetings: list[dict[str, Any]]
) -> dict[tuple[str, str], dict[str, Any]]:
    """Per-(display_name, meeting_id) presence ratio and challenge counts."""
    meeting_durations = pl.LazyFrame(
        {
            "meeting_id": [m["meeting_id"] for m in meetings],
            "duration_ms": [int(m["duration_ms"]) for m in meetings],
            "duration_seconds": [m["duration_seconds"] for m in meetings],
        }
    )

    pres = (
        _presence_frame(events)
        .join(meeting_durations, on="meeting_id", how="left")
        .with_columns(
            pl.when(pl.col("left_ms").is_null())
            .then((pl.col("duration_ms") - pl.col("joined_ms")) / 1_000.0)
            .otherwise(pl.col("presence_seconds"))
            .alias("presence_seconds")
        )
        .group_by(["display_name", "meeting_id"])
        .agg(
            pl.col("presence_seconds").sum(),
            pl.col("duration_seconds").first(),
        )
        .with_columns(
            (pl.col("presence_seconds").fill_null(0.0) / pl.col("duration_seconds"))
            .clip(0.0, 1.0)
            .round(4)
            .alias("presence_ratio")
        )
    )

    chal = (
        _challenge_frame(events)
        .group_by(["display_name", "meeting_id"])
        .agg(
            pl.len().alias("challenges_issued"),
            (pl.col("state") == "correct")
            .sum()
            .cast(pl.Int64)
            .alias("challenges_correct"),
        )
    )

    df: pl.DataFrame = (  # type: ignore  # ty limitation
        pres.join(chal, on=["display_name", "meeting_id"], how="full", coalesce=True)
        .with_columns(
            pl.col("presence_ratio").fill_null(0.0),
            pl.col("challenges_issued").fill_null(0),
            pl.col("challenges_correct").fill_null(0),
        )
        .collect()
    )

    out: dict[tuple[str, str], dict[str, Any]] = {}
    for row in df.to_dicts():
        out[(row["display_name"], row["meeting_id"])] = {
            "presence_ratio": float(row["presence_ratio"]),
            "challenges_issued": int(row["challenges_issued"]),
            "challenges_correct": int(row["challenges_correct"]),
        }
    return out


def _collect_segments(
    events: pl.LazyFrame, meetings: list[dict[str, Any]]
) -> dict[tuple[str, str], list[dict[str, Any]]]:
    """Per-(display_name, meeting_id) list of presence band segments (percentages)."""
    meeting_durations = pl.LazyFrame(
        {
            "meeting_id": [m["meeting_id"] for m in meetings],
            "duration_ms": [int(m["duration_ms"]) for m in meetings],
        }
    )

    df: pl.DataFrame = (  # type: ignore  # ty limitation
        _presence_frame(events)
        .join(meeting_durations, on="meeting_id", how="left")
        .with_columns(
            pl.col("left_ms").fill_null(pl.col("duration_ms")).alias("end_ms"),
        )
        .with_columns(
            (pl.col("joined_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("start_pct"),
            ((pl.col("end_ms") - pl.col("joined_ms")) / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("width_pct"),
        )
        .sort(["display_name", "meeting_id", "joined_ms"])
        .collect()
    )

    out: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for row in df.to_dicts():
        key = (row["display_name"], row["meeting_id"])
        out.setdefault(key, []).append(
            {
                "start_pct": float(row["start_pct"]),
                "width_pct": float(row["width_pct"]),
                "present": True,
            }
        )
    return out


def _collect_markers(
    events: pl.LazyFrame, meetings: list[dict[str, Any]]
) -> dict[tuple[str, str], list[dict[str, Any]]]:
    """Per-(display_name, meeting_id) list of challenge markers (percentages)."""
    meeting_durations = pl.LazyFrame(
        {
            "meeting_id": [m["meeting_id"] for m in meetings],
            "duration_ms": [int(m["duration_ms"]) for m in meetings],
        }
    )

    df: pl.DataFrame = (  # type: ignore  # ty limitation
        _challenge_frame(events)
        .join(meeting_durations, on="meeting_id", how="left")
        .with_columns(
            (pl.col("issued_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("x_pct"),
            pl.col("state").fill_null("unanswered").alias("result"),
        )
        .sort(["display_name", "meeting_id", "issued_ms"])
        .collect()
    )

    out: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for row in df.to_dicts():
        key = (row["display_name"], row["meeting_id"])
        out.setdefault(key, []).append(
            {
                "x_pct": float(row["x_pct"]),
                "challenge_type": row["challenge_type"],
                "result": row["result"],
                "challenge_id": row["challenge_id"],
                "question_id": row["question_id"],
                "timestamp_ms": int(row["issued_ms"]),
            }
        )
    return out


def _assemble_participants(
    meetings: list[dict[str, Any]],
    summary: dict[tuple[str, str], dict[str, Any]],
    segments: dict[tuple[str, str], list[dict[str, Any]]],
    markers: dict[tuple[str, str], list[dict[str, Any]]],
    include_absent: bool,
) -> list[dict[str, Any]]:
    names = sorted({n for (n, _) in summary}, key=str.lower)
    meeting_ids = [m["meeting_id"] for m in meetings]

    out: list[dict[str, Any]] = []
    for name in names:
        rows: list[dict[str, Any]] = []
        for mid in meeting_ids:
            key = (name, mid)
            if key in summary:
                s = summary[key]
                rows.append(
                    {
                        "meeting_id": mid,
                        "absent": False,
                        "presence_ratio": s["presence_ratio"],
                        "challenges_issued": s["challenges_issued"],
                        "challenges_correct": s["challenges_correct"],
                        "segments": segments.get(key, []),
                        "markers": markers.get(key, []),
                    }
                )
            elif include_absent:
                rows.append(
                    {
                        "meeting_id": mid,
                        "absent": True,
                        "presence_ratio": 0.0,
                        "challenges_issued": 0,
                        "challenges_correct": 0,
                        "segments": [],
                        "markers": [],
                    }
                )
        out.append({"display_name": name, "rows": rows})
    return out
