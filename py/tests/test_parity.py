"""Lock the CSV report and GUI stats onto one presence/meeting derivation.

reports.py and stats.py both build on ptrack_analytics.frames.presence_bands /
meeting_times. These tests fail if the two surfaces ever diverge again (the
class of bug fixed in commit f764fc3) or if the shared pairing changes shape.
"""

from __future__ import annotations

import csv
import io

import polars as pl

from ptrack_analytics.frames import meeting_times, presence_bands
from ptrack_analytics.schema import EVENT_SCHEMA
from ptrack_py.reports import generate_aggregate_csv
from ptrack_py.stats import generate_stats


def _events(rows: list[dict[str, object]]) -> pl.LazyFrame:
    return pl.DataFrame(rows, schema=pl.Schema(EVENT_SCHEMA)).lazy()


def _ev(
    meeting_id: str,
    timestamp: int,
    event_type: str,
    display_name: str | None = None,
    challenge_id: str | None = None,
    question_id: str | None = None,
    metadata: str | None = None,
) -> dict[str, object]:
    return {
        "meeting_id": meeting_id,
        "timestamp": timestamp,
        "event_type": event_type,
        "display_name": display_name,
        "challenge_id": challenge_id,
        "question_id": question_id,
        "metadata": metadata,
    }


def _sample() -> pl.LazyFrame:
    # Two meetings; a rejoin (Alice in m1), an open band (Carol in m2 never
    # leaves), and every challenge state so both surfaces see real data.
    left = '{"reason":"left"}'
    rows = [
        _ev("m1", 1_700_000_000_000, "session_started", metadata='{"platform":"bbb"}'),
        _ev("m1", 0, "participant_joined", "Alice"),
        _ev("m1", 1_000, "participant_joined", "Bob"),
        _ev("m1", 100_000, "challenge_issued", "Alice", "c1", "q1"),
        _ev("m1", 102_000, "challenge_answered_correct", "Alice", "c1", "q1"),
        _ev("m1", 200_000, "participant_left", "Alice", metadata=left),
        _ev("m1", 250_000, "participant_joined", "Alice"),
        _ev("m1", 300_000, "challenge_issued", "Bob", "c2", "q2"),
        _ev("m1", 305_000, "challenge_answered_incorrect", "Bob", "c2", "q2"),
        _ev("m1", 600_000, "participant_left", "Alice", metadata=left),
        _ev("m1", 600_000, "participant_left", "Bob", metadata=left),
        _ev("m1", 600_000, "session_ended", metadata='{"cause":"manual"}'),
        _ev("m2", 1_700_100_000_000, "session_started", metadata='{"platform":"meet"}'),
        _ev("m2", 0, "participant_joined", "Carol"),
        _ev("m2", 10_000, "challenge_issued", "Carol", "c3", "q3"),
        _ev("m2", 15_000, "challenge_unanswered", "Carol", "c3", "q3"),
        _ev("m2", 300_000, "session_ended", metadata='{"cause":"manual"}'),
    ]
    return _events(rows)


def test_report_and_stats_presence_ratios_agree() -> None:
    events = _sample()

    reader = csv.DictReader(io.StringIO(generate_aggregate_csv(events)))
    report_ratio = {
        (r["name"], r["meeting"]): float(r["presence_ratio"]) for r in reader
    }

    payload = generate_stats(events, mode="cross_meeting")
    started = {m["meeting_id"]: m["started_at"] for m in payload["meetings"]}
    stats_ratio = {
        (p["display_name"], started[row["meeting_id"]]): row["presence_ratio"]
        for p in payload["participants"]
        for row in p["rows"]
        if not row["absent"]
    }

    assert report_ratio.keys() == stats_ratio.keys()
    for key, ratio in report_ratio.items():
        # The CSV rounds to 4 dp; the stats JSON does not. Same source, so they
        # must match once rounded.
        assert round(stats_ratio[key], 4) == ratio, key


def test_presence_bands_pairs_rejoin_as_two_bands() -> None:
    bands = (
        presence_bands(_sample())
        .filter((pl.col("display_name") == "Alice") & (pl.col("meeting_id") == "m1"))
        .sort("joined_ms")
        .collect()
    )
    assert bands["joined_ms"].to_list() == [0, 250_000]
    assert bands["left_ms"].to_list() == [200_000, 600_000]


def test_presence_bands_leaves_open_band_null() -> None:
    bands = (
        presence_bands(_sample()).filter(pl.col("display_name") == "Carol").collect()
    )
    assert bands.height == 1
    assert bands["left_ms"].to_list() == [None]


def test_meeting_times_prefers_session_ended_offset() -> None:
    times = meeting_times(_sample()).sort("meeting_id").collect()
    durations = dict(zip(times["meeting_id"], times["duration_ms"], strict=True))
    # session_ended offsets, not the absolute session_started timestamp.
    assert durations == {"m1": 600_000, "m2": 300_000}
