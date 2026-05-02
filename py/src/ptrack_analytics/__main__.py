"""
CLI entry point for the ptrack_py binary (PyInstaller target).

Subcommands:
  report      Generate a per-meeting PDF report.
  aggregate   Generate an aggregate PDF over multiple meetings.

TODO: challenger subcommand (AI-generated challenges) not implemented yet.
"""

from __future__ import annotations

import typer

app = typer.Typer(
    name="ptrack_py", help="ptrack Python analytics and generation binary."
)


@app.command()
def report(
    input: str = typer.Option(..., "--in", help="Path to meeting .parquet file"),
    output: str = typer.Option(..., "--out", help="Output PDF path"),
) -> None:
    """Generate a per-meeting PDF report from a Parquet file."""
    # TODO: implement PDF generation (matplotlib + fpdf2).
    typer.echo(f"[TODO] report: {input} → {output}", err=True)
    typer.echo("PDF generation not yet implemented.", err=True)
    raise typer.Exit(code=1)


@app.command()
def aggregate(
    input: str = typer.Option(
        ..., "--in", help="Glob pattern for meeting .parquet files"
    ),
    output: str = typer.Option(..., "--out", help="Output PDF path"),
) -> None:
    """Generate an aggregate PDF report over multiple meetings."""
    # TODO: implement aggregate PDF generation.
    typer.echo(f"[TODO] aggregate: {input} → {output}", err=True)
    typer.echo("PDF generation not yet implemented.", err=True)
    raise typer.Exit(code=1)


if __name__ == "__main__":
    app()
