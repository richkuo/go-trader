"""Standard ATR injection for check scripts.

Provides a consistent ATR indicator for position entry stamping when the
open strategy doesn't emit its own `atr` column (e.g. tema_cross, ema_crossover).
The math lives in the shared open-tree module
``shared_strategies/open/indicators_core.py`` (#1281) — the same true-range
smoothing every strategy site uses — loaded here by file path (the
``close_registry_loader`` pattern) so this module stays the check-script entry
point without ambiguous bare imports.

#1277: every entry point threads ``method`` to the shared choke point —
``"simple"`` (default; frozen legacy rolling mean with the ``>= 100``
integer-rounding convention) or ``"wilder"`` (published Wilder RMA, never
rounded). The Go scheduler resolves the per-strategy ``atr_method`` config
and forwards it via ``--atr-method``; the default keeps every existing
surface byte-identical.
"""

from __future__ import annotations

import importlib.util
import os

import pandas as pd

_INDICATORS_CORE_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "..", "shared_strategies", "open", "indicators_core.py",
)


def _load_indicators_core():
    spec = importlib.util.spec_from_file_location(
        "_go_trader_indicators_core", _INDICATORS_CORE_PATH
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


_core = _load_indicators_core()


def normalize_atr_method(method: str | None) -> str:
    """Normalize/validate an ATR method name — see indicators_core (#1277)."""
    return _core.normalize_atr_method(method)


def standard_atr(df: pd.DataFrame, period: int = 14, method: str = "simple") -> pd.Series:
    """Compute ATR over `period` bars of True Range.

    `method="simple"` (default) is the frozen legacy rolling mean with the
    `>= 100` integer-rounding convention; `method="wilder"` is the published
    Wilder RMA (#1277, never rounded). Requires `high`, `low`, `close`
    columns. Returns a Series aligned to df.index. Rows with insufficient
    history return NaN.
    """
    return _core.atr_sma(df, period, method=method)


def ensure_atr_indicator(df: pd.DataFrame, period: int = 14, method: str = "simple") -> pd.DataFrame:
    """Ensure `df` has an `atr` column, injecting standard_atr if absent.

    No-op when `atr` is already present (preserves strategy-defined ATR —
    strategy-emitted columns are NOT re-based by the #1277 `atr_method`
    cutover; see the cutover roster in docs/research/1277-wilder-atr-cutover.md).
    Returns `df` with the column added in-place (the same object).
    """
    if "atr" not in df.columns:
        df["atr"] = standard_atr(df, period, method=method)
    return df


def latest_atr(df: pd.DataFrame, period: int = 14, method: str = "simple") -> float:
    """Return the most recent finite, positive ATR value, or 0.0 if none.

    Used by check scripts to populate `market_ctx["atr"]` so live close
    evaluators (e.g. tiered_tp_atr_live) see current volatility instead of
    falling back to the entry-time ATR snapshot.
    """
    series = standard_atr(df, period, method=method)
    if series.empty:
        return 0.0
    value = series.iloc[-1]
    try:
        value = float(value)
    except (TypeError, ValueError):
        return 0.0
    if not (value > 0):  # rejects NaN, 0, negative (NaN > 0 is False)
        return 0.0
    return value
