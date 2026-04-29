---
description: Python-specific conventions for this project
globs: ["py/**/*.py", "py/pyproject.toml"]
---

# Python style rules

## Language and tooling

- Python 3.12+. Full type hints on every public function.
  `from __future__ import annotations` at the top of modules.
- Managed with `uv`. All commands run from `py/`. Never `pip install` —
  use `uv add <pkg>` which updates `pyproject.toml` and the lockfile.
- Formatting and linting: `ruff format` + `ruff check`. Type check with
  `ty check`.
- Logging via stdlib `logging`. Configured once in each CLI entrypoint.
  Other modules use `logger = logging.getLogger(__name__)`.
- Errors: raise specific exception types, not bare `Exception`. Public
  API functions document which exceptions they can raise.
- Never print directly from library modules. CLI entrypoints may print.

## Dataframes (reporter)

- Polars over Pandas. Default to the **lazy** API (`pl.scan_parquet`,
  `LazyFrame`) whenever touching more than one file. Call `.collect()`
  only at the end.
- No magic `DataFrame` returns from typed functions — use
  `pl.DataFrame` / `pl.LazyFrame` in signatures explicitly.
- For dataframe assertions in tests use
  `polars.testing.assert_frame_equal`.

## CLIs

- CLI with `typer`. No `argparse`, no `click` for new code.
- Each Python sub-project exposes `python -m <pkg> <subcommand>` via its
  `__main__.py`. Subcommands are added per feature.

## Templates and I/O

- When generating PDFs or HTML, templates live in
  `py/src/ptrack_analytics/templates/` and are rendered with Jinja2. Do not build
  HTML with string concatenation.
- Side-effectful code (file writes, subprocess, network) goes in thin
  adapter modules. Analysis modules take and return dataframes; they do
  not read or write files themselves.

## Challenger service (v1 stretch / v2)

- The challenger service (`py/src/challenger/`) is a long-running HTTP
  server on localhost invoked by Go. It owns the ASR and LLM processes
  so they stay warm across the meeting.
- Models are loaded at startup, not on first request. Log the load time
  and memory footprint clearly — these are the numbers that determine
  whether the teacher's laptop can run the system.
- For local inference use `faster-whisper` (ASR) and `llama-cpp-python`
  (LLM). Model paths come from config, not hardcoded.
- For hosted inference (OpenAI / Gemini) the service abstracts over a
  small `Generator` protocol — local and hosted backends are
  interchangeable.
- The service never persists transcript data to disk. Transcript lives
  in a rolling in-memory window sized per config. This is a hard privacy
  requirement, not a nice-to-have.
