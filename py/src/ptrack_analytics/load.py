from __future__ import annotations

import glob as _glob
import json
from collections.abc import Iterable
from pathlib import Path
from typing import Any

import polars as pl

from .schema import EVENT_SCHEMA, QUESTIONS_SCHEMA

EVENTS_FILE = "events.parquet"
QUESTIONS_FILE = "questions.jsonl"

_GLOB_CHARS = "*?["


class LoadError(Exception):
    pass


class IncompleteMeetingError(Exception):
    """Raised when a meeting's events.parquet has no session_ended event yet —
    treating the last observed timestamp as the meeting end would mislead.
    """

    def __init__(self, path: str) -> None:
        super().__init__(
            f"{path}: meeting is still in progress (no session_ended event); "
            "stop the tracking session and try again."
        )
        self.path = path


def scan_events(path: str | Path) -> pl.LazyFrame:
    """Lazy-scan one events.parquet with the canonical event schema applied."""
    return pl.scan_parquet(str(path), schema=pl.Schema(EVENT_SCHEMA))


def collect_df(lf: pl.LazyFrame) -> pl.DataFrame:
    """Eager-collect *lf* into a DataFrame. Centralizes the ty-vs-polars
    "collect returns a union" annotation so call sites stay clean.
    """
    return lf.collect()  # type: ignore  # ty: collect() return is typed as a union


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


def ensure_session_ended(path: str | Path) -> None:
    """Raise IncompleteMeetingError if *path* has no session_ended event."""
    df = collect_df(
        scan_events(path)
        .filter(pl.col("event_type") == "session_ended")
        .select(pl.len())
    )
    if int(df.item()) == 0:
        raise IncompleteMeetingError(str(path))


def load_meetings(
    *patterns: str,
    validate: bool = True,
) -> tuple[list[Path], pl.LazyFrame]:
    """Resolve *patterns*, load their events, and (by default) reject any
    meeting still in progress. Returns (resolved_dirs, lazy_events).

    Pass validate=False to peek at a live session from a notebook.
    """
    dirs = resolve_meetings(*patterns)
    if validate:
        for d in dirs:
            ensure_session_ended(d / EVENTS_FILE)
    return dirs, load_events(dirs)


def meeting_source_dirs(meeting_dirs: Iterable[Path | str]) -> dict[str, str]:
    """Map each directory's meeting_id to its source-directory path.

    Scans the first row of each events.parquet for the meeting_id. Used by the
    GUI stats loader to thread source paths back into the rendered payload.
    """
    out: dict[str, str] = {}
    for d in meeting_dirs:
        dir_path = Path(d)
        df = collect_df(
            scan_events(dir_path / EVENTS_FILE).select(
                pl.col("meeting_id").first().alias("meeting_id")
            )
        )
        if df.height == 0:
            continue
        mid = df.row(0)[0]
        if isinstance(mid, str) and mid:
            out[mid] = str(dir_path)
    return out


def load_questions_index(
    meeting_dirs: Iterable[Path | str],
) -> dict[str, dict[str, Any]]:
    """Map question_id -> full question record across the given meetings.

    Missing files and malformed lines are skipped silently; this mirrors the
    GUI stats consumer, which treats absent records as "no question payload".
    """
    out: dict[str, dict[str, Any]] = {}
    for d in meeting_dirs:
        path = Path(d) / QUESTIONS_FILE
        if not path.exists():
            continue
        with path.open("r", encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    record = json.loads(line)
                except json.JSONDecodeError:
                    continue
                qid = record.get("question_id")
                if not qid:
                    continue
                out[qid] = record
    return out


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
        return pl.LazyFrame(schema=QUESTIONS_SCHEMA)

    return pl.concat(frames)
