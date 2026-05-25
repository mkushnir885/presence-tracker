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

- When generating HTML or any other templated text output, templates live
  next to the producer that uses them (e.g. `py/src/ptrack_py/templates/`)
  and are rendered with Jinja2. Do not build HTML with string concatenation.
- Side-effectful code (file writes, subprocess, network) goes in thin
  adapter modules. Analysis modules take and return dataframes; they do
  not read or write files themselves.

## Challenger service (v1 stretch / v2)

- The challenger service (`py/src/challenger/`) is a long-running child
  process of the Go daemon, started once per session when
  `challenges.auto_generation.enabled` is true. It is a **YAML
  producer**, not an in-process RPC server: it accepts audio from Go,
  maintains a rolling transcript, and on its own schedule writes a
  generated bank to the pending directory (`/tmp/ptrack/` on Linux,
  `%TEMP%\ptrack\` on Windows).
- When `challenges.auto_generation.auto_submit` is true, the service
  invokes `ptrack poll --type=aigenerated <path>` itself immediately
  after writing the file. When false, it stops at writing and the
  teacher submits manually from the GUI. The CLI re-entry is the only
  place where this Python code triggers any side effect outside its own
  process.
- Audio is captured by the browser GUI through `getUserMedia` and
  relayed by Go over a small localhost HTTP endpoint
  (`POST /context/audio`) or over the child process's stdin. Either
  way, the service never opens the OS-level audio device itself.
- Models are loaded at startup by default (`preload_models: true`), not
  on first request. Log the load time and memory footprint clearly —
  these are the numbers that determine whether the teacher's laptop can
  run the system. Setting `preload_models: false` swaps the trade-off
  to lazy loading on first generation.
- Models stay resident across meetings; they are released only by an
  explicit `POST /control/unload-models` call (triggered by the GUI's
  Free models button) or by the configured
  `idle_unload_after_seconds` timeout. The process itself terminates on
  `POST /control/shutdown` or SIGTERM, which is what the GUI's Shut
  down button ultimately triggers via Go.
- For local inference use `faster-whisper` (ASR) and `llama-cpp-python`
  (LLM). Model paths come from config, not hardcoded.
- For hosted inference (OpenAI / Gemini) the service abstracts over a
  small `Generator` protocol — local and hosted backends are
  interchangeable.
- The service never persists transcript or raw audio data to disk.
  Transcript lives in a rolling in-memory window sized per config; the
  only files written are the generated YAML banks in the pending
  directory, and those are short-lived. This is a hard privacy
  requirement, not a nice-to-have.
