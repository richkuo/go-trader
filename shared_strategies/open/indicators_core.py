
from __future__ import annotations

import math
from functools import lru_cache
from typing import Optional

import numpy as np
import pandas as pd


def wilder_rsi(close: pd.Series, period: int) -> pd.Series:
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
    high = high.astype(float)
    low = low.astype(float)
    prev_close = close.astype(float).shift(1)
    return pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)


def true_range(df: pd.DataFrame) -> pd.Series:
    return true_range_series(df["high"], df["low"], df["close"])


ATR_METHOD_SIMPLE = "simple"
ATR_METHOD_WILDER = "wilder"
ATR_METHODS = (ATR_METHOD_SIMPLE, ATR_METHOD_WILDER)


def normalize_atr_method(method: Optional[str]) -> str:
    norm = str(method or "").strip().lower()
    if not norm:
        return ATR_METHOD_SIMPLE
    if norm not in ATR_METHODS:
        raise ValueError(
            f"atr_method must be one of {list(ATR_METHODS)}, got {method!r}"
        )
    return norm


def round_atr_large(atr: pd.Series) -> pd.Series:
    return atr.where(atr < 100, atr.round(0))


def atr_from_true_range(
    tr: pd.Series,
    period: int,
    *,
    round_large: bool = True,
    min_periods: Optional[int] = None,
    method: str = ATR_METHOD_SIMPLE,
) -> pd.Series:
    method = normalize_atr_method(method)
    if method == ATR_METHOD_WILDER:
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
    return atr_sma_series(
        df["high"],
        df["low"],
        df["close"],
        period,
        round_large=round_large,
        min_periods=min_periods,
        method=method,
    )


HURST_DFA_MIN_POINTS = 100
_HURST_DFA_MIN_SCALE = 8
_HURST_DFA_NUM_SCALES = 12


def _hurst_dfa_scales(n_points: int) -> np.ndarray:
    max_scale = max(_HURST_DFA_MIN_SCALE, n_points // 4)
    if max_scale <= _HURST_DFA_MIN_SCALE:
        return np.array([max_scale], dtype=int)
    scales = np.geomspace(_HURST_DFA_MIN_SCALE, max_scale, num=_HURST_DFA_NUM_SCALES)
    return np.unique(scales.astype(int))


def _hurst_dfa_fluctuation(profile: np.ndarray, scale: int) -> float:
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
    pinv_design = np.linalg.pinv(design)
    sq_residuals: list[np.ndarray] = []
    for block in starts:
        segments = block.reshape(n_segments, scale)
        coeffs = segments @ pinv_design.T
        trend = coeffs @ design.T
        sq_residuals.append(np.mean((segments - trend) ** 2, axis=1))
    return float(np.sqrt(np.mean(np.concatenate(sq_residuals))))


HURST_RS_MIN_POINTS = 100
_HURST_RS_MIN_BLOCK = 16
_HURST_RS_NUM_BLOCKS = 12


def _hurst_rs_block_sizes(n_points: int) -> np.ndarray:
    max_block = max(_HURST_RS_MIN_BLOCK, n_points // 4)
    if max_block <= _HURST_RS_MIN_BLOCK:
        return np.array([max_block], dtype=int)
    blocks = np.geomspace(_HURST_RS_MIN_BLOCK, max_block, num=_HURST_RS_NUM_BLOCKS)
    return np.unique(blocks.astype(int))


def _hurst_rs_statistic(series: np.ndarray, block: int) -> float:
    n = len(series)
    n_blocks = n // block
    if n_blocks < 1:
        return float("nan")
    partitions = [series[: n_blocks * block]]
    tail = series[n - n_blocks * block :]
    if not np.array_equal(tail, partitions[0]):
        partitions.append(tail)
    ratios: list[np.ndarray] = []
    for part in partitions:
        segments = part.reshape(n_blocks, block)
        deviations = segments - np.mean(segments, axis=1, keepdims=True)
        cumulative = np.cumsum(deviations, axis=1)
        spread = np.max(cumulative, axis=1) - np.min(cumulative, axis=1)
        scale = np.std(segments, axis=1)
        usable = scale > 0
        if not np.any(usable):
            continue
        ratios.append(spread[usable] / scale[usable])
    if not ratios:
        return float("nan")
    return float(np.mean(np.concatenate(ratios)))


@lru_cache(maxsize=None)
def _anis_lloyd_expected_rs(block: int) -> float:
    n = int(block)
    if n < 2:
        return float("nan")
    i = np.arange(1, n, dtype=float)
    tail = float(np.sum(np.sqrt((n - i) / i)))
    if n > 340:
        front = 1.0 / math.sqrt(n * math.pi / 2.0)
    else:
        front = math.exp(
            math.lgamma((n - 1) / 2.0) - math.lgamma(n / 2.0)
        ) / math.sqrt(math.pi)
    return float(((n - 0.5) / n) * front * tail)


def hurst_rescaled_range(close: pd.Series, *,
                         min_points: int = HURST_RS_MIN_POINTS,
                         corrected: bool = True) -> float:
    prices = close.astype(float).to_numpy()
    prices = prices[np.isfinite(prices)]
    if len(prices) < min_points + 1 or np.any(prices <= 0):
        return float("nan")
    log_returns = np.diff(np.log(prices))
    log_returns = log_returns[np.isfinite(log_returns)]
    n = len(log_returns)
    if n < min_points:
        return float("nan")

    blocks = _hurst_rs_block_sizes(n)
    if len(blocks) < 2:
        return float("nan")

    ratios = np.array([_hurst_rs_statistic(log_returns, int(b)) for b in blocks])
    if not np.all(np.isfinite(ratios)) or np.any(ratios <= 0):
        return float("nan")

    log_blocks = np.log(blocks.astype(float))
    log_ratios = np.log(ratios)
    if corrected:
        expected = np.array([_anis_lloyd_expected_rs(int(b)) for b in blocks])
        if not np.all(np.isfinite(expected)) or np.any(expected <= 0):
            return float("nan")
        log_ratios = log_ratios - np.log(expected)
    slope, _intercept = np.polyfit(log_blocks, log_ratios, 1)
    if not np.isfinite(slope):
        return float("nan")
    return float(slope + 0.5) if corrected else float(slope)


def hurst_exponent(close: pd.Series, *, min_points: int = HURST_DFA_MIN_POINTS) -> float:
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
