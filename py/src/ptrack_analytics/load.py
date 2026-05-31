from __future__ import annotations

import glob as _glob
from pathlib import Path

import polars as pl

from .schema import EVENT_SCHEMA

EVENTS_FILE = "events.parquet"
QUESTIONS_FILE = "questions.jsonl"


class LoadError(Exception):
    pass


def resolve_meeting_dirs(pattern: str) -> list[Path]:
    """Expand *pattern* (a path or glob) into meeting-directory paths.

    Each match must be a directory containing events.parquet. Order is
    deterministic (sorted by path); duplicates are removed.
    """
    matches = sorted(_glob.glob(str(Path(pattern).expanduser())))
    if not matches:
        raise LoadError(f"no paths matched: {pattern}")

    seen: set[str] = set()
    dirs: list[Path] = []
    for m in matches:
        p = Path(m)
        if not p.is_dir():
            continue
        if not (p / EVENTS_FILE).exists():
            continue
        resolved = str(p.resolve())
        if resolved in seen:
            continue
        seen.add(resolved)
        dirs.append(p)
    if not dirs:
        raise LoadError(f"no meeting directories under {pattern}")
    return dirs


def load_events(pattern: str) -> pl.LazyFrame:
    """Load events.parquet from every meeting directory matching *pattern*."""
    dirs = resolve_meeting_dirs(pattern)
    frames: list[pl.LazyFrame] = []
    for d in dirs:
        frames.append(
            pl.scan_parquet(str(d / EVENTS_FILE), schema=pl.Schema(EVENT_SCHEMA))
        )
    return pl.concat(frames)


def load_questions(meeting_dirs: list[Path] | list[str]) -> pl.LazyFrame:
    """Load questions.jsonl from each meeting directory; missing files skipped."""
    frames: list[pl.LazyFrame] = []
    for d in meeting_dirs:
        path = Path(d) / QUESTIONS_FILE
        if path.exists():
            frames.append(pl.scan_ndjson(str(path)))

    # No question files (e.g. a tracking-only session): return a typed empty
    # frame so the `questions` schema stays stable for downstream joins.
    if not frames:
        return pl.LazyFrame(
            schema={
                "question_id": pl.String,
                "auto_submitted": pl.Boolean,
                "question_type": pl.String,
                "prompt": pl.String,
                "choices": pl.List(pl.String),
                "correct_answer": pl.String,
                "match_mode": pl.String,
                "tolerance": pl.Float64,
            }
        )

    return pl.concat(frames)
