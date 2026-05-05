"""
Derived lazy frames computed from the raw event log.

All functions take the raw event LazyFrame and return a new LazyFrame.
They do not read files or call collect(); that is the caller's responsibility.

timestamp column semantics (schema v2): the meeting_started row stores an
absolute Unix ms value; every other row stores a ms offset from that anchor.
The frames below work with raw offsets — callers that need wall-clock times
should join against the meetings frame which exposes started_at (Datetime).
"""

from __future__ import annotations

import polars as pl


def presence(events: pl.LazyFrame) -> pl.LazyFrame:
    """
    Derive presence intervals: one row per (participant_id, meeting_id, interval).

    Columns: participant_id, meeting_id, joined_ms, left_ms, presence_seconds,
             display_name.
    joined_ms and left_ms are ms offsets from the meeting start.
    left_ms is null if the participant was still present when the meeting ended.
    """
    joined = events.filter(pl.col("event_type") == "participant_joined").select(
        pl.col("participant_id"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("joined_ms"),
        pl.col("display_name"),
    )
    left = events.filter(pl.col("event_type") == "participant_left").select(
        pl.col("participant_id"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("left_ms"),
    )

    # Pair joins with their nearest following leave in the same meeting.
    return (
        joined.join(left, on=["participant_id", "meeting_id"], how="left")
        .filter(
            pl.col("left_ms").is_null() | (pl.col("left_ms") >= pl.col("joined_ms"))
        )
        .sort(["participant_id", "meeting_id", "joined_ms", "left_ms"])
        .group_by(["participant_id", "meeting_id", "joined_ms"])
        .agg(pl.col("left_ms").first(), pl.col("display_name").first())
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

    Columns: participant_id, meeting_id, challenge_id, question_id,
             challenge_type, issued_ms, state, latency_ms.
    issued_ms is a ms offset from the meeting start.
    """
    issued = events.filter(pl.col("event_type") == "challenge_issued").select(
        pl.col("participant_id"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("issued_ms"),
        # Extract from JSON metadata
        pl.col("metadata").str.json_path_match("$.challenge_id").alias("challenge_id"),
        pl.col("metadata").str.json_path_match("$.question_id").alias("question_id"),
        pl.col("metadata")
        .str.json_path_match("$.challenge_type")
        .alias("challenge_type"),
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
        pl.col("metadata").str.json_path_match("$.challenge_id").alias("challenge_id"),
        pl.col("event_type").alias("state"),
        pl.col("metadata")
        .str.json_path_match("$.latency_ms")
        .cast(pl.Int64, strict=False)
        .alias("latency_ms"),
    )

    result_events = result_events.with_columns(
        pl.col("state")
        .str.replace("challenge_answered_correct", "correct")
        .str.replace("challenge_answered_incorrect", "incorrect")
        .str.replace("challenge_unanswered", "unanswered")
    )

    return issued.join(result_events, on="challenge_id", how="left")
