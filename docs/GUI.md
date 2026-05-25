# GUI

Local web app served from `ptrack serve`. Accessible at
`http://127.0.0.1:8080` by default; opens in the system browser.
Rendered server-side with templ; interactive bits use htmx.

## Design principles

1. **One person on one machine.** No auth, no multi-tenancy. The bind
   address is loopback-only by default; if the teacher changes it, a
   prominent warning is shown.
2. **Analysis from files only.** Participant timelines and statistics are
   rendered from saved Parquet files, not a live event stream. The
   status dashboard during an active meeting shows system status only.
3. **Minimalistic and professional.** Clean layout, no decorative
   elements. Reference: Syncthing's web UI.
4. **Theme and language.** Supports dark/light/system color themes and
   English/Ukrainian UI languages. Preference stored in localStorage.
   Translation files: `go/src/internal/gui/locales/<lang>.json` (adding a
   new language requires only a new JSON file and registering it in the
   language selector).

## Routes

| Route                                                | Purpose                                                                        |
| ---------------------------------------------------- | ------------------------------------------------------------------------------ |
| `GET /`                                              | Dashboard: recent meetings list, connect-to-meeting form                       |
| `POST /session`                                      | Start tracking a meeting (provider + meeting ID); redirects to `/status`       |
| `DELETE /session`                                    | Stop the active tracking session                                               |
| `GET /status`                                        | Meeting status dashboard (active meeting only)                                 |
| `GET /status/unregistered`                           | htmx fragment: list of unregistered participants                               |
| `GET /stats?file=<a>&file=<b>…`                      | Unified stats view. One `file` value → per-meeting timeband list; more than one → paged cross-meeting container (one participant per page, search bar). `file` values are basenames in `meetings_dir`. |
| `GET /stats.csv?file=<a>[&file=<b>…]`                | Download the CSV equivalent of the same stats request (single-file or cross-meeting, decided by the number of `file` values). |
| `PATCH /participants/{display_name}/display-name?file=<a>[&file=<b>…]&new=<name>` | Rewrite `display_name` from the URL path to `new` in every `file` listed in the query. `{display_name}` is URL-encoded. |
| `GET /registry`                                      | Participant registry page — list all registered display-name entries           |
| `DELETE /registry/{display_name}`                    | Remove one registry entry (URL-encoded display name)                           |
| `DELETE /registry`                                   | Clear all registry entries (pairing data only; Parquet files untouched)        |
| `POST /poll`                                         | Trigger a poll on the active session (body: `{type, bank_path}`); 409 if none  |
| `PATCH /poll/config`                                 | Update auto-generation poll config mid-meeting                                 |
| `GET /poll/pending`                                  | htmx fragment: pending auto-generated YAML (file path, timestamp) or empty     |
| `GET /poll/pending/preview`                          | Return the pending YAML's contents for inline preview / edit                   |
| `GET /questions/{id}`                                | Return question text for a marker hover tooltip (reads from `.jsonl`)          |
| `POST /audio/stream`                                 | WebSocket upgrade; browser pushes PCM/Opus frames captured via `getUserMedia`  |
| `POST /system/unload-models`                         | Release the Python challenger's resident ASR + LLM (the process keeps running) |
| `POST /system/shutdown`                              | Stop the active session, terminate the challenger, close all listeners         |
| `GET /config`                                        | Config editor page                                                             |
| `POST /config`                                       | Save config (validated before write)                                           |

## Dashboard

`GET /` is the home page. It shows the list of recent saved meeting
files (from `meetings_dir`) and a form to start tracking.

### Connect to meeting

```
Provider  [BigBlueButton ▾]   Meeting ID  [________________]   [Connect]
```

- **Provider** — dropdown populated from the providers enabled in
  config. Defaults to the last-used provider (stored in localStorage).
- **Meeting ID** — free-text field. The exact format depends on the
  provider (BBB internal meeting ID, Meet room code, Zoom meeting
  number). A short inline hint below the field shows the expected format
  for the selected provider.
- **Connect** — submits `POST /session`; the server starts the tracking
  goroutine and redirects to `GET /status`.

While a session is active, the Connect form is replaced by a
**"Stop tracking"** button that submits `DELETE /session`, stops the
session cleanly, and redirects back to `GET /`.

## Meeting status dashboard

`GET /status` is shown when a meeting is active. It does not render
participant timelines — those are post-meeting only.

The page is organized as a **card grid** at the top, followed by a
system log panel below.

### Status cards

Each card shows a single metric at a glance. Clicking an interactive
card reveals its detail panel inline (htmx swap).

**Unregistered participants card:**

```
┌──────────────────────────────┐
│  Unregistered participants   │
│                              │
│            3                 │
│                              │
│  ▾ Show participants         │
└──────────────────────────────┘
```

- Count reflects participants who are currently in the meeting but have
  no confirmed messenger pairing.
- Clicking "Show participants" triggers `GET /status/unregistered` and
  expands the card to list the display names and platform identifiers of
  the unregistered participants.
- List is collapsed again by clicking "Hide".
- Count updates automatically while the meeting is active (htmx polling
  or server-sent events).

**Other cards (initial set):**

- **Last poll** — time since the last challenge poll, or "None yet".
  For file-based: a "Start Poll" button is inlined. For AI-generated:
  shows next scheduled poll time.
- **System status** — green/yellow/red indicator; yellow if any warnings
  in the log, red if any errors.

### System log panel

Below the cards: a rolling log of warnings, errors, and informational
events (delivery failures, missed acks, config-reload notices, poller
events). Severity-coded; new entries appear at the top.

### Poll controls

A single **Trigger poll** button on the status dashboard opens a small
menu with two options:

```
┌────────────────────────────────────────┐
│  Trigger poll                          │
├────────────────────────────────────────┤
│  ▸ Custom bank…                        │
│  ▸ Auto-generated (2 min ago)   [view] │
└────────────────────────────────────────┘
```

- **Custom bank…** opens a file picker (browser-native), pre-validates
  the chosen YAML, and on confirmation submits
  `POST /poll` with `{"type": "custom", "bank_path": "<chosen>"}`.
  Equivalent to running `ptrack poll --type=custom <bank>` from a shell.
- **Auto-generated** is enabled only when `GET /poll/pending` returns a
  non-empty file. The label shows the file's age. Clicking it submits
  `POST /poll` with
  `{"type": "combined", "bank_path": "<pending-file>"}`. A small
  **[view]** affordance next to the option opens the YAML in a modal
  for inline preview/edit before submission (backed by
  `GET /poll/pending/preview`); the edited copy is saved back to the
  same path before the poll is dispatched. On successful submission the
  file is removed from the pending directory.

If auto-generation is configured with `auto_submit: true`, the Python
challenger submits its own polls (with `type=aigenerated`) without ever
populating the menu — the **Auto-generated** option appears only when
the teacher's intervention is expected.

The "Last poll" card shows the most recent poll's `type`, dispatch
time, and result counts (delivered / correct / incorrect / unanswered),
plus a cooldown indicator (`min_gap_between_challenges_seconds`).

Inline edit fields for `auto_generation.poll_interval_seconds` and
`auto_generation.questions_per_poll` are shown when auto-generation is
enabled. Changes apply via `PATCH /poll/config`.

### Audio capture (auto-generation only)

When `challenges.auto_generation.enabled` is true, the status dashboard
shows an **Audio** card with the microphone permission state and a
mute/resume toggle. Audio is captured by the browser through
`navigator.mediaDevices.getUserMedia` and streamed to the daemon over
a WebSocket at `POST /audio/stream`; the daemon relays the frames to
the Python challenger for ASR.

```
┌──────────────────────────────┐
│  Audio                       │
│                              │
│  ● Streaming · Built-in mic  │
│                              │
│  [ Change device ]  [ Mute ] │
└──────────────────────────────┘
```

- **Change device** opens the browser's native device picker via
  `enumerateDevices()`. The chosen device is remembered in
  `localStorage`.
- **Mute** pauses the WebSocket stream; the challenger receives a
  silence marker so faster-whisper does not mis-segment around the
  gap. Resume reopens the stream.
- The card surfaces permission-denied and device-error states with a
  short remediation hint.

Browser-side capture means the same flow works on a mobile browser —
including Android-on-Termux — without any extra OS plumbing.

### System controls

The status dashboard footer (and the bottom of the config editor)
exposes two buttons:

- **Free models** — `POST /system/unload-models`. Releases the
  challenger's resident ASR + LLM and reclaims the multi-gigabyte
  memory footprint. The challenger process keeps running; the next
  generation reloads on demand. Useful between long pauses in
  back-to-back lessons.
- **Shut down** — `POST /system/shutdown`. Stops the active session,
  terminates the challenger, closes all listeners, and replaces the
  current page with a static "ptrack has stopped — you can close this
  browser tab" screen. A confirmation modal is shown first.

Closing the browser tab on its own does **not** shut anything down —
the daemon and the challenger keep running, models stay warm, and a
new tab reconnects to the same session at the same
`http://127.0.0.1:<port>` URL. The **Shut down** button is the only
graceful exit path from the GUI.

## Stats view

`GET /stats` is the single post-meeting analysis page. It accepts one
or more `file` query values, each a basename in `meetings_dir`:

- **One `file` value** — per-meeting mode: one row per participant in
  that file.
- **More than one `file` value** — cross-meeting mode: one participant
  shown at a time, with prev/next paging through all participants seen
  across the requested files, plus a search bar to jump to a specific
  participant by display name.

### Data source

Stats are computed by `ptrack_py stats --in <a.parquet> [--in <b.parquet> …]`,
which returns a JSON document covering everything the templ template
needs: meeting boundaries, per-participant presence ratio, segments,
challenge counts, and challenge markers (with `question_id` for tooltip
lookup). Go shells out, caches the JSON on disk next to the inputs,
and serves it from cache on subsequent requests for the same file set.

Cache invalidation rule: if any of the listed input files' mtime is
newer than the cached JSON's mtime, the JSON is regenerated. The
display-name PATCH (below) rewrites the relevant Parquet files in
place, which naturally bumps their mtime and triggers regeneration on
the next view.

The same JSON powers both modes — the cross-meeting JSON simply
includes the per-meeting block for every (participant, meeting) cell.
Paging and search are done client-side over the loaded JSON; no extra
subprocess is launched per page-flip.

### Page layout — per-meeting mode (1 file)

```
Meeting — 2026-05-01 23:44 – 2026-05-02 00:53    [↓ Export CSV]

[✏ Rename]  Alice Smith                    83% present  3/5 ✓   [██████░░████████░ ...]
[✏ Rename]  Bohdan | B. Kovalenko  ⚠ hint  75% present  2/3 ✓   [░░██████████████░ ...]
[✏ Rename]  Dmytro Petrenko               100% present  4/4 ✓   [████████████████░ ...]
```

Title shows meeting start and end timestamps (ISO local time, no
seconds). The "↓ Export CSV" button downloads
`GET /stats.csv?file=<a.parquet>`.

Each participant row contains:

| Element             | Details                                                                                                                 |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| **✏ Rename**        | Opens an inline edit field; saves via `PATCH /participants/{display_name}/display-name?file=<a.parquet>&new=<new-name>` |
| **Display name**    | The canonical registered name as recorded in this Parquet file                                                          |
| **Presence ratio**  | `XX% present`                                                                                                           |
| **Challenge stats** | `N/M ✓` (correctly answered / total issued)                                                                             |
| **Presence band**   | Inline SVG timeline bar; click opens the presence legend popup                                                          |

### Page layout — cross-meeting mode (>1 file)

```
[🔍 Search participants…]   ◀ 3 / 27 ▶   [↓ Export CSV (all)]

[✏ Rename (all files)]  Alice Smith  [↓ Export CSV]

2026-04-21 09:00 – 10:30  ─── 83% present  3/5 ✓  [████████░░████████ ...]
2026-04-23 09:00 – 10:30  ─── Was not present
2026-04-25 09:00 – 10:30  ─── 75% present  1/2 ✓  [░░░░████████████   ...]
```

The cross-meeting page is a container around a single participant
"card" (Alice Smith above). The container provides:

- **Prev / Next** arrows and a "*N* of *M*" indicator, advancing
  through participants in canonical display-name order
  (case-insensitive). State (current participant index) lives in the
  URL fragment (e.g. `#3`) so reloads land on the same participant.
- **🔍 Search** box that filters the in-page participant list and
  navigates to the matching entry on selection. No new HTTP request:
  search runs over the JSON already in memory.
- **↓ Export CSV (all)** — downloads
  `GET /stats.csv?file=<a>&file=<b>…`, the full cross-meeting CSV for
  the same file set.

Each participant card contains:

- **✏ Rename (all files)** — opens an inline edit field; saves via
  `PATCH /participants/{old}/display-name?file=<a>&file=<b>…&new=<new>`,
  which rewrites every file in the query. Files outside the current
  request — including future meetings — are never touched.
- **↓ Export CSV** — currently a placeholder; the underlying per-
  participant CSV slice is filtered client-side from the full export.
- **Meeting rows** — sorted chronologically. For meetings where the
  participant was absent the row shows "Was not present" and no band.
  Presence bands are scaled proportionally to meeting duration so
  longer meetings get longer bands.

### Per-file rename semantics

The rename PATCH writes only to the Parquet files explicitly listed in
its `file` query parameters: it rewrites the `display_name` column for
every event that currently shows the old name, replacing it with the
new name. Other files (and the participant registry) are never
touched.

Because the session coordinator always writes the canonical registered
name, one Parquet file normally contains exactly one variant per
participant. Renames are useful when a teacher wants to change how a
student appears in stats after the fact, or to merge two histories
that should have been one. Renames never create a persistent name
override for future meetings.

## Timeline chart

The core visual on the stats view in both modes. In per-meeting mode
each row is a participant; in cross-meeting mode each row is one of
the requested meetings for the participant currently on screen.
X-axis is meeting time.

```
time ──►  0:00         0:10         0:20         0:30 ...
         ┌──────────────────────────────────────────────┐
Alice    │██████████████░░░░░░░░░████████ ▲○   ●      ▼ │
Bohdan   │  ██████████████████████████████  ◇✕    ▲○   │
Dmytro   │████ ░░░ ██████████████████████  ●○  ▼ ◇●    │
         └──────────────────────────────────────────────┘
           ^presence band                ^challenge markers
```

### Presence band

A horizontal bar filled where the participant was connected. Split by
thin vertical rules at each state change.

- Solid fill: participant present.
- Light fill / hatched: participant disconnected.

### Challenge markers

Each marker is drawn at the X-coordinate of the `challenge_issued` event.
All markers use the same shape (● circle); the `--type` label of the
poll is shown in the hover tooltip but does not influence the marker's
shape.

| Marker color | Result state                                             |
| ------------ | -------------------------------------------------------- |
| green        | `correct`                                                |
| red          | `incorrect`                                              |
| gray         | `unanswered`                                             |
| hollow       | `skipped_unregistered` or `skipped_offline` (diagnostic) |

**Hover on a marker** fetches `GET /questions/{id}` (which looks up the
record in the meeting's `.jsonl` file) and shows a tooltip with: exact
timestamp, poll `type` label, time-to-answer (for answered ones), and
the question prompt and choices.

### Presence band legend

Clicking anywhere on a presence band (or a dedicated **?** button at the
end of the bar) opens an inline legend popup for that row:

```
        09:05     09:47    09:52     10:12
          |         |        |         |
  [absent] [present] [absent] [present] [absent]
  |             |              |          |    |
09:00         09:30          10:00      10:30  <end>
```

The legend shows the exact join and leave timestamps for each interval,
drawn on the meeting's time axis. Clicking again (or pressing Escape)
dismisses the popup.

## CSV reports

CSV generation is delegated to `ptrack_py`. Go obtains the CSV by
calling `ptrack_py report --in <file> --format csv --out -` (or
`ptrack_py aggregate --in '<glob>' --format csv --out -` for the
multi-file case) and streams the result to the response. The stats
view's `↓ Export CSV` buttons hit `GET /stats.csv?file=…` (one or more
`file` values), which dispatches to the correct `ptrack_py` subcommand
based on the file count.

The stats JSON returned by `ptrack_py stats` and the CSV returned by
the report/aggregate subcommands are computed from the same Polars
expressions, so presence ratios and challenge accuracy never disagree
between the GUI table and the downloaded CSV.

### Single meeting CSV

One row per participant in the opened Parquet file.

| Column               | Description                                                          |
| -------------------- | -------------------------------------------------------------------- |
| `display_name`       | All name variants joined with `\|` if multiple, else the single name |
| `presence_ratio`     | Decimal 0–1 (e.g. `0.83`)                                            |
| `challenges_issued`  | Integer count of challenges issued to this participant               |
| `challenges_correct` | Integer count of correctly answered challenges                       |

### Cross-meeting CSV

One row per (participant, meeting) pair.

| Column               | Description                                |
| -------------------- | ------------------------------------------ |
| `display_name`       | Canonical registered name                  |
| `meeting`            | Meeting identifier (ISO datetime of start) |
| `presence_ratio`     | Decimal 0–1                                |
| `challenges_issued`  | Integer                                    |
| `challenges_correct` | Integer                                    |

Rows are sorted by `display_name` (case-insensitive) then by `meeting`
(chronological). Meetings where the participant was absent appear with
`presence_ratio = 0`, `challenges_issued = 0`, `challenges_correct = 0`.

CLI equivalents:

```
ptrack_py report --in meeting.parquet --format csv --out report.csv
ptrack_py aggregate --in 'meetings/*.parquet' --format csv --out semester.csv
```

## Registry page

`GET /registry` shows every display-name entry currently stored in the
participant registry — one row per registered display name. Each
messenger account holds at most one registration at a time.

### Page layout

```
Registry — Registered Participants          [Clear all]

Display name       Messenger        Registered
────────────────   ──────────────   ──────────────────
Alice Smith        Telegram @alice  2026-04-15 09:12   [Delete]
Ivan Kovalenko     Telegram @ivan   2026-03-01 14:30   [Delete]
```

The table is sorted by display name (case-insensitive). The
**Messenger** column shows the messenger name and the human-readable
label stored at registration time (Telegram @username if available, or
first name).

**[Delete]** calls `DELETE /registry/{display_name}` for that entry,
where `{display_name}` is URL-encoded. A brief inline confirmation is
shown before the request is sent (htmx confirm). The row disappears on
success without a full page reload.

**[Clear all]** calls `DELETE /registry`. A modal confirmation dialog is
shown first. Parquet files are not affected.

When the registry is empty the page shows a short explanation of how
students register (send `/register <display name>` to the bot).

A link to the registry page is placed in the top navigation bar (visible
at all times, not just during a meeting).

## Config editor

See `@docs/CONFIG.md` for what's editable. The GUI editor renders a form
from the JSON Schema:

- Each schema section becomes a collapsible card.
- Required fields marked with `*`.
- Enum fields render as dropdowns; bool as toggles; numbers with min/max
  hints; strings with format validation.
- Secrets live in a separate "Secrets" section with write-only inputs.
  The "Edit secrets" flow writes the secrets file directly without
  round-tripping values to the browser.
- Each section is labeled with its reload scope.
- A language selector and a theme selector (dark/light/system) are
  available in the top navigation bar.

A **"Manage participant registry"** link at the bottom of the config
page navigates to `GET /registry`. Use it to remove stale or incorrect
entries, or to clear the registry at the start of a new course cohort.

## Responsive layout

The GUI is designed to run comfortably in a narrow-screen browser,
including mobile browsers on a phone running Termux or a tablet.

On screens narrower than ~600 px:

- The stats columns (presence ratio, challenge stats) and the presence
  band wrap onto a second line below the participant's name / meeting
  title string.
- The search box and paging controls on the cross-meeting stats view
  remain full-width.
- The navigation bar collapses to a hamburger menu.

## Accessibility

All interactive elements have keyboard handlers. Color is paired with
shape for challenge markers so result states are distinguishable without
relying on color alone.
