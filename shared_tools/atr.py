"""Standard ATR injection for check scripts.

Provides a consistent ATR indicator for position entry stamping when the
open strategy doesn't emit its own `atr` column (e.g. tema_cross, ema_crossover).
Uses a simple rolling mean of True Range — the same method used by strategies
that do emit ATR (see shared_strategies/open/registry.py: breakout_strategy,
atr_breakout_strategy) — so stamped values are consistent across strategies.
"""

from __future__ import annotations

import pandas as pd


def standard_atr(df: pd.DataFrame, period: int = 14) -> pd.Series:
    """Compute ATR via simple rolling mean of True Range over `period` bars.

    Requires `high`, `low`, `close` columns. Returns a Series aligned to df.index.
    Rows with insufficient history return NaN.
    """
    high = df["high"].astype(float)
    low = df["low"].astype(float)
    prev_close = df["close"].astype(float).shift(1)
    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)
    return tr.rolling(window=period).mean()


def ensure_atr_indicator(df: pd.DataFrame, period: int = 14) -> pd.DataFrame:
    """Ensure `df` has an `atr` column, injecting standard_atr if absent.

    No-op when `atr` is already present (preserves strategy-defined ATR).
    Returns `df` with the column added in-place (the same object).
    """
    if "atr" not in df.columns:
        df["atr"] = standard_atr(df, period)
    return df


def latest_atr(df: pd.DataFrame, period: int = 14) -> float:
    """Return the most recent finite, positive ATR value, or 0.0 if none.

    Used by check scripts to populate `market_ctx["atr"]` so live close
    evaluators (e.g. tiered_tp_atr_live) see current volatility instead of
    falling back to the entry-time ATR snapshot.
    """
    series = standard_atr(df, period)
    if series.empty:
        return 0.0
    value = series.iloc[-1]
    try:
        value = float(value)
    except (TypeError, ValueError):
        return 0.0
    if not (value > 0) or value != value:  # rejects NaN, 0, negative
        return 0.0
    return value
