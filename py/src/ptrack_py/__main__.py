"""
CLI entry point for the ptrack_py binary (PyInstaller target).

Subcommands:
  report      Generate a per-meeting CSV report.
  aggregate   Generate a cross-meeting aggregate CSV report.
  stats       Emit JSON stats payload consumed by the Go GUI's /stats view.

TODO: challenger subcommand (AI-generated challenges) not implemented yet.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import polars as pl
import typer

from ptrack_analytics.load import LoadError, load_events, load_questions
from ptrack_analytics.schema import EVENT_SCHEMA

from .reports import generate_aggregate_csv, generate_csv
from .stats import generate_stats

app = typer.Typer(
    name="ptrack_py", help="ptrack Python analytics and generation binary."
)


@app.command()
def report(
    input: str = typer.Option(..., "--in", help="Path to a meeting .parquet file"),
    output: str = typer.Option(..., "--out", help="Output CSV path, or - for stdout"),
) -> None:
    """Generate a per-meeting CSV report from a Parquet file."""
    try:
        events = load_events(input)
        csv_text = generate_csv(events)
    except LoadError as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    if output == "-":
        sys.stdout.write(csv_text)
    else:
        out = Path(output)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(csv_text, encoding="utf-8")


@app.command()
def aggregate(
    inputs: list[str] = typer.Option(
        ...,
        "--in",
        help=(
            "Path to a meeting .parquet file. Repeat for multiple files. "
            "(A single glob pattern is also accepted for back-compat.)"
        ),
    ),
    output: str = typer.Option(..., "--out", help="Output CSV path, or - for stdout"),
) -> None:
    """Generate an aggregate CSV report over multiple meetings."""
    try:
        if len(inputs) == 1:
            events = load_events(inputs[0])
        else:
            frames = [
                pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)) for p in inputs
            ]
            events = pl.concat(frames)
        csv_text = generate_aggregate_csv(events)
    except (LoadError, OSError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    if output == "-":
        sys.stdout.write(csv_text)
    else:
        out = Path(output)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(csv_text, encoding="utf-8")


@app.command()
def stats(
    inputs: list[str] = typer.Option(
        ...,
        "--in",
        help=(
            "Path to a meeting .parquet file. Repeat for cross-meeting mode: "
            "with one --in the response describes a single meeting; with more "
            "than one it describes every (participant, meeting) cell."
        ),
    ),
    output: str = typer.Option(
        "-", "--out", help="Output JSON path, or - for stdout (default)."
    ),
) -> None:
    """Emit the GUI stats JSON for one or more Parquet files."""
    if not inputs:
        typer.echo("at least one --in is required", err=True)
        raise typer.Exit(code=2)

    try:
        frames = [pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)) for p in inputs]
        events = pl.concat(frames)
        mode = "meeting" if len(inputs) == 1 else "cross_meeting"
        questions = _load_questions_for(inputs)
        source_files = _build_source_file_map(inputs)
        payload = generate_stats(events, mode=mode, questions=questions)
        for meeting in payload["meetings"]:
            src = source_files.get(meeting["meeting_id"])
            if src:
                meeting["source_file"] = src
    except (LoadError, OSError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    text = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
    if output == "-":
        sys.stdout.write(text)
    else:
        out = Path(output)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(text, encoding="utf-8")


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
