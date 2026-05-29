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

import json
from typing import Any

import polars as pl

from ptrack_analytics.frames import challenge_results as _challenge_frame


def generate_stats(
    events: pl.LazyFrame,
    mode: str,
    questions: pl.LazyFrame | None = None,
) -> dict[str, Any]:
    """
    Build the JSON-serialisable stats document for *events*.

    *mode* is "meeting" when *events* covers exactly one Parquet file and
    "cross_meeting" otherwise; the caller (CLI) decides based on the
    number of --in arguments. Single-meeting mode omits absent-participant
    placeholder rows; cross-meeting mode includes one row per (participant,
    meeting) cell with `absent: true` when the participant did not appear.

    *questions* is an optional LazyFrame loaded from the meeting's
    `.jsonl` file(s); when supplied, marker tooltips include the prompt
    and the canonical correct answer.
    """
    meetings = _collect_meetings(events)
    segments = _collect_segments(events, meetings)
    summary = _collect_summary(events, meetings, segments)
    markers = _collect_markers(events, meetings, questions)
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


# ── collection helpers ──────────────────────────────────────────────────────


def _collect_meetings(events: pl.LazyFrame) -> list[dict[str, Any]]:
    start = (
        events.filter(pl.col("event_type") == "session_started")
        .group_by("meeting_id")
        .agg(
            pl.from_epoch(pl.col("timestamp").first(), time_unit="ms").alias(
                "started_at"
            ),
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
    duration = (
        events.filter(pl.col("event_type") != "session_started")
        .group_by("meeting_id")
        .agg(pl.col("timestamp").max().alias("duration_ms"))
    )
    ended = (
        events.filter(pl.col("event_type") == "session_ended")
        .group_by("meeting_id")
        .agg(
            pl.col("metadata")
            .str.json_path_match("$.cause")
            .first()
            .alias("ended_cause"),
        )
    )
    df: pl.DataFrame = (  # type: ignore  # ty limitation: collect returns InProcessQuery union
        start.join(duration, on="meeting_id", how="left")
        .join(ended, on="meeting_id", how="left")
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


def _collect_segments(
    events: pl.LazyFrame, meetings: list[dict[str, Any]]
) -> dict[tuple[str, str], list[dict[str, Any]]]:
    """Per-(display_name, meeting_id) list of presence band segments.

    Each segment carries both percentages (for SVG layout) and raw ms
    offsets + the source events' metadata fields (for tooltip text).
    Segments are pulled directly from participant_joined / _left rows so
    the metadata columns ride along without re-parsing.
    """
    durations = pl.LazyFrame(
        {
            "meeting_id": [m["meeting_id"] for m in meetings],
            "duration_ms": [int(m["duration_ms"]) for m in meetings],
        }
    )

    joined = events.filter(pl.col("event_type") == "participant_joined").select(
        pl.col("display_name"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("joined_ms"),
        pl.col("metadata").str.json_path_match("$.join_method").alias("join_method"),
    )
    left = events.filter(pl.col("event_type") == "participant_left").select(
        pl.col("display_name"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("left_ms"),
        pl.col("metadata").str.json_path_match("$.reason").alias("leave_reason"),
    )

    paired = (
        joined.join(left, on=["display_name", "meeting_id"], how="left")
        .filter(
            pl.col("left_ms").is_null() | (pl.col("left_ms") >= pl.col("joined_ms"))
        )
        .sort(["display_name", "meeting_id", "joined_ms", "left_ms"])
        .group_by(["display_name", "meeting_id", "joined_ms", "join_method"])
        .agg(
            pl.col("left_ms").first(),
            pl.col("leave_reason").first(),
        )
    )

    df: pl.DataFrame = (  # type: ignore  # ty limitation
        paired.join(durations, on="meeting_id", how="left")
        .with_columns(
            pl.col("left_ms").fill_null(pl.col("duration_ms")).alias("end_ms"),
            pl.col("left_ms").is_null().alias("still_present"),
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
    """Per-(display_name, meeting_id) presence ratio + per-state challenge counts."""
    duration_by_id = {m["meeting_id"]: m["duration_seconds"] for m in meetings}

    presence_rows: dict[tuple[str, str], float] = {}
    for key, segs in segments.items():
        total = 0
        for s in segs:
            total += s["end_ms"] - s["start_ms"]
        presence_rows[key] = total / 1_000.0

    chal_df: pl.DataFrame = (  # type: ignore  # ty limitation
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
    """Peak concurrent participant count per meeting (sweep-line on join/leave)."""
    joined = events.filter(pl.col("event_type") == "participant_joined").select(
        pl.col("meeting_id"),
        pl.col("timestamp").alias("t"),
        pl.lit(1, dtype=pl.Int64).alias("delta"),
    )
    left = events.filter(pl.col("event_type") == "participant_left").select(
        pl.col("meeting_id"),
        pl.col("timestamp").alias("t"),
        pl.lit(-1, dtype=pl.Int64).alias("delta"),
    )
    # descending=[..., True] on delta sorts +1 before -1 at the same timestamp
    # so a simultaneous join-and-leave doesn't undercount the moment.
    df: pl.DataFrame = (  # type: ignore  # ty limitation
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
    questions: pl.LazyFrame | None,
) -> dict[tuple[str, str], list[dict[str, Any]]]:
    """Per-(display_name, meeting_id) list of challenge markers.

    Issued challenges (correct/incorrect/unanswered) and skipped
    challenges are rendered as markers; the latter never reached the
    participant and so carry a ``skip_reason`` instead of question/
    answer fields.
    """
    durations = pl.LazyFrame(
        {
            "meeting_id": [m["meeting_id"] for m in meetings],
            "duration_ms": [int(m["duration_ms"]) for m in meetings],
        }
    )

    issued = (
        _challenge_frame(events)
        .join(durations, on="meeting_id", how="left")
        .with_columns(
            (pl.col("issued_ms") / pl.col("duration_ms") * 100.0)
            .clip(0.0, 100.0)
            .alias("x_pct"),
            pl.col("state").fill_null("unanswered").alias("result"),
            pl.lit("").alias("skip_reason"),
        )
    )

    if questions is not None:
        q = questions.select(
            pl.col("question_id"),
            pl.col("prompt").alias("question_prompt"),
            pl.col("question_type"),
            pl.col("choices"),
            pl.col("correct_answer").alias("question_correct_answer"),
            pl.col("match_mode"),
            pl.col("tolerance"),
        ).unique(subset=["question_id"])
        issued = issued.join(q, on="question_id", how="left")

    issued_df: pl.DataFrame = issued.sort(  # type: ignore  # ty limitation
        ["display_name", "meeting_id", "issued_ms"]
    ).collect()

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
                "prompt": row.get("question_prompt") or "",
                "question_type": row.get("question_type") or "",
                "choices": list(row.get("choices") or []),
                "correct_answer": _stringify_answer(row.get("question_correct_answer")),
                "match_mode": row.get("match_mode") or "",
                "tolerance": float(row["tolerance"])
                if row.get("tolerance") is not None
                else 0.0,
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
                "prompt": "",
                "question_type": "",
                "choices": [],
                "correct_answer": "",
                "match_mode": "",
                "tolerance": 0.0,
                "submitted_answer": "",
            }
        )
    for key, markers in out.items():
        markers.sort(key=lambda m: m["timestamp_ms"])
        out[key] = markers
    return out


def _collect_skipped(
    events: pl.LazyFrame, durations: pl.LazyFrame
) -> pl.DataFrame:
    """One row per challenge_skipped event with x_pct + skip_reason."""
    df: pl.DataFrame = (  # type: ignore  # ty limitation: collect() return includes InProcessQuery
        events.filter(pl.col("event_type") == "challenge_skipped")
        .select(
            pl.col("display_name"),
            pl.col("meeting_id"),
            pl.col("challenge_id"),
            pl.col("timestamp").alias("skipped_ms"),
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


def _stringify_answer(value: object) -> str:
    """Coerce a question's correct-answer cell to a single string.

    JSONL stores MCQ/short-text answers as JSON arrays and numeric answers
    as numbers; Polars surfaces them as Python list/float/str depending on
    inferred dtype. The GUI marker payload carries a single string field,
    so multi-choice arrays are re-encoded as JSON and numerics are
    stringified verbatim.
    """
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return json.dumps(value)
    return str(value)


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
