"""JSON stats payload for the GUI's /stats view. See docs/GUI.md for the
consumer contract.
"""

from __future__ import annotations

from typing import Any

import polars as pl

from ptrack_analytics.frames import (
    challenge_results,
    challenge_stats,
    concurrent_participants,
    meeting_times,
    presence_closed,
    presence_totals,
)
from ptrack_analytics.load import collect_df


def generate_stats(
    events: pl.LazyFrame,
    mode: str,
    questions: dict[str, dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Build the stats document. mode "cross_meeting" adds absent-participant
    placeholder rows; "meeting" omits them. *questions* maps each question_id
    to its full record (prompt, type, choices, …, meeting_id).
    """
    meetings = _collect_meetings(events)
    segments = _collect_segments(events)
    summary = _collect_summary(events)
    markers = _collect_markers(events)

    return {
        "mode": mode,
        "meetings": [
            {
                "meeting_id": m["meeting_id"],
                "started_at": int(m["started_at_ms"]),
                "duration_seconds": m["duration_seconds"],
                "platform": m["platform"],
                "started_cause": m["started_cause"],
                "ended_cause": m["ended_cause"],
                "max_participants": int(m["max_participants"]),
            }
            for m in meetings
        ],
        "participants": _assemble_participants(
            meetings, summary, segments, markers, include_absent=mode == "cross_meeting"
        ),
        "questions": questions or {},
    }


def _collect_meetings(events: pl.LazyFrame) -> list[dict[str, Any]]:
    # started_at + duration come from the shared frames.meeting_times; only the
    # GUI-specific platform/cause metadata is pulled out here.
    times = meeting_times(events)
    start_meta = (
        events.filter(pl.col("event_type") == "session_started")
        .group_by("meeting_id")
        .agg(
            pl.col("metadata")
            .str.json_path_match("$.platform")
            .first()
            .alias("platform"),
            pl.col("metadata")
            .str.json_path_match("$.cause")
            .first()
            .alias("started_cause"),
        )
    )
    ended_meta = (
        events.filter(pl.col("event_type") == "session_ended")
        .group_by("meeting_id")
        .agg(
            pl.col("metadata")
            .str.json_path_match("$.cause")
            .first()
            .alias("ended_cause"),
        )
    )
    df = collect_df(
        times.join(start_meta, on="meeting_id", how="left")
        .join(ended_meta, on="meeting_id", how="left")
        .join(concurrent_participants(events), on="meeting_id", how="left")
        .with_columns(
            pl.col("started_at").dt.timestamp("ms").alias("started_at_ms"),
            pl.col("platform").fill_null(""),
            pl.col("started_cause").fill_null(""),
            pl.col("ended_cause").fill_null(""),
            pl.col("max_participants").fill_null(0),
        )
        .sort("started_at")
    )
    return df.to_dicts()


def _collect_segments(
    events: pl.LazyFrame,
) -> dict[tuple[str, str], list[dict[str, Any]]]:
    # Express each closed band as start/width percentages of the meeting for
    # the SVG. presence_closed already pairs joins↔leaves and clips at
    # duration; this only adds the SVG geometry.
    df = collect_df(
        presence_closed(events)
        .with_columns(
            (pl.col("joined_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("start_pct"),
            (pl.col("end_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("end_pct"),
        )
        .with_columns(
            (pl.col("end_pct") - pl.col("start_pct")).alias("width_pct"),
        )
        .sort(["display_name", "meeting_id", "joined_ms"])
    )

    out: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for row in df.to_dicts():
        key = (row["display_name"], row["meeting_id"])
        out.setdefault(key, []).append(
            {
                "start_pct": float(row["start_pct"]),
                "width_pct": float(row["width_pct"]),
                "present": True,
                "start_ms": int(row["joined_ms"]),
                "end_ms": int(row["end_ms"]),
                "still_present": bool(row["still_present"]),
                "join_method": row["join_method"] or "",
                "leave_reason": (
                    "" if row["still_present"] else (row["leave_reason"] or "")
                ),
            }
        )
    return out


def _collect_summary(
    events: pl.LazyFrame,
) -> dict[tuple[str, str], dict[str, Any]]:
    # Full-outer join: a participant who has presence but no challenges (or
    # vice versa) still gets a complete cell after fill_null.
    times = meeting_times(events).select(["meeting_id", "duration_seconds"])
    joined = (
        presence_totals(events)
        .join(
            challenge_stats(events, by=["display_name", "meeting_id"]),
            on=["display_name", "meeting_id"],
            how="full",
            coalesce=True,
        )
        .join(times, on="meeting_id", how="left")
        .with_columns(
            pl.col("presence_seconds").fill_null(0.0),
            pl.col("challenges_issued").fill_null(0),
            pl.col("challenges_correct").fill_null(0),
            pl.col("challenges_incorrect").fill_null(0),
            pl.col("challenges_unanswered").fill_null(0),
        )
        .with_columns(
            pl.when(pl.col("duration_seconds") > 0)
            .then(
                (pl.col("presence_seconds") / pl.col("duration_seconds")).clip(0.0, 1.0)
            )
            .otherwise(0.0)
            .alias("presence_ratio")
        )
    )
    df = collect_df(joined)
    return {
        (row["display_name"], row["meeting_id"]): {
            "presence_seconds": float(row["presence_seconds"]),
            "presence_ratio": float(row["presence_ratio"]),
            "challenges_issued": int(row["challenges_issued"]),
            "challenges_correct": int(row["challenges_correct"]),
            "challenges_incorrect": int(row["challenges_incorrect"]),
            "challenges_unanswered": int(row["challenges_unanswered"]),
        }
        for row in df.to_dicts()
    }


_MARKER_COLS = [
    "display_name",
    "meeting_id",
    "challenge_id",
    "question_id",
    "auto_submitted",
    "result",
    "skip_reason",
    "timestamp_ms",
    "x_pct",
    "latency_ms",
    "submitted_answer",
]


def _collect_markers(
    events: pl.LazyFrame,
) -> dict[tuple[str, str], list[dict[str, Any]]]:
    # Issued and skipped challenges share one schema so they sort onto a single
    # timeline together; the question payload is merged in later by the Go
    # stats loader from the paired JSONL.
    durations = meeting_times(events).select(["meeting_id", "duration_ms"])

    issued = (
        challenge_results(events)
        .join(durations, on="meeting_id", how="left")
        .with_columns(
            pl.col("issued_ms").alias("timestamp_ms"),
            (pl.col("issued_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("x_pct"),
            pl.col("state").fill_null("unanswered").alias("result"),
            pl.lit("").alias("skip_reason"),
            pl.col("latency_ms").fill_null(0),
        )
        .select(_MARKER_COLS)
    )

    skipped = (
        events.filter(pl.col("event_type") == "challenge_skipped")
        .join(durations, on="meeting_id", how="left")
        .with_columns(
            pl.col("from_start_ms").alias("timestamp_ms"),
            (pl.col("from_start_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("x_pct"),
            pl.lit("skipped").alias("result"),
            pl.col("metadata")
            .str.json_path_match("$.reason")
            .fill_null("")
            .alias("skip_reason"),
            pl.col("metadata")
            .str.json_path_match("$.auto_submitted")
            .eq("true")
            .alias("auto_submitted"),
            pl.lit("").alias("question_id"),
            pl.lit(0, dtype=pl.Int64).alias("latency_ms"),
            pl.lit("").alias("submitted_answer"),
        )
        .select(_MARKER_COLS)
    )

    df = collect_df(
        pl.concat([issued, skipped]).sort(
            ["display_name", "meeting_id", "timestamp_ms"]
        )
    )

    out: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for row in df.to_dicts():
        key = (row["display_name"], row["meeting_id"])
        out.setdefault(key, []).append(
            {
                "x_pct": float(row["x_pct"]),
                "auto_submitted": bool(row["auto_submitted"]),
                "result": row["result"],
                "skip_reason": row["skip_reason"],
                "challenge_id": row["challenge_id"],
                "question_id": row["question_id"],
                "timestamp_ms": int(row["timestamp_ms"]),
                "latency_ms": int(row["latency_ms"]),
                "submitted_answer": row["submitted_answer"],
            }
        )
    return out


def _absent_participant_row(meeting_id: str) -> dict[str, Any]:
    return {
        "meeting_id": meeting_id,
        "absent": True,
        "presence_ratio": 0.0,
        "presence_seconds": 0.0,
        "challenges_issued": 0,
        "challenges_correct": 0,
        "challenges_incorrect": 0,
        "challenges_unanswered": 0,
        "segments": [],
        "markers": [],
    }


def _assemble_participants(
    meetings: list[dict[str, Any]],
    summary: dict[tuple[str, str], dict[str, Any]],
    segments: dict[tuple[str, str], list[dict[str, Any]]],
    markers: dict[tuple[str, str], list[dict[str, Any]]],
    include_absent: bool,
) -> list[dict[str, Any]]:
    # One row per (participant, meeting). In cross-meeting mode every
    # participant gets a cell for every meeting — an absent placeholder where
    # they did not appear — so the GUI can page through a uniform grid.
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
                        "presence_seconds": s["presence_seconds"],
                        "challenges_issued": s["challenges_issued"],
                        "challenges_correct": s["challenges_correct"],
                        "challenges_incorrect": s["challenges_incorrect"],
                        "challenges_unanswered": s["challenges_unanswered"],
                        "segments": segments.get(key, []),
                        "markers": markers.get(key, []),
                    }
                )
            elif include_absent:
                rows.append(_absent_participant_row(mid))
        out.append({"display_name": name, "rows": rows})
    return out
