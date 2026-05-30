"""Derived lazy frames over the raw event log (raw ms offsets, no I/O).

A null left_ms is an open presence band: the participant was still present
at session end. Close it at the meetings frame's duration_ms when needed.
"""

from __future__ import annotations

import polars as pl


def presence(events: pl.LazyFrame) -> pl.LazyFrame:
    """One row per (display_name, meeting_id) presence interval."""
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

    # Pair each join with its earliest leave at or after it: cross all leaves
    # for the name, drop earlier ones, then take the first remaining per join.
    # A join with no later leave keeps left_ms null (still present at end).
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
    """One row per challenge_issued, annotated with its final state."""
    # Per-challenge fields (auto_submitted, latency, submitted_answer) live in
    # the JSON metadata column; pull them out with json_path_match.
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

    # Collapse the result event_type into a short state. The left join leaves
    # state null only for an issued challenge with no recorded outcome.
    result_events = result_events.with_columns(
        pl.col("state")
        .str.replace("challenge_answered_correct", "correct")
        .str.replace("challenge_answered_incorrect", "incorrect")
        .str.replace("challenge_unanswered", "unanswered")
    )

    return issued.join(result_events, on="challenge_id", how="left")
