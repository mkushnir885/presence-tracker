"""Tabular CSV report generation for single-meeting and cross-meeting views."""

from __future__ import annotations

import polars as pl

from ptrack_analytics.frames import challenge_results as _challenge_frame


def generate_csv(events: pl.LazyFrame) -> str:
    """
    Generate a single-meeting CSV report.

    Expects *events* to contain data for exactly one meeting. For multiple
    meetings use generate_aggregate_csv instead.

    Output columns: name, presence_ratio, challenges_correct,
    challenges_issued. Sorted case-insensitively by name.
    """
    meeting_times = _meeting_times(events)

    pres = (
        _presence_seconds(events, meeting_times)
        .join(
            meeting_times.select(["meeting_id", "duration_seconds"]),
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

    df: pl.DataFrame = (  # type: ignore  # ty limitation: collect() return includes InProcessQuery
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
    """
    Generate a cross-meeting CSV report.

    Output columns: name, meeting, presence_ratio, challenges_correct,
    challenges_issued. 'meeting' is ISO-8601 UTC of the meeting start time.
    Sorted by name (case-insensitive) then meeting (chronological).
    """
    meeting_times = _meeting_times(events)

    pres = _presence_seconds(events, meeting_times).join(
        meeting_times.select(
            ["meeting_id", "started_at", "duration_seconds"]
        ),
        on="meeting_id",
        how="left",
    )

    chal = _challenge_stats(events, group_by=["display_name", "meeting_id"])

    df: pl.DataFrame = (  # type: ignore  # ty limitation: collect() return includes InProcessQuery
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


# ── helpers ──────────────────────────────────────────────────────────────────


def _meeting_times(events: pl.LazyFrame) -> pl.LazyFrame:
    """
    Per-meeting start time (absolute Datetime) and duration.

    Columns: meeting_id, started_at (Datetime UTC), duration_ms (Int64),
             duration_seconds (Float64, floored at 1 s).

    The session_started event stores an absolute Unix ms timestamp; all other
    events store ms offsets, so duration_ms is taken from session_ended when
    available, otherwise the max offset across non-started rows.
    """
    start = (
        events.filter(pl.col("event_type") == "session_started")
        .group_by("meeting_id")
        .agg(
            pl.from_epoch(pl.col("timestamp").first(), time_unit="ms").alias(
                "started_at"
            )
        )
    )
    ended = (
        events.filter(pl.col("event_type") == "session_ended")
        .group_by("meeting_id")
        .agg(pl.col("timestamp").first().alias("ended_ms"))
    )
    tail = (
        events.filter(pl.col("event_type") != "session_started")
        .group_by("meeting_id")
        .agg(pl.col("timestamp").max().alias("tail_ms"))
    )
    duration = tail.join(ended, on="meeting_id", how="left").select(
        pl.col("meeting_id"),
        pl.coalesce([pl.col("ended_ms"), pl.col("tail_ms")]).alias("duration_ms"),
    )
    return start.join(duration, on="meeting_id").with_columns(
        pl.when(pl.col("duration_ms") > 0)
        .then(pl.col("duration_ms") / 1_000.0)
        .otherwise(pl.lit(1.0))
        .alias("duration_seconds")
    )


def _presence_seconds(
    events: pl.LazyFrame, meeting_times: pl.LazyFrame
) -> pl.LazyFrame:
    """
    Per-(display_name, meeting_id) total presence time in seconds.

    Joins and leaves are paired positionally per (display_name, meeting_id):
    the n-th join matches the n-th leave so a rejoin becomes its own band.
    A surplus join (no matching leave) or a leave beyond meeting duration
    closes at duration_ms. Matches stats.py's segment math so CSV
    presence_ratio agrees with the GUI.
    """
    durations = meeting_times.select(["meeting_id", "duration_ms"])

    joined = (
        events.filter(pl.col("event_type") == "participant_joined")
        .select(
            pl.col("display_name"),
            pl.col("meeting_id"),
            pl.col("timestamp").alias("joined_ms"),
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
            pl.col("timestamp").alias("left_ms"),
        )
        .sort(["display_name", "meeting_id", "left_ms"])
        .with_columns(
            pl.int_range(pl.len(), dtype=pl.UInt32)
            .over(["display_name", "meeting_id"])
            .alias("pair_idx"),
        )
    )
    paired = joined.join(
        left, on=["display_name", "meeting_id", "pair_idx"], how="left"
    ).drop("pair_idx")

    return (
        paired.join(durations, on="meeting_id", how="left")
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
