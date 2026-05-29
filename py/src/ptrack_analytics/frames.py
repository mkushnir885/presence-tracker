"""
Derived lazy frames computed from the raw event log.

All functions take the raw event LazyFrame and return a new LazyFrame.
They do not read files or call collect(); that is the caller's responsibility.

timestamp column semantics: the session_started row stores an absolute Unix
ms value; every other row stores a ms offset from that anchor. The frames
below work with raw offsets — callers that need wall-clock times should
join against the meetings frame which exposes started_at (Datetime).

Open presence bands (no participant_left after the last participant_joined)
are intentional and indicate the participant was still present at session
end. The presence frame leaves left_ms null in that case so consumers can
distinguish observed leaves from implicit ones; close at the meetings
frame's duration_ms when a closed band is required.

display_name is the participant identity end to end.
"""

from __future__ import annotations

import polars as pl


def presence(events: pl.LazyFrame) -> pl.LazyFrame:
    """
    Derive presence intervals: one row per (display_name, meeting_id, interval).

    Columns: display_name, meeting_id, joined_ms, left_ms, presence_seconds.
    joined_ms and left_ms are ms offsets from session start.
    left_ms is null if the participant was still present at session end.
    """
    joined = events.filter(pl.col("event_type") == "participant_joined").select(
        pl.col("display_name"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("joined_ms"),
    )
    left = events.filter(pl.col("event_type") == "participant_left").select(
        pl.col("display_name"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("left_ms"),
    )

    # Pair joins with their nearest following leave in the same meeting.
    return (
        joined.join(left, on=["display_name", "meeting_id"], how="left")
        .filter(
            pl.col("left_ms").is_null() | (pl.col("left_ms") >= pl.col("joined_ms"))
        )
        .sort(["display_name", "meeting_id", "joined_ms", "left_ms"])
        .group_by(["display_name", "meeting_id", "joined_ms"])
        .agg(pl.col("left_ms").first())
        .with_columns(
            pl.when(pl.col("left_ms").is_not_null())
            .then((pl.col("left_ms") - pl.col("joined_ms")) / 1_000.0)
            .otherwise(None)
            .alias("presence_seconds")
        )
    )


def challenge_results(events: pl.LazyFrame) -> pl.LazyFrame:
    """
    One row per challenge_issued event, annotated with its final state.

    Columns: display_name, meeting_id, challenge_id, question_id,
             auto_submitted, issued_ms, state, latency_ms, submitted_answer.
    issued_ms is a ms offset from the meeting start.
    submitted_answer is the student's verbatim response (JSON-encoded
    array for MCQ, plain text otherwise); empty string when no answer
    was submitted.
    """
    issued = events.filter(pl.col("event_type") == "challenge_issued").select(
        pl.col("display_name"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("issued_ms"),
        pl.col("challenge_id"),
        pl.col("question_id"),
        pl.col("metadata")
        .str.json_path_match("$.auto_submitted")
        .eq("true")
        .alias("auto_submitted"),
    )

    result_events = events.filter(
        pl.col("event_type").is_in(
            [
                "challenge_answered_correct",
                "challenge_answered_incorrect",
                "challenge_unanswered",
            ]
        )
    ).select(
        pl.col("challenge_id"),
        pl.col("event_type").alias("state"),
        pl.col("metadata")
        .str.json_path_match("$.latency_ms")
        .cast(pl.Int64, strict=False)
        .alias("latency_ms"),
        pl.col("metadata")
        .str.json_path_match("$.submitted_answer")
        .fill_null("")
        .alias("submitted_answer"),
    )

    result_events = result_events.with_columns(
        pl.col("state")
        .str.replace("challenge_answered_correct", "correct")
        .str.replace("challenge_answered_incorrect", "incorrect")
        .str.replace("challenge_unanswered", "unanswered")
    )

    return issued.join(result_events, on="challenge_id", how="left")
