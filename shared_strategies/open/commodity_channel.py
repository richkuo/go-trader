import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

LOOKBACK_MIN = 2
LOOKBACK_MAX = 100
TREND_PERIOD_MIN = 2
TREND_PERIOD_MAX = 150
SCALE = 0.015
REQUIRED_COLUMNS = ("high", "low", "close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "trend_blocked": 2,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "inconsistent_hlc": 13,
    "timestamp_order": 14,
    "zero_mean_deviation": 15,
    "missing_columns": 16,
}


def _validate_int(name: str, value, lo: int, hi: int) -> int:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"commodity_channel_trend: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"commodity_channel_trend: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"commodity_channel_trend: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def _validate_threshold(value) -> float:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"commodity_channel_trend: threshold must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or as_float <= 0.0:
        raise ValueError(f"commodity_channel_trend: threshold must be finite and > 0, got {value!r}")
    return as_float


def validate_cct_params(lookback, threshold, trend_period) -> tuple:
    return (
        _validate_int("lookback", lookback, LOOKBACK_MIN, LOOKBACK_MAX),
        _validate_threshold(threshold),
        _validate_int("trend_period", trend_period, TREND_PERIOD_MIN, TREND_PERIOD_MAX),
    )


def required_history_bars(lookback: int, trend_period: int) -> int:
    return max(int(lookback) + 1, int(trend_period))


def _timestamps_ms(df: pd.DataFrame):
    if isinstance(df.index, pd.DatetimeIndex):
        idx = df.index
        if idx.tz is not None:
            idx = idx.tz_convert("UTC").tz_localize(None)
        ns_per_unit = {"s": 1_000_000_000, "ms": 1_000_000, "us": 1_000, "ns": 1}[idx.unit]
        missing = np.asarray(idx.isna())
        raw = np.where(missing, 0, idx.asi8)
        values = ((raw * ns_per_unit) // 1_000_000).astype("float64")
        values[missing] = np.nan
        return values
    if "timestamp" in df.columns:
        return pd.to_numeric(df["timestamp"], errors="coerce").to_numpy(dtype="float64")
    return None


def _window_all(flags: np.ndarray, window: int) -> np.ndarray:
    n = len(flags)
    out = np.zeros(n, dtype=bool)
    if n < window:
        return out
    out[window - 1:] = sliding_window_view(flags, window).all(axis=1)
    return out


def _window_mean(values: np.ndarray, window: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window:
        return out
    out[window - 1:] = sliding_window_view(values, window).mean(axis=1)
    return out


def _window_mean_deviation(values: np.ndarray, means: np.ndarray, window: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window:
        return out
    views = sliding_window_view(values, window)
    centers = means[window - 1:]
    out[window - 1:] = np.abs(views - centers[:, None]).mean(axis=1)
    return out


def _empty_result(result: pd.DataFrame, threshold: float, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in ("cct_typical", "cct_mean", "cct_mean_dev", "cct_index", "cct_prev_index",
                "cct_trend_sma", "cct_eval_ts_ms"):
        result[col] = np.full(n, np.nan)
    result["cct_threshold"] = np.full(n, threshold)
    result["cct_valid"] = np.zeros(n, dtype=bool)
    result["cct_reason"] = np.full(n, reason, dtype=object)
    result["cct_reason_code"] = np.full(n, REASON_CODES["missing_columns"], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def commodity_channel_trend_core(
    df: pd.DataFrame,
    lookback: int = 20,
    threshold: float = 100.0,
    trend_period: int = 50,
) -> pd.DataFrame:
    lookback, threshold, trend_period = validate_cct_params(lookback, threshold, trend_period)
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _empty_result(result, threshold, "missing_columns:" + ",".join(missing))

    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(high) & np.isfinite(low) & np.isfinite(close)
    with np.errstate(invalid="ignore"):
        inconsistent = finite & ((high < low) | (close > high) | (close < low))

    ts = _timestamps_ms(result)
    bad_ts = np.zeros(n, dtype=bool)
    if ts is not None:
        bad_ts = ~np.isfinite(ts)
        if n > 1:
            with np.errstate(invalid="ignore"):
                bad_ts[1:] |= ~(ts[1:] > ts[:-1])

    row_reason = np.full(n, "", dtype=object)
    row_reason[bad_ts] = "timestamp_order"
    row_reason[inconsistent] = "inconsistent_hlc"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~inconsistent & ~bad_ts

    typical = np.where(row_ok, (high + low + close) / 3.0, 0.0)
    safe_close = np.where(row_ok, close, 0.0)

    index_window_ok = _window_all(row_ok, lookback)
    tp_mean = _window_mean(typical, lookback)
    mean_dev = _window_mean_deviation(typical, tp_mean, lookback)
    index_ok = index_window_ok & (mean_dev > 0)
    zero_dev = index_window_ok & ~(mean_dev > 0)
    cci = np.full(n, np.nan)
    cci[index_ok] = (typical[index_ok] - tp_mean[index_ok]) / (SCALE * mean_dev[index_ok])

    prev_cci = np.full(n, np.nan)
    prev_ok = np.zeros(n, dtype=bool)
    prev_zero = np.zeros(n, dtype=bool)
    if n > 1:
        prev_cci[1:] = cci[:-1]
        prev_ok[1:] = index_ok[:-1]
        prev_zero[1:] = zero_dev[:-1]

    trend_ok = _window_all(row_ok, trend_period)
    trend_sma = _window_mean(safe_close, trend_period)
    trend_sma = np.where(trend_ok, trend_sma, np.nan)

    decision_ok = index_ok & prev_ok & trend_ok
    with np.errstate(invalid="ignore"):
        cross_up = decision_ok & (prev_cci <= threshold) & (cci > threshold)
        cross_down = decision_ok & (prev_cci >= -threshold) & (cci < -threshold)
        long_entry = cross_up & (close > trend_sma)
        short_entry = cross_down & (close < trend_sma)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[(cross_up & ~long_entry) | (cross_down & ~short_entry)] = "trend_blocked"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(lookback, trend_period)
    invalid = ~decision_ok
    for i in np.flatnonzero(invalid):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        lo = max(0, i - need + 1)
        prior = [r for r in row_reason[lo:i] if r]
        if prior:
            reason[i] = prior[-1]
        elif zero_dev[i] or (prev_zero[i] and trend_ok[i]):
            reason[i] = "zero_mean_deviation"
        else:
            reason[i] = "insufficient_history"

    result["cct_typical"] = np.where(row_ok, typical, np.nan)
    result["cct_mean"] = np.where(index_window_ok, tp_mean, np.nan)
    result["cct_mean_dev"] = np.where(index_window_ok, mean_dev, np.nan)
    result["cct_index"] = cci
    result["cct_prev_index"] = np.where(prev_ok, prev_cci, np.nan)
    result["cct_trend_sma"] = trend_sma
    result["cct_threshold"] = np.full(n, threshold)
    result["cct_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["cct_valid"] = decision_ok
    result["cct_reason"] = reason
    result["cct_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
