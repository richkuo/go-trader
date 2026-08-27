from __future__ import annotations
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
    return 100 - 100 / (1 + rs)

def true_range_series(high: pd.Series, low: pd.Series, close: pd.Series) -> pd.Series:
    high = high.astype(float)
    low = low.astype(float)
    prev_close = close.astype(float).shift(1)
    return pd.concat([high - low, (high - prev_close).abs(), (low - prev_close).abs()], axis=1).max(axis=1)

def true_range(df: pd.DataFrame) -> pd.Series:
    return true_range_series(df['high'], df['low'], df['close'])
ATR_METHOD_SIMPLE = 'simple'
ATR_METHOD_WILDER = 'wilder'
ATR_METHODS = (ATR_METHOD_SIMPLE, ATR_METHOD_WILDER)

def normalize_atr_method(method: Optional[str]) -> str:
    norm = str(method or '').strip().lower()
    if not norm:
        return ATR_METHOD_SIMPLE
    if norm not in ATR_METHODS:
        raise ValueError(f'atr_method must be one of {list(ATR_METHODS)}, got {method!r}')
    return norm

def round_atr_large(atr: pd.Series) -> pd.Series:
    return atr.where(atr < 100, atr.round(0))

def atr_from_true_range(tr: pd.Series, period: int, *, round_large: bool=True, min_periods: Optional[int]=None, method: str=ATR_METHOD_SIMPLE) -> pd.Series:
    method = normalize_atr_method(method)
    if method == ATR_METHOD_WILDER:
        mp = period if min_periods is None else min_periods
        return tr.ewm(alpha=1 / period, min_periods=mp, adjust=False).mean()
    atr = tr.rolling(window=period, min_periods=min_periods).mean()
    if round_large:
        atr = round_atr_large(atr)
    return atr

def atr_sma_series(high: pd.Series, low: pd.Series, close: pd.Series, period: int, *, round_large: bool=True, min_periods: Optional[int]=None, method: str=ATR_METHOD_SIMPLE) -> pd.Series:
    return atr_from_true_range(true_range_series(high, low, close), period, round_large=round_large, min_periods=min_periods, method=method)

def atr_sma(df: pd.DataFrame, period: int, *, round_large: bool=True, min_periods: Optional[int]=None, method: str=ATR_METHOD_SIMPLE) -> pd.Series:
    return atr_sma_series(df['high'], df['low'], df['close'], period, round_large=round_large, min_periods=min_periods, method=method)
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
        return float('nan')
    t = np.arange(scale, dtype=float)
    starts = [profile[:n_segments * scale]]
    tail = profile[n - n_segments * scale:]
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

def hurst_exponent(close: pd.Series, *, min_points: int=HURST_DFA_MIN_POINTS) -> float:
    prices = close.astype(float).to_numpy()
    prices = prices[np.isfinite(prices)]
    if len(prices) < min_points + 1 or np.any(prices <= 0):
        return float('nan')
    log_returns = np.diff(np.log(prices))
    log_returns = log_returns[np.isfinite(log_returns)]
    n = len(log_returns)
    if n < min_points:
        return float('nan')
    profile = np.cumsum(log_returns - np.mean(log_returns))
    scales = _hurst_dfa_scales(n)
    if len(scales) < 2:
        return float('nan')
    fluctuations = np.array([_hurst_dfa_fluctuation(profile, int(s)) for s in scales])
    if not np.all(np.isfinite(fluctuations)) or np.any(fluctuations <= 0):
        return float('nan')
    log_scales = np.log(scales.astype(float))
    log_fluctuations = np.log(fluctuations)
    slope, _intercept = np.polyfit(log_scales, log_fluctuations, 1)
    if not np.isfinite(slope):
        return float('nan')
    return float(slope)
