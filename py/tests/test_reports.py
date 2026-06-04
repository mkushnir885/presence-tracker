from __future__ import annotations

import csv
import io

import polars as pl
import pytest

import ptrack_analytics.frames as _af
from ptrack_analytics.schema import EVENT_SCHEMA
from ptrack_py.reports import generate_csv


def _events(rows: list[dict]) -> pl.LazyFrame:
    return pl.DataFrame(rows, schema=pl.Schema(EVENT_SCHEMA)).lazy()


def _ev(
    meeting_id: str,
    from_start_ms: int,
    event_type: str,
    display_name: str | None = None,
    challenge_id: str | None = None,
    question_id: str | None = None,
    metadata: str | None = None,
) -> dict:
    return {
        "meeting_id": meeting_id,
        "from_start_ms": from_start_ms,
        "event_type": event_type,
        "display_name": display_name,
        "challenge_id": challenge_id,
        "question_id": question_id,
        "metadata": metadata,
    }


def _session(meeting_id: str, start_ms: int, duration_ms: int) -> list[dict]:
    end_ms = start_ms + duration_ms
    return [
        _ev(
            meeting_id,
            0,
            "session_started",
            metadata=f'{{"platform":"bbb","timestamp_ms":"{start_ms}"}}',
        ),
        _ev(
            meeting_id,
            duration_ms,
            "session_ended",
            metadata=f'{{"cause":"manual","timestamp_ms":"{end_ms}"}}',
        ),
    ]


def parse_csv(text: str) -> list[dict[str, str]]:
    return list(csv.DictReader(io.StringIO(text)))


# Force UTC so meeting_started_at values in cross-meeting tests are deterministic.
@pytest.fixture(autouse=True)
def _utc(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(_af, "TZ", "UTC")


class TestGenerateCsvPerMeeting:
    def _events(self) -> pl.LazyFrame:
        return _events(
            _session("m1", 1_700_000_000_000, 600_000)
            + [
                _ev("m1", 0, "participant_joined", "Alice"),
                _ev(
                    "m1",
                    300_000,
                    "participant_left",
                    "Alice",
                    metadata='{"reason":"left"}',
                ),
                _ev("m1", 100_000, "challenge_issued", "Alice", "c1", "q1"),
                _ev("m1", 110_000, "challenge_answered_correct", "Alice", "c1", "q1"),
                _ev("m1", 0, "participant_joined", "Bob"),
                _ev(
                    "m1",
                    600_000,
                    "participant_left",
                    "Bob",
                    metadata='{"reason":"left"}',
                ),
            ]
        )

    def test_columns(self) -> None:
        rows = parse_csv(generate_csv(self._events()))
        assert set(rows[0].keys()) == {
            "name",
            "presence_ratio",
            "challenges_correct",
            "challenges_issued",
        }

    def test_presence_ratio_capped_at_one(self) -> None:
        rows = {r["name"]: r for r in parse_csv(generate_csv(self._events()))}
        assert float(rows["Bob"]["presence_ratio"]) == pytest.approx(1.0, abs=1e-4)

    def test_presence_ratio_partial(self) -> None:
        rows = {r["name"]: r for r in parse_csv(generate_csv(self._events()))}
        # Alice present 300s out of 600s → 0.5
        assert float(rows["Alice"]["presence_ratio"]) == pytest.approx(0.5, abs=1e-4)

    def test_challenge_counts(self) -> None:
        rows = {r["name"]: r for r in parse_csv(generate_csv(self._events()))}
        assert int(rows["Alice"]["challenges_issued"]) == 1
        assert int(rows["Alice"]["challenges_correct"]) == 1
        assert int(rows["Bob"]["challenges_issued"]) == 0

    def test_no_challenges_zero_counts(self) -> None:
        rows = {r["name"]: r for r in parse_csv(generate_csv(self._events()))}
        assert rows["Bob"]["challenges_issued"] == "0"
        assert rows["Bob"]["challenges_correct"] == "0"

    def test_sorted_by_name_case_insensitive(self) -> None:
        extra_events = _events(
            _session("m1", 1_700_000_000_000, 600_000)
            + [
                _ev("m1", 0, "participant_joined", "charlie"),
                _ev("m1", 0, "participant_joined", "Alice"),
                _ev("m1", 0, "participant_joined", "bob"),
            ]
        )
        names = [r["name"] for r in parse_csv(generate_csv(extra_events))]
        assert names == sorted(names, key=str.lower)

    def test_presence_ratio_rounded_to_4dp(self) -> None:
        rows = parse_csv(generate_csv(self._events()))
        for r in rows:
            ratio_str = r["presence_ratio"]
            if "." in ratio_str:
                decimal_places = len(ratio_str.split(".")[1])
                assert decimal_places <= 4


class TestGenerateCsvCrossMeeting:
    def _events(self) -> pl.LazyFrame:
        return _events(
            _session("m1", 1_700_000_000_000, 600_000)
            + _session("m2", 1_700_100_000_000, 300_000)
            + [
                _ev("m1", 0, "participant_joined", "Alice"),
                _ev(
                    "m1",
                    300_000,
                    "participant_left",
                    "Alice",
                    metadata='{"reason":"left"}',
                ),
                _ev("m2", 0, "participant_joined", "Alice"),
                _ev(
                    "m2",
                    300_000,
                    "participant_left",
                    "Alice",
                    metadata='{"reason":"left"}',
                ),
            ]
        )

    def test_columns_include_meeting_started_at(self) -> None:
        rows = parse_csv(generate_csv(self._events(), cross_meeting=True))
        assert "meeting_started_at" in rows[0]

    def test_one_row_per_participant_per_meeting(self) -> None:
        rows = parse_csv(generate_csv(self._events(), cross_meeting=True))
        assert len(rows) == 2  # Alice in m1 and Alice in m2

    def test_presence_ratio_per_meeting(self) -> None:
        rows = parse_csv(generate_csv(self._events(), cross_meeting=True))
        for r in rows:
            # Alice present 300s out of 600s (m1) and 300s out of 300s (m2)
            ratio = float(r["presence_ratio"])
            assert ratio in (pytest.approx(0.5, abs=1e-4), pytest.approx(1.0, abs=1e-4))

    def test_skipped_challenges_not_counted(self) -> None:
        evs = _events(
            _session("m1", 1_700_000_000_000, 600_000)
            + [
                _ev("m1", 0, "participant_joined", "Alice"),
                _ev("m1", 100_000, "challenge_issued", "Alice", "c1", "q1"),
                _ev("m1", 110_000, "challenge_answered_correct", "Alice", "c1", "q1"),
                _ev(
                    "m1",
                    200_000,
                    "challenge_skipped",
                    "Alice",
                    "c2",
                    "q2",
                    metadata='{"reason":"not_registered"}',
                ),
            ]
        )
        rows = {r["name"]: r for r in parse_csv(generate_csv(evs))}
        assert int(rows["Alice"]["challenges_issued"]) == 1
