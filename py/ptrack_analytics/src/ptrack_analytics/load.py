from __future__ import annotations

import glob as _glob
import json
from collections.abc import Iterable
from pathlib import Path
from typing import Any

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
    inner_keys = [k for k in QUESTIONS_SCHEMA if k != "question_id"]
    frame_schema = {
        "question_id": pl.String,
        "question": pl.Struct({k: QUESTIONS_SCHEMA[k] for k in inner_keys}),
    }

    rows: list[dict[str, Any]] = []
    seen: set[str] = set()
    for d in meeting_dirs:
        path = Path(d) / QUESTIONS_FILE
        if not path.exists():
            continue
        with path.open(encoding="utf-8") as f:
            for line in f:
                if not line.strip():
                    continue
                rec = json.loads(line)
                qid = rec.get("question_id")
                if not isinstance(qid, str) or qid in seen:
                    continue
                seen.add(qid)
                ans = rec.get("correct_answer")
                inner = {k: rec.get(k) for k in inner_keys}
                inner["correct_answer"] = (
                    None if ans is None else json.dumps(ans, ensure_ascii=False)
                )
                rows.append({"question_id": qid, "question": inner})

    return pl.LazyFrame(rows, schema=frame_schema)
