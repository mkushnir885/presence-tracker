"""Tabular CSV report generation for single-meeting and cross-meeting views."""

from __future__ import annotations

import polars as pl

from ptrack_analytics.frames import (
    challenge_results as _challenge_frame,
)
from ptrack_analytics.frames import (
    meeting_times,
    presence_bands,
)


def generate_csv(events: pl.LazyFrame) -> str:
    """Single-meeting CSV: name, presence_ratio, challenges_correct/issued."""
    times = meeting_times(events)

    pres = (
        _presence_seconds(events, times)
        .join(
            times.select(["meeting_id", "duration_seconds"]),
            on="meeting_id",
            how="left",
        )
        .group_by("display_name")
        .agg(
            pl.col("presence_seconds").sum(),
            pl.col("duration_seconds").first(),
        )
    )

    chal = _challenge_stats(events, group_by=["display_name"])

    df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        pres.join(chal, on="display_name", how="left")
        .with_columns(
            _presence_ratio(),
            pl.col("challenges_issued").fill_null(0),
            pl.col("challenges_correct").fill_null(0),
        )
        .sort(pl.col("display_name").str.to_lowercase())
        .select(
            pl.col("display_name").alias("name"),
            pl.col("presence_ratio"),
            pl.col("challenges_correct"),
            pl.col("challenges_issued"),
        )
        .collect()
    )
    return df.write_csv()


def generate_aggregate_csv(events: pl.LazyFrame) -> str:
    """Cross-meeting CSV: adds a 'meeting' column (ISO-8601 UTC start)."""
    times = meeting_times(events)

    pres = _presence_seconds(events, times).join(
        times.select(["meeting_id", "started_at", "duration_seconds"]),
        on="meeting_id",
        how="left",
    )

    chal = _challenge_stats(events, group_by=["display_name", "meeting_id"])

    df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
        pres.join(chal, on=["display_name", "meeting_id"], how="left")
        .with_columns(
            _presence_ratio(),
            pl.col("challenges_issued").fill_null(0),
            pl.col("challenges_correct").fill_null(0),
            pl.col("started_at").dt.strftime("%Y-%m-%dT%H:%M:%SZ").alias("meeting"),
        )
        .sort([pl.col("display_name").str.to_lowercase(), pl.col("started_at")])
        .select(
            pl.col("display_name").alias("name"),
            pl.col("meeting"),
            pl.col("presence_ratio"),
            pl.col("challenges_correct"),
            pl.col("challenges_issued"),
        )
        .collect()
    )
    return df.write_csv()


def _presence_seconds(events: pl.LazyFrame, times: pl.LazyFrame) -> pl.LazyFrame:
    # Sum each (name, meeting) presence band from the shared pairing
    # (frames.presence_bands), closing an open or over-long band at duration_ms.
    # Shared with stats.py so CSV presence_ratio and the GUI timeline agree.
    durations = times.select(["meeting_id", "duration_ms"])
    return (
        presence_bands(events)
        .join(durations, on="meeting_id", how="left")
        .with_columns(
            pl.when(
                pl.col("left_ms").is_null()
                | (pl.col("left_ms") > pl.col("duration_ms"))
            )
            .then(pl.col("duration_ms"))
            .otherwise(pl.col("left_ms"))
            .alias("end_ms"),
        )
        .with_columns(
            ((pl.col("end_ms") - pl.col("joined_ms")) / 1_000.0).alias("band_seconds")
        )
        .group_by(["display_name", "meeting_id"])
        .agg(pl.col("band_seconds").sum().alias("presence_seconds"))
    )


def _challenge_stats(events: pl.LazyFrame, group_by: list[str]) -> pl.LazyFrame:
    """Count issued challenges and correct answers per group."""
    return (
        _challenge_frame(events)
        .group_by(group_by)
        .agg(
            pl.len().alias("challenges_issued"),
            (pl.col("state") == "correct")
            .sum()
            .cast(pl.Int64)
            .alias("challenges_correct"),
        )
    )


def _presence_ratio() -> pl.Expr:
    return (
        (pl.col("presence_seconds").fill_null(0.0) / pl.col("duration_seconds"))
        .clip(0.0, 1.0)
        .round(4)
        .alias("presence_ratio")
    )
