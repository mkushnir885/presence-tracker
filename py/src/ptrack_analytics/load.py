from __future__ import annotations

import glob as _glob
from collections.abc import Iterable
from pathlib import Path

import polars as pl

from .schema import EVENT_SCHEMA

EVENTS_FILE = "events.parquet"
QUESTIONS_FILE = "questions.jsonl"

_GLOB_CHARS = "*?["


class LoadError(Exception):
    pass


def scan_events(path: str | Path) -> pl.LazyFrame:
    """Lazy-scan one events.parquet with the canonical event schema applied."""
    return pl.scan_parquet(str(path), schema=pl.Schema(EVENT_SCHEMA))


def resolve_meetings(*patterns: str) -> list[Path]:
    """Expand one or more meeting-directory paths or globs into resolved dirs.

    Each match must be a directory containing events.parquet. Order is
    deterministic (sorted by path); duplicates across patterns are removed.

    Raises LoadError with a literal-vs-glob-aware message when nothing matches.
    """
    if not patterns:
        raise LoadError("no patterns given")

    seen: set[str] = set()
    dirs: list[Path] = []
    for pattern in patterns:
        matches = sorted(_glob.glob(str(Path(pattern).expanduser())))
        if not matches and not any(ch in pattern for ch in _GLOB_CHARS):
            raise LoadError(f"meeting dir not found: {pattern}")
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
        raise LoadError(f"no meeting directories matched: {' '.join(patterns)}")
    return dirs


def load_events(meeting_dirs: Iterable[Path | str]) -> pl.LazyFrame:
    """Lazy-concat events.parquet from every resolved meeting directory."""
    frames = [scan_events(Path(d) / EVENTS_FILE) for d in meeting_dirs]
    if not frames:
        raise LoadError("no meeting directories given")
    return pl.concat(frames)


def load_questions(meeting_dirs: Iterable[Path | str]) -> pl.LazyFrame:
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
