from __future__ import annotations

from typing import cast

import polars as pl
import pytest

from ptrack_analytics.schema import EVENT_SCHEMA
from ptrack_py.frames import challenge_stats, concurrent_participants, presence_totals


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


# ---- presence_totals ----


class TestPresenceTotals:
    def test_single_band(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev(
                "m1", 200_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
        ]
        totals = cast(pl.DataFrame, presence_totals(_events(rows)).collect())
        row = totals.filter(pl.col("display_name") == "Alice")
        assert row["presence_seconds"].to_list()[0] == pytest.approx(200.0)

    def test_two_bands_summed(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev(
                "m1", 100_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
            _ev("m1", 200_000, "participant_joined", "Alice"),
            _ev(
                "m1", 350_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
        ]
        totals = cast(pl.DataFrame, presence_totals(_events(rows)).collect())
        row = totals.filter(pl.col("display_name") == "Alice")
        # 100s + 150s = 250s
        assert row["presence_seconds"].to_list()[0] == pytest.approx(250.0)

    def test_open_band_clips_to_duration(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 300_000) + [
            _ev("m1", 0, "participant_joined", "Alice")
        ]
        totals = cast(pl.DataFrame, presence_totals(_events(rows)).collect())
        row = totals.filter(pl.col("display_name") == "Alice")
        assert row["presence_seconds"].to_list()[0] == pytest.approx(300.0)

    def test_multiple_participants_independent(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev(
                "m1", 60_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
            _ev("m1", 0, "participant_joined", "Bob"),
            _ev("m1", 120_000, "participant_left", "Bob", metadata='{"reason":"left"}'),
        ]
        totals = cast(
            pl.DataFrame, presence_totals(_events(rows)).sort("display_name").collect()
        )
        seconds = dict(zip(totals["display_name"], totals["presence_seconds"]))
        assert seconds["Alice"] == pytest.approx(60.0)
        assert seconds["Bob"] == pytest.approx(120.0)


# ---- challenge_stats ----


class TestChallengeStats:
    def _meeting_with_results(self) -> pl.LazyFrame:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 100_000, "challenge_issued", "Alice", "c1", "q1"),
            _ev("m1", 110_000, "challenge_answered_correct", "Alice", "c1", "q1"),
            _ev("m1", 200_000, "challenge_issued", "Alice", "c2", "q2"),
            _ev("m1", 215_000, "challenge_answered_incorrect", "Alice", "c2", "q2"),
            _ev("m1", 300_000, "challenge_issued", "Bob", "c3", "q3"),
            _ev("m1", 330_000, "challenge_unanswered", "Bob", "c3", "q3"),
        ]
        return _events(rows)

    def test_issued_correct_incorrect_unanswered(self) -> None:
        stats = cast(
            pl.DataFrame,
            challenge_stats(
                self._meeting_with_results(), by=["display_name", "meeting_id"]
            )
            .sort("display_name")
            .collect(),
        )
        alice = stats.filter(pl.col("display_name") == "Alice")
        assert alice["challenges_issued"].to_list()[0] == 2
        assert alice["challenges_correct"].to_list()[0] == 1
        assert alice["challenges_incorrect"].to_list()[0] == 1
        assert alice["challenges_unanswered"].to_list()[0] == 0

        bob = stats.filter(pl.col("display_name") == "Bob")
        assert bob["challenges_issued"].to_list()[0] == 1
        assert bob["challenges_unanswered"].to_list()[0] == 1

    def test_skipped_excluded(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
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
        stats = cast(
            pl.DataFrame, challenge_stats(_events(rows), by=["display_name"]).collect()
        )
        assert stats["challenges_issued"].to_list()[0] == 1

    def test_group_by_display_name_only(self) -> None:
        rows = (
            _session("m1", 1_700_000_000_000, 600_000)
            + _session("m2", 1_700_100_000_000, 300_000)
            + [
                _ev("m1", 100_000, "challenge_issued", "Alice", "c1", "q1"),
                _ev("m1", 110_000, "challenge_answered_correct", "Alice", "c1", "q1"),
                _ev("m2", 50_000, "challenge_issued", "Alice", "c2", "q2"),
                _ev("m2", 65_000, "challenge_answered_incorrect", "Alice", "c2", "q2"),
            ]
        )
        stats = cast(
            pl.DataFrame, challenge_stats(_events(rows), by=["display_name"]).collect()
        )
        assert stats.height == 1
        assert stats["challenges_issued"].to_list()[0] == 2
        assert stats["challenges_correct"].to_list()[0] == 1


# ---- concurrent_participants ----


class TestConcurrentParticipants:
    def test_single_participant(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev("m1", 300_000, "participant_left", "Alice"),
        ]
        result = cast(pl.DataFrame, concurrent_participants(_events(rows)).collect())
        assert result["max_participants"].to_list() == [1]

    def test_peak_when_all_present(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev("m1", 0, "participant_joined", "Bob"),
            _ev("m1", 0, "participant_joined", "Carol"),
            _ev("m1", 300_000, "participant_left", "Alice"),
            _ev("m1", 400_000, "participant_left", "Bob"),
            _ev("m1", 500_000, "participant_left", "Carol"),
        ]
        result = cast(pl.DataFrame, concurrent_participants(_events(rows)).collect())
        assert result["max_participants"].to_list() == [3]

    def test_simultaneous_join_before_leave(self) -> None:
        # At t=100: Carol joins (+1), Alice leaves (-1).
        # Sort puts +1 before -1 at same instant, so peak is 3 (not 2).
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev("m1", 0, "participant_joined", "Bob"),
            _ev("m1", 100_000, "participant_joined", "Carol"),
            _ev("m1", 100_000, "participant_left", "Alice"),
        ]
        result = cast(pl.DataFrame, concurrent_participants(_events(rows)).collect())
        assert result["max_participants"].to_list() == [3]

    def test_independent_meetings(self) -> None:
        rows = (
            _session("m1", 1_700_000_000_000, 600_000)
            + _session("m2", 1_700_100_000_000, 300_000)
            + [
                _ev("m1", 0, "participant_joined", "Alice"),
                _ev("m1", 0, "participant_joined", "Bob"),
                _ev("m2", 0, "participant_joined", "Carol"),
            ]
        )
        result = cast(
            pl.DataFrame,
            concurrent_participants(_events(rows)).sort("meeting_id").collect(),
        )
        maxes = dict(zip(result["meeting_id"], result["max_participants"]))
        assert maxes["m1"] == 2
        assert maxes["m2"] == 1

    def test_no_participants_returns_empty(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000)
        result = cast(pl.DataFrame, concurrent_participants(_events(rows)).collect())
        assert result.height == 0
