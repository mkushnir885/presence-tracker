"""
CLI entry point for the ptrack_py binary (PyInstaller target).

Subcommands:
  report      Generate a CSV report; per-meeting with one --in, aggregate
              with more than one.
  stats       Emit JSON stats payload consumed by the Go GUI's /stats view.

TODO: challenger subcommand (AI-generated challenges) not implemented yet.
"""

from __future__ import annotations

import glob as _glob
import json
import sys
from pathlib import Path

import polars as pl
import typer

from ptrack_analytics.load import LoadError, load_questions
from ptrack_analytics.schema import EVENT_SCHEMA

from .reports import generate_aggregate_csv, generate_csv
from .stats import generate_stats

app = typer.Typer(
    name="ptrack_py", help="ptrack Python analytics and generation binary."
)


@app.command()
def report(
    inputs: list[str] = typer.Argument(
        ...,
        metavar="PATHS...",
        help=(
            "Parquet file paths or glob patterns matching several. "
            "Exactly one matched file produces a per-meeting CSV; more "
            "than one produces the cross-meeting aggregate. Output is "
            "written to stdout — redirect to a file with `> report.csv`."
        ),
    ),
) -> None:
    """Generate a CSV report from one or more Parquet files."""
    paths = _expand_globs(inputs)

    try:
        frames = [pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)) for p in paths]
        events = pl.concat(frames)
        csv_text = (
            generate_csv(events) if len(paths) == 1 else generate_aggregate_csv(events)
        )
    except OSError as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    sys.stdout.write(csv_text)


@app.command()
def stats(
    inputs: list[str] = typer.Argument(
        ...,
        metavar="PATHS...",
        help=(
            "Parquet file paths or glob patterns matching several. With "
            "one matched file the response describes a single meeting; "
            "with more it describes every (participant, meeting) cell. "
            "Output is written to stdout."
        ),
    ),
) -> None:
    """Emit the GUI stats JSON for one or more Parquet files."""
    paths = _expand_globs(inputs)

    try:
        frames = [pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)) for p in paths]
        events = pl.concat(frames)
        mode = "meeting" if len(paths) == 1 else "cross_meeting"
        questions = _load_questions_for(paths)
        source_files = _build_source_file_map(paths)
        payload = generate_stats(events, mode=mode, questions=questions)
        for meeting in payload["meetings"]:
            src = source_files.get(meeting["meeting_id"])
            if src:
                meeting["source_file"] = src
    except (LoadError, OSError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    sys.stdout.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")))


def _expand_globs(patterns: list[str]) -> list[str]:
    """Expand glob patterns into a sorted list of Parquet paths.

    Exits the program with code 1 if no pattern matches anything; that
    matches how the previous per-pattern loader surfaced missing files.
    """
    paths: list[str] = []
    for pattern in patterns:
        paths.extend(sorted(_glob.glob(str(Path(pattern).expanduser()))))
    if not paths:
        typer.echo(f"no Parquet files matched: {' '.join(patterns)}", err=True)
        raise typer.Exit(code=1)
    return paths


def _build_source_file_map(inputs: list[str]) -> dict[str, str]:
    """Map each meeting_id to the parquet path it came from.

    Lets the GUI display the actual filename even when it differs from
    the meeting_id convention.
    """
    out: dict[str, str] = {}
    for p in inputs:
        df: pl.DataFrame = (  # type: ignore  # ty limitation: collect returns InProcessQuery union
            pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA))
            .select(pl.col("meeting_id").first().alias("meeting_id"))
            .collect()
        )
        if df.height == 0:
            continue
        mid = df.row(0)[0]
        if isinstance(mid, str) and mid:
            out[mid] = p
    return out


def _load_questions_for(inputs: list[str]) -> pl.LazyFrame | None:
    """Discover the questions/ sibling directory for the given parquet inputs.

    Returns a concatenated LazyFrame of every JSONL file matching the
    parquet basenames, or None if no questions are found. Mirrors the
    convention used by `ptrack_analytics.load()`: questions live next to
    the meetings directory under `../questions/<meeting_id>.jsonl`.
    """
    meeting_ids = [Path(p).stem for p in inputs]
    seen: set[Path] = set()
    frames: list[pl.LazyFrame] = []
    for p in inputs:
        qdir = Path(p).parent.parent / "questions"
        if qdir in seen or not qdir.is_dir():
            continue
        seen.add(qdir)
        sub = load_questions(str(qdir), meeting_ids)
        frames.append(sub)
    if not frames:
        return None
    return pl.concat(frames)


if __name__ == "__main__":
    app()
