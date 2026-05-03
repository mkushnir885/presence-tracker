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
| `PATCH /meetings/{id}/participants/{p}/display-name` | Rename a participant's display name in this meeting's Parquet file             |
| `GET /participants/{p}`                              | Cross-meeting aggregate view for one participant                               |
| `GET /participants/{p}/export.csv`                   | Download cross-meeting CSV for one participant                                 |
| `GET /participants/export.csv`                       | Download cross-meeting CSV for all participants                                |
| `DELETE /participants`                               | Clear all registered participants (pairing data only; Parquet files untouched) |
| `POST /meetings/{id}/polls`                          | Trigger a file-based poll for an active meeting                                |
| `PATCH /meetings/{id}/polls/config`                  | Update AI-generated poll config mid-meeting                                    |
| `GET /questions/{id}`                                | Return question text for a marker hover tooltip (reads from `.jsonl`)          |
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

For **file-based** polls: file picker with pre-validation, "Start Poll"
button, most-recent poll status (delivered / answered / unanswered
counts), cooldown indicator. These controls live in the "Last poll" card
or in a dedicated poll panel below the cards.

For **AI-generated** polls: `poll_interval_seconds` and
`questions_per_poll` with inline edit fields. Changes applied via
`PATCH /meetings/{id}/polls/config`. Last-poll summary.

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
| ------------------- | ------------------------------------------------------------------------------------------- |
| **✏ Rename**        | Opens an inline edit field; saves via `PATCH /meetings/{id}/participants/{p}/display-name`  |
| **Display name**    | All distinct `display_name` values seen in the file for this participant, joined with `\|`  |
| **⚠ hint**          | Shown only when multiple name variants exist; clicking reveals "Rename to unify" suggestion |
| **Presence ratio**  | `XX% present`, sourced from the CSV                                                         |
| **Challenge stats** | `N/M ✓` (correctly answered / total issued), sourced from the CSV                           |
| **Presence band**   | Inline SVG timeline bar; click opens the presence legend popup                              |

### Display name variants

If the platform or the user sent different names for the same
`participant_id` within one meeting, the Parquet file contains multiple
distinct `display_name` strings for that participant. The GUI:

1. Joins all variants with `|` in every place a name is displayed
   (row label, CSV, reports).
2. Flags the row with a ⚠ indicator and a tooltip: "Multiple display
   names detected — click ✏ Rename to set a single canonical name."

The ⚠ indicator disappears once a per-file rename has been applied.

### Per-file rename

The **✏ Rename** button on a meeting analysis row writes only to that
Parquet file: it rewrites the `display_name` column for all events
belonging to `participant_id` in that file, replacing every variant with
the new name. No other files are affected.

Renames never create a persistent name override for future meetings.
Future meetings record whatever name the platform provides at join time.

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

| Marker shape | Challenge type |
| ------------ | -------------- |
| ● circle     | `filebased`    |
| ◆ diamond    | `aigenerated`  |
| ▲ triangle   | future types   |

| Marker color | Result state                                             |
| ------------ | -------------------------------------------------------- |
| green        | `correct`                                                |
| red          | `incorrect`                                              |
| gray         | `unanswered`                                             |
| hollow       | `skipped_unregistered` or `skipped_offline` (diagnostic) |

**Hover on a marker** fetches `GET /questions/{id}` (which looks up the
record in the meeting's `.jsonl` file) and shows a tooltip with: exact
timestamp, challenge type, time-to-answer (for answered ones), and the
question prompt and choices.

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
| `display_name`       | Same multi-variant format as above         |
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

A **"Clear registered participants"** button is shown at the bottom of
the config page (outside the schema-driven form). It calls
`DELETE /participants`, which wipes all pairing records from the
participant registry. A confirmation dialog is shown before the action
executes. Parquet files are not affected — only the pairing data that
links Telegram handles to platform identifiers is removed. Use this when
starting a new course cohort or when testing the pairing flow.

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
