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
| `GET /meetings/{id}`                                 | Single meeting analysis view (from Parquet file)                               |
| `GET /meetings/{id}/export.csv`                      | Download CSV report for one meeting                                            |
| `PATCH /meetings/{id}/participants/{display_name}/display-name` | Rename a participant's display name in this meeting's Parquet file. `{display_name}` is URL-encoded. |
| `GET /participants/{display_name}`                   | Cross-meeting aggregate view for one participant (URL-encoded display name)     |
| `GET /participants/{display_name}/export.csv`        | Download cross-meeting CSV for one participant                                 |
| `GET /participants/export.csv`                       | Download cross-meeting CSV for all participants                                |
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

## Meeting analysis view

`GET /meetings/{id}` is the post-meeting per-file analysis page. It is
driven by data from the Python-generated CSV (obtained via
`ptrack_py report --in <file> --out -`); the timeline chart is rendered
from the same Parquet file on the Go side.

### Page layout

```
Meeting — 2026-05-01 23:44 – 2026-05-02 00:53    [↓ Export CSV]

[✏ Rename]  Alice Smith                    83% present  3/5 ✓   [██████░░████████░ ...]
[✏ Rename]  Bohdan | B. Kovalenko  ⚠ hint  75% present  2/3 ✓   [░░██████████████░ ...]
[✏ Rename]  Dmytro Petrenko               100% present  4/4 ✓   [████████████████░ ...]
```

Title shows meeting start and end timestamps (ISO local time, no
seconds). The "↓ Export CSV" button downloads `GET /meetings/{id}/export.csv`.

Each participant row contains:

| Element             | Details                                                                                     |
| ------------------- | -------------------------------------------------------------------------------------------------- |
| **✏ Rename**        | Opens an inline edit field; saves via `PATCH /meetings/{id}/participants/{display_name}/display-name` |
| **Display name**    | The canonical registered name as recorded in this Parquet file                                     |
| **Presence ratio**  | `XX% present`, sourced from the CSV                                                                |
| **Challenge stats** | `N/M ✓` (correctly answered / total issued), sourced from the CSV                                  |
| **Presence band**   | Inline SVG timeline bar; click opens the presence legend popup                                     |

### Per-file rename

The **✏ Rename** button on a meeting analysis row writes only to that
Parquet file: it rewrites the `display_name` column for every event that
currently shows the old name, replacing it with the new name. No other
files are affected.

Because the session coordinator always writes the canonical registered
name (not the raw platform-side display name), one Parquet file
normally contains exactly one variant per participant. Renames are
useful when a teacher wants to change how a student appears in the
analysis view after the fact, or to merge two histories that should
have been one.

Renames never create a persistent name override for future meetings.

## Cross-meeting participant view

`GET /participants/{p}` shows all meetings in which a participant
appears, one row per meeting.

### Page layout

```
[🔍 Search participants…]  [↓ Export CSV (all)]

[✏ Rename (all files)]  Alice Smith  [↓ Export CSV]

2026-04-21 09:00 – 10:30  ─── 83% present  3/5 ✓  [████████░░████████ ...]
2026-04-23 09:00 – 10:30  ─── Was not present
2026-04-25 09:00 – 10:30  ─── 75% present  1/2 ✓  [░░░░████████████   ...]
```

The **🔍 Search** box filters and navigates to other participant pages.
Typing opens a suggestions dropdown; selecting an entry performs a full
page navigation to `GET /participants/{p}`.

**↓ Export CSV (all)** — placed next to the search box; downloads
`GET /participants/export.csv`, a cross-meeting CSV covering every
participant across all saved meeting files.

**↓ Export CSV** — placed next to the participant name; downloads
`GET /participants/{p}/export.csv`, a cross-meeting CSV for this
participant only.

The **✏ Rename (all files)** button rewrites the `display_name` column
for this participant in every Parquet file currently shown in the view
(i.e. the files that produced the meeting rows on this page). Files not
shown — including any future meetings — are never touched.

Meeting rows are sorted chronologically. For meetings where the
participant was absent the row shows "Was not present" and no band.

Presence bands on this page are scaled proportionally to meeting
duration so longer meetings get longer bands, giving an accurate visual
weight.

## Timeline chart

The core visual on both the meeting analysis view and the cross-meeting
view. One row per participant (meeting analysis) or one row per meeting
(cross-meeting). X-axis is meeting time.

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
calling `ptrack_py report --in <file> --format csv --out -` (stdout) and
caches the result in memory for the lifetime of the page. The same data
powers both the GUI stats columns and the downloadable CSV file, so
presence ratios and challenge accuracy are computed only once, in Python.

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
- The search box on the cross-meeting view remains full-width.
- The navigation bar collapses to a hamburger menu.

## Accessibility

All interactive elements have keyboard handlers. Color is paired with
shape for challenge markers so result states are distinguishable without
relying on color alone.
