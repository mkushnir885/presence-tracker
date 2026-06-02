from __future__ import annotations

import polars as pl

from .frames import (
    _TZ,
    challenge_results,
    meeting_times,
    presence_bands,
)

_CHALLENGE_STATE = pl.Enum(["correct", "incorrect", "unanswered", "skipped"])
_DUR_MS = pl.Duration(time_unit="ms")


def _to_local_starts(events: pl.LazyFrame) -> pl.LazyFrame:
    # meeting_times keeps started_at UTC for stats; views retag to local.
    return meeting_times(events).with_columns(
        pl.col("started_at").dt.convert_time_zone(_TZ()).dt.cast_time_unit("ms"),
    )


def _ms_after(start: pl.Expr, offset_ms: pl.Expr) -> pl.Expr:
    # Polars widens datetime + duration to microseconds; cast back so
    # every emitted column stays Datetime("ms", _TZ()).
    return (start + pl.duration(milliseconds=offset_ms)).cast(
        pl.Datetime(time_unit="ms", time_zone=_TZ())
    )


def meetings_view(events: pl.LazyFrame) -> pl.LazyFrame:
    times = _to_local_starts(events).select("meeting_id", "started_at", "duration_ms")
    start_meta = (
        events.filter(pl.col("event_type") == "session_started")
        .group_by("meeting_id")
        .agg(
            pl.col("metadata")
            .str.json_path_match("$.platform")
            .first()
            .alias("platform"),
            pl.col("metadata")
            .str.json_path_match("$.cause")
            .first()
            .alias("start_cause"),
        )
    )
    end_meta = (
        events.filter(pl.col("event_type") == "session_ended")
        .group_by("meeting_id")
        .agg(
            pl.col("metadata")
            .str.json_path_match("$.cause")
            .first()
            .alias("end_cause"),
        )
    )
    return (
        times.join(start_meta, on="meeting_id", how="left")
        .join(end_meta, on="meeting_id", how="left")
        .with_columns(
            _ms_after(pl.col("started_at"), pl.col("duration_ms")).alias("ended_at"),
            pl.duration(milliseconds=pl.col("duration_ms"))
            .cast(_DUR_MS)
            .alias("duration"),
        )
        .select(
            "meeting_id",
            "platform",
            "started_at",
            "ended_at",
            "duration",
            "start_cause",
            "end_cause",
        )
    )


def presence_view(events: pl.LazyFrame) -> pl.LazyFrame:
    times = _to_local_starts(events).select("meeting_id", "started_at", "duration_ms")
    return (
        presence_bands(events)
        .join(times, on="meeting_id", how="left")
        .with_columns(
            _ms_after(pl.col("started_at"), pl.col("joined_ms")).alias("joined_at"),
            _ms_after(pl.col("started_at"), pl.col("end_ms")).alias("left_at"),
            pl.duration(milliseconds=pl.col("end_ms") - pl.col("joined_ms"))
            .cast(_DUR_MS)
            .alias("duration"),
        )
        .sort(["display_name", "meeting_id", "joined_at"])
        .with_columns(
            pl.struct(
                "joined_at", "left_at", "duration", "join_method", "leave_reason"
            ).alias("band"),
        )
        .group_by(["display_name", "meeting_id"], maintain_order=True)
        .agg(
            pl.col("duration").sum().alias("total_duration"),
            pl.col("present_till_end").any().alias("present_till_end"),
            pl.col("band").alias("bands"),
            pl.col("duration_ms").first().alias("duration_ms"),
        )
        .with_columns(
            pl.when(pl.col("duration_ms") > 0)
            .then(
                pl.col("total_duration").dt.total_milliseconds() / pl.col("duration_ms")
            )
            .otherwise(0.0)
            .clip(0.0, 1.0)
            .alias("ratio"),
        )
        .select(
            "display_name",
            "meeting_id",
            "total_duration",
            "ratio",
            "present_till_end",
            "bands",
        )
    )


def challenges_view(events: pl.LazyFrame) -> pl.LazyFrame:
    starts = _to_local_starts(events).select("meeting_id", "started_at")
    answered = pl.col("state").is_in(["correct", "incorrect"])
    return (
        challenge_results(events)
        .join(starts, on="meeting_id", how="left")
        .with_columns(
            _ms_after(pl.col("started_at"), pl.col("fired_ms")).alias("fired_at"),
            pl.when(answered)
            .then(
                _ms_after(
                    pl.col("started_at"), pl.col("fired_ms") + pl.col("latency_ms")
                )
            )
            .otherwise(None)
            .alias("answered_at"),
            pl.when(answered)
            .then(pl.duration(milliseconds=pl.col("latency_ms")).cast(_DUR_MS))
            .otherwise(None)
            .alias("latency"),
        )
        .with_columns(pl.col("state").cast(_CHALLENGE_STATE).alias("state"))
        .select(
            "display_name",
            "meeting_id",
            "challenge_id",
            "question_id",
            "fired_at",
            "answered_at",
            "latency",
            "state",
            "submitted_answer",
            "skip_reason",
            "auto_submitted",
        )
    )
