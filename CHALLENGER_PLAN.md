# Auto-generation rewrite — implementation plan

Status: design locked, ready to implement.
Branch to start work from: `main` (commit `7859fd3` carries the doc updates).
Previous attempt archived at: `archive/python-challenger` (do not merge — see "Migration notes" below for cherry-pick targets).

This plan supersedes the Python-challenger design. The new design is **in-process Go** talking to **external OpenAI-compatible HTTP** backends, with **browser-driven chunking** via `MediaRecorder`.

---

## 1. Goal

Replace the long-running Python challenger subprocess with an in-process Go package that:

- accepts audio from the browser as discrete Opus/WebM blobs (one per poll interval),
- transcribes each blob via an OpenAI-compatible `/v1/audio/transcriptions` endpoint,
- accumulates transcripts across intervals until there is enough text to generate at least one question,
- calls an OpenAI-compatible `/v1/chat/completions` endpoint to produce a YAML question bank,
- when `auto_submit = true`, dispatches the bank in-process via the existing `challenges.Pipeline` (no disk write); otherwise writes the bank YAML to `challenges.auto_generation.review_dir` for the teacher to inspect and dispatch via the GUI menu.

The default local backend for both ASR and LLM is **Ollama**. Hosted backends (OpenAI, Gemini, any compatible gateway) are interchangeable — only `base_url`, `api_key`, and `model` change.

ptrack owns no model weights. There is no resident inference memory in the daemon. Lifecycle of the backend (start/stop/free) is the operator's concern.

---

## 2. Architecture

```
Browser:  getUserMedia → MediaRecorder (Opus)
              │  stop() on setInterval(poll_interval_seconds)
              ▼  onstop → fetch POST /audio/segment with the WebM blob
              │  (then immediately rec.start() for the next interval)
Go:    /audio/segment → challenger.Service.Generate(blob, mime)
              │            → ASR (OpenAI-compat /v1/audio/transcriptions)
              │            → accumulator append
              │            → if words >= min_words_per_question:
              │                 LLM (OpenAI-compat /v1/chat/completions)
              │                 → YAML normalize + validate
              │                 → if auto_submit:  Pipeline.RunPoll(in-memory bank, "aigenerated")
              │                   else:            write bank YAML to review_dir
              ▼
        JSON response: { status: generated|skipped|failed, ... }
```

No WebSocket, no audio ring buffer in Go, no server-side cut policy, no scheduler in Go.

---

## 3. Flow (authoritative pseudocode)

`Service.Generate(ctx, audio io.Reader, mime string) (Result, error)`:

```
transcript = ASR(audio, mime)
if err != nil:
    return Result{Status: Failed, Reason: "asr_error"}, nil
if wordCount(transcript) < silenceFloor (5):
    return Result{Status: Skipped, Reason: "silence_or_too_short",
                  words: accumulator.words, needed: needed}, nil

accumulator.append(transcript)
needed = cfg.MinWordsPerQuestion           // = words needed for 1 question
n = min(cfg.MaxQuestionsPerPoll,
        accumulator.words / cfg.MinWordsPerQuestion)

if n < 1:
    return Result{Status: Skipped, Reason: "below_threshold",
                  words: accumulator.words, needed: needed}, nil

bank, err = producer.Generate(ctx, accumulator.text, n)
if err != nil:
    return Result{Status: Failed, Reason: "llm_error"}, nil
    // accumulator NOT cleared — content is still useful next time

if cfg.AutoSubmit:
    err = dispatcher.RunPoll(ctx, bank, "aigenerated")
    if err != nil:
        return Result{Status: Failed, Reason: "dispatch_error"}, nil
        // accumulator NOT cleared
else:
    err = reviewDir.Write(bank)        // sweeps older auto-*.yaml as it writes
    if err != nil:
        return Result{Status: Failed, Reason: "write_error"}, nil

accumulator.clear()
return Result{Status: Generated, Questions: len(bank.Questions)}, nil
```

Key invariant: nothing is written to disk in the `auto_submit = true` path.

The accumulator is a `strings.Builder` + word count, scoped per-session, protected by a mutex. Cleared on session start (sweep stale state) and on successful generation. Not persisted to disk.

---

## 4. Config schema additions

In `go/src/internal/config/config.go`, add under `challenges.auto_generation`:

```jsonc
{
  "enabled":                  false,   // bool
  "auto_submit":              false,   // bool
  "poll_interval_seconds":    300,     // int, minimum 30
  "min_words_per_question":    30,     // int, minimum 5
  "max_questions_per_poll":     5,     // int, minimum 1, maximum 20
  "review_dir":                "~/Documents/ptrack/pending-banks",  // string; only used when auto_submit = false
  "asr": {
    "base_url": "http://127.0.0.1:11434",   // string, required when enabled
    "api_key":  "",                          // string, writeOnly, optional for local
    "model":    "whisper"                    // string, required when enabled
  },
  "llm": {
    "base_url": "http://127.0.0.1:11434",
    "api_key":  "",
    "model":    "qwen2.5:3b"
  }
}
```

Validation:
- When `enabled = true`, both `asr.base_url` and `llm.base_url` are required.
- When `enabled = true` and `auto_submit = false`, `review_dir` is required and must be writable.
- `api_key` fields are `writeOnly: true` (masked in the config editor).
- `poll_interval_seconds` minimum 30 (smaller is impractical for question generation).
- `review_dir` supports `~` expansion and forward-slash separators on Windows; normalized to OS-native absolute path on load.

Regenerate `config.schema.json` via `go run ./src/cmd/schemagen` after each schema change.

Reload semantics: `challenges.*` already applies on the next poll round per `docs/CONFIG.md`. Schema editor edits take effect on the next `/audio/segment` POST.

---

## 5. File inventory

### New files

```
go/src/internal/challenger/
├── doc.go         // package comment
├── challenger.go  // Service type, New, Generate, accumulator
├── asr.go         // OpenAI-compat /v1/audio/transcriptions client
├── llm.go         // OpenAI-compat /v1/chat/completions client
├── prompts.go     // system prompt — documented public contract
├── producer.go    // LLM response → Bank (parse JSON or YAML, normalize, validate)
├── reviewdir.go   // review-dir helpers (atomic write, list, sweep, remove); only used when auto_submit = false
└── *_test.go      // httptest-driven tests for asr, llm, producer, reviewdir, accumulator

go/src/internal/gui/views/assets/audio.js   // MediaRecorder lifecycle
```

Estimated size: ~400 lines of Go incl. tests, ~120 lines of JS.

### Modified files

- `go/src/internal/config/config.go` — add `AutoGeneration` struct + validation
- `go/src/cmd/ptrack/main.go` — instantiate `Service` when `enabled`; pass `challenges.Pipeline` as `Dispatcher`; register `/audio/segment` handler
- `go/src/internal/gui/server.go` — register `/audio/segment`, `/poll/pending`, `/poll/pending/preview`, `/system/shutdown`
- `go/src/internal/gui/views/status.templ` — Audio card, Trigger Poll menu (Custom / Auto-generated), Shut down button
- `go/src/internal/gui/locales/en.json` + `uk.json` — strings for new UI
- `go/src/internal/session/coordinator.go` — clear accumulator on session start/end (or scope `Service` per session)

### Interfaces / contracts

```go
// internal/challenger/challenger.go
type Dispatcher interface {
    RunPoll(ctx context.Context, bank challenges.Bank, typeLabel string) error
}

type Service struct {
    cfg        Config
    asr        *ASRClient
    llm        *LLMClient
    producer   *Producer
    pending    *PendingDir
    dispatcher Dispatcher

    mu          sync.Mutex
    accumulator strings.Builder
    words       int
}

func New(cfg Config, dispatcher Dispatcher) *Service
func (s *Service) Generate(ctx context.Context, audio io.Reader, mime string) (Result, error)
func (s *Service) ResetAccumulator()  // called on session start
```

```go
// internal/challenger/asr.go
type ASRClient struct { /* http.Client, base_url, api_key, model */ }
func (c *ASRClient) Transcribe(ctx context.Context, audio io.Reader, mime string) (string, error)
```

```go
// internal/challenger/llm.go
type LLMClient struct { /* same */ }
func (c *LLMClient) Generate(ctx context.Context, prompt string) (string, error)
```

```go
// internal/challenger/reviewdir.go
type ReviewDir struct { /* dir path */ }
func (r *ReviewDir) Write(bank challenges.Bank) (path string, err error)  // atomic, newest-wins (sweeps older auto-*.yaml)
func (r *ReviewDir) List() ([]Entry, error)
func (r *ReviewDir) Read(path string) (challenges.Bank, error)
func (r *ReviewDir) Remove(path string) error
func (r *ReviewDir) Sweep() error  // delete all auto-*.yaml; called on session start
```

Only constructed and used when `auto_submit = false`. The `auto_submit = true` path never touches disk.

```go
// internal/challenger/challenger.go (response type returned via /audio/segment)
type Result struct {
    Status    string `json:"status"`              // generated | skipped | failed
    Reason    string `json:"reason,omitempty"`    // for skipped / failed
    Questions int    `json:"questions,omitempty"` // for generated
    Words     int    `json:"words,omitempty"`     // for skipped (accumulator state)
    Needed    int    `json:"needed,omitempty"`    // for skipped (= min_words_per_question)
}
```

---

## 6. Phases

Each phase is independently committable and leaves `main` shippable.

### Phase 1 — Config schema

- Add `AutoGeneration` struct to `internal/config/config.go` with the keys from §4.
- Add validation rules (cross-field: when `enabled`, require base URLs).
- Regenerate `config.schema.json` via `schemagen`.
- No GUI surfacing yet.

**Commit:** `feat(config): add auto_generation schema`

### Phase 2 — Challenger package skeleton

- New `internal/challenger/` directory.
- `doc.go`, `challenger.go` with the `Service` / `Dispatcher` types from §5.
- `Generate` logs and returns `Result{Status: "skipped", Reason: "not_implemented"}`.
- Unit test that constructs a `Service` and calls `Generate` with a stub `io.Reader`.

**Commit:** `feat(challenger): package skeleton`

### Phase 3 — ASR client

- `asr.go`: `Transcribe` posts multipart to `<base_url>/v1/audio/transcriptions`, streams body through, parses `{"text": "..."}` response.
- `asr_test.go` with `httptest.Server`: success path, 5xx, malformed response, timeout.
- Manually verify once against a local Ollama running Whisper.

**Commit:** `feat(challenger): ASR client (OpenAI-compatible)`

### Phase 4 — LLM client + prompts + YAML normalization

- `llm.go`: `Generate` posts chat-completions request with `model`, `temperature` (default 0.4), `messages` (system + user), parses `choices[0].message.content`. No `response_format` field — see "Output-format strategy" below.
- `prompts.go`: system prompt instructing the LLM to produce a YAML bank with N questions from the given transcript, **with 2–3 worked examples in the prompt body**. The schema is conveyed by example, not by embedded JSON Schema or `response_format`. **This file is the documented public contract** — treat changes as breaking.
- `producer.go`: parse LLM response (tolerate JSON or YAML; try both; tolerate prose wrapping the YAML/JSON block), normalize to `challenges.Bank` shape, run `challenges.Validate`, drop invalid questions silently. Return typed errors so `Generate` can distinguish `asr_error` from `llm_error` from `validation_error`.
- Tests: golden inputs for valid YAML, valid JSON, malformed YAML, empty response, response with extra prose around the YAML block.

#### Output-format strategy (locked-in)

Use **prose-and-examples in the prompt only**. Do *not* embed a JSON Schema, do *not* set `response_format`. Rationale:

- The default backend is small local models (Qwen 2.5 3B class), which follow examples more reliably than abstract schemas.
- `response_format: json_schema` is not portably supported (Ollama: model-dependent; OpenAI: 4o-series; Gemini: separate API). Adopting it would silently re-couple us to specific backends — exactly the property the rewrite removed.
- The cost of malformed output is bounded: `producer.go` drops invalid questions silently; if the whole bank is unusable, `Generate` emits `challenge_generator_failed` and retains the accumulator for the next interval. We already absorb this case gracefully.

The validator in `internal/challenges/validate.go` remains the single source of truth for what counts as valid. The prompt is *guidance*; mismatches just mean the validator drops questions. No effort to keep prompt and validator in lockstep.

A future optimisation (phase 4.1, not in scope now) is to opportunistically set `response_format: { type: "json_object" }` when the backend advertises support. Token cost is small; portability impact is smaller than full json_schema mode. Defer until real malformed-output rates justify it.

**Commit:** `feat(challenger): LLM client, prompts, YAML producer`

### Phase 5 — Review directory helpers

- `reviewdir.go`: atomic write (tmp file in same dir + rename); list returns sorted by mtime desc; sweep removes all `auto-*.yaml` matching the pattern.
- Filename pattern: `auto-<RFC3339-compact>.yaml`, e.g. `auto-20260527T140532Z.yaml`.
- On successful write, delete older `auto-*.yaml` so only the newest remains (newest-wins policy from `docs/CHALLENGES.md`).
- Directory created on first write if it does not exist (with parents).
- Only constructed by `Service` when `auto_submit = false`. The `auto_submit = true` code path does not touch this module.
- Tests: atomicity (interrupt mid-write), sweep, newest-wins behaviour, directory creation.

**Commit:** `feat(challenger): review directory helpers`

### Phase 6 — Wire Generate end-to-end

- Replace the phase-2 stub with the §3 pseudocode.
- Add the accumulator (mutex-protected `strings.Builder` + `words int`).
- Apply the 5-word silence floor before appending.
- Emit `challenge_generator_failed` events on `asr_error` / `llm_error` (via the existing `eventstore` API).
- Test end-to-end with mocked ASR + LLM HTTP servers.

**Commit:** `feat(challenger): wire Generate end-to-end`

### Phase 7 — POST /audio/segment endpoint

- New handler in `cmd/ptrack/main.go` (mounted alongside existing `/poll` and GUI routes).
- Body limit: 64 MB (rejects accidental PCM, covers ~30 min of Opus).
- Conditional registration: only mount when `challenges.auto_generation.enabled = true`.
- Returns 409 if no active session; otherwise calls `Service.Generate` and serializes `Result` as JSON.
- Response: 200 always (semantic outcome is in the body); 4xx only for protocol errors.
- Wire `challenges.Pipeline` as `Dispatcher` when constructing `Service`.

**Commit:** `feat(challenger): POST /audio/segment endpoint`

### Phase 8 — Browser audio capture

- New `internal/gui/views/assets/audio.js`, loaded on `/status` when `auto_generation.enabled`.
- On mount: `navigator.mediaDevices.getUserMedia({ audio: true })`, render permission state into Audio card.
- Create `MediaRecorder({ mimeType: 'audio/webm;codecs=opus' })`, start it.
- `setInterval(poll_interval_seconds * 1000)` calls `rec.stop()`.
- In `rec.onstop`: build Blob, immediately `rec.start()`, then `fetch('/audio/segment', { method: 'POST', body: blob, headers: { 'Content-Type': 'audio/webm' } })`.
- Render JSON response into Audio card status line:
  - `generated`: "Last poll generated 3 questions at HH:MM"
  - `skipped` (silence): "Silent interval; nothing to generate"
  - `skipped` (below threshold): "Not enough speech this interval (25/30 words); merging into next"
  - `failed`: "Generation failed: ASR backend unreachable"
- Manual "trigger auto-generated poll now" button calls `rec.stop()` directly.
- Mute = `rec.pause()` / `rec.resume()` (built-in; resulting blob skips paused intervals natively).
- Device picker: `enumerateDevices()`, store chosen `deviceId` in `localStorage`, restart capture with the new `deviceId` on change.
- Surface `poll_interval_seconds` to JS via a `data-poll-interval` attribute rendered by templ.

**Commit:** `feat(gui): browser-side audio capture via MediaRecorder`

### Phase 9 — GUI surfaces

- Replace the single "Trigger poll" button on `/status` with the Custom / Auto-generated menu (templ + htmx, per `docs/GUI.md`).
- `GET /poll/pending` — htmx fragment: filename + age, or "no pending YAML".
- `GET /poll/pending/preview` — returns the pending YAML for the modal; PUT-back saves before dispatching as `combined`.
- `POST /system/shutdown` — handler + button: stops session, drains in-flight `Generate` calls (close `Service` ctx, wait), closes listeners, returns 200. Browser then renders the static "you can close this tab" page.
- Audio card on `/status`: only rendered when `auto_generation.enabled`. Shows microphone permission state, current device, mute toggle, status line from `audio.js`.
- i18n strings in `gui/locales/en.json` + `uk.json` for all new UI.

**Commit:** `feat(gui): trigger poll menu, audio card, shutdown control`

### Phase 10 — Config editor surfacing

- The schema-driven form should automatically pick up the new `auto_generation` subtree once the schema is regenerated.
- Verify: `*.api_key` is masked (write-only); `poll_interval_seconds` and `max_questions_per_poll` show min/max hints; `min_words_per_question` shows its minimum.
- Add a small inline hint on the section pointing to Ollama setup ("default backend assumes a local Ollama at 127.0.0.1:11434").

**Commit:** `feat(gui): surface auto_generation in config editor`

### Phase 11 — End-to-end smoke test

- Drive a fixture meeting with `--provider=mock`.
- Point `asr.base_url` and `llm.base_url` at a local Ollama instance with Whisper + Qwen 2.5 3B.
- Confirm:
  - Granting mic permission starts capture.
  - After `poll_interval_seconds`, a blob is POSTed; the response is `skipped` or `generated`.
  - With `auto_submit = false`: a YAML lands in `review_dir` on `generated`.
  - With `auto_submit = true`: no disk write happens; a `challenge_issued` event appears in the meeting's Parquet directly.
  - With `auto_submit = true`, a `challenge_issued` event appears in the meeting's Parquet, and a Telegram DM arrives in the mock messenger.
  - With `auto_submit = false`, the GUI menu shows the file and manual dispatch works.
  - Mute → audio card reflects it; the next blob omits muted audio.

No new commit; this is a sanity check before opening a PR to merge the cumulative work.

---

## 7. Locked-in design decisions

Recorded so future-you doesn't relitigate them:

| Decision | What | Why |
|---|---|---|
| **Browser-side chunking** | `MediaRecorder` stop/restart per poll, not server-side framing of a PCM stream | Eliminates the WebSocket, ring buffer, scheduler, and WAV encoder. Trade-off: 10–50 ms gap on stop/restart (negligible) and a longer gap during ASR processing (acceptable). |
| **One-shot per poll** | Single ASR call per `poll_interval`, not continuous transcription | Simpler code; Whisper handles long-form better than chopped segments. Trade-off: no transcript before poll time and all-or-nothing failure per cycle (mitigated by accumulator + retain-on-failure). |
| **Adaptive question count** | `n = min(max_questions, words / min_words_per_question)` | One fewer config knob (no `questions_per_poll`). Number of questions tracks how much the teacher actually said. |
| **Skip-and-accumulate on thin transcript** | Hold transcript across intervals until `min_words_per_question` is met | Natural backoff during silent stretches; no polls fire when there is nothing to ask about. |
| **No advisory recording length** | Browser keeps a metronomic `setInterval(poll_interval_seconds)` regardless of server response | Simpler client; the accumulator on the server handles all variable-density situations. |
| **OpenAI-compat HTTP only** | No vendor-specific streaming protocols, no in-process model weights | Backend portability: Ollama today, hosted API tomorrow, research model the day after. |
| **No "Free models" button** | ptrack holds no weights; memory is the backend's problem | Removed `/system/unload-models`, `preload_models`, `idle_unload_after_seconds`. |
| **No `transcript_window_minutes`** | Accumulator self-bounds: resets on success | Removed from config. Persistent-silence meetings naturally generate nothing. |
| **5-word silence floor** | Drop ASR results with <5 words before appending | Cheapest defence against Whisper hallucinations ("Thanks for watching!", "Subtitles by..."). Tunable; tracking-only if it filters real terse speech. |
| **Force-generation cap removed** | No `max_buffer_words` / `max_cycle_seconds` | Accumulator only grows during sub-threshold intervals. Real pathology (LLM always returns garbage) shows up as `challenge_generator_failed` events for the teacher to act on. |
| **No disk write on auto-submit** | When `auto_submit = true`, `Bank` is passed in-memory to `Pipeline.RunPoll`. When `auto_submit = false`, written to `review_dir` (user-configurable, defaults under `~/Documents/ptrack/`). | The pending-dir-on-`/tmp/` was a Python-IPC holdover. In-process dispatch needs no disk hand-off. The review-dir path is user-facing data and belongs next to other settable dirs, not in temp. |
| **Output format conveyed by examples in prompt, not by `response_format` or embedded JSON Schema** | Prose + 2–3 worked examples in `prompts.go`. No `response_format` field on the LLM request. | (1) Small local models follow examples better than schemas. (2) `response_format: json_schema` is not portably supported — adopting it would re-couple us to specific backends. (3) `producer.go` already tolerates JSON/YAML/prose-wrapped output, so the marginal accuracy win doesn't pay for the portability loss. |

---

## 8. Edge case behaviour (test matrix)

| Situation | Expected behaviour |
|---|---|
| Teacher monologues 400 words in 5 min | `floor(400/30) = 13` capped at `max_questions_per_poll = 5` → 5-question poll |
| Teacher quiet — 25 words in 5 min | `skipped`, 25 words held; next interval adds 20 → 45 words → 1-question poll, accumulator clears |
| Whisper returns "Thanks for watching!" (4 words) on silent input | <5 → dropped before append, accumulator untouched |
| Whisper returns "Thank you for watching, please subscribe!" (6 words) | Accumulated. (Acceptable for v1; revisit with a blocklist if real-world hits this.) |
| Entire meeting near-silent | No polls fire. Correct outcome — no content to question. |
| Narrow-miss pattern (24, 24, 24, …) | After 2nd interval: 48 words → 1 question, reset |
| ASR backend down | `failed` (`asr_error`), accumulator untouched, next interval retries naturally |
| LLM returns malformed YAML | `failed` (`llm_error`), accumulator **retained**, next interval retries with more context |
| LLM returns malformed YAML repeatedly | `challenge_generator_failed` events accumulate in the session log; teacher sees and can stop manually |
| Session ends mid-cycle with words in accumulator | Accumulator dropped on session end; no cross-session leakage |
| Teacher closes the GUI tab | Capture stops; on tab reopen, fresh `MediaRecorder` starts; server accumulator from before the close is still there until next successful generation (acceptable) |
| Teacher changes `poll_interval` mid-meeting | Browser page reload required to pick up the new value (v1 limitation, documented) |
| `auto_submit = true` and `Pipeline.RunPoll` fails (no eligible participants, messenger offline, etc.) | `Result.Status = failed`, accumulator retained, no disk write happens. Next interval retries with more context. |
| `auto_submit = false` and `review_dir` is unwritable | `Result.Status = failed`, `Reason = write_error`. Teacher sees it in the system log; fix the path in the config editor. |

---

## 9. Open questions (decide from data, not up-front)

- **5-word silence floor.** Right enough to ship. Lower if real lessons show terse utterances ("Open page forty-two") being dropped; raise or add a blocklist if 6+ word hallucinations come through.
- **`min_words_per_question` default of 30.** Guess. Log the input word count on every `Generated` result and look at the ratio of good vs. bad questions after a few real lessons.
- **`temperature = 0.4` for the LLM call.** Conservative middle ground. Higher → more variety, more drift; lower → more deterministic, more repetitive across polls.
- **Idempotency of segment POSTs.** A network blip during a POST means the browser might retry; on success the server has already accumulated the transcript. Risk: double-count in the accumulator. Mitigations to consider when phase 7 is in code: `Idempotency-Key` header from the browser (random per stop event), server dedupes on a small LRU.
- **Backoff on persistent ASR failure.** Currently: zero retries, next `poll_interval` is the natural cadence. If real backends turn out to be flaky, consider a single retry with jitter inside `Generate`.
- **Audio mime acceptance.** Recommendation: accept anything on the endpoint, let ASR be the validator. Re-check if a backend in scope only accepts WAV.

---

## 10. Migration notes — `archive/python-challenger`

The branch holds the abandoned Python-subprocess approach. **Do not merge.** Selectively cherry-pick:

| Commit | Useful for | Notes |
|---|---|---|
| `6843ab1` feat(gui): surface auto_generation keys in config editor | Phase 10 | Schema keys differ — review before applying. Some labels/help text reusable. |
| `f057fb6` feat(challenger): Go-side wiring for auto-generation | **Don't cherry-pick** | This was the subprocess supervisor; obsolete. |
| `a9db801` feat(challenger): OpenAI-compatible ASR + LLM backends (Phase B) | Phase 3 / Phase 4 reference | Python code, not directly reusable, but the prompt text in `prompts.py` is a sensible starting point for `prompts.go`. |
| `624ff3f` feat(challenger): Python auto-generation service (Phase A) | **Don't cherry-pick** | Subprocess infra, obsolete. |
| `4499c42` feat(audio): browser-captured audio relay (Go side) | Phase 8 (browser-side capture JS) and Phase 9 (Audio card templ) | Browser code that uses WebSocket + AudioWorklet is **not reusable** — `MediaRecorder` + `fetch` replaces it. But the Audio card layout (templ) and the device picker UI are reusable. |

When in doubt, read the diff against `f7ce83d` and copy the parts that are language-agnostic (templ, CSS, JSON locales) rather than the parts that encode the old architecture.

---

## 11. Definition of done

- All 11 phases committed.
- `just test` passes from repo root.
- `just lint` clean.
- End-to-end smoke test (phase 11) confirmed manually against a local Ollama.
- `docs/CHALLENGES.md`, `docs/ARCHITECTURE.md`, `docs/GUI.md`, `docs/CONFIG.md`, `CLAUDE.md` are consistent with the implemented behaviour. (Most are already updated in `7859fd3`; `CONFIG.md` may need a small tweak for the new keys.)
- `archive/python-challenger` is deleted (or kept indefinitely as historical reference — decide before final merge).
- This file (`CHALLENGER_PLAN.md`) is deleted in the same PR that closes out phase 11.
