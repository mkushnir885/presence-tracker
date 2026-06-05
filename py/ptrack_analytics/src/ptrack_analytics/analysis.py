from __future__ import annotations

from typing import cast

import polars as pl


def plot_concurrent_participants(
    presence: pl.LazyFrame, meetings: pl.LazyFrame
) -> None:
    """Step chart of the number of participants present over time, one
    figure per meeting.

    Example::

        from ptrack_analytics import load, meetings, presence
        from ptrack_analytics.analysis import plot_concurrent_participants

        load("~/Documents/ptrack/meetings/2026-04-*")
        plot_concurrent_participants(presence, meetings)
    """
    import matplotlib.pyplot as plt

    bands = cast(
        pl.DataFrame,
        presence.explode("bands")
        .unnest("bands")
        .select("meeting_id", "joined_at", "left_at")
        .collect(),
    )
    schedule = cast(
        pl.DataFrame,
        meetings.select("meeting_id", "started_at", "ended_at").collect(),
    )

    for row in schedule.iter_rows(named=True):
        m = bands.filter(pl.col("meeting_id") == row["meeting_id"])
        deltas = (
            pl.concat(
                [
                    m.select(
                        pl.col("joined_at").alias("t"),
                        pl.lit(1, dtype=pl.Int64).alias("d"),
                    ),
                    m.select(
                        pl.col("left_at").alias("t"),
                        pl.lit(-1, dtype=pl.Int64).alias("d"),
                    ),
                ]
            )
            .sort(["t", "d"], descending=[False, True])
            .with_columns(pl.col("d").cum_sum().alias("count"))
        )

        times = [row["started_at"], *deltas["t"].to_list(), row["ended_at"]]
        counts = [0, *deltas["count"].to_list(), 0]

        fig, ax = plt.subplots(figsize=(10, 3))
        ax.step(times, counts, where="post")
        ax.set_title(f"Concurrent participants — {row['meeting_id']}")
        ax.set_xlabel("time")
        ax.set_ylabel("participants")
        ax.set_ylim(bottom=0)
        fig.autofmt_xdate()
        plt.show()


def challenge_accuracy(challenges: pl.LazyFrame) -> pl.DataFrame:
    """Per-participant correct-answer ratio across the loaded meetings.
    Skipped challenges are excluded; accuracy is ``correct / issued``.

    Example::

        from ptrack_analytics import challenges, load
        from ptrack_analytics.analysis import challenge_accuracy

        load("~/Documents/ptrack/meetings/2026-04-*")
        challenge_accuracy(challenges)
    """
    return cast(
        pl.DataFrame,
        challenges.filter(pl.col("state") != "skipped")
        .group_by("display_name")
        .agg(
            (pl.col("state") == "correct").sum().alias("correct"),
            pl.len().alias("issued"),
        )
        .with_columns((pl.col("correct") / pl.col("issued")).alias("accuracy"))
        .sort("display_name")
        .collect(),
    )


def plot_presence_heatmap(
    presence: pl.LazyFrame,
    meetings: pl.LazyFrame,
    display_name: str | None = None,
) -> None:
    """Heatmap of presence ratio with one horizontal row per participant
    and one column per meeting; each cell is labeled with its ratio.
    Pass *display_name* to keep only that participant's row.

    Example::

        from ptrack_analytics import load, meetings, presence
        from ptrack_analytics.analysis import plot_presence_heatmap

        load("~/Documents/ptrack/meetings/2026-04-*")
        plot_presence_heatmap(presence, meetings)
        plot_presence_heatmap(presence, meetings, display_name="Alice")
    """
    import matplotlib.pyplot as plt

    schedule = cast(
        pl.DataFrame,
        meetings.select("meeting_id", "started_at").sort("started_at").collect(),
    )
    meeting_ids = schedule["meeting_id"].to_list()
    labels = [t.strftime("%Y-%m-%d %H:%M") for t in schedule["started_at"].to_list()]

    rows = presence.select("display_name", "meeting_id", "ratio")
    if display_name is not None:
        rows = rows.filter(pl.col("display_name") == display_name)
    pivoted = cast(pl.DataFrame, rows.collect()).pivot(
        on="meeting_id", index="display_name", values="ratio"
    )
    for mid in meeting_ids:
        if mid not in pivoted.columns:
            pivoted = pivoted.with_columns(pl.lit(None, dtype=pl.Float64).alias(mid))
    pivoted = (
        pivoted.select(["display_name", *meeting_ids])
        .fill_null(0.0)
        .sort("display_name")
    )

    names = pivoted["display_name"].to_list()
    data = pivoted.drop("display_name").to_numpy()

    fig, ax = plt.subplots(
        figsize=(max(len(meeting_ids) * 1.2, 4), max(len(names) * 0.5 + 1, 2))
    )
    im = ax.imshow(data, aspect="auto", vmin=0.0, vmax=1.0, cmap="RdYlGn")
    ax.set_xticks(range(len(meeting_ids)))
    ax.set_xticklabels(labels, rotation=45, ha="right")
    ax.set_yticks(range(len(names)))
    ax.set_yticklabels(names)
    for i in range(len(names)):
        for j in range(len(meeting_ids)):
            ax.text(j, i, f"{data[i][j]:.2f}", ha="center", va="center", fontsize=8)
    fig.colorbar(im, ax=ax, label="presence ratio")
    fig.tight_layout()
    plt.show()
