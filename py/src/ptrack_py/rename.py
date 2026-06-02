from __future__ import annotations

from pathlib import Path

import polars as pl

from ptrack_analytics.schema import EVENT_SCHEMA


def rename_display_name(parquet_path: Path, old: str, new: str) -> None:
    if old == new:
        return

    tmp = parquet_path.with_suffix(parquet_path.suffix + ".tmp")
    (
        pl.scan_parquet(str(parquet_path), schema=pl.Schema(EVENT_SCHEMA))
        .with_columns(
            pl.when(pl.col("display_name") == old)
            .then(pl.lit(new))
            .otherwise(pl.col("display_name"))
            .alias("display_name"),
        )
        .sink_parquet(str(tmp), compression="zstd")
    )
    tmp.replace(parquet_path)
