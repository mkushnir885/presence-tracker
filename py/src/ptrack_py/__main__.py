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

EVENTS_FILE = "events.parquet"
QUESTIONS_FILE = "questions.jsonl"

app = typer.Typer(
    name="ptrack_py", help="ptrack Python analytics and generation binary."
)


@app.command()
def report(
    inputs: list[str] = typer.Argument(
        ...,
        metavar="MEETING_DIRS...",
        help=(
            "Meeting-directory paths or glob patterns. Each directory must "
            "contain events.parquet. Exactly one matched directory produces a "
            "per-meeting CSV; more produces the cross-meeting aggregate. "
            "Output is written to stdout — redirect to a file with `> report.csv`."
        ),
    ),
) -> None:
    """Generate a CSV report from one or more meeting directories."""
    dirs = _expand_globs(inputs)
    parquet_paths = _events_paths(dirs)
    _validate_complete(parquet_paths)

    try:
        frames = [
            pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)) for p in parquet_paths
        ]
        events = pl.concat(frames)
        csv_text = (
            generate_csv(events)
            if len(parquet_paths) == 1
            else generate_aggregate_csv(events)
        )
    except OSError as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    sys.stdout.write(csv_text)


@app.command()
def stats(
    inputs: list[str] = typer.Argument(
        ...,
        metavar="MEETING_DIRS...",
        help=(
            "Meeting-directory paths or glob patterns. Each directory must "
            "contain events.parquet. With one matched directory the response "
            "describes a single meeting; with more it describes every "
            "(participant, meeting) cell. Output is written to stdout."
        ),
    ),
) -> None:
    """Emit the GUI stats JSON for one or more meeting directories."""
    dirs = _expand_globs(inputs)
    parquet_paths = _events_paths(dirs)
    _validate_complete(parquet_paths)

    try:
        frames = [
            pl.scan_parquet(p, schema=pl.Schema(EVENT_SCHEMA)) for p in parquet_paths
        ]
        events = pl.concat(frames)
        mode = "meeting" if len(dirs) == 1 else "cross_meeting"
        source_dirs = _build_source_dir_map(parquet_paths, dirs)
        payload = generate_stats(events, mode=mode)
        for meeting in payload["meetings"]:
            src = source_dirs.get(meeting["meeting_id"])
            if src:
                meeting["source_dir"] = src
    except (LoadError, OSError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    sys.stdout.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")))


def _validate_complete(parquet_paths: list[str]) -> None:
    # Exit code 3 lets the Go GUI show "meeting still in progress".
    for p in parquet_paths:
        try:
            ensure_session_ended(p)
        except IncompleteMeetingError as exc:
            typer.echo(str(exc), err=True)
            raise typer.Exit(code=INCOMPLETE_MEETING_EXIT_CODE) from exc


def _expand_globs(patterns: list[str]) -> list[str]:
    dirs: list[str] = []
    seen: set[str] = set()
    for pattern in patterns:
        matches = sorted(_glob.glob(str(Path(pattern).expanduser())))
        if not matches and not any(ch in pattern for ch in "*?["):
            # literal path: surface a clearer message than glob's silent empty
            typer.echo(f"meeting dir not found: {pattern}", err=True)
            raise typer.Exit(code=1)
        for m in matches:
            mp = Path(m)
            if not mp.is_dir():
                continue
            if not (mp / EVENTS_FILE).exists():
                continue
            resolved = str(mp.resolve())
            if resolved in seen:
                continue
            seen.add(resolved)
            dirs.append(resolved)
    if not dirs:
        typer.echo(f"no meeting directories matched: {' '.join(patterns)}", err=True)
        raise typer.Exit(code=1)
    return dirs


def _events_paths(dirs: list[str]) -> list[str]:
    return [str(Path(d) / EVENTS_FILE) for d in dirs]


def _build_source_dir_map(
    parquet_paths: list[str], dirs: list[str]
) -> dict[str, str]:
    out: dict[str, str] = {}
    for parquet, dir_ in zip(parquet_paths, dirs, strict=True):
        df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
            pl.scan_parquet(parquet, schema=pl.Schema(EVENT_SCHEMA))
            .select(pl.col("meeting_id").first().alias("meeting_id"))
            .collect()
        )
        if df.height == 0:
            continue
        mid = df.row(0)[0]
        if isinstance(mid, str) and mid:
            out[mid] = dir_
    return out


if __name__ == "__main__":
    app()
