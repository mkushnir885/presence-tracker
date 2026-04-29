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
   Translation files: `go/src/internal/gui/locales/<lang>.json` (adding a new
   language requires only a new JSON file and registering it in the
   language selector).

## Routes

| Route                               | Purpose                                                                    |
|-------------------------------------|----------------------------------------------------------------------------|
| `GET /`                             | Dashboard: recent meetings list, quick stats                               |
| `GET /status`                       | Meeting status dashboard (active meeting only)                             |
| `GET /status/unregistered`          | htmx fragment: list of unregistered participants for the status card       |
| `GET /meetings/{id}`                | Single meeting analysis view (from Parquet file)                           |
| `GET /participants`                 | Participant registry: list, pairing status, display name editor            |
| `PATCH /participants/{p}`           | Update a participant's custom display name                                 |
| `GET /participants/{p}`             | Cross-meeting aggregate view for one participant                           |
| `POST /meetings/{id}/polls`         | Trigger a file-based poll for an active meeting                            |
| `PATCH /meetings/{id}/polls/config` | Update AI-generated poll config mid-meeting                                |
| `POST /reports/meeting/{id}`        | Generate PDF report for one meeting                                        |
| `POST /reports/aggregate`           | Multi-meeting aggregate PDF (body: meeting IDs)                            |
| `GET /questions/{id}`               | Return question text for a marker hover tooltip (reads from meeting `.jsonl`) |
| `GET /config`                       | Config editor page                                                         |
| `POST /config`                      | Save config (validated before write)                                       |

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

- **Active participants** — number of participants currently in the
  meeting.
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

"Generate PDF" button disabled while the meeting is active; enabled once
`meeting_ended` fires.

## Timeline chart

The core visual for the meeting analysis view. One row per participant;
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

| Marker shape | Challenge type  |
|--------------|-----------------|
| ● circle     | `filebased`     |
| ◆ diamond    | `aigenerated`   |
| ▲ triangle   | future types    |

| Marker color | Result state                                               |
|--------------|------------------------------------------------------------|
| green        | `correct`                                                  |
| red          | `incorrect`                                                |
| gray         | `unanswered`                                               |
| hollow       | `skipped_unregistered` or `skipped_offline` (diagnostic)  |

**Hover on a marker** fetches `GET /questions/{id}` (which looks up the
record in the meeting's `.jsonl` file) and shows a tooltip with: exact
timestamp, challenge type, time-to-answer (for answered ones), and the
question prompt and choices.

### Cross-meeting participant view

`GET /participants/{p}` stacks timeline charts per meeting. The teacher
can select N saved meetings and see the cross-meeting view aggregated
over that selection.

## Participant registry view

`GET /participants` shows all known participants with:

- Internal ID, platform identifiers, pairing status per platform.
- Custom display name field (editable inline; saved via
  `PATCH /participants/{p}`).
- Participants who appeared in a meeting but are not yet paired are
  highlighted as "needs pairing".

## PDF reports

Two report types, both delegated to `ptrack_py`:

**Meeting report** (one meeting, one PDF):
- Cover page with meeting date, duration, attendees summary.
- Per-participant one-line summary (presence ratio, challenge accuracy).
- Timeline chart per participant (same visual as the GUI, scaled down).

**Aggregate report** (many meetings, one PDF):
- Cover page with date range and meeting list.
- Per-participant: cross-meeting trend chart + a table summarizing each
  meeting.

PDF and charts are generated with matplotlib + fpdf2 so the same code
path works in Jupyter Notebooks without modification.

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

## Accessibility

All interactive elements have keyboard handlers. Color is paired with
shape for challenge markers so result states are distinguishable without
relying on color alone.
