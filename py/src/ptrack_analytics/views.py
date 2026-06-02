"""Notebook-facing lazy frames exported via :mod:`ptrack_analytics`.

The views convert raw ms offsets from the internal helpers in
:mod:`ptrack_analytics.frames` to ``Datetime`` / ``Duration`` and pack
per-event metadata into struct columns.
"""

from __future__ import annotations

import polars as pl

from .frames import challenge_results, meeting_times, presence_bands

_CHALLENGE_STATE = pl.Enum(["correct", "incorrect", "unanswered"])
_DT_MS_UTC = pl.Datetime(time_unit="ms", time_zone="UTC")
_DUR_MS = pl.Duration(time_unit="ms")


def _ms_after(start: pl.Expr, offset_ms: pl.Expr) -> pl.Expr:
    """Datetime("ms","UTC") at start + offset_ms. Polars datetime + duration
    arithmetic widens to microseconds; we cast back so the public schema
    stays a stable Datetime("ms","UTC").
    """
    return (start + pl.duration(milliseconds=offset_ms)).cast(_DT_MS_UTC)


def meetings_view(events: pl.LazyFrame) -> pl.LazyFrame:
    """Notebook-facing per-meeting frame.

    Columns: meeting_id, platform, started_at (Datetime),
    ended_at (Datetime), duration (Duration), start (Struct{cause}),
    end (Struct{cause}).
    """
    times = meeting_times(events).select("meeting_id", "started_at", "duration_ms")
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
            pl.col("started_at").cast(_DT_MS_UTC),
            _ms_after(pl.col("started_at"), pl.col("duration_ms")).alias("ended_at"),
            pl.duration(milliseconds=pl.col("duration_ms"))
            .cast(_DUR_MS)
            .alias("duration"),
            pl.struct(pl.col("start_cause").alias("cause")).alias("start"),
            pl.struct(pl.col("end_cause").alias("cause")).alias("end"),
        )
        .select(
            "meeting_id",
            "platform",
            "started_at",
            "ended_at",
            "duration",
            "start",
            "end",
        )
    )


def presence_view(events: pl.LazyFrame) -> pl.LazyFrame:
    """Notebook-facing per-band presence frame.

    One row per join; rejoins produce additional rows. Open bands (no
    matching leave) are clipped at the meeting's end and flagged with
    still_present, so left_at and duration are always populated.

    Columns: display_name, meeting_id, joined_at (Datetime),
    left_at (Datetime), duration (Duration),
    total_duration (Duration; sum of all bands for this participant in this
    meeting), still_present (Bool), join (Struct{method}),
    leave (Struct{reason}).
    """
    starts = meeting_times(events).select("meeting_id", "started_at")
    return (
        presence_bands(events)
        .join(starts, on="meeting_id", how="left")
        .with_columns(
            _ms_after(pl.col("started_at"), pl.col("joined_ms")).alias("joined_at"),
            _ms_after(pl.col("started_at"), pl.col("end_ms")).alias("left_at"),
            pl.duration(milliseconds=pl.col("end_ms") - pl.col("joined_ms"))
            .cast(_DUR_MS)
            .alias("duration"),
            pl.struct(pl.col("join_method").alias("method")).alias("join"),
            pl.struct(pl.col("leave_reason").alias("reason")).alias("leave"),
        )
        .with_columns(
            pl.col("duration")
            .sum()
            .over(["display_name", "meeting_id"])
            .alias("total_duration"),
        )
        .select(
            "display_name",
            "meeting_id",
            "joined_at",
            "left_at",
            "duration",
            "total_duration",
            "still_present",
            "join",
            "leave",
        )
    )


def challenges_view(events: pl.LazyFrame) -> pl.LazyFrame:
    """Notebook-facing per-challenge frame.

    One row per challenge_issued. answered_at, latency, and
    submitted_answer are null when state == "unanswered" — no answer
    was submitted, so there is no real instant to record. Question
    payload is intentionally not joined in: it lives in the questions
    frame and is reused across rows via question_id.

    Columns: display_name, meeting_id, challenge_id, question_id,
    issued_at (Datetime), answered_at (Datetime, nullable),
    latency (Duration, nullable), state (Enum),
    submitted_answer (Utf8, nullable), auto_submitted (Bool).
    """
    starts = meeting_times(events).select("meeting_id", "started_at")
    answered = pl.col("state").is_in(["correct", "incorrect"])
    return (
        challenge_results(events)
        .join(starts, on="meeting_id", how="left")
        .with_columns(
            _ms_after(pl.col("started_at"), pl.col("issued_ms")).alias("issued_at"),
            pl.when(answered)
            .then(
                _ms_after(
                    pl.col("started_at"), pl.col("issued_ms") + pl.col("latency_ms")
                )
            )
            .otherwise(None)
            .alias("answered_at"),
            pl.when(answered)
            .then(pl.duration(milliseconds=pl.col("latency_ms")).cast(_DUR_MS))
            .otherwise(None)
            .alias("latency"),
            pl.when(answered)
            .then(pl.col("submitted_answer"))
            .otherwise(None)
            .alias("submitted_answer"),
        )
        .with_columns(
            pl.col("state")
            .fill_null("unanswered")
            .cast(_CHALLENGE_STATE)
            .alias("state"),
        )
        .select(
            "display_name",
            "meeting_id",
            "challenge_id",
            "question_id",
            "issued_at",
            "answered_at",
            "latency",
            "state",
            "submitted_answer",
            "auto_submitted",
        )
    )
