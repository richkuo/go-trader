import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

LOOKBACK_MIN = 2
LOOKBACK_MAX = 100
LEVEL_MIN = -100.0
LEVEL_MAX = 0.0
REQUIRED_COLUMNS = ("high", "low", "close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "nonpositive_price": 12,
    "inconsistent_hlc": 13,
    "timestamp_order": 14,
    "zero_range_window": 16,
    "missing_columns": 17,
}

_NAME = "williams_r_reversal"


def _validate_lookback(value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{_NAME}: lookback must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{_NAME}: lookback must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < LOOKBACK_MIN or as_int > LOOKBACK_MAX:
        raise ValueError(
            f"{_NAME}: lookback must be in [{LOOKBACK_MIN}, {LOOKBACK_MAX}], got {value!r}"
        )
    return as_int


def _validate_level(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{_NAME}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or as_float < LEVEL_MIN or as_float > LEVEL_MAX:
        raise ValueError(
            f"{_NAME}: {name} must be finite and in [{LEVEL_MIN:g}, {LEVEL_MAX:g}], got {value!r}"
        )
    return as_float


def validate_williams_r_params(lookback, oversold, overbought) -> tuple:
    lookback = _validate_lookback(lookback)
    oversold = _validate_level("oversold", oversold)
    overbought = _validate_level("overbought", overbought)
    if not oversold < overbought:
        raise ValueError(
            f"{_NAME}: oversold must be below overbought, got {oversold!r} >= {overbought!r}"
        )
    return lookback, oversold, overbought


def williams_r_param_validator(params: dict) -> None:
    params = params or {}
    validate_williams_r_params(params.get("lookback"), params.get("oversold"), params.get("overbought"))


def required_history_bars(lookback: int) -> int:
    return int(lookback) + 1


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


def _trailing_all(flags: np.ndarray, window: int) -> np.ndarray:
    n = len(flags)
    out = np.zeros(n, dtype=bool)
    if n < window:
        return out
    out[window - 1:] = sliding_window_view(flags, window).all(axis=1)
    return out


def _trailing_extreme(values: np.ndarray, window: int, fn) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window:
        return out
    out[window - 1:] = fn(sliding_window_view(values, window), axis=1)
    return out


def _write(result: pd.DataFrame, cols: dict) -> pd.DataFrame:
    for key, values in cols.items():
        result[key] = values
    return result


def williams_r_reversal_core(
    df: pd.DataFrame,
    lookback: int = 14,
    oversold: float = -80.0,
    overbought: float = -20.0,
) -> pd.DataFrame:
    lookback, oversold, overbought = validate_williams_r_params(lookback, oversold, overbought)
    result = df.copy()
    n = len(result)
    nan = np.full(n, np.nan)
    levels = {
        "wr_lookback": np.full(n, lookback, dtype=np.int64),
        "wr_oversold": np.full(n, oversold),
        "wr_overbought": np.full(n, overbought),
    }

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        reason = "missing_columns:" + ",".join(missing)
        return _write(result, {
            "wr_highest_high": nan, "wr_lowest_low": nan, "wr": nan, "wr_prev": nan,
            **levels,
            "wr_eval_ts_ms": nan, "wr_valid": np.zeros(n, dtype=bool),
            "wr_reason": np.full(n, reason, dtype=object),
            "wr_reason_code": np.full(n, REASON_CODES["missing_columns"], dtype=np.int64),
            "signal": np.zeros(n, dtype=np.int64),
        })

    hi = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    cl = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(hi) & np.isfinite(lo) & np.isfinite(cl)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ~((hi > 0) & (lo > 0) & (cl > 0))
        inconsistent = finite & ~nonpositive & ((hi < lo) | (cl > hi) | (cl < lo))

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
    row_reason[nonpositive] = "nonpositive_price"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~nonpositive & ~inconsistent & ~bad_ts

    window_ok = _trailing_all(row_ok, lookback)
    highest = _trailing_extreme(np.where(row_ok, hi, np.nan), lookback, np.max)
    lowest = _trailing_extreme(np.where(row_ok, lo, np.nan), lookback, np.min)
    highest[~window_ok] = np.nan
    lowest[~window_ok] = np.nan
    with np.errstate(invalid="ignore"):
        span = highest - lowest
        zero_range = window_ok & ~(span > 0)
    wr_ok = window_ok & ~zero_range
    wr = np.full(n, np.nan)
    wr[wr_ok] = -100.0 * (highest[wr_ok] - cl[wr_ok]) / span[wr_ok]

    prev = np.full(n, np.nan)
    decision_ok = np.zeros(n, dtype=bool)
    if n > 1:
        decision_ok[1:] = wr_ok[1:] & wr_ok[:-1]
        prev[1:] = wr[:-1]
    prev[~decision_ok] = np.nan

    with np.errstate(invalid="ignore"):
        long_entry = decision_ok & (prev <= oversold) & (wr > oversold)
        short_entry = decision_ok & (prev >= overbought) & (wr < overbought)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(lookback)
    for i in np.flatnonzero(~decision_ok):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        start = max(0, i - need + 1)
        prior = [r for r in row_reason[start:i] if r]
        if prior:
            reason[i] = prior[-1]
        elif i + 1 < need:
            reason[i] = "insufficient_history"
        else:
            reason[i] = "zero_range_window"

    return _write(result, {
        "wr_highest_high": highest,
        "wr_lowest_low": lowest,
        "wr": wr,
        "wr_prev": prev,
        **levels,
        "wr_eval_ts_ms": ts if ts is not None else nan,
        "wr_valid": decision_ok,
        "wr_reason": reason,
        "wr_reason_code": np.array([REASON_CODES[r] for r in reason], dtype=np.int64),
        "signal": signal,
    })
