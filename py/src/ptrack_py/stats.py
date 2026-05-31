"""JSON stats payload for the GUI's /stats view. See docs/GUI.md for the
consumer contract.
"""

from __future__ import annotations

from typing import Any

import polars as pl

from ptrack_analytics.frames import (
    challenge_results as _challenge_frame,
)
from ptrack_analytics.frames import (
    meeting_times,
    presence_bands,
)


def generate_stats(
    events: pl.LazyFrame,
    mode: str,
) -> dict[str, Any]:
    """Build the stats document. mode "cross_meeting" adds absent-participant
    placeholder rows; "meeting" omits them.
    """
    meetings = _collect_meetings(events)
    segments = _collect_segments(events, meetings)
    summary = _collect_summary(events, meetings, segments)
    markers = _collect_markers(events, meetings)
    max_participants = _collect_max_participants(events)

    return {
        "mode": mode,
        "meetings": [
            {
                "meeting_id": m["meeting_id"],
                "started_at": m["started_at_iso"],
                "duration_seconds": m["duration_seconds"],
                "platform": m.get("platform") or "",
                "started_cause": m.get("started_cause") or "",
                "ended_cause": m.get("ended_cause") or "",
                "max_participants": int(max_participants.get(m["meeting_id"], 0)),
            }
            for m in meetings
        ],
        "participants": _assemble_participants(
            meetings, summary, segments, markers, include_absent=mode == "cross_meeting"
        ),
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
    df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        times.join(start_meta, on="meeting_id", how="left")
        .join(ended_meta, on="meeting_id", how="left")
        .with_columns(
            pl.col("started_at")
            .dt.strftime("%Y-%m-%dT%H:%M:%SZ")
            .alias("started_at_iso"),
        )
        .sort("started_at")
        .collect()
    )
    return df.to_dicts()


def _collect_segments(
    events: pl.LazyFrame, meetings: list[dict[str, Any]]
) -> dict[tuple[str, str], list[dict[str, Any]]]:
    # n-th join pairs with n-th leave (a rejoin is its own segment); a
    # surplus join gets null left_ms. Must match reports.py's pairing.
    durations = pl.LazyFrame(
        {
            "meeting_id": [m["meeting_id"] for m in meetings],
            "duration_ms": [int(m["duration_ms"]) for m in meetings],
        }
    )

    paired = presence_bands(events)

    # Express each band as start/width percentages of the meeting for the SVG;
    # a band with no leave (or one past the end) is still-present and closes at
    # duration.
    df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        paired.join(durations, on="meeting_id", how="left")
        .with_columns(
            (
                pl.col("left_ms").is_null()
                | (pl.col("left_ms") > pl.col("duration_ms"))
            ).alias("still_present"),
        )
        .with_columns(
            pl.when(pl.col("still_present"))
            .then(pl.col("duration_ms"))
            .otherwise(pl.col("left_ms"))
            .alias("end_ms"),
        )
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
    meetings: list[dict[str, Any]],
    segments: dict[tuple[str, str], list[dict[str, Any]]],
) -> dict[tuple[str, str], dict[str, Any]]:
    duration_by_id = {m["meeting_id"]: m["duration_seconds"] for m in meetings}

    # Sum presence straight from the already-built segments so the totals match
    # the timeline exactly, rather than recomputing the bands here.
    presence_rows: dict[tuple[str, str], float] = {}
    for key, segs in segments.items():
        total = 0
        for s in segs:
            total += s["end_ms"] - s["start_ms"]
        presence_rows[key] = total / 1_000.0

    chal_df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        _challenge_frame(events)
        .group_by(["display_name", "meeting_id"])
        .agg(
            pl.len().alias("challenges_issued"),
            (pl.col("state") == "correct")
            .sum()
            .cast(pl.Int64)
            .alias("challenges_correct"),
            (pl.col("state") == "incorrect")
            .sum()
            .cast(pl.Int64)
            .alias("challenges_incorrect"),
            (pl.col("state") == "unanswered")
            .sum()
            .cast(pl.Int64)
            .alias("challenges_unanswered"),
        )
        .collect()
    )

    out: dict[tuple[str, str], dict[str, Any]] = {}
    for row in chal_df.to_dicts():
        out[(row["display_name"], row["meeting_id"])] = {
            "presence_seconds": 0.0,
            "presence_ratio": 0.0,
            "challenges_issued": int(row["challenges_issued"]),
            "challenges_correct": int(row["challenges_correct"]),
            "challenges_incorrect": int(row["challenges_incorrect"]),
            "challenges_unanswered": int(row["challenges_unanswered"]),
        }

    for key, secs in presence_rows.items():
        meeting_dur = duration_by_id.get(key[1], 0.0) or 0.0
        cell = out.setdefault(
            key,
            {
                "presence_seconds": 0.0,
                "presence_ratio": 0.0,
                "challenges_issued": 0,
                "challenges_correct": 0,
                "challenges_incorrect": 0,
                "challenges_unanswered": 0,
            },
        )
        cell["presence_seconds"] = float(secs)
        cell["presence_ratio"] = float(
            min(1.0, max(0.0, secs / meeting_dur)) if meeting_dur > 0 else 0.0
        )

    return out


def _collect_max_participants(events: pl.LazyFrame) -> dict[str, int]:
    # Sweep-line: +1 per join, -1 per leave, track the running max.
    joined = events.filter(pl.col("event_type") == "participant_joined").select(
        pl.col("meeting_id"),
        pl.col("from_start_ms").alias("t"),
        pl.lit(1, dtype=pl.Int64).alias("delta"),
    )
    left = events.filter(pl.col("event_type") == "participant_left").select(
        pl.col("meeting_id"),
        pl.col("from_start_ms").alias("t"),
        pl.lit(-1, dtype=pl.Int64).alias("delta"),
    )
    # Sorting delta descending puts joins (+1) before leaves (-1) at the same
    # instant, so a simultaneous swap still counts the momentary peak.
    df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        pl.concat([joined, left])
        .sort(["meeting_id", "t", "delta"], descending=[False, False, True])
        .with_columns(pl.col("delta").cum_sum().over("meeting_id").alias("concurrent"))
        .group_by("meeting_id")
        .agg(pl.col("concurrent").max().alias("max_participants"))
        .collect()
    )
    return {row["meeting_id"]: int(row["max_participants"]) for row in df.to_dicts()}


def _collect_markers(
    events: pl.LazyFrame,
    meetings: list[dict[str, Any]],
) -> dict[tuple[str, str], list[dict[str, Any]]]:
    # Issued and skipped challenges become markers; the question payload is
    # merged in later by the Go stats loader from the paired JSONL.
    durations = pl.LazyFrame(
        {
            "meeting_id": [m["meeting_id"] for m in meetings],
            "duration_ms": [int(m["duration_ms"]) for m in meetings],
        }
    )

    issued_df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        _challenge_frame(events)
        .join(durations, on="meeting_id", how="left")
        .with_columns(
            (pl.col("issued_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("x_pct"),
            pl.col("state").fill_null("unanswered").alias("result"),
        )
        .sort(["display_name", "meeting_id", "issued_ms"])
        .collect()
    )

    skipped_df = _collect_skipped(events, durations)

    out: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for row in issued_df.to_dicts():
        key = (row["display_name"], row["meeting_id"])
        out.setdefault(key, []).append(
            {
                "x_pct": float(row["x_pct"]),
                "auto_submitted": bool(row["auto_submitted"]),
                "result": row["result"],
                "skip_reason": "",
                "challenge_id": row["challenge_id"],
                "question_id": row["question_id"],
                "timestamp_ms": int(row["issued_ms"]),
                "latency_ms": int(row["latency_ms"])
                if row.get("latency_ms") is not None
                else 0,
                "submitted_answer": row.get("submitted_answer") or "",
            }
        )
    for row in skipped_df.to_dicts():
        key = (row["display_name"], row["meeting_id"])
        out.setdefault(key, []).append(
            {
                "x_pct": float(row["x_pct"]),
                "auto_submitted": bool(row["auto_submitted"]),
                "result": "skipped",
                "skip_reason": row["skip_reason"] or "",
                "challenge_id": row["challenge_id"],
                "question_id": "",
                "timestamp_ms": int(row["skipped_ms"]),
                "latency_ms": 0,
                "submitted_answer": "",
            }
        )
    for key, markers in out.items():
        markers.sort(key=lambda m: m["timestamp_ms"])
        out[key] = markers
    return out


def _collect_skipped(events: pl.LazyFrame, durations: pl.LazyFrame) -> pl.DataFrame:
    df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        events.filter(pl.col("event_type") == "challenge_skipped")
        .select(
            pl.col("display_name"),
            pl.col("meeting_id"),
            pl.col("challenge_id"),
            pl.col("from_start_ms").alias("skipped_ms"),
            pl.col("metadata")
            .str.json_path_match("$.reason")
            .fill_null("")
            .alias("skip_reason"),
            pl.col("metadata")
            .str.json_path_match("$.auto_submitted")
            .eq("true")
            .alias("auto_submitted"),
        )
        .join(durations, on="meeting_id", how="left")
        .with_columns(
            (pl.col("skipped_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("x_pct"),
        )
        .collect()
    )
    return df


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
                rows.append(
                    {
                        "meeting_id": mid,
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
                )
        out.append({"display_name": name, "rows": rows})
    return out
