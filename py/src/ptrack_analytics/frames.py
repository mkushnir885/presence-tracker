"""
Derived lazy frames computed from the raw event log.

All functions take the raw event LazyFrame and return a new LazyFrame.
They do not read files or call collect(); that is the caller's responsibility.
"""

from __future__ import annotations

import polars as pl


def presence(events: pl.LazyFrame) -> pl.LazyFrame:
    """
    Derive presence intervals: one row per (participant_id, meeting_id, interval).

    Columns: participant_id, meeting_id, joined_at, left_at, presence_seconds.
    left_at is null if the participant was still present when the meeting ended.
    """
    joined = events.filter(pl.col("event_type") == "participant_joined").select(
        pl.col("participant_id"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("joined_at"),
        pl.col("display_name"),
    )
    left = events.filter(pl.col("event_type") == "participant_left").select(
        pl.col("participant_id"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("left_at"),
    )

    # Pair joins with their nearest following leave in the same meeting.
    # Simple approach: for each joined event, find the minimum left_at that
    # is >= joined_at for the same participant+meeting.
    combined = (
        joined.join(left, on=["participant_id", "meeting_id"], how="left")
        .filter(
            pl.col("left_at").is_null() | (pl.col("left_at") >= pl.col("joined_at"))
        )
        .sort(["participant_id", "meeting_id", "joined_at", "left_at"])
        .group_by(["participant_id", "meeting_id", "joined_at"])
        .agg(pl.col("left_at").first(), pl.col("display_name").first())
        .with_columns(
            pl.when(pl.col("left_at").is_not_null())
            .then((pl.col("left_at") - pl.col("joined_at")).dt.total_seconds())
            .otherwise(None)
            .alias("presence_seconds")
        )
    )
    return combined


def challenge_results(events: pl.LazyFrame) -> pl.LazyFrame:
    """
    One row per challenge_issued event, annotated with its final state.

    Columns: participant_id, meeting_id, challenge_id, question_id,
             challenge_type, issued_at, state, latency_ms.
    """
    issued = events.filter(pl.col("event_type") == "challenge_issued").select(
        pl.col("participant_id"),
        pl.col("meeting_id"),
        pl.col("timestamp").alias("issued_at"),
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

    # Normalise state to correct/incorrect/unanswered.
    result_events = result_events.with_columns(
        pl.col("state")
        .str.replace("challenge_answered_correct", "correct")
        .str.replace("challenge_answered_incorrect", "incorrect")
        .str.replace("challenge_unanswered", "unanswered")
    )

    return issued.join(result_events, on="challenge_id", how="left")
