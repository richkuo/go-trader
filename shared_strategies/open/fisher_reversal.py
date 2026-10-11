import math
from numbers import Real

import numpy as np
import pandas as pd

NAME = "fisher_reversal"
LOOKBACK_MIN = 2
LOOKBACK_MAX = 32
EXTREME_MIN = 0.5
EXTREME_MAX = 3.0
RECURRENCE_HORIZON = 64
SMOOTH_NEW_WEIGHT = 0.33
SMOOTH_PREV_WEIGHT = 0.67
FISHER_PREV_WEIGHT = 0.5
CLAMP_EPSILON = 0.000001
CLAMP_LOW = -1.0 + CLAMP_EPSILON
CLAMP_HIGH = 1.0 - CLAMP_EPSILON
REQUIRED_COLUMNS = ("open", "high", "low", "close", "volume")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "insufficient_history": 10,
    "missing_columns": 11,
    "missing_timestamps": 12,
    "nonfinite_input": 13,
    "nonpositive_price": 14,
    "invalid_candle_geometry": 15,
    "negative_volume": 16,
    "timestamp_order": 17,
    "irregular_cadence": 18,
    "flat_range": 19,
}

VALUE_COLUMNS = (
    "fisher_value", "fisher_trigger", "fisher_prev_trigger", "fisher_normalized",
    "fisher_smoothed", "fisher_midpoint", "fisher_range_low", "fisher_range_high",
    "fisher_eval_ts_ms",
)

FLAG_COLUMNS = (
    "fisher_value_valid", "fisher_trigger_valid", "fisher_prev_trigger_valid",
    "fisher_normalized_valid", "fisher_valid",
)


def validate_fisher_params(lookback, extreme) -> tuple:
    if isinstance(lookback, (bool, np.bool_)) or not isinstance(lookback, Real):
        raise ValueError(f"{NAME}: lookback must be an integer, got {lookback!r}")
    as_float = float(lookback)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: lookback must be an integer, got {lookback!r}")
    lookback = int(as_float)
    if lookback < LOOKBACK_MIN or lookback > LOOKBACK_MAX:
        raise ValueError(f"{NAME}: lookback must be in [{LOOKBACK_MIN}, {LOOKBACK_MAX}], got {lookback!r}")
    if isinstance(extreme, (bool, np.bool_)) or not isinstance(extreme, Real):
        raise ValueError(f"{NAME}: extreme must be a number, got {extreme!r}")
    extreme = float(extreme)
    if not math.isfinite(extreme) or extreme < EXTREME_MIN or extreme > EXTREME_MAX:
        raise ValueError(f"{NAME}: extreme must be finite and in [{EXTREME_MIN}, {EXTREME_MAX}], got {extreme!r}")
    return lookback, extreme


def value_history_bars(lookback: int) -> int:
    return int(lookback) + RECURRENCE_HORIZON - 1


def required_history_bars(lookback: int) -> int:
    return int(lookback) + RECURRENCE_HORIZON + 1


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


def _window_clear(bad: np.ndarray, length: int) -> np.ndarray:
    n = len(bad)
    out = np.zeros(n, dtype=bool)
    if length <= 0:
        out[:] = True
        return out
    if n < length:
        return out
    csum = np.concatenate([[0], np.cumsum(bad.astype(np.int64))])
    ends = np.arange(length - 1, n)
    out[ends] = (csum[ends + 1] - csum[ends + 1 - length]) == 0
    return out


def _fisher_at(normalized: list, end: int) -> tuple:
    x = 0.0
    fisher = 0.0
    for k in range(end - RECURRENCE_HORIZON + 1, end + 1):
        x = SMOOTH_NEW_WEIGHT * normalized[k] + SMOOTH_PREV_WEIGHT * x
        if x > CLAMP_HIGH:
            x = CLAMP_HIGH
        elif x < CLAMP_LOW:
            x = CLAMP_LOW
        fisher = 0.5 * math.log((1.0 + x) / (1.0 - x)) + FISHER_PREV_WEIGHT * fisher
    return x, fisher


def _attach(result: pd.DataFrame, values: dict, flags: dict, reason: np.ndarray, signal: np.ndarray,
            lookback: int, extreme: float) -> pd.DataFrame:
    n = len(result)
    for col in VALUE_COLUMNS:
        result[col] = values[col]
    for col in FLAG_COLUMNS:
        result[col] = flags[col].astype("float64")
    result["fisher_lookback"] = np.full(n, float(lookback))
    result["fisher_extreme"] = np.full(n, extreme)
    result["fisher_horizon"] = np.full(n, float(RECURRENCE_HORIZON))
    result["fisher_required_history"] = np.full(n, float(required_history_bars(lookback)))
    result["fisher_reason"] = reason
    result["fisher_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result


def fisher_reversal_core(df: pd.DataFrame, lookback: int = 9, extreme: float = 1.5) -> pd.DataFrame:
    lookback, extreme = validate_fisher_params(lookback, extreme)
    result = df.copy()
    n = len(result)
    values = {col: np.zeros(n, dtype="float64") for col in VALUE_COLUMNS}
    flags = {col: np.zeros(n, dtype=bool) for col in FLAG_COLUMNS}
    signal = np.zeros(n, dtype=np.int64)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _attach(result, values, flags, np.full(n, "missing_columns", dtype=object), signal,
                       lookback, extreme)

    ts = _timestamps_ms(result)
    if ts is None:
        return _attach(result, values, flags, np.full(n, "missing_timestamps", dtype=object), signal,
                       lookback, extreme)

    open_ = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    volume = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")

    ts_missing = ~np.isfinite(ts)
    finite = np.isfinite(open_) & np.isfinite(high) & np.isfinite(low) & np.isfinite(close) & np.isfinite(volume)
    with np.errstate(invalid="ignore", over="ignore"):
        nonpositive = finite & ((open_ <= 0) | (high <= 0) | (low <= 0) | (close <= 0))
        geometry = finite & ~nonpositive & (
            (high < low) | (open_ > high) | (open_ < low) | (close > high) | (close < low))
        negative_volume = finite & ~nonpositive & ~geometry & (volume < 0)
        midpoint = (high + low) / 2.0
    midpoint_bad = finite & ~np.isfinite(midpoint)

    row_reason = np.full(n, "", dtype=object)
    row_reason[negative_volume] = "negative_volume"
    row_reason[geometry] = "invalid_candle_geometry"
    row_reason[nonpositive] = "nonpositive_price"
    row_reason[~finite | midpoint_bad] = "nonfinite_input"
    row_reason[ts_missing] = "missing_timestamps"
    row_bad = row_reason != ""

    diff = np.full(n, np.nan)
    if n > 1:
        with np.errstate(invalid="ignore"):
            diff[1:] = ts[1:] - ts[:-1]
    order_bad = np.zeros(n, dtype=bool)
    cadence_bad = np.zeros(n, dtype=bool)
    diff_known = np.zeros(n, dtype=bool)
    if n > 1:
        diff_known[1:] = ~ts_missing[1:] & ~ts_missing[:-1]
        with np.errstate(invalid="ignore"):
            order_bad[1:] = diff_known[1:] & ~(diff[1:] > 0)
    if n > 2:
        with np.errstate(invalid="ignore"):
            cadence_bad[2:] = diff_known[2:] & diff_known[1:-1] & ~(diff[2:] == diff[1:-1])

    span = value_history_bars(lookback)
    rows_ok = _window_clear(row_bad, span)
    order_ok = np.zeros(n, dtype=bool)
    cadence_ok = np.zeros(n, dtype=bool)
    if span >= 2:
        order_ok[span - 1:] = _window_clear(order_bad, span - 1)[span - 1:]
    if span >= 3:
        cadence_ok[span - 1:] = _window_clear(cadence_bad, span - 2)[span - 1:]

    norm_rows_ok = _window_clear(row_bad, lookback)
    range_low = np.zeros(n, dtype="float64")
    range_high = np.zeros(n, dtype="float64")
    normalized = np.zeros(n, dtype="float64")
    flat = np.zeros(n, dtype=bool)
    if n >= lookback:
        clean_mid = np.where(row_bad, 0.0, midpoint)
        windows = np.lib.stride_tricks.sliding_window_view(clean_mid, lookback)
        lows = windows.min(axis=1)
        highs = windows.max(axis=1)
        ends = np.arange(lookback - 1, n)
        usable = norm_rows_ok[ends]
        range_low[ends[usable]] = lows[usable]
        range_high[ends[usable]] = highs[usable]
        width = highs - lows
        flat_end = usable & ~(width > 0)
        flat[ends[flat_end]] = True
        good = usable & (width > 0)
        idx = ends[good]
        normalized[idx] = 2.0 * (clean_mid[idx] - lows[good]) / width[good] - 1.0
    norm_ok = norm_rows_ok & ~flat
    flat_clear = _window_clear(flat | ~norm_rows_ok, RECURRENCE_HORIZON)

    value_ok = rows_ok & order_ok & cadence_ok & flat_clear
    fisher = np.zeros(n, dtype="float64")
    smoothed = np.zeros(n, dtype="float64")
    norm_list = normalized.tolist()
    for t in np.flatnonzero(value_ok):
        x, f = _fisher_at(norm_list, int(t))
        smoothed[t] = x
        fisher[t] = f

    trigger_ok = np.zeros(n, dtype=bool)
    prev_trigger_ok = np.zeros(n, dtype=bool)
    trigger = np.zeros(n, dtype="float64")
    prev_trigger = np.zeros(n, dtype="float64")
    if n > 1:
        trigger_ok[1:] = value_ok[:-1]
        trigger[1:] = fisher[:-1]
    if n > 2:
        prev_trigger_ok[2:] = value_ok[:-2]
        prev_trigger[2:] = fisher[:-2]
    decision_ok = value_ok & trigger_ok & prev_trigger_ok

    long_entry = decision_ok & (fisher > trigger) & (trigger <= prev_trigger) & (trigger < -extreme)
    short_entry = decision_ok & (fisher < trigger) & (trigger >= prev_trigger) & (trigger > extreme)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"
    need = required_history_bars(lookback)
    for i in np.flatnonzero(~decision_ok):
        lo = max(0, i - need + 1)
        defects = np.flatnonzero(row_bad[lo:i + 1])
        if len(defects):
            reason[i] = row_reason[lo + defects[-1]]
        elif order_bad[lo + 1:i + 1].any():
            reason[i] = "timestamp_order"
        elif cadence_bad[lo + 2:i + 1].any():
            reason[i] = "irregular_cadence"
        elif i < need - 1:
            reason[i] = "insufficient_history"
        else:
            reason[i] = "flat_range"

    values["fisher_value"] = np.where(value_ok, fisher, 0.0)
    values["fisher_trigger"] = np.where(trigger_ok, trigger, 0.0)
    values["fisher_prev_trigger"] = np.where(prev_trigger_ok, prev_trigger, 0.0)
    values["fisher_normalized"] = np.where(norm_ok, normalized, 0.0)
    values["fisher_smoothed"] = np.where(value_ok, smoothed, 0.0)
    values["fisher_midpoint"] = np.where(row_bad, 0.0, midpoint)
    values["fisher_range_low"] = np.where(norm_rows_ok, range_low, 0.0)
    values["fisher_range_high"] = np.where(norm_rows_ok, range_high, 0.0)
    values["fisher_eval_ts_ms"] = np.where(ts_missing, 0.0, ts)
    flags["fisher_value_valid"] = value_ok
    flags["fisher_trigger_valid"] = trigger_ok
    flags["fisher_prev_trigger_valid"] = prev_trigger_ok
    flags["fisher_normalized_valid"] = norm_ok
    flags["fisher_valid"] = decision_ok
    return _attach(result, values, flags, reason, signal, lookback, extreme)
