# Configuration

Single JSON config file (`config.json`), validated against a JSON Schema.
The schema is the source of truth for runtime validation
(`go/src/internal/config/`) and for the web-based config editor's form
layout (`go/src/internal/gui/`).

## File locations

Config file resolved in this order (first existing wins):

1. `--config <path>` command-line flag.
2. **Linux:** `$XDG_CONFIG_HOME/ptrack/config.json`
   (default `~/.config/ptrack/config.json`)
3. **macOS:** `~/Library/Application Support/ptrack/config.json`
4. **Windows:** `%APPDATA%\ptrack\config.json`
5. `./config.json` in the current directory.

When no file exists anywhere, `ptrack serve` boots with built-in
defaults and binds the Config object to the canonical default path
(`<XDG_CONFIG_HOME>/ptrack/config.json` on Linux). The first save —
whether via the GUI's config editor or `ptrack reload` against a file
the user has hand-created — writes there. `ptrack track` requires a
real file (it needs credentials) and errors out if none is found.

## File format

JSON only. YAML is no longer accepted — config saves rewrite the file
in canonical form, and round-tripping YAML through Go would drop
comments and reorder keys.

Secrets (OAuth client IDs/secrets, bot tokens, BBB shared secret) live
**inline** in `config.json`. There is no separate `secrets.yaml`. The
file is written with mode `0600` on Unix; on Windows the user is
responsible for ACLs.

The runtime never writes a schema file next to `config.json` — the
canonical `config.schema.json` is published on GitHub (built by
`go run ./src/cmd/schemagen`) and the user adds a `"$schema"` reference
to that URL by hand when they want editor (VSCode, JetBrains) support.
Any `"$schema"` value the user puts in the file is preserved verbatim
across saves; the system never injects or strips it.

## Example

```json
{
  "$schema": "https://raw.githubusercontent.com/<owner>/presence-tracker/main/config.schema.json",
  "providers": {
    "bbb": {
      "enabled": true,
      "base_url": "https://bbb.example.edu/bigbluebutton/",
      "shared_secret": "..."
    }
  },
  "messengers": {
    "telegram": {
      "enabled": true,
      "bot_token": "123456:ABC-DEF..."
    }
  },
  "gui": {
    "port": 9090
  }
}
```

The file is intentionally minimal — only fields that differ from
defaults are persisted. Setting a field back to its default value
removes it from the file on the next save.

## Schema (v1)

Generated from the Go struct at `internal/config/config.go`; the
canonical text lives in the emitted `config.schema.json`. Field
constraints (port ranges, enum values, minima) are declared in
`internal/config/config.go:applyConstraints` and surface in both
runtime validation and the schema artifact.

Secret fields are annotated with `"writeOnly": true`. The GUI's config
editor renders these as masked inputs; the existing value is never
echoed in the form. An empty submission means "keep the existing
value"; a non-empty submission updates it.

The four `writeOnly` fields:

- `providers.bbb.shared_secret`
- `providers.meet.oauth.client_secret`
- `providers.zoom.oauth.client_secret`
- `messengers.telegram.bot_token`

## OAuth tokens

OAuth access/refresh tokens (for Meet, Zoom) are stored separately
from `config.json` under the internal data dir
(`config.DataDir()` — see `@docs/ARCHITECTURE.md#storage-layout` for
the platform-specific location). Only the long-lived `client_id` /
`client_secret` for the OAuth app live in `config.json`.

## Internal directories

The app's internal storage paths (data dir, cache dir) and the config
dir itself are not user-settable. They follow platform conventions
(XDG on Linux, `~/Library/Application Support` / `~/Library/Caches`
on macOS, `%LOCALAPPDATA%` / `%APPDATA%` on Windows). A user who wants
to relocate one of these — for example, to put model weights on a fast
SSD — should symlink the platform-default path. Keeping these fixed
avoids a class of footguns (silently empty participant registry after a
path change, half-completed cross-device moves).

The settable directories — `meetings_dir`, `questions_dir`,
`reports_dir` — are user-facing content paths. Defaults sit under
`~/Documents/ptrack/{meetings,questions,reports}`. Paths support `~`
and forward-slash separators on Windows; the loader normalises both
into OS-native absolute paths.

## Save and reload

The runtime holds the resolved values behind
`config.Config.Get()` (a lock-free atomic snapshot). Two write paths:

- **`Config.Apply(mutator func(*Values))`** — used by the GUI's save
  handler. Mutates a snapshot, validates the resulting overrides
  (default-equal fields pruned), writes the file atomically, and
  publishes the new snapshot to readers.
- **`Config.Reload()`** — re-reads the file (file is authoritative),
  validates, prunes, rewrites, and publishes. Exposed via the
  `POST /config/reload` daemon endpoint, called by the
  `ptrack reload` CLI.

Both go through one shared `commit(v Values)` pipeline:
validate → prune → atomic write → store. Reads (`Get()`) are lock-free
and always return the most recently published snapshot.

## Live reload behaviour

Adapters re-read the config on each natural boundary (provider poll
tick, messenger initialization at session start, etc.) via
`cfg.Get()`. After `Reload`/`Apply`:

| Section                            | When the change is observed                              |
|------------------------------------|----------------------------------------------------------|
| `providers.*.poll_interval_seconds`| Next poll tick                                           |
| `providers.*.base_url` / secrets   | Next meeting (provider Authenticate runs at session start) |
| `messengers.*`                     | Next meeting                                             |
| `gui.*`                            | Requires `ptrack serve` restart (listener already bound) |
| `logging.*`                        | Requires daemon restart                                  |
| `challenges.*`                     | Next poll round                                          |

Session-scoped invariants (`answer_window_seconds`,
`min_gap_between_challenges_seconds`, `eventstore.*`) are snapshotted
into `session.Config` at session start and are not affected by
mid-session reloads.

## Validation

Validation runs against the **minimal-overrides** representation
(default-equal fields removed). Defaults are trusted. Schema enforces:

- Port numbers in TCP range.
- `eventstore.compression` ∈ {`zstd`, `snappy`, `none`}.
- `logging.level` ∈ {`debug`, `info`, `warn`, `error`};
  `logging.format` ∈ {`text`, `json`}.
- Non-empty strings for path fields when set.
- Positive minima on poll intervals, answer window, row group size.

Validation errors include a JSON Pointer to the offending field so the
GUI can highlight it. `ptrack reload` surfaces the error message to
stderr and leaves the running config unchanged.
