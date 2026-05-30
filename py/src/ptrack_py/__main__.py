"""CLI entry point for the ptrack_py binary (PyInstaller target)."""

from __future__ import annotations

import glob as _glob
import json
import sys
from pathlib import Path

import polars as pl
import typer

from ptrack_analytics.load import LoadError
from ptrack_analytics.schema import EVENT_SCHEMA
from ptrack_py.reports import generate_aggregate_csv, generate_csv
from ptrack_py.stats import generate_stats
from ptrack_py.validate import IncompleteMeetingError, ensure_session_ended

INCOMPLETE_MEETING_EXIT_CODE = 3

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
    _validate_complete(paths)

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
    _validate_complete(paths)

    try:
        frames = [pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)) for p in paths]
        events = pl.concat(frames)
        mode = "meeting" if len(paths) == 1 else "cross_meeting"
        source_files = _build_source_file_map(paths)
        payload = generate_stats(events, mode=mode)
        for meeting in payload["meetings"]:
            src = source_files.get(meeting["meeting_id"])
            if src:
                meeting["source_file"] = src
    except (LoadError, OSError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    sys.stdout.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")))


def _validate_complete(paths: list[str]) -> None:
    # Exit code 3 lets the Go GUI show "meeting still in progress".
    for p in paths:
        try:
            ensure_session_ended(p)
        except IncompleteMeetingError as exc:
            typer.echo(str(exc), err=True)
            raise typer.Exit(code=INCOMPLETE_MEETING_EXIT_CODE) from exc


def _expand_globs(patterns: list[str]) -> list[str]:
    paths: list[str] = []
    for pattern in patterns:
        paths.extend(sorted(_glob.glob(str(Path(pattern).expanduser()))))
    if not paths:
        typer.echo(f"no Parquet files matched: {' '.join(patterns)}", err=True)
        raise typer.Exit(code=1)
    return paths


def _build_source_file_map(inputs: list[str]) -> dict[str, str]:
    out: dict[str, str] = {}
    for p in inputs:
        df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
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


if __name__ == "__main__":
    app()
