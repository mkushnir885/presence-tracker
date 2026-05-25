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

from ptrack_analytics.load import LoadError, load_events
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
    input: str = typer.Option(
        ..., "--in", help="Glob pattern for meeting .parquet files"
    ),
    output: str = typer.Option(..., "--out", help="Output CSV path, or - for stdout"),
) -> None:
    """Generate an aggregate CSV report over multiple meetings."""
    try:
        events = load_events(input)
        csv_text = generate_aggregate_csv(events)
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
        payload = generate_stats(events, mode=mode)
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


if __name__ == "__main__":
    app()
