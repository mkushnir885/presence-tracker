# Ethics

This tool processes continuous attendance and engagement data about real
students and delivers private messages to them. It sits close enough to
classroom surveillance that treating ethics as an afterthought would
undermine both the thesis defense and the tool's legitimacy.

This document is read whenever touching features that collect, store,
transmit, or export participant data.

## Principles

1. **Minimize what's collected.** Every feature defaults to the
   least-invasive mode. Chat content is never stored. Transcript for
   AI-generated challenges never touches disk.
2. **Presence evidence, not behavioral surveillance.** The tool answers
   "was this student engaged?" — not "what were they doing?". Challenge
   scores, not keystroke timing. Join/leave events, not continuous
   activity streams.
3. **Consent is upstream.** Students must know the tool exists, what it
   records, and that they can decline to register a messenger (with the
   acknowledged consequence: no challenges sent, teacher sees this in
   the UI). Consent is not the tool's job to enforce, but the tool
   supports it — see "Required teacher-facing disclosures" below.
4. **Local by default.** All data lives on the teacher's machine; no
   cloud persistence; no analytics phoned home. When a hosted AI backend
   is used, transcripts sent to that backend are the only external
   transmission, and the GUI must warn the teacher before enabling it.
5. **Retention is bounded.** Default 180 days, configurable down; a
   weekly purge job runs. Exported CSV and Parquet files are the
   teacher's responsibility after export.

## Retention rules

| Data                        | Where                         | Default retention | Notes                                                          |
|-----------------------------|-------------------------------|-------------------|----------------------------------------------------------------|
| Raw meeting events          | `meetings/*.parquet`          | 180 days          | Purged by background job                                       |
| Participant registry        | `participants.db`             | Indefinite        | Teacher-managed; GUI can remove entries                        |
| Question content            | `questions/<id>.jsonl`        | Same as meetings  | Purged alongside the corresponding meeting Parquet file          |
| Audio frames (AI-gen)       | Browser → Go → Python; in-memory only | Seconds   | Captured via `getUserMedia`, streamed over WebSocket, consumed by ASR. Never written to disk by Go or Python. |
| Transcript (AI-gen)         | In-memory rolling window only | 20 min            | Never written to disk. Violating this is a design error        |
| Screen-share frames (AI-gen)| In-memory for 1–2 frames only | Seconds           | Discarded after OCR; never persisted                           |
| Auto-generated YAML banks   | `/tmp/ptrack/` (or `%TEMP%\ptrack\`) | Until submitted or superseded | Removed when the bank is dispatched as a poll, or when a newer bank replaces it |
| Generated CSV reports       | `reports/*.csv`               | Until deleted     | Teacher's responsibility                                       |
| Secrets (tokens, keys)      | `secrets.yaml` (protected)    | Until rotated     | Never copied into the event log                                |

## Privacy-preserving defaults

- Chat messages: **not stored** at all. Chat is not monitored — pairing
  uses display-name registration through the messenger bot, not chat
  scraping.
- Telegram handles: stored in the participant registry (not the event
  log); the event log only references the canonical `display_name` that
  the student registered. The Telegram chat ID never leaves the
  registry.
- Unverified joins are buffered in memory only; if the student denies
  verification or leaves before answering, no Parquet event is written
  for them.
- No mic, camera, or screen-share state of meeting participants is
  tracked or stored.
- Meeting recordings: out of scope. The tool does not record audio or
  video.
- The teacher's microphone (used only when AI-generated challenges are
  enabled) is captured through the browser's `getUserMedia` flow, with
  the platform's native permission dialog and an explicit mute toggle
  in the GUI. Audio is streamed in memory only; neither the browser,
  the Go daemon, nor the Python challenger writes audio bytes to disk.

## Required teacher-facing disclosures

The GUI and first-run wizard present these to the teacher before enabling
data collection:

- **What will be collected:** names, presence intervals (join/leave),
  messenger handles (via voluntary registration), challenge answers.
- **What will not be collected:** chat content, video frames, mic/camera
  state, audio recordings.
- **What happens if AI-generated challenges are enabled:** transcript of
  the teacher's speech is processed in memory; if a hosted LLM backend
  is selected, that transcript leaves the teacher's machine. A distinct
  warning banner is shown for this configuration.
- **A suggested announcement template** for the teacher to read to
  students at the start of the first lesson with the tool. Available in
  English and Ukrainian. Located in
  `py/src/ptrack_analytics/templates/consent-notice/`.

## Opt-out path

A student who declines to register is:

- Still tracked for provider-side events (join/leave — the meeting
  platform sees them either way; the tool reads that).
- Never sent challenges.
- Shown in the GUI with a "not registered" indicator.
- Not penalized in the tool's output — it produces a record, not a grade.

Whether participation in challenges is required for grading is a decision
for the teacher and the institution. The tool takes no position.

## Topics for the thesis ethics chapter

Minimum bar:

- Mention the Zoom "attention tracking" feature withdrawal (2020) as the
  cautionary parallel.
- Cite Proctorio / Respondus and contrast — this tool explicitly rejects
  face recognition, browser lockdown, gaze tracking, and room scans.
- GDPR framing: legitimate-interest basis, DPIA considerations if used
  in an EU institution, right-to-erasure (how a student request is
  honored — removing their row from the registry and redacting their
  events from Parquet files).
- Acknowledge the surveillance-aesthetic problem: even a well-designed
  tool changes the classroom dynamic. Recommend it as a *supplement* to
  teacher judgment, not a replacement.
- Acknowledge failure modes that harm students: false `unanswered` marks
  from connectivity issues, messenger delivery failures, challenge
  misgenerations. The tool must distinguish these from genuine
  non-engagement in both events and GUI.

## Features requiring explicit GUI acknowledgement before enabling

The following changes surface a blocking modal that the teacher must
read and confirm before proceeding:

- Setting `gui.bind_addr` to anything other than `127.0.0.1`.
- Enabling `challenges.auto_generation.generator.backend` to a hosted
  provider (OpenAI, Gemini).
- Raising `retention_days` above 365.

The acknowledgement is logged as a `system` event so there's a record
of when the teacher opted in.
