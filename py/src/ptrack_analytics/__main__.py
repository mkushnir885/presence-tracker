"""
CLI entry point for the ptrack_py binary (PyInstaller target).

Subcommands:
  report      Generate a per-meeting CSV report.
  aggregate   Generate a cross-meeting aggregate CSV report.

TODO: challenger subcommand (AI-generated challenges) not implemented yet.
"""

from __future__ import annotations

import sys
from pathlib import Path

import typer

from .load import LoadError, load_events
from .reports import generate_aggregate_csv, generate_csv

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


if __name__ == "__main__":
    app()
