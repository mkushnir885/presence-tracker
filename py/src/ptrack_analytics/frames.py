"""Internal raw-offset lazy frames over the event log.

These helpers (``presence_bands``, ``meeting_times``, ``challenge_results``)
carry raw ms offsets and feed both the notebook-facing views in
:mod:`ptrack_analytics.views` and the GUI/CSV aggregations in
:mod:`ptrack_py._frames`.
"""

from __future__ import annotations

from typing import cast

import polars as pl


def collect_df(lf: pl.LazyFrame) -> pl.DataFrame:
    """Eager-collect *lf* into a DataFrame. Centralizes the cast around
    polars' DataFrame | InProcessQuery union so call sites stay clean.
    """
    return cast(pl.DataFrame, lf.collect())


def presence_bands(events: pl.LazyFrame) -> pl.LazyFrame:
    """Presence intervals, one row per join, with every band closed at the
    meeting's duration.

    The n-th join is paired with the n-th leave within each
    (display_name, meeting_id); a rejoin is its own band. left_ms is null for a
    band with no matching leave (still present at session end). An open band
    or one whose leave landed past duration_ms is closed at duration_ms;
    still_present flags those rows. The closed end_ms is what both the CSV
    report and the GUI timeline use to compute presence seconds — clipping
    here keeps the two surfaces in sync.
    """
    joined = (
        events.filter(pl.col("event_type") == "participant_joined")
        .select(
            pl.col("display_name"),
            pl.col("meeting_id"),
            pl.col("from_start_ms").alias("joined_ms"),
            pl.col("metadata")
            .str.json_path_match("$.join_method")
            .alias("join_method"),
        )
        .sort(["display_name", "meeting_id", "joined_ms"])
        .with_columns(
            pl.int_range(pl.len(), dtype=pl.UInt32)
            .over(["display_name", "meeting_id"])
            .alias("pair_idx"),
        )
    )
    left = (
        events.filter(pl.col("event_type") == "participant_left")
        .select(
            pl.col("display_name"),
            pl.col("meeting_id"),
            pl.col("from_start_ms").alias("left_ms"),
            pl.col("metadata").str.json_path_match("$.reason").alias("leave_reason"),
        )
        .sort(["display_name", "meeting_id", "left_ms"])
        .with_columns(
            pl.int_range(pl.len(), dtype=pl.UInt32)
            .over(["display_name", "meeting_id"])
            .alias("pair_idx"),
        )
    )
    durations = meeting_times(events).select(["meeting_id", "duration_ms"])
    return (
        joined.join(left, on=["display_name", "meeting_id", "pair_idx"], how="left")
        .drop("pair_idx")
        .join(durations, on="meeting_id", how="left")
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
    )


def meeting_times(events: pl.LazyFrame) -> pl.LazyFrame:
    """Per-meeting start time and duration.

    started_at is the absolute meeting start, read from the session_started
    metadata "timestamp_ms" anchor (the from_start_ms column is 0 there).
    duration_ms prefers the session_ended offset, else the largest non-start
    event offset (both are ms from the session start). duration_seconds floors
    at 1.0 so presence ratios stay finite.
    """
    start = (
        events.filter(pl.col("event_type") == "session_started")
        .group_by("meeting_id")
        .agg(
            pl.from_epoch(
                pl.col("metadata")
                .str.json_path_match("$.timestamp_ms")
                .cast(pl.Int64)
                .first(),
                time_unit="ms",
            ).alias("started_at")
        )
    )
    ended = (
        events.filter(pl.col("event_type") == "session_ended")
        .group_by("meeting_id")
        .agg(pl.col("from_start_ms").first().alias("ended_ms"))
    )
    tail = (
        events.filter(pl.col("event_type") != "session_started")
        .group_by("meeting_id")
        .agg(pl.col("from_start_ms").max().alias("tail_ms"))
    )
    duration = tail.join(ended, on="meeting_id", how="left").select(
        pl.col("meeting_id"),
        pl.coalesce([pl.col("ended_ms"), pl.col("tail_ms")]).alias("duration_ms"),
    )
    return start.join(duration, on="meeting_id", how="left").with_columns(
        pl.when(pl.col("duration_ms") > 0)
        .then(pl.col("duration_ms") / 1_000.0)
        .otherwise(pl.lit(1.0))
        .alias("duration_seconds")
    )


def challenge_results(events: pl.LazyFrame) -> pl.LazyFrame:
    """One row per challenge_issued, annotated with its final state."""
    # Per-challenge fields (auto_submitted, latency, submitted_answer) live in
    # the JSON metadata column; pull them out with json_path_match.
    issued = events.filter(pl.col("event_type") == "challenge_issued").select(
        pl.col("display_name"),
        pl.col("meeting_id"),
        pl.col("from_start_ms").alias("issued_ms"),
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
