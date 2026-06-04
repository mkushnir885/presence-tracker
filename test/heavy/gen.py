#!/usr/bin/env python3
"""Generate test/heavy/fixture.jsonl and test/heavy/expected.json.

100 users join the meeting; 75 are registered (written to Parquet), 25 are not.

  users 001–065  registered, stay all session
  users 066–075  registered, leave after poll 2
  users 076–100  unregistered — join/leave are live-only, no Parquet events

5 polls, 2-3 questions each. All questions in a poll share the same correct
answer so fixture answer events are deterministic regardless of the random
per-participant question assignment.

BoltDB write speed on btrfs ≈ 6 ms/write → 75 registrations ≈ 460 ms real.
At speed=100 the fixture clock ticks 100× faster. We set the first join at
200 000 ms fixture (2 000 ms real) to give a comfortable 1 500 ms buffer.

Poll timing (fixture ms → real ms at speed=100):
  poll1  280 000 → 2 800 ms    answers 281 000–283 600
  poll2  430 000 → 4 300 ms    answers 431 000–433 600
  leave  450 000               users 066–100 leave
  poll3  580 000 → 5 800 ms    answers 581 000–583 400
  poll4  730 000 → 7 300 ms    answers 731 000–733 400
  poll5  880 000 → 8 800 ms    answers 881 000–883 400
  end    900 000 → 9 000 ms

Inter-poll gap: 1 500 ms real. AnswerWindowSeconds=1 in tests →  unanswered
goroutines expire 1 000 ms after the poll fires, leaving a 500 ms buffer
before the next poll's SendChallenge calls.
"""

from __future__ import annotations

import json
import pathlib

HERE = pathlib.Path(__file__).parent

N_TOTAL      = 100
N_REGISTERED = 75   # users 001–075
N_STAY       = 65   # users 001–065  (stay all session)
N_LEAVE      = 10   # users 066–075  (leave after poll 2)
# users 076–100 are unregistered

users_registered = [f"{i:03d}" for i in range(1, N_REGISTERED + 1)]   # 001–075
users_stay       = [f"{i:03d}" for i in range(1, N_STAY + 1)]          # 001–065
users_leave      = [f"{i:03d}" for i in range(N_STAY + 1, N_REGISTERED + 1)]  # 066–075
users_unreg      = [f"{i:03d}" for i in range(N_REGISTERED + 1, N_TOTAL + 1)] # 076–100
users_all        = [f"{i:03d}" for i in range(1, N_TOTAL + 1)]

def handle(u: str) -> str: return f"tg_u{u}"
def pid(u: str) -> str:    return f"w_u{u}"
def name(u: str) -> str:   return f"User {u}"

lines: list[dict] = []

def add(**kw: object) -> None:
    lines.append({k: v for k, v in kw.items() if v is not None})


# ── Registrations — users 001–075 only (100 ms apart) ────────────────────────
for i, u in enumerate(users_registered):
    add(kind="registration", offset_ms=100 + i * 100,
        handle=handle(u), display_name=name(u), language="en")

# ── Meeting start ─────────────────────────────────────────────────────────────
add(kind="meeting_started", offset_ms=80_000, extra={"platform": "mock"})

# ── All 100 users join (200 000–219 800 ms) ───────────────────────────────────
# First join at 200 000 ms = 2 000 ms real — well after all BoltDB writes done.
for i, u in enumerate(users_all):
    join = 200_000 + i * 200
    add(kind="participant_joined", offset_ms=join,
        platform_id=pid(u), display_name=name(u), extra={"role": "VIEWER"})

# Join confirmations only for registered users (join_offset + 500 ms).
for i, u in enumerate(users_registered):
    join = 200_000 + i * 200
    add(kind="join_confirmation", offset_ms=join + 500,
        handle=handle(u), confirmed=True)
# All 75 confirmations arrive by 214 800 + 500 = 215 300 ms (2 153 ms real).

# ── Poll 1 at 280 000 ms — 3 MCQ, correct answer "Paris" ─────────────────────
#   eligible: 75 (users 001–075)
#   correct: 53 (001–053), incorrect: 15 (054–068), unanswered: 7 (069–075)
POLL1_BANK = {
    "questions": [
        {"prompt": "Capital of France?", "type": "multiple_choice",
         "choices": ["Paris", "London", "Berlin", "Rome"], "answer": ["Paris"]},
        {"prompt": "Where is the Eiffel Tower?", "type": "multiple_choice",
         "choices": ["Paris", "London", "Berlin", "Rome"], "answer": ["Paris"]},
        {"prompt": "Which city hosted the 2024 Olympics?", "type": "multiple_choice",
         "choices": ["Paris", "London", "Berlin", "Rome"], "answer": ["Paris"]},
    ]
}
add(kind="poll", offset_ms=280_000, bank=POLL1_BANK)
for i, u in enumerate(users_registered[:53]):
    add(kind="answer_received", offset_ms=281_000 + i * 20, handle=handle(u), selected=["Paris"])
for i, u in enumerate(users_registered[53:68]):
    add(kind="answer_received", offset_ms=281_000 + i * 20, handle=handle(u), selected=["London"])
# users 069–075 (index 68–74) — no answer

# ── Poll 2 at 430 000 ms — 3 short_text, correct answer "water" ──────────────
#   eligible: 75 (users 001–075)
#   correct: 60 (001–060), incorrect: 8 (061–068), unanswered: 7 (069–075)
POLL2_BANK = {
    "questions": [
        {"prompt": "What molecule is H2O?", "type": "short_text", "answer": ["water"]},
        {"prompt": "What liquid covers 71% of Earth?", "type": "short_text", "answer": ["water"]},
        {"prompt": "Name the compound formed by 2H and O.", "type": "short_text", "answer": ["water"]},
    ]
}
add(kind="poll", offset_ms=430_000, bank=POLL2_BANK)
for i, u in enumerate(users_registered[:60]):
    add(kind="answer_received", offset_ms=431_000 + i * 20, handle=handle(u), answer="water")
for i, u in enumerate(users_registered[60:68]):
    add(kind="answer_received", offset_ms=431_000 + i * 20, handle=handle(u), answer="earth")
# users 069–075 (index 68–74) — no answer

# ── 35 users leave at 450 000 ms ─────────────────────────────────────────────
# Registered leavers (066–075): generate Parquet participant_left events.
# Unregistered (076–100): leave event ignored at session layer — no Parquet.
for u in users_leave + users_unreg:
    add(kind="participant_left", offset_ms=450_000,
        platform_id=pid(u), display_name=name(u), extra={"reason": "left"})

# ── Poll 3 at 580 000 ms — 2 numeric, correct answer 100 ─────────────────────
#   eligible: 65 (users 001–065)
#   correct: 48 (001–048), incorrect: 11 (049–059), unanswered: 6 (060–065)
POLL3_BANK = {
    "questions": [
        {"prompt": "Boiling point of water in Celsius?", "type": "numeric",
         "answer": 100, "tolerance": 1},
        {"prompt": "What is 10 squared?", "type": "numeric",
         "answer": 100, "tolerance": 0.5},
    ]
}
add(kind="poll", offset_ms=580_000, bank=POLL3_BANK)
for i, u in enumerate(users_stay[:48]):
    add(kind="answer_received", offset_ms=581_000 + i * 20, handle=handle(u), answer="100")
for i, u in enumerate(users_stay[48:59]):
    add(kind="answer_received", offset_ms=581_000 + i * 20, handle=handle(u), answer="0")
# users 060–065 (index 59–64) — no answer

# ── Poll 4 at 730 000 ms — 3 MCQ, correct answer "Earth" ─────────────────────
#   eligible: 65, correct: 52 (001–052), incorrect: 7 (053–059), unanswered: 6
POLL4_BANK = {
    "questions": [
        {"prompt": "Which planet do we live on?", "type": "multiple_choice",
         "choices": ["Earth", "Mars", "Venus", "Jupiter"], "answer": ["Earth"]},
        {"prompt": "Third planet from the Sun?", "type": "multiple_choice",
         "choices": ["Earth", "Mars", "Venus", "Jupiter"], "answer": ["Earth"]},
        {"prompt": "Which planet has the Moon as its satellite?", "type": "multiple_choice",
         "choices": ["Earth", "Mars", "Venus", "Jupiter"], "answer": ["Earth"]},
    ]
}
add(kind="poll", offset_ms=730_000, bank=POLL4_BANK)
for i, u in enumerate(users_stay[:52]):
    add(kind="answer_received", offset_ms=731_000 + i * 20, handle=handle(u), selected=["Earth"])
for i, u in enumerate(users_stay[52:59]):
    add(kind="answer_received", offset_ms=731_000 + i * 20, handle=handle(u), selected=["Mars"])
# users 060–065 (index 59–64) — no answer

# ── Poll 5 at 880 000 ms — 2 MCQ, correct answer "4" ─────────────────────────
#   eligible: 65, correct: 50 (001–050), incorrect: 9 (051–059), unanswered: 6
POLL5_BANK = {
    "questions": [
        {"prompt": "How many seasons are there?", "type": "multiple_choice",
         "choices": ["4", "2", "6", "8"], "answer": ["4"]},
        {"prompt": "How many legs does a cat have?", "type": "multiple_choice",
         "choices": ["4", "2", "6", "8"], "answer": ["4"]},
    ]
}
add(kind="poll", offset_ms=880_000, bank=POLL5_BANK)
for i, u in enumerate(users_stay[:50]):
    add(kind="answer_received", offset_ms=881_000 + i * 20, handle=handle(u), selected=["4"])
for i, u in enumerate(users_stay[50:59]):
    add(kind="answer_received", offset_ms=881_000 + i * 20, handle=handle(u), selected=["8"])
# users 060–065 (index 59–64) — no answer

# ── Remaining 65 registered users leave, meeting ends ────────────────────────
for u in users_stay:
    add(kind="participant_left", offset_ms=890_000,
        platform_id=pid(u), display_name=name(u), extra={"reason": "left"})
add(kind="meeting_ended", offset_ms=900_000)

lines.sort(key=lambda x: x["offset_ms"])

fixture_path = HERE / "fixture.jsonl"
with fixture_path.open("w") as f:
    for line in lines:
        f.write(json.dumps(line) + "\n")
print(f"Wrote {len(lines)} lines to {fixture_path}")

# ── Expected Parquet event counts ─────────────────────────────────────────────
# Unregistered users (076–100) generate no Parquet events at all.
expected = {
    "session_started": 1,
    "session_ended": 1,
    "participant_joined": 75,           # registered users only
    "participant_left": 75,             # 10 (066–075) + 65 (001–065)
    "challenge_issued": 345,            # 75+75+65+65+65
    "challenge_answered_correct": 263,  # 53+60+48+52+50
    "challenge_answered_incorrect": 50, # 15+8+11+7+9
    "challenge_unanswered": 32,         # 7+7+6+6+6
}
assert expected["challenge_issued"] == (
    expected["challenge_answered_correct"]
    + expected["challenge_answered_incorrect"]
    + expected["challenge_unanswered"]
), "challenge counts don't add up"

expected_path = HERE / "expected.json"
with expected_path.open("w") as f:
    json.dump(expected, f, indent=2)
    f.write("\n")
print(f"Wrote {expected_path}")
print("Expected:", json.dumps(expected, indent=2))
