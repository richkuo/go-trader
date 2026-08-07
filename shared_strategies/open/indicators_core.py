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

import numpy as np
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


# --- Hurst exponent (#1409) -------------------------------------------------
#
# Detrended fluctuation analysis (DFA) over log returns. DFA is used instead
# of classic R/S (rescaled range) because R/S is materially noisier at the
# series lengths this system observes in practice (a few hundred bars).
#
# Advisory-only, observability metric: no consumer of this function may use
# its output for gating, sizing, or config surfaces (see #1409). It never
# raises and never returns a fabricated 0.5 for "not enough data" — it
# returns NaN so "unknown" stays distinguishable from "measured random walk".

HURST_DFA_MIN_POINTS = 100
# #1419 review: segment scales below ~8 bars bias the estimate upward on memoryless
# (Gaussian random walk) data — measured mean H ~= 0.54 at n=200 and ~= 0.55 at n=101
# with min_scale=4, vs ~= 0.51 / ~= 0.52 with min_scale=8, matching the short-scale DFA
# bias documented in "On the Validity of Detrended Fluctuation Analysis at Short Scales"
# (Entropy 24(1):61) — the bias does not vanish with a longer fetch. 8 was verified to
# keep AR(1) persistent/mean-reverting separation intact (regression: test_indicators_core.py).
_HURST_DFA_MIN_SCALE = 8
_HURST_DFA_NUM_SCALES = 12


def _hurst_dfa_scales(n_points: int) -> np.ndarray:
    """Log-spaced integer segment sizes for DFA, from 4 bars to n/4 bars."""
    max_scale = max(_HURST_DFA_MIN_SCALE, n_points // 4)
    if max_scale <= _HURST_DFA_MIN_SCALE:
        return np.array([max_scale], dtype=int)
    scales = np.geomspace(_HURST_DFA_MIN_SCALE, max_scale, num=_HURST_DFA_NUM_SCALES)
    return np.unique(scales.astype(int))


def _hurst_dfa_fluctuation(profile: np.ndarray, scale: int) -> float:
    """RMS detrended fluctuation of ``profile`` at one segment length.

    Non-overlapping segments from both the start and the end of the profile
    (the standard DFA convention) so short scales still get two independent
    passes over the leftover bars a single direction would drop.

    #1419 review: every segment at a given ``scale`` shares the same
    design matrix (``t = arange(scale)``), so the per-segment linear
    detrend is one fixed-design least-squares fit applied to the whole
    reshaped segment block via its pseudo-inverse, rather than one
    ``np.polyfit`` call per segment (verified numerically equivalent to
    the old per-segment-polyfit path to <1e-9 relative tolerance at the
    scales this estimator actually uses, sub-1e-12 in fuzz testing).
    """
    n = len(profile)
    n_segments = n // scale
    if n_segments < 1:
        return float("nan")
    t = np.arange(scale, dtype=float)
    starts = [profile[: n_segments * scale]]
    tail = profile[n - n_segments * scale :]
    if not np.array_equal(tail, starts[0]):
        starts.append(tail)
    design = np.column_stack([t, np.ones_like(t)])
    pinv_design = np.linalg.pinv(design)  # (2, scale): shared by every segment at this scale
    sq_residuals: list[np.ndarray] = []
    for block in starts:
        segments = block.reshape(n_segments, scale)  # (n_segments, scale)
        coeffs = segments @ pinv_design.T  # (n_segments, 2) = [slope, intercept] per segment
        trend = coeffs @ design.T  # (n_segments, scale)
        sq_residuals.append(np.mean((segments - trend) ** 2, axis=1))
    return float(np.sqrt(np.mean(np.concatenate(sq_residuals))))


def hurst_exponent(close: pd.Series, *, min_points: int = HURST_DFA_MIN_POINTS) -> float:
    """Hurst exponent via detrended fluctuation analysis (DFA) over log returns.

    Operates on log returns of ``close`` (not raw prices), so the estimate is
    scale-invariant across assets. Deterministic — no randomness, no fitted
    randomness-dependent seed.

    Returns **NaN** — never raises, never a fabricated 0.5 — whenever there
    are fewer than ``min_points`` valid log-return observations (default 100,
    the floor below which the DFA scaling-exponent regression becomes
    unstable), whenever prices are non-positive (breaks ``log``), or whenever
    the series is degenerate (e.g. constant price -> zero-variance profile,
    undefined log-log slope). Callers must treat NaN as "unknown", never as
    "random walk" — a caller emitting this into a JSON payload must OMIT the
    key rather than serialize NaN (bare ``NaN`` is not valid JSON and breaks
    strict parsers such as Go's ``encoding/json``).

    H > 0.5 marks a persistent/trending series; H < 0.5 marks a
    mean-reverting series; H ~= 0.5 marks a Gaussian random walk (no memory).

    Null-distribution caveat: DFA carries a known small upward bias at short
    sample sizes that a longer fetch does not remove. Measured at this scale
    floor (``_HURST_DFA_MIN_SCALE``) on memoryless (Gaussian random walk)
    data, the estimate's own mean is ~0.51 at n=200 points and ~0.52 at
    n=101 (sd ~0.06-0.09) rather than the "true" 0.50 -- a reading in the
    ~0.50-0.55 band near those sample sizes is consistent with no memory at
    all, not confirmed persistence.
    """
    prices = close.astype(float).to_numpy()
    prices = prices[np.isfinite(prices)]
    if len(prices) < min_points + 1 or np.any(prices <= 0):
        return float("nan")
    log_returns = np.diff(np.log(prices))
    log_returns = log_returns[np.isfinite(log_returns)]
    n = len(log_returns)
    if n < min_points:
        return float("nan")

    profile = np.cumsum(log_returns - np.mean(log_returns))
    scales = _hurst_dfa_scales(n)
    if len(scales) < 2:
        return float("nan")

    fluctuations = np.array([_hurst_dfa_fluctuation(profile, int(s)) for s in scales])
    if not np.all(np.isfinite(fluctuations)) or np.any(fluctuations <= 0):
        return float("nan")

    log_scales = np.log(scales.astype(float))
    log_fluctuations = np.log(fluctuations)
    slope, _intercept = np.polyfit(log_scales, log_fluctuations, 1)
    if not np.isfinite(slope):
        return float("nan")
    return float(slope)
