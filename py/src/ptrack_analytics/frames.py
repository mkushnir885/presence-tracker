"""Derived lazy frames over the raw event log.

Internal helpers (``presence_bands``, ``presence_closed``, ``meeting_times``,
``challenge_results``) carry raw ms offsets and feed the notebook-facing
``*_view`` builders. The views convert to ``Datetime``/``Duration``, pack
per-event metadata into struct columns, and clip open bands at the meeting's
end. Only the views are exported from :mod:`ptrack_analytics`.
"""

from __future__ import annotations

import polars as pl


def presence_bands(events: pl.LazyFrame) -> pl.LazyFrame:
    """Raw presence intervals, one row per join.

    The n-th join is paired with the n-th leave within each
    (display_name, meeting_id); a rejoin is its own band. left_ms is null for a
    band with no matching leave (still present at session end). Carries the
    join/leave metadata callers may need (join_method, leave_reason).

    This is the single pairing used by both the CSV report and the GUI stats so
    their presence numbers cannot drift.
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
    return joined.join(
        left, on=["display_name", "meeting_id", "pair_idx"], how="left"
    ).drop("pair_idx")


def presence_closed(events: pl.LazyFrame) -> pl.LazyFrame:
    """Presence bands with every band closed at the meeting's duration.

    An open band (no matching leave) or one whose leave landed past
    duration_ms is closed at duration_ms; still_present flags those rows. The
    closed end_ms is what both the CSV report and the GUI timeline use to
    compute presence seconds — clipping here keeps the two surfaces in sync.
    """
    durations = meeting_times(events).select(["meeting_id", "duration_ms"])
    return (
        presence_bands(events)
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
    left_at (Datetime), duration (Duration), still_present (Bool),
    join (Struct{method}), leave (Struct{reason}).
    """
    starts = meeting_times(events).select("meeting_id", "started_at")
    return (
        presence_closed(events)
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
        .select(
            "display_name",
            "meeting_id",
            "joined_at",
            "left_at",
            "duration",
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
