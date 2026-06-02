"""Atomic display_name rewrite for one events.parquet."""

from __future__ import annotations

from pathlib import Path

import polars as pl

from ptrack_analytics.load import scan_events


def rename_display_name(parquet_path: Path, old: str, new: str) -> None:
    """Rewrite every row whose display_name == *old* to *new*, in place.

    The rewrite goes through events.parquet.tmp + os.replace so a crash
    mid-write leaves the original intact.
    """
    if old == new:
        return

    tmp = parquet_path.with_suffix(parquet_path.suffix + ".tmp")
    (
        scan_events(parquet_path)
        .with_columns(
            pl.when(pl.col("display_name") == old)
            .then(pl.lit(new))
            .otherwise(pl.col("display_name"))
            .alias("display_name"),
        )
        .sink_parquet(str(tmp), compression="zstd")
    )
    tmp.replace(parquet_path)
