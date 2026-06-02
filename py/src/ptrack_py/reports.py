from __future__ import annotations

import polars as pl

from ptrack_analytics.frames import collect_df, meeting_times
from ptrack_py._frames import challenge_stats, presence_totals


def generate_csv(events: pl.LazyFrame, cross_meeting: bool = False) -> str:
    times = meeting_times(events)
    by = ["display_name", "meeting_id"] if cross_meeting else ["display_name"]

    pres = presence_totals(events).join(
        times.select(
            ["meeting_id", "started_at", "duration_seconds"]
            if cross_meeting
            else ["meeting_id", "duration_seconds"]
        ),
        on="meeting_id",
        how="left",
    )
    if not cross_meeting:
        pres = pres.group_by("display_name").agg(
            pl.col("presence_seconds").sum(),
            pl.col("duration_seconds").first(),
        )

    chal = challenge_stats(events, by=by).select(
        *by, "challenges_issued", "challenges_correct"
    )

    base = pres.join(chal, on=by, how="left").with_columns(
        (pl.col("presence_seconds").fill_null(0.0) / pl.col("duration_seconds"))
        .clip(0.0, 1.0)
        .round(4)
        .alias("presence_ratio"),
        pl.col("challenges_issued").fill_null(0),
        pl.col("challenges_correct").fill_null(0),
    )

    if cross_meeting:
        df = collect_df(
            base.with_columns(
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
        )
    else:
        df = collect_df(
            base.sort(pl.col("display_name").str.to_lowercase()).select(
                pl.col("display_name").alias("name"),
                pl.col("presence_ratio"),
                pl.col("challenges_correct"),
                pl.col("challenges_issued"),
            )
        )
    return df.write_csv()
