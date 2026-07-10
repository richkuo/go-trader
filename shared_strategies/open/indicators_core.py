"""Shared indicator math for the open-strategy tree (#1281).

Single source of truth for the Wilder RSI and true-range/ATR blocks that were
previously copy-pasted across ``registry.py``, the strategy core modules,
``shared_tools/atr.py``, and ``backtest/consolidation_research.py``.

Import contract: this module lives in ``shared_strategies/open/`` so it is
importable by ``registry.py`` (which inserts this directory onto ``sys.path``
before importing core modules) and by every core module the registry loads —
WITHOUT depending on ``shared_tools`` being importable at module-load time
(the registry parity test loads ``registry.py`` via ``importlib`` with a bare
``sys.path``). Consumers outside this tree (``shared_tools/atr.py``,
``backtest/consolidation_research.py``) load it by file path via
``importlib.util.spec_from_file_location`` — mirror that pattern rather than
a bare ``import indicators_core`` from an ambiguous root.

Default numerics are frozen: at ``method="simple"`` (the default) these
functions reproduce the replaced inline blocks byte-for-byte, including the
``>= 100`` integer-rounding convention split (``round_large``) and per-site
``min_periods`` overrides. #1277 adds ``method="wilder"`` — the published
Wilder RMA ATR (``ewm(alpha=1/period, adjust=False)``) — as an explicit
opt-in behind the config-gated ``atr_method`` cutover. The wilder path never
applies the ``>= 100`` integer rounding; the simple path stays byte-frozen
for baseline reproducibility.
"""

from __future__ import annotations

from typing import Optional

import pandas as pd


def wilder_rsi(close: pd.Series, period: int) -> pd.Series:
    """Wilder RSI via ``ewm(alpha=1/period, min_periods=period, adjust=False)``.

    NaN through the warmup window; 100 when the window has no losses.
    """
    delta = close.diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1 / period, min_periods=period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1 / period, min_periods=period, adjust=False).mean()
    rs = avg_gain / avg_loss
    return 100 - (100 / (1 + rs))


def true_range_series(
    high: pd.Series, low: pd.Series, close: pd.Series
) -> pd.Series:
    """True range from aligned high/low/close Series.

    ``max(high-low, |high-prev_close|, |low-prev_close|)`` per bar; first bar
    falls back to ``high-low`` (the shifted-close legs are NaN).
    """
    high = high.astype(float)
    low = low.astype(float)
    prev_close = close.astype(float).shift(1)
    return pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)


def true_range(df: pd.DataFrame) -> pd.Series:
    """True range from a DataFrame with ``high``/``low``/``close`` columns."""
    return true_range_series(df["high"], df["low"], df["close"])


# ATR smoothing methods (#1277). "simple" is the frozen legacy rolling mean;
# "wilder" is the published Wilder RMA. Config-side vocabulary must stay in
# lockstep with scheduler/config.go (ATRMethodSimple/ATRMethodWilder).
ATR_METHOD_SIMPLE = "simple"
ATR_METHOD_WILDER = "wilder"
ATR_METHODS = (ATR_METHOD_SIMPLE, ATR_METHOD_WILDER)


def normalize_atr_method(method: Optional[str]) -> str:
    """Normalize/validate an ATR smoothing-method name (#1277).

    Empty/None falls back to ``"simple"`` (the frozen default); anything
    outside :data:`ATR_METHODS` fails loud — the ATR feeds live stop
    geometry, so a typo must never silently degrade to a default.
    """
    norm = str(method or "").strip().lower()
    if not norm:
        return ATR_METHOD_SIMPLE
    if norm not in ATR_METHODS:
        raise ValueError(
            f"atr_method must be one of {list(ATR_METHODS)}, got {method!r}"
        )
    return norm


def round_atr_large(atr: pd.Series) -> pd.Series:
    """Repo ATR rounding convention (#887): integer-round only when >= 100.

    BTC-scale assets round to whole numbers; sub-100 assets pass through at
    full precision (rounding those would zero sub-dollar ATRs).
    """
    return atr.where(atr < 100, atr.round(0))


def atr_from_true_range(
    tr: pd.Series,
    period: int,
    *,
    round_large: bool = True,
    min_periods: Optional[int] = None,
    method: str = ATR_METHOD_SIMPLE,
) -> pd.Series:
    """ATR from a precomputed true-range Series (see ``atr_sma_series``).

    For call sites that also consume the raw ``tr`` downstream (breakout,
    session_breakout) so true range isn't computed twice. This is the single
    smoothing choke point: ``method="wilder"`` (#1277) switches to the
    published Wilder RMA and never rounds; ``round_large`` only applies to
    the simple path.
    """
    method = normalize_atr_method(method)
    if method == ATR_METHOD_WILDER:
        # Wilder RMA: ewm(alpha=1/period, adjust=False). min_periods defaults
        # to the full period so warmup bars stay NaN, mirroring the rolling
        # default of the simple path. The >= 100 integer rounding (#887) is a
        # simple-mean-era convention frozen for baseline reproducibility —
        # the wilder path always returns full precision.
        mp = period if min_periods is None else min_periods
        return tr.ewm(alpha=1 / period, min_periods=mp, adjust=False).mean()
    atr = tr.rolling(window=period, min_periods=min_periods).mean()
    if round_large:
        atr = round_atr_large(atr)
    return atr


def atr_sma_series(
    high: pd.Series,
    low: pd.Series,
    close: pd.Series,
    period: int,
    *,
    round_large: bool = True,
    min_periods: Optional[int] = None,
    method: str = ATR_METHOD_SIMPLE,
) -> pd.Series:
    """ATR over ``period`` bars from aligned high/low/close Series.

    ``method="simple"`` (default) is the frozen legacy rolling mean of true
    range; ``method="wilder"`` is the published Wilder RMA (#1277, never
    rounded). On the simple path, ``round_large=True`` applies the ``>= 100``
    integer-rounding convention (``standard_atr``); ``round_large=False``
    preserves the raw rolling mean (supertrend / squeeze_momentum /
    order_blocks / session_breakout / sweep_squeeze_combo / chart_patterns
    convention). ``min_periods`` defaults to a full window (NaN warmup).
    """
    return atr_from_true_range(
        true_range_series(high, low, close),
        period,
        round_large=round_large,
        min_periods=min_periods,
        method=method,
    )


def atr_sma(
    df: pd.DataFrame,
    period: int,
    *,
    round_large: bool = True,
    min_periods: Optional[int] = None,
    method: str = ATR_METHOD_SIMPLE,
) -> pd.Series:
    """``atr_sma_series`` over a DataFrame with ``high``/``low``/``close``."""
    return atr_sma_series(
        df["high"],
        df["low"],
        df["close"],
        period,
        round_large=round_large,
        min_periods=min_periods,
        method=method,
    )
