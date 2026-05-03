"""Tabular CSV report generation for single-meeting and cross-meeting views."""

from __future__ import annotations

import polars as pl

from .frames import challenge_results as _challenge_frame
from .frames import presence as _presence_frame


def generate_csv(events: pl.LazyFrame) -> str:
    """
    Generate a single-meeting CSV report.

    Expects *events* to contain data for exactly one meeting. For multiple
    meetings use generate_aggregate_csv instead.

    Output columns: display_name, presence_ratio, challenges_issued,
    challenges_correct. Sorted case-insensitively by display_name.
    """
    meeting_times = _meeting_times(events)

    pres = (
        _presence_frame(events)
        .join(
            meeting_times.select(["meeting_id", "ended_at", "duration_seconds"]),
            on="meeting_id",
            how="left",
        )
        .with_columns(
            pl.when(pl.col("left_at").is_null())
            .then((pl.col("ended_at") - pl.col("joined_at")).dt.total_seconds())
            .otherwise(pl.col("presence_seconds"))
            .alias("presence_seconds")
        )
        .group_by("participant_id")
        .agg(
            pl.col("presence_seconds").sum(),
            pl.col("duration_seconds").first(),
            pl.col("display_name").drop_nulls().last(),
        )
    )

    chal = _challenge_stats(events, group_by=["participant_id"])

    df: pl.DataFrame = (  # type: ignore  # ty limitation: collect() return includes InProcessQuery
        pres.join(chal, on="participant_id", how="left")
        .with_columns(
            _presence_ratio(),
            pl.col("challenges_issued").fill_null(0),
            pl.col("challenges_correct").fill_null(0),
            pl.col("display_name").fill_null("(unknown)"),
        )
        .sort(pl.col("display_name").str.to_lowercase())
        .select(
            ["display_name", "presence_ratio",
             "challenges_issued", "challenges_correct"]
        )
        .collect()
    )
    return df.write_csv()


def generate_aggregate_csv(events: pl.LazyFrame) -> str:
    """
    Generate a cross-meeting CSV report.

    Output columns: display_name, meeting, presence_ratio, challenges_issued,
    challenges_correct. 'meeting' is ISO-8601 UTC of the meeting start time.
    Sorted by display_name (case-insensitive) then meeting (chronological).
    """
    meeting_times = _meeting_times(events)

    pres = (
        _presence_frame(events)
        .join(
            meeting_times.select(
                ["meeting_id", "started_at", "ended_at", "duration_seconds"]
            ),
            on="meeting_id",
            how="left",
        )
        .with_columns(
            pl.when(pl.col("left_at").is_null())
            .then((pl.col("ended_at") - pl.col("joined_at")).dt.total_seconds())
            .otherwise(pl.col("presence_seconds"))
            .alias("presence_seconds")
        )
        .group_by(["participant_id", "meeting_id"])
        .agg(
            pl.col("presence_seconds").sum(),
            pl.col("duration_seconds").first(),
            pl.col("started_at").first(),
            pl.col("display_name").drop_nulls().last(),
        )
    )

    chal = _challenge_stats(events, group_by=["participant_id", "meeting_id"])

    df: pl.DataFrame = (  # type: ignore  # ty limitation: collect() return includes InProcessQuery
        pres.join(chal, on=["participant_id", "meeting_id"], how="left")
        .with_columns(
            _presence_ratio(),
            pl.col("challenges_issued").fill_null(0),
            pl.col("challenges_correct").fill_null(0),
            pl.col("display_name").fill_null("(unknown)"),
            pl.col("started_at").dt.strftime("%Y-%m-%dT%H:%M:%SZ").alias("meeting"),
        )
        .sort([pl.col("display_name").str.to_lowercase(), pl.col("started_at")])
        .select(
            ["display_name", "meeting", "presence_ratio",
             "challenges_issued", "challenges_correct"]
        )
        .collect()
    )
    return df.write_csv()


# ── helpers ──────────────────────────────────────────────────────────────────


def _meeting_times(events: pl.LazyFrame) -> pl.LazyFrame:
    """Per-meeting start/end/duration; duration is floored at 1 second."""
    return events.group_by("meeting_id").agg(
        pl.col("timestamp").min().alias("started_at"),
        pl.col("timestamp").max().alias("ended_at"),
    ).with_columns(
        pl.when(
            (pl.col("ended_at") - pl.col("started_at")).dt.total_seconds() > 0
        )
        .then((pl.col("ended_at") - pl.col("started_at")).dt.total_seconds())
        .otherwise(pl.lit(1.0))
        .alias("duration_seconds")
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
