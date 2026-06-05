"""ptrack_analytics — meeting analysis for Jupyter.

Quick start::

    import ptrack_analytics as pt
    import polars as pl

    pt.load("~/Documents/ptrack/meetings/spring-2026-*")

    # Every frame is a polars LazyFrame — call .collect() when you want
    # a concrete DataFrame back.
    pt.presence.filter(pl.col("ratio") > 0.8).collect()

After calling :func:`load`, the four module-level frames are populated:

* ``meetings`` — one row per meeting.
* ``presence`` — one row per (participant, meeting), with per-join
  bands packed into a list.
* ``challenges`` — one row per issued or skipped challenge.
* ``questions`` — one row per unique question id (join key for
  ``challenges``).

Use ``.schema`` on any frame to see its columns.

If you prefer individual names, import them *after* calling :func:`load`::

    from ptrack_analytics import load
    load("~/Documents/ptrack/meetings/spring-2026-*")
    from ptrack_analytics import meetings, presence
    meetings.sort("started_at").collect()
"""

from __future__ import annotations

import sys
import types

import polars as pl

from . import frames
from .analysis import (
    challenge_accuracy,
    plot_concurrent_participants,
    plot_presence_heatmap,
)


class _Module(types.ModuleType):
    """Module subclass so ``ptrack_analytics.TZ = "..."`` writes through
    to :data:`frames.TZ`, the override consulted before ``tzlocal``
    autodetection (and ultimately ``"UTC"``).
    """

    @property
    def TZ(self) -> str | None:
        return frames.TZ

    @TZ.setter
    def TZ(self, value: str | None) -> None:
        frames.TZ = value


sys.modules[__name__].__class__ = _Module


__all__ = [
    "TZ",
    "load",
    "meetings",
    "presence",
    "challenges",
    "questions",
    "plot_concurrent_participants",
    "challenge_accuracy",
    "plot_presence_heatmap",
]

meetings: pl.LazyFrame
"""One row per meeting. Available after :func:`load` is called.

Columns:

* ``meeting_id`` (Utf8)
* ``platform`` (Utf8) — ``bbb`` / ``meet`` / ``zoom`` / ``mock``
* ``started_at`` / ``ended_at`` (Datetime("ms"), system local tz)
* ``duration`` (Duration("ms"))
* ``start_cause`` / ``end_cause`` (Utf8) — why the session began and ended

Example::

    meetings.sort("started_at").collect()
"""

presence: pl.LazyFrame
"""One row per (participant, meeting). Available after :func:`load` is called.

Columns:

* ``display_name`` / ``meeting_id`` (Utf8)
* ``total_duration`` (Duration("ms")) — sum of band durations
* ``ratio`` (Float64) — ``total_duration / meeting duration``, clipped
  to ``[0, 1]``
* ``present_till_end`` (Bool) — true when any band stayed open at
  session end
* ``bands`` (List[Struct]) — per-join bands ordered by ``joined_at``,
  each carrying ``joined_at``, ``left_at``, ``duration``,
  ``join_method``, ``leave_reason``. Open bands are clipped at the
  meeting end.

Use ``presence.explode("bands").unnest("bands")`` to flatten back to
one row per band.

Example::

    presence.filter(pl.col("ratio") > 0.8).collect()
"""

challenges: pl.LazyFrame
"""One row per issued or skipped challenge. Available after :func:`load`
is called.

Columns:

* ``display_name`` / ``meeting_id`` / ``challenge_id`` / ``question_id`` (Utf8)
* ``fired_at`` (Datetime("ms"), system local tz) — when the challenge
  was issued or skipped
* ``answered_at`` (Datetime("ms"), system local tz, nullable) — set
  only when ``state`` is ``correct`` or ``incorrect``
* ``latency`` (Duration("ms"), nullable) — same nullability as
  ``answered_at``
* ``state`` (Enum{correct, incorrect, unanswered, skipped})
* ``submitted_answer`` (Utf8, nullable) — same nullability as
  ``answered_at``
* ``skip_reason`` (Utf8, nullable) — set only when
  ``state == "skipped"``
* ``auto_submitted`` (Bool) — poll dispatched without teacher review

Question text is not joined in; it lives once in :data:`questions` and
the join key is ``question_id``.

Example::

    # Hardest questions across the loaded set
    (
        challenges
        .join(questions, on="question_id")
        .group_by("question_id")
        .agg((pl.col("state") == "correct").mean().alias("accuracy"))
        .sort("accuracy")
        .collect()
    )
"""

questions: pl.LazyFrame
"""One row per unique ``question_id`` seen across the loaded meetings.
Available after :func:`load` is called.

Columns:

* ``question_id`` (Utf8) — join key from :data:`challenges`
* ``question`` (Struct) — full record packed into one column:
  ``auto_submitted``, ``question_type``, ``prompt``, ``choices``,
  ``correct_answer``, ``match_mode``, ``tolerance``.
  ``correct_answer`` is JSON-encoded (``["foo"]`` for choice/text,
  ``5040`` for numeric) — decode with ``json.loads`` when needed.

Example::

    challenges.join(questions, on="question_id").select(
        "display_name", "state", pl.col("question").struct.field("prompt"),
    ).collect()
"""


def load(*patterns: str) -> None:
    """Load meetings matching *patterns* (paths or globs) and populate
    :data:`meetings`, :data:`presence`, :data:`challenges`,
    :data:`questions` at module level.

    Each matched directory must contain ``events.parquet``; a sibling
    ``questions.jsonl`` is read when present. In-progress meetings (no
    ``session_ended`` event) are rejected — every frame is guaranteed
    closed bands and a concrete end time.

    Example::

        load("meetings/2026-04-21")           # one meeting
        load("meetings/spring-2026-*")        # a semester
        load("meetings/a", "meetings/b/*")    # mix paths and globs
    """
    global meetings, presence, challenges, questions

    from .load import load_events, load_questions, resolve_meetings
    from .views import challenges_view, meetings_view, presence_view

    dirs = resolve_meetings(*patterns)
    events = load_events(dirs)
    meetings = meetings_view(events)
    presence = presence_view(events)
    challenges = challenges_view(events)
    questions = load_questions(dirs)
