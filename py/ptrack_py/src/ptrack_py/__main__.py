from __future__ import annotations

import json
import sys
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path

import polars as pl
import typer
from ptrack_analytics.frames import collect_df
from ptrack_analytics.load import (
    EVENTS_FILE,
    load_events,
    load_questions,
    resolve_meetings,
)
from ptrack_analytics.schema import EVENT_SCHEMA
from ptrack_py.rename import rename_display_name
from ptrack_py.reports import generate_csv
from ptrack_py.stats import generate_stats

INVALID_DATA_EXIT_CODE = 3

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
    with _cli_errors():
        dirs = resolve_meetings(*inputs)
        events = load_events(dirs)
        csv_text = generate_csv(events, cross_meeting=len(dirs) > 1)

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
    with _cli_errors():
        dirs = resolve_meetings(*inputs)
        events = load_events(dirs)
        mode = "meeting" if len(dirs) == 1 else "cross_meeting"
        source_dirs = _meeting_source_dirs(dirs)
        questions = {
            qid: {
                "question_id": qid,
                **rec,
                "correct_answer": (
                    json.loads(rec["correct_answer"])
                    if rec.get("correct_answer") is not None
                    else None
                ),
            }
            for qid, rec in collect_df(load_questions(dirs)).iter_rows()
        }
        payload = generate_stats(events, mode=mode, questions=questions)
        for meeting in payload["meetings"]:
            src = source_dirs.get(meeting["meeting_id"])
            if src:
                meeting["source_dir"] = src

    sys.stdout.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")))


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
    with _cli_errors():
        # Skip the session-ended check — a teacher may need to rename
        # someone mid-meeting.
        dirs = resolve_meetings(*inputs)
        for d in dirs:
            rename_display_name(d / EVENTS_FILE, from_name, to_name)


def _meeting_source_dirs(dirs: list[Path]) -> dict[str, str]:
    """Map each ``meeting_id`` to the directory it was loaded from."""
    schema = pl.Schema(EVENT_SCHEMA)
    out: dict[str, str] = {}
    for d in dirs:
        df = collect_df(
            pl.scan_parquet(str(d / EVENTS_FILE), schema=schema).select(
                pl.col("meeting_id").first()
            )
        )
        if df.height == 0:
            continue
        mid = df.row(0)[0]
        if isinstance(mid, str) and mid:
            out[mid] = str(d)
    return out


@contextmanager
def _cli_errors() -> Iterator[None]:
    """Translate analytics errors into ``typer.Exit``. Exit 3 = invalid
    data (e.g. no ``session_ended``); exit 1 = OS error.
    """
    try:
        yield
    except ValueError as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=INVALID_DATA_EXIT_CODE) from exc
    except OSError as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc


if __name__ == "__main__":
    app()
