import math
from numbers import Real

import numpy as np
import pandas as pd

PERIOD_MIN = 1
PERIOD_MAX = 100
PREDECESSOR_LOOKBACK = 8
REQUIRED_COLUMNS = ("high", "low")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "zero_oscillator": 2,
    "zero_run_exceeded": 3,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "inconsistent_hl": 13,
    "timestamp_order": 14,
    "missing_columns": 16,
}


def _validate_period(name: str, value) -> int:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"awesome_oscillator: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"awesome_oscillator: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < PERIOD_MIN or as_int > PERIOD_MAX:
        raise ValueError(
            f"awesome_oscillator: {name} must be in [{PERIOD_MIN}, {PERIOD_MAX}], got {value!r}"
        )
    return as_int


def validate_ao_params(fast_period, slow_period) -> tuple:
    fast = _validate_period("fast_period", fast_period)
    slow = _validate_period("slow_period", slow_period)
    if fast >= slow:
        raise ValueError(
            f"awesome_oscillator: fast_period must be below slow_period, got {fast} and {slow}"
        )
    return fast, slow


def required_history_bars(slow_period: int) -> int:
    return int(slow_period) + PREDECESSOR_LOOKBACK


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
    bad = np.concatenate([[0], np.cumsum(~flags)])
    out[window - 1:] = (bad[window:] - bad[:n - window + 1]) == 0
    return out


def _window_mean(values: np.ndarray, window: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window:
        return out
    count = n - window + 1
    acc = np.zeros(count)
    for j in range(window):
        acc = acc + values[j:j + count]
    out[window - 1:] = acc / window
    return out


def _empty_result(result: pd.DataFrame, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in ("ao", "ao_fast_mean", "ao_slow_mean", "ao_prev_nonzero", "ao_prev_lag", "ao_eval_ts_ms"):
        result[col] = np.full(n, np.nan)
    result["ao_valid"] = np.zeros(n, dtype=bool)
    result["ao_reason"] = np.full(n, reason, dtype=object)
    result["ao_reason_code"] = np.full(n, REASON_CODES["missing_columns"], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def awesome_oscillator_core(
    df: pd.DataFrame,
    fast_period: int = 5,
    slow_period: int = 34,
) -> pd.DataFrame:
    fast_period, slow_period = validate_ao_params(fast_period, slow_period)
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _empty_result(result, "missing_columns:" + ",".join(missing))

    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(high) & np.isfinite(low)
    with np.errstate(invalid="ignore"):
        inconsistent = finite & (high < low)

    ts = _timestamps_ms(result)
    bad_ts = np.zeros(n, dtype=bool)
    if ts is not None:
        bad_ts = ~np.isfinite(ts)
        if n > 1:
            with np.errstate(invalid="ignore"):
                bad_ts[1:] |= ~(ts[1:] > ts[:-1])

    row_reason = np.full(n, "", dtype=object)
    row_reason[bad_ts] = "timestamp_order"
    row_reason[inconsistent] = "inconsistent_hl"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~inconsistent & ~bad_ts

    mid = np.where(row_ok, (np.where(row_ok, high, 0.0) + np.where(row_ok, low, 0.0)) / 2.0, 0.0)
    ao_ok = _trailing_all(row_ok, slow_period)
    fast_mean = np.where(ao_ok, _window_mean(mid, fast_period), np.nan)
    slow_mean = np.where(ao_ok, _window_mean(mid, slow_period), np.nan)
    ao = np.full(n, np.nan)
    ao[ao_ok] = fast_mean[ao_ok] - slow_mean[ao_ok]
    sign = np.zeros(n, dtype=np.int64)
    sign[ao_ok & (ao > 0)] = 1
    sign[ao_ok & (ao < 0)] = -1

    need = required_history_bars(slow_period)
    decision_ok = _trailing_all(row_ok, need)

    prev_sign = np.zeros(n, dtype=np.int64)
    prev_lag = np.full(n, np.nan)
    prev_val = np.full(n, np.nan)
    found = np.zeros(n, dtype=bool)
    for lag in range(1, PREDECESSOR_LOOKBACK + 1):
        if n <= lag:
            break
        idx = np.arange(lag, n)
        src = idx - lag
        take = ~found[idx] & (sign[src] != 0)
        tgt = idx[take]
        prev_sign[tgt] = sign[src[take]]
        prev_lag[tgt] = float(lag)
        prev_val[tgt] = ao[src[take]]
        found[tgt] = True

    long_entry = decision_ok & (sign == 1) & (prev_sign == -1)
    short_entry = decision_ok & (sign == -1) & (prev_sign == 1)
    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[decision_ok & (sign != 0) & ~found] = "zero_run_exceeded"
    reason[decision_ok & (sign == 0)] = "zero_oscillator"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    for i in np.flatnonzero(~decision_ok):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        lo = max(0, i - need + 1)
        prior = [r for r in row_reason[lo:i] if r]
        reason[i] = prior[-1] if prior else "insufficient_history"

    prev_lag[~decision_ok] = np.nan
    prev_val[~decision_ok] = np.nan

    result["ao"] = ao
    result["ao_fast_mean"] = fast_mean
    result["ao_slow_mean"] = slow_mean
    result["ao_prev_nonzero"] = prev_val
    result["ao_prev_lag"] = prev_lag
    result["ao_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["ao_valid"] = decision_ok
    result["ao_reason"] = reason
    result["ao_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
