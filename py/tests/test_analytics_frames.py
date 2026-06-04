from __future__ import annotations

from typing import cast

import polars as pl

from ptrack_analytics.frames import challenge_results, meeting_times, presence_bands
from ptrack_analytics.schema import EVENT_SCHEMA


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


# ---- meeting_times ----


class TestMeetingTimes:
    def test_returns_one_row_per_meeting(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000)
        times = cast(pl.DataFrame, meeting_times(_events(rows)).collect())
        assert times.height == 1
        assert times["meeting_id"].to_list() == ["m1"]

    def test_duration_from_session_ended_offset(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 300_000) + [
            _ev("m1", 1_000, "participant_joined", "Alice")
        ]
        times = cast(pl.DataFrame, meeting_times(_events(rows)).collect())
        assert times["duration_ms"].to_list() == [300_000]

    def test_duration_fallback_to_max_event_when_no_session_ended(self) -> None:
        rows = [
            _ev(
                "m1",
                0,
                "session_started",
                metadata='{"platform":"bbb","timestamp_ms":"1000000000000"}',
            ),
            _ev("m1", 100_000, "participant_joined", "Alice"),
            _ev("m1", 250_000, "participant_left", "Alice"),
        ]
        times = cast(pl.DataFrame, meeting_times(_events(rows)).collect())
        # No session_ended; largest non-start event offset is 250_000.
        assert times["duration_ms"].to_list() == [250_000]

    def test_duration_seconds_floors_at_one(self) -> None:
        # Zero-duration meeting (only session_started) must not divide by zero.
        rows = [
            _ev(
                "m1",
                0,
                "session_started",
                metadata='{"platform":"bbb","timestamp_ms":"1000000000000"}',
            ),
        ]
        times = cast(pl.DataFrame, meeting_times(_events(rows)).collect())
        assert times["duration_seconds"].to_list()[0] >= 1.0

    def test_started_at_is_utc_datetime(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 60_000)
        times = cast(pl.DataFrame, meeting_times(_events(rows)).collect())
        # Polars Datetime column dtype carries the timezone.
        assert str(cast(pl.Datetime, times["started_at"].dtype).time_zone) == "UTC"

    def test_two_meetings(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + _session(
            "m2", 1_700_100_000_000, 300_000
        )
        times = cast(
            pl.DataFrame, meeting_times(_events(rows)).sort("meeting_id").collect()
        )
        assert times["meeting_id"].to_list() == ["m1", "m2"]
        assert times["duration_ms"].to_list() == [600_000, 300_000]


# ---- presence_bands ----


class TestPresenceBands:
    def _simple_meeting(self) -> pl.LazyFrame:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev(
                "m1", 200_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
        ]
        return _events(rows)

    def test_pairs_joined_and_left(self) -> None:
        bands = cast(pl.DataFrame, presence_bands(self._simple_meeting()).collect())
        assert bands.height == 1
        assert bands["joined_ms"].to_list() == [0]
        assert bands["left_ms"].to_list() == [200_000]
        assert bands["end_ms"].to_list() == [200_000]

    def test_open_band_when_no_leave(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice")
        ]
        bands = cast(pl.DataFrame, presence_bands(_events(rows)).collect())
        assert bands["left_ms"].to_list() == [None]
        assert bands["present_till_end"].to_list() == [True]
        assert bands["end_ms"].to_list() == [600_000]

    def test_rejoin_creates_two_bands(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev(
                "m1", 200_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
            _ev("m1", 300_000, "participant_joined", "Alice"),
            _ev(
                "m1", 500_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
        ]
        bands = cast(
            pl.DataFrame,
            presence_bands(_events(rows)).sort("joined_ms").collect(),
        )
        assert bands.height == 2
        assert bands["joined_ms"].to_list() == [0, 300_000]
        assert bands["left_ms"].to_list() == [200_000, 500_000]

    def test_end_ms_clipped_to_duration_for_overflow(self) -> None:
        # left_ms > duration_ms → clipped and flagged as present_till_end.
        rows = _session("m1", 1_700_000_000_000, 500_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev(
                "m1", 600_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
        ]
        bands = cast(pl.DataFrame, presence_bands(_events(rows)).collect())
        assert bands["present_till_end"].to_list() == [True]
        assert bands["end_ms"].to_list() == [500_000]

    def test_multiple_participants_independent(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 0, "participant_joined", "Alice"),
            _ev(
                "m1", 300_000, "participant_left", "Alice", metadata='{"reason":"left"}'
            ),
            _ev("m1", 100_000, "participant_joined", "Bob"),
            _ev("m1", 400_000, "participant_left", "Bob", metadata='{"reason":"left"}'),
        ]
        bands = cast(
            pl.DataFrame, presence_bands(_events(rows)).sort("display_name").collect()
        )
        assert bands.height == 2
        assert bands["display_name"].to_list() == ["Alice", "Bob"]
        assert bands["joined_ms"].to_list() == [0, 100_000]


# ---- challenge_results ----


class TestChallengeResults:
    def _meeting_with_challenges(self) -> pl.LazyFrame:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev("m1", 100_000, "challenge_issued", "Alice", "c1", "q1"),
            _ev(
                "m1",
                110_000,
                "challenge_answered_correct",
                "Alice",
                "c1",
                "q1",
                metadata='{"latency_ms":"10000","submitted_answer":"42"}',
            ),
            _ev("m1", 200_000, "challenge_issued", "Bob", "c2", "q2"),
            _ev(
                "m1",
                215_000,
                "challenge_answered_incorrect",
                "Bob",
                "c2",
                "q2",
                metadata='{"latency_ms":"15000","submitted_answer":"wrong"}',
            ),
            _ev("m1", 300_000, "challenge_issued", "Carol", "c3", "q3"),
            _ev("m1", 330_000, "challenge_unanswered", "Carol", "c3", "q3"),
        ]
        return _events(rows)

    def test_one_row_per_issued(self) -> None:
        results = cast(
            pl.DataFrame, challenge_results(self._meeting_with_challenges()).collect()
        )
        assert results.height == 3

    def test_states_mapped_correctly(self) -> None:
        results = cast(
            pl.DataFrame,
            challenge_results(self._meeting_with_challenges())
            .sort("challenge_id")
            .collect(),
        )
        assert results["state"].to_list() == ["correct", "incorrect", "unanswered"]

    def test_latency_set_only_when_answered(self) -> None:
        results = cast(
            pl.DataFrame,
            challenge_results(self._meeting_with_challenges())
            .sort("challenge_id")
            .collect(),
        )
        latencies = results["latency_ms"].to_list()
        assert latencies[0] == 10_000  # correct
        assert latencies[1] == 15_000  # incorrect
        assert latencies[2] is None  # unanswered

    def test_submitted_answer_set_only_when_answered(self) -> None:
        results = cast(
            pl.DataFrame,
            challenge_results(self._meeting_with_challenges())
            .sort("challenge_id")
            .collect(),
        )
        answers = results["submitted_answer"].to_list()
        assert answers[0] == "42"
        assert answers[1] == "wrong"
        assert answers[2] is None

    def test_skipped_challenge_included(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev(
                "m1",
                100_000,
                "challenge_skipped",
                "Alice",
                "c1",
                "q1",
                metadata='{"reason":"not_registered"}',
            ),
        ]
        results = cast(pl.DataFrame, challenge_results(_events(rows)).collect())
        assert results.height == 1
        assert results["state"].to_list() == ["skipped"]
        assert results["skip_reason"].to_list() == ["not_registered"]

    def test_auto_submitted_flag(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000) + [
            _ev(
                "m1",
                100_000,
                "challenge_issued",
                "Alice",
                "c1",
                "q1",
                metadata='{"auto_submitted":"true"}',
            ),
            _ev("m1", 115_000, "challenge_unanswered", "Alice", "c1", "q1"),
        ]
        results = cast(pl.DataFrame, challenge_results(_events(rows)).collect())
        assert results["auto_submitted"].to_list() == [True]

    def test_no_challenges_returns_empty(self) -> None:
        rows = _session("m1", 1_700_000_000_000, 600_000)
        results = cast(pl.DataFrame, challenge_results(_events(rows)).collect())
        assert results.height == 0
