"""CLI entry point for the ptrack_py binary (PyInstaller target)."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import typer

from ptrack_analytics.load import (
    EVENTS_FILE,
    LoadError,
    load_events,
    load_questions_index,
    meeting_source_dirs,
    resolve_meetings,
)
from ptrack_analytics.validate import IncompleteMeetingError, ensure_session_ended
from ptrack_py.rename import rename_display_name
from ptrack_py.reports import generate_aggregate_csv, generate_csv
from ptrack_py.stats import generate_stats

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
        source_dirs = meeting_source_dirs(dirs)
        questions = load_questions_index(dirs)
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
