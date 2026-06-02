from __future__ import annotations

import glob as _glob
from collections.abc import Iterable
from pathlib import Path

import polars as pl

from .frames import collect_df
from .schema import EVENT_SCHEMA, QUESTIONS_SCHEMA

EVENTS_FILE = "events.parquet"
QUESTIONS_FILE = "questions.jsonl"

_GLOB_CHARS = "*?["


def resolve_meetings(*patterns: str) -> list[Path]:
    if not patterns:
        raise ValueError("no patterns given")

    seen: set[str] = set()
    dirs: list[Path] = []
    for pattern in patterns:
        matches = sorted(_glob.glob(str(Path(pattern).expanduser())))
        if not matches and not any(ch in pattern for ch in _GLOB_CHARS):
            raise FileNotFoundError(f"meeting dir not found: {pattern}")
        for m in matches:
            p = Path(m)
            if not p.is_dir() or not (p / EVENTS_FILE).exists():
                continue
            resolved = str(p.resolve())
            if resolved in seen:
                continue
            seen.add(resolved)
            dirs.append(p)

    if not dirs:
        raise FileNotFoundError(f"no meeting directories matched: {' '.join(patterns)}")
    return dirs


def load_events(meeting_dirs: Iterable[Path | str]) -> pl.LazyFrame:
    schema = pl.Schema(EVENT_SCHEMA)
    frames: list[pl.LazyFrame] = []
    for d in meeting_dirs:
        path = Path(d) / EVENTS_FILE
        lf = pl.scan_parquet(str(path), schema=schema)
        ended = collect_df(
            lf.filter(pl.col("event_type") == "session_ended").select(pl.len())
        )
        if int(ended.item()) == 0:
            raise ValueError(
                f"{path}: meeting events are invalid (no session_ended event)."
            )
        frames.append(lf)
    return pl.concat(frames)


def load_questions(meeting_dirs: Iterable[Path | str]) -> pl.LazyFrame:
    inner_schema = {k: v for k, v in QUESTIONS_SCHEMA.items() if k != "question_id"}
    empty = pl.LazyFrame(
        schema={
            "question_id": pl.String,
            "question": pl.Struct(inner_schema),
        }
    )

    frames: list[pl.LazyFrame] = []
    for d in meeting_dirs:
        path = Path(d) / QUESTIONS_FILE
        if path.exists():
            frames.append(pl.scan_ndjson(str(path)))
    if not frames:
        return empty

    return (
        pl.concat(frames)
        .unique(subset=["question_id"])
        .select(
            pl.col("question_id"),
            pl.struct(pl.exclude("question_id")).alias("question"),
        )
    )
