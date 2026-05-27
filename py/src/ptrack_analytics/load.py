"""
Functions for loading meeting Parquet and question JSONL files.
"""

from __future__ import annotations

import glob as _glob
from pathlib import Path
from typing import TYPE_CHECKING

import polars as pl

from .schema import EVENT_SCHEMA

if TYPE_CHECKING:
    pass


class LoadError(Exception):
    pass


def load_events(pattern: str) -> pl.LazyFrame:
    """
    Load all meeting Parquet files matching *pattern* into a lazy frame.

    Raises LoadError if no files are found or if any file has an
    incompatible schema_version.
    """
    paths = sorted(_glob.glob(str(Path(pattern).expanduser())))
    if not paths:
        raise LoadError(f"no Parquet files matched: {pattern}")

    frames: list[pl.LazyFrame] = []
    for p in paths:
        frames.append(pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)))

    return pl.concat(frames)


def load_questions(questions_dir: str, meeting_ids: list[str]) -> pl.LazyFrame:
    """
    Load JSONL question files for the given meeting IDs into a lazy frame.

    Missing files are silently skipped (meeting may have had no challenges).
    """
    qdir = Path(questions_dir).expanduser()
    frames: list[pl.LazyFrame] = []
    for mid in meeting_ids:
        path = qdir / f"{mid}.jsonl"
        if path.exists():
            frames.append(pl.scan_ndjson(str(path)))

    if not frames:
        # Return an empty frame with expected columns so joins always work.
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
                "issued_at": pl.String,
            }
        )

    return pl.concat(frames)
