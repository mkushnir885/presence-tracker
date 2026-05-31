from __future__ import annotations

import glob as _glob
from pathlib import Path

import polars as pl

from .schema import EVENT_SCHEMA


class LoadError(Exception):
    pass


def load_events(pattern: str) -> pl.LazyFrame:
    """Load every meeting Parquet matching *pattern* into one lazy frame."""
    paths = sorted(_glob.glob(str(Path(pattern).expanduser())))
    if not paths:
        raise LoadError(f"no Parquet files matched: {pattern}")

    frames: list[pl.LazyFrame] = []
    for p in paths:
        frames.append(pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)))

    return pl.concat(frames)


def load_questions(questions_dir: str, meeting_ids: list[str]) -> pl.LazyFrame:
    """Load the question JSONL for each meeting; missing files are skipped."""
    qdir = Path(questions_dir).expanduser()
    frames: list[pl.LazyFrame] = []
    for mid in meeting_ids:
        path = qdir / f"{mid}.jsonl"
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
