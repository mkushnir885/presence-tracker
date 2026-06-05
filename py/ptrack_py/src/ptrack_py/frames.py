from __future__ import annotations

import polars as pl

from ptrack_analytics.frames import challenge_results, presence_bands


def presence_totals(events: pl.LazyFrame) -> pl.LazyFrame:
    """Per (display_name, meeting_id) total presence seconds. The single
    source for "how long was X present in Y" — shared by the CSV report
    and the GUI stats payload so they cannot drift.
    """
    return (
        presence_bands(events)
        .with_columns(
            ((pl.col("end_ms") - pl.col("joined_ms")) / 1_000.0).alias("band_seconds")
        )
        .group_by(["display_name", "meeting_id"])
        .agg(pl.col("band_seconds").sum().alias("presence_seconds"))
    )


def challenge_stats(events: pl.LazyFrame, by: list[str]) -> pl.LazyFrame:
    """Per-group challenge counts: ``issued``, ``correct``,
    ``incorrect``, ``unanswered``. *by* is the group-by key.
    """
    return (
        challenge_results(events)
        .filter(pl.col("state") != "skipped")
        .group_by(by)
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
    )


def concurrent_participants(events: pl.LazyFrame) -> pl.LazyFrame:
    """Peak concurrent participants per meeting via a sweep-line.

    Sorting delta descending puts joins (+1) before leaves (-1) at the
    same instant, so a simultaneous swap still counts the peak.
    """
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
    return (
        pl.concat([joined, left])
        .sort(["meeting_id", "t", "delta"], descending=[False, False, True])
        .with_columns(pl.col("delta").cum_sum().over("meeting_id").alias("concurrent"))
        .group_by("meeting_id")
        .agg(pl.col("concurrent").max().alias("max_participants"))
    )
