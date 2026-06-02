"""CLI entry point for the ptrack_py binary (PyInstaller target)."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import polars as pl
import typer

from ptrack_analytics.load import (
    EVENTS_FILE,
    QUESTIONS_FILE,
    LoadError,
    load_events,
    resolve_meetings,
    scan_events,
)
from ptrack_py.rename import rename_display_name
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
    try:
        dirs = resolve_meetings(*inputs)
        _validate_complete(dirs)
        events = load_events(dirs)
        csv_text = (
            generate_csv(events)
            if len(dirs) == 1
            else generate_aggregate_csv(events)
        )
    except (LoadError, OSError) as exc:
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
    try:
        dirs = resolve_meetings(*inputs)
        _validate_complete(dirs)
        events = load_events(dirs)
        mode = "meeting" if len(dirs) == 1 else "cross_meeting"
        source_dirs = _build_source_dir_map(dirs)
        questions = _load_questions(dirs)
        payload = generate_stats(events, mode=mode, questions=questions)
        for meeting in payload["meetings"]:
            src = source_dirs.get(meeting["meeting_id"])
            if src:
                meeting["source_dir"] = src
    except (LoadError, OSError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc

    sys.stdout.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")))


def _validate_complete(dirs: list[Path]) -> None:
    # Exit code 3 lets the Go GUI show "meeting still in progress".
    for d in dirs:
        path = str(d / EVENTS_FILE)
        try:
            ensure_session_ended(path)
        except IncompleteMeetingError as exc:
            typer.echo(str(exc), err=True)
            raise typer.Exit(code=INCOMPLETE_MEETING_EXIT_CODE) from exc


def _load_questions(dirs: list[Path]) -> dict[str, dict[str, Any]]:
    out: dict[str, dict[str, Any]] = {}
    for d in dirs:
        path = d / QUESTIONS_FILE
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


def _build_source_dir_map(dirs: list[Path]) -> dict[str, str]:
    out: dict[str, str] = {}
    for d in dirs:
        df: pl.DataFrame = (  # type: ignore  # ty: collect() return is typed as a union
            scan_events(d / EVENTS_FILE)
            .select(pl.col("meeting_id").first().alias("meeting_id"))
            .collect()
        )
        if df.height == 0:
            continue
        mid = df.row(0)[0]
        if isinstance(mid, str) and mid:
            out[mid] = str(d)
    return out


@app.command()
def rename(
    inputs: list[str] = typer.Argument(
        ...,
        metavar="MEETING_DIRS...",
        help=(
            "Meeting-directory paths or glob patterns. Each directory's "
            "events.parquet is rewritten in place via an atomic temp-and-rename."
        ),
    ),
    from_name: str = typer.Option(..., "--from", help="Display name to replace."),
    to_name: str = typer.Option(..., "--to", help="New display name."),
) -> None:
    """Rewrite display_name across one or more meetings."""
    if from_name == to_name:
        return
    try:
        dirs = resolve_meetings(*inputs)
        for d in dirs:
            rename_display_name(d / EVENTS_FILE, from_name, to_name)
    except (LoadError, OSError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc


if __name__ == "__main__":
    app()
