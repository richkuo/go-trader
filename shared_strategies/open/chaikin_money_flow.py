import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

WINDOW_MIN = 2
WINDOW_MAX = 100
REQUIRED_COLUMNS = ("high", "low", "close", "volume")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_breakout": 0,
    "flow_below_threshold": 2,
    "breakout_continuation": 3,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "negative_volume": 12,
    "inconsistent_hlc": 13,
    "timestamp_order": 14,
    "zero_volume_window": 15,
    "missing_columns": 16,
}


def _validate_window(name: str, value) -> int:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"chaikin_money_flow_breakout: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"chaikin_money_flow_breakout: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < WINDOW_MIN or as_int > WINDOW_MAX:
        raise ValueError(
            f"chaikin_money_flow_breakout: {name} must be in [{WINDOW_MIN}, {WINDOW_MAX}], got {value!r}"
        )
    return as_int


def _validate_threshold(value) -> float:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"chaikin_money_flow_breakout: flow_threshold must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or as_float < 0.0 or as_float >= 1.0:
        raise ValueError(
            f"chaikin_money_flow_breakout: flow_threshold must be finite and in [0, 1), got {value!r}"
        )
    return as_float


def validate_cmf_params(flow_window, breakout_window, flow_threshold) -> tuple:
    return (
        _validate_window("flow_window", flow_window),
        _validate_window("breakout_window", breakout_window),
        _validate_threshold(flow_threshold),
    )


def required_history_bars(flow_window: int, breakout_window: int) -> int:
    return max(int(flow_window), int(breakout_window) + 2)


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


def _window_all(flags: np.ndarray, window: int, end_offset: int) -> np.ndarray:
    n = len(flags)
    out = np.zeros(n, dtype=bool)
    if window <= 0 or n < window + end_offset:
        return out
    views = sliding_window_view(flags, window).all(axis=1)
    start = window - 1 + end_offset
    out[start:] = views[: n - start]
    return out


def _window_reduce(values: np.ndarray, window: int, end_offset: int, op) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window + end_offset:
        return out
    views = op(sliding_window_view(values, window), axis=1)
    start = window - 1 + end_offset
    out[start:] = views[: n - start]
    return out


def _empty_result(result: pd.DataFrame, threshold: float, reason: str) -> pd.DataFrame:
    n = len(result)
    result["cmf"] = np.full(n, np.nan)
    result["cmf_upper"] = np.full(n, np.nan)
    result["cmf_lower"] = np.full(n, np.nan)
    result["cmf_breakout_state"] = np.full(n, np.nan)
    result["cmf_threshold"] = np.full(n, threshold)
    result["cmf_eval_ts_ms"] = np.full(n, np.nan)
    result["cmf_valid"] = np.zeros(n, dtype=bool)
    result["cmf_reason"] = np.full(n, reason, dtype=object)
    result["cmf_reason_code"] = np.full(n, REASON_CODES["missing_columns"], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def chaikin_money_flow_breakout_core(
    df: pd.DataFrame,
    flow_window: int = 20,
    breakout_window: int = 20,
    flow_threshold: float = 0.05,
) -> pd.DataFrame:
    flow_window, breakout_window, flow_threshold = validate_cmf_params(
        flow_window, breakout_window, flow_threshold
    )
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _empty_result(result, flow_threshold, "missing_columns:" + ",".join(missing))

    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    volume = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(high) & np.isfinite(low) & np.isfinite(close) & np.isfinite(volume)
    with np.errstate(invalid="ignore"):
        negative_volume = finite & (volume < 0)
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
    row_reason[negative_volume] = "negative_volume"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~negative_volume & ~inconsistent & ~bad_ts

    safe_high = np.where(row_ok, high, 0.0)
    safe_low = np.where(row_ok, low, 0.0)
    safe_close = np.where(row_ok, close, 0.0)
    safe_volume = np.where(row_ok, volume, 0.0)
    span = safe_high - safe_low
    multiplier = np.zeros(n)
    positive_span = row_ok & (span > 0)
    multiplier[positive_span] = (
        (2.0 * safe_close[positive_span] - safe_high[positive_span] - safe_low[positive_span])
        / span[positive_span]
    )
    signed_flow = multiplier * safe_volume

    flow_ok = _window_all(row_ok, flow_window, 0)
    flow_sum = _window_reduce(signed_flow, flow_window, 0, np.sum)
    volume_sum = _window_reduce(safe_volume, flow_window, 0, np.sum)
    zero_volume = flow_ok & (volume_sum <= 0)
    cmf = np.full(n, np.nan)
    cmf_ok = flow_ok & ~zero_volume
    cmf[cmf_ok] = flow_sum[cmf_ok] / volume_sum[cmf_ok]

    prior_ok = _window_all(row_ok, breakout_window, 1)
    upper = _window_reduce(np.where(row_ok, high, -np.inf), breakout_window, 1, np.max)
    lower = _window_reduce(np.where(row_ok, low, np.inf), breakout_window, 1, np.min)
    state_ok = prior_ok & row_ok
    upper = np.where(state_ok, upper, np.nan)
    lower = np.where(state_ok, lower, np.nan)

    state = np.full(n, np.nan)
    with np.errstate(invalid="ignore"):
        up = state_ok & (close > upper)
        down = state_ok & (close < lower)
    state[state_ok] = 0.0
    state[up] = 1.0
    state[down] = -1.0

    prev_state_ok = np.zeros(n, dtype=bool)
    prev_up = np.zeros(n, dtype=bool)
    prev_down = np.zeros(n, dtype=bool)
    if n > 1:
        prev_state_ok[1:] = state_ok[:-1]
        prev_up[1:] = up[:-1]
        prev_down[1:] = down[:-1]

    decision_ok = state_ok & prev_state_ok & cmf_ok
    new_up = decision_ok & up & ~prev_up
    new_down = decision_ok & down & ~prev_down
    with np.errstate(invalid="ignore"):
        long_entry = new_up & (cmf > flow_threshold)
        short_entry = new_down & (cmf < -flow_threshold)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_breakout", dtype=object)
    reason[decision_ok & ((up & prev_up) | (down & prev_down))] = "breakout_continuation"
    reason[(new_up & ~long_entry) | (new_down & ~short_entry)] = "flow_below_threshold"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    window_defect = np.full(n, "", dtype=object)
    need = required_history_bars(flow_window, breakout_window)
    for i in np.flatnonzero(~decision_ok):
        if row_reason[i]:
            window_defect[i] = row_reason[i]
            continue
        lo = max(0, i - need + 1)
        prior = [r for r in row_reason[lo:i] if r]
        if zero_volume[i]:
            window_defect[i] = "zero_volume_window"
        elif prior:
            window_defect[i] = prior[-1]
        else:
            window_defect[i] = "insufficient_history"
    invalid = ~decision_ok
    reason[invalid] = window_defect[invalid]

    result["cmf"] = cmf
    result["cmf_upper"] = upper
    result["cmf_lower"] = lower
    result["cmf_breakout_state"] = state
    result["cmf_threshold"] = np.full(n, flow_threshold)
    result["cmf_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["cmf_valid"] = decision_ok
    result["cmf_reason"] = reason
    result["cmf_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
