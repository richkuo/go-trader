import math
import os
import sys
from numbers import Real

import numpy as np
import pandas as pd

_TOOLS_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "shared_tools"))
if _TOOLS_DIR not in sys.path:
    sys.path.append(_TOOLS_DIR)

from closed_bar import (
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)

NAME = "mass_index_reversal"
REQUIRED_COLUMNS = ("open", "high", "low", "close", "volume")
MAX_REQUIRED_HISTORY = 199
RECURSION_MULTIPLE = 8

DEFAULT_PARAMS = {
    "ema_period": 9,
    "sum_period": 25,
    "upper_threshold": 27.0,
    "lower_threshold": 26.5,
    "trend_period": 50,
    "slope_span": 5,
    "confirm_lookback": 5,
    "max_bulge_age": 50,
    "recursion_length": 72,
    "bulge_filter": 1,
}

INT_MINIMUMS = {
    "ema_period": 2,
    "sum_period": 1,
    "trend_period": 2,
    "slope_span": 1,
    "confirm_lookback": 1,
    "max_bulge_age": 1,
    "recursion_length": 1,
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_bulge": 0,
    "armed": 2,
    "confirmation_failed": 3,
    "zero_slope": 4,
    "insufficient_history": 10,
    "missing_columns": 11,
    "missing_cadence": 12,
    "missing_timestamps": 13,
    "nonfinite_input": 14,
    "nonpositive_price": 15,
    "invalid_candle_geometry": 16,
    "negative_volume": 17,
    "timestamp_order": 18,
    "cadence_gap": 19,
    "nonpositive_average": 20,
    "nonfinite_derived": 21,
}

VALUE_COLUMNS = (
    "mir_range", "mir_ema1", "mir_ema2", "mir_ratio", "mir_mass_index", "mir_prev_mass_index",
    "mir_trend_sma", "mir_trend_slope", "mir_upper_cross", "mir_lower_cross",
    "mir_bulge_armed_ts_ms", "mir_bulge_age", "mir_bulge_slope", "mir_bulge_direction", "mir_bulge_completed",
    "mir_tested_side", "mir_confirmed", "mir_filtered", "mir_close", "mir_confirm_high", "mir_confirm_low",
)


def _int_param(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    return int(as_float)


def _float_param(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"{NAME}: {name} must be finite, got {value!r}")
    return as_float


def required_history_bars(sum_period: int, trend_period: int, slope_span: int, confirm_lookback: int,
                          max_bulge_age: int, recursion_length: int) -> int:
    return max(int(max_bulge_age) + int(recursion_length) + int(sum_period) + 1,
               int(max_bulge_age) + int(trend_period) + int(slope_span),
               int(confirm_lookback) + 1)


def validate_mir_params(ema_period, sum_period, upper_threshold, lower_threshold, trend_period, slope_span,
                        confirm_lookback, max_bulge_age, recursion_length, bulge_filter) -> dict:
    raw = {"ema_period": ema_period, "sum_period": sum_period, "trend_period": trend_period,
           "slope_span": slope_span, "confirm_lookback": confirm_lookback, "max_bulge_age": max_bulge_age,
           "recursion_length": recursion_length}
    out = {}
    for key, value in raw.items():
        as_int = _int_param(key, value)
        if as_int < INT_MINIMUMS[key]:
            raise ValueError(f"{NAME}: {key} must be >= {INT_MINIMUMS[key]}, got {value!r}")
        out[key] = as_int
    flag = _int_param("bulge_filter", bulge_filter)
    if flag not in (0, 1):
        raise ValueError(f"{NAME}: bulge_filter must be 0 or 1, got {bulge_filter!r}")
    out["bulge_filter"] = flag
    upper = _float_param("upper_threshold", upper_threshold)
    lower = _float_param("lower_threshold", lower_threshold)
    if not (upper > lower > 0.0):
        raise ValueError(f"{NAME}: thresholds need upper_threshold > lower_threshold > 0, "
                         f"got {upper_threshold!r} and {lower_threshold!r}")
    out["upper_threshold"] = upper
    out["lower_threshold"] = lower
    if out["recursion_length"] < RECURSION_MULTIPLE * out["ema_period"]:
        raise ValueError(f"{NAME}: recursion_length must be >= {RECURSION_MULTIPLE} x ema_period "
                         f"({RECURSION_MULTIPLE * out['ema_period']}), got {recursion_length!r}")
    need = required_history_bars(out["sum_period"], out["trend_period"], out["slope_span"],
                                 out["confirm_lookback"], out["max_bulge_age"], out["recursion_length"])
    if need > MAX_REQUIRED_HISTORY:
        raise ValueError(
            f"{NAME}: required history max(max_bulge_age + recursion_length + sum_period + 1, "
            f"max_bulge_age + trend_period + slope_span, confirm_lookback + 1) = {need} rows exceeds "
            f"{MAX_REQUIRED_HISTORY}, the closed portion of a 200-row check frame")
    out["required_history"] = need
    return out


def mir_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{NAME}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS))
    if unknown:
        raise ValueError(f"{NAME}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **params}
    validate_mir_params(**merged)


def frame_cadence(df: pd.DataFrame) -> tuple:
    raw = df.attrs.get(FRAME_INTERVAL_ATTR)
    interval = None
    if not isinstance(raw, (bool, np.bool_)) and isinstance(raw, (int, np.integer)) and int(raw) > 0:
        interval = int(raw)
    kind = df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR)
    if kind not in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE):
        kind = None
    return interval, kind


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


def _window_clear(bad: np.ndarray, start_offset: int, length: int) -> np.ndarray:
    n = len(bad)
    out = np.zeros(n, dtype=bool)
    if length <= 0:
        out[:] = True
        return out
    csum = np.concatenate(([0], np.cumsum(bad.astype(np.int64))))
    first = start_offset + length - 1
    if n <= first:
        return out
    ends = np.arange(first, n)
    lo = ends - start_offset - length + 1
    hi = ends - start_offset
    out[ends] = (csum[hi + 1] - csum[lo]) == 0
    return out


def _last_defect(reasons: np.ndarray, lo: int, hi: int):
    lo = max(lo, 0)
    if hi < lo:
        return None
    hits = np.flatnonzero(reasons[lo:hi + 1] != "")
    if len(hits) == 0:
        return None
    pos = lo + int(hits[-1])
    return pos, reasons[pos]


def _ratios(rng: np.ndarray, ema_period: int, recursion_length: int) -> dict:
    n = len(rng)
    out = {k: np.full(n, np.nan) for k in ("ema1", "ema2", "ratio")}
    out["nonpositive"] = np.zeros(n, dtype=bool)
    if n <= recursion_length:
        return out
    alpha = 2.0 / (ema_period + 1.0)
    keep = 1.0 - alpha
    x = np.arange(recursion_length, n)
    with np.errstate(over="ignore", invalid="ignore"):
        e1 = rng[x - recursion_length].copy()
        e2 = e1.copy()
        for k in range(1, recursion_length + 1):
            e1 = alpha * rng[x - recursion_length + k] + keep * e1
            e2 = alpha * e1 + keep * e2
        good = np.isfinite(e1) & np.isfinite(e2) & (e2 > 0.0)
        ratio = np.where(good, e1 / np.where(good, e2, 1.0), np.nan)
    out["ema1"][x] = e1
    out["ema2"][x] = e2
    out["ratio"][x] = np.where(np.isfinite(ratio), ratio, np.nan)
    out["nonpositive"][x] = np.isfinite(e2) & (e2 <= 0.0)
    return out


def _trailing_sum(values: np.ndarray, length: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < length:
        return out
    k = np.arange(length - 1, n)
    with np.errstate(over="ignore", invalid="ignore"):
        total = values[k - length + 1].copy()
        for j in range(1, length):
            total = total + values[k - length + 1 + j]
    out[k] = total
    return out


def _trailing_extreme(values: np.ndarray, length: int, fn) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n <= length:
        return out
    t = np.arange(length, n)
    acc = values[t - length].copy()
    for j in range(1, length):
        acc = fn(acc, values[t - length + j])
    out[t] = acc
    return out


def _last_index(flags: np.ndarray) -> np.ndarray:
    idx = np.where(flags, np.arange(len(flags)), -1)
    return np.maximum.accumulate(idx) if len(idx) else idx


def _attach(result: pd.DataFrame, values: dict, valid: np.ndarray, reason: np.ndarray, signal: np.ndarray,
            params: dict, interval, eval_ts: np.ndarray) -> pd.DataFrame:
    n = len(result)
    result["mir_eval_ts_ms"] = eval_ts
    for col in VALUE_COLUMNS:
        result[col] = values[col]
    result["mir_interval_ms"] = np.full(n, float(interval) if interval is not None else np.nan)
    for key in ("ema_period", "sum_period", "upper_threshold", "lower_threshold", "trend_period", "slope_span",
                "confirm_lookback", "max_bulge_age", "recursion_length", "bulge_filter", "required_history"):
        result[f"mir_{key}"] = np.full(n, float(params[key]))
    result["mir_valid"] = valid.astype("float64")
    result["mir_reason"] = reason
    result["mir_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result


def _hold_all(result: pd.DataFrame, reason: str, params: dict, interval) -> pd.DataFrame:
    n = len(result)
    values = {col: np.full(n, np.nan) for col in VALUE_COLUMNS}
    return _attach(result, values, np.zeros(n, dtype=bool), np.full(n, reason, dtype=object),
                   np.zeros(n, dtype=np.int64), params, interval, np.full(n, np.nan))


def mass_index_reversal_core(
    df: pd.DataFrame,
    ema_period: int = 9,
    sum_period: int = 25,
    upper_threshold: float = 27.0,
    lower_threshold: float = 26.5,
    trend_period: int = 50,
    slope_span: int = 5,
    confirm_lookback: int = 5,
    max_bulge_age: int = 50,
    recursion_length: int = 72,
    bulge_filter: int = 1,
) -> pd.DataFrame:
    params = validate_mir_params(ema_period, sum_period, upper_threshold, lower_threshold, trend_period,
                                 slope_span, confirm_lookback, max_bulge_age, recursion_length, bulge_filter)
    e = params["ema_period"]
    p = params["sum_period"]
    upper = params["upper_threshold"]
    lower = params["lower_threshold"]
    trend = params["trend_period"]
    span = params["slope_span"]
    look = params["confirm_lookback"]
    age_max = params["max_bulge_age"]
    length = params["recursion_length"]
    filtered_mode = params["bulge_filter"] == 1
    need = params["required_history"]

    result = df.copy()
    n = len(result)
    interval, kind = frame_cadence(result)
    if any(c not in result.columns for c in REQUIRED_COLUMNS):
        return _hold_all(result, "missing_columns", params, interval)
    if interval is None:
        return _hold_all(result, "missing_cadence", params, None)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, "missing_timestamps", params, interval)

    open_ = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    volume = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")

    ts_ok = np.isfinite(ts)
    finite = np.isfinite(open_) & np.isfinite(high) & np.isfinite(low) & np.isfinite(close) & np.isfinite(volume)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ((open_ <= 0) | (high <= 0) | (low <= 0) | (close <= 0))
        geometry = finite & ~nonpositive & (
            (high < low) | (open_ > high) | (open_ < low) | (close > high) | (close < low))
        negative_volume = finite & ~nonpositive & ~geometry & (volume < 0)

    row_reason = np.full(n, "", dtype=object)
    row_reason[negative_volume] = "negative_volume"
    row_reason[geometry] = "invalid_candle_geometry"
    row_reason[nonpositive] = "nonpositive_price"
    row_reason[~finite] = "nonfinite_input"
    row_reason[~ts_ok] = "missing_timestamps"
    received = result.attrs.get(FRAME_RECEIVED_ORDER_DEFECTS_ATTR) or ()
    if len(received):
        flagged = np.isin(ts, np.asarray(list(received), dtype="float64"))
        row_reason[flagged & (row_reason == "")] = "timestamp_order"
    row_bad = row_reason != ""

    edge_reason = np.full(n, "", dtype=object)
    if n > 1:
        both = ts_ok[1:] & ts_ok[:-1]
        with np.errstate(invalid="ignore"):
            step = ts[1:] - ts[:-1]
        tail = np.full(n - 1, "", dtype=object)
        with np.errstate(invalid="ignore"):
            tail[both & (step != float(interval))] = "cadence_gap"
            tail[both & (step <= 0)] = "timestamp_order"
        tail[~both] = "missing_timestamps"
        edge_reason[1:] = tail
    edge_bad = edge_reason != ""

    h = np.where(row_bad, np.nan, high)
    lo_ = np.where(row_bad, np.nan, low)
    c = np.where(row_bad, np.nan, close)
    rng = h - lo_

    stages = _ratios(rng, e, length)
    ratio = stages["ratio"]
    mass = _trailing_sum(ratio, p)
    with np.errstate(invalid="ignore", over="ignore"):
        sma = _trailing_sum(c, trend) / float(trend)
    slope = np.full(n, np.nan)
    if n > span:
        with np.errstate(invalid="ignore", over="ignore"):
            slope[span:] = sma[span:] - sma[:-span]
    prev_mass = np.full(n, np.nan)
    if n > 1:
        prev_mass[1:] = mass[:-1]

    both_mass = np.isfinite(mass) & np.isfinite(prev_mass)
    with np.errstate(invalid="ignore"):
        up = both_mass & (prev_mass <= upper) & (mass > upper)
        down = both_mass & (prev_mass >= lower) & (mass < lower)

    last_up = _last_index(up)
    last_down = _last_index(down)
    arm = np.full(n, -1, dtype=np.int64)
    consumed_after = np.full(n, -1, dtype=np.int64)
    if n > 1:
        arm[1:] = last_up[:-1]
        consumed_after[1:] = last_down[:-1]
    t_idx = np.arange(n)
    age = np.where(arm >= 0, t_idx - arm, -1)
    in_age = (arm >= 0) & (age >= 1) & (age <= age_max) & (consumed_after < arm)
    arm_slope = np.full(n, np.nan)
    arm_slope[in_age] = slope[arm[in_age]]
    with np.errstate(invalid="ignore"):
        candidate = in_age & np.isfinite(arm_slope) & (arm_slope != 0.0)
        zero_slope_arm = in_age & np.isfinite(arm_slope) & (arm_slope == 0.0)
    bulge_dir = np.zeros(n, dtype=np.int64)
    bulge_dir[candidate & (arm_slope < 0)] = 1
    bulge_dir[candidate & (arm_slope > 0)] = -1
    completed = candidate & down

    confirm_high = _trailing_extreme(h, look, np.maximum)
    confirm_low = _trailing_extreme(lo_, look, np.minimum)

    tested = np.zeros(n, dtype=np.int64)
    if filtered_mode:
        tested[completed] = bulge_dir[completed]
    else:
        with np.errstate(invalid="ignore"):
            tested[np.isfinite(slope) & (slope < 0)] = 1
            tested[np.isfinite(slope) & (slope > 0)] = -1
    with np.errstate(invalid="ignore"):
        long_ok = (tested == 1) & (c > confirm_high)
        short_ok = (tested == -1) & (c < confirm_low)
    confirmed = long_ok | short_ok

    rows_ok = _window_clear(row_bad, 0, need)
    edges_ok = _window_clear(edge_bad, 0, need - 1)
    ratio_span = age_max + p + 1
    slope_span_rows = age_max + 1
    ratio_bad = ~np.isfinite(ratio)
    nonpos_flag = stages["nonpositive"]
    ratio_ok = _window_clear(ratio_bad, 0, ratio_span)
    slope_ok = _window_clear(~np.isfinite(slope), 0, slope_span_rows)
    window_ok = rows_ok & edges_ok
    if n:
        window_ok[:min(need - 1, n)] = False
    edge_values_ok = (np.isfinite(mass) & np.isfinite(prev_mass) & np.isfinite(c) & np.isfinite(confirm_high)
                      & np.isfinite(confirm_low))
    decision_ok = window_ok & ratio_ok & slope_ok & edge_values_ok

    long_entry = decision_ok & long_ok
    short_entry = decision_ok & short_ok
    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_bulge", dtype=object)
    if filtered_mode:
        reason[candidate & ~completed] = "armed"
        reason[zero_slope_arm & ~candidate] = "zero_slope"
        reason[completed & ~confirmed] = "confirmation_failed"
    else:
        reason[tested == 0] = "zero_slope"
        reason[(tested != 0) & ~confirmed] = "confirmation_failed"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"
    for i in np.flatnonzero(~decision_ok):
        lo = i - need + 1
        found = [hit for hit in (_last_defect(row_reason, lo, i), _last_defect(edge_reason, lo + 1, i))
                 if hit is not None]
        if found:
            reason[i] = max(found, key=lambda hit: hit[0])[1]
        elif i < need - 1:
            reason[i] = "insufficient_history"
        elif nonpos_flag[max(0, i - ratio_span + 1):i + 1].any():
            reason[i] = "nonpositive_average"
        else:
            reason[i] = "nonfinite_derived"

    eval_ts = np.full(n, np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        eval_ts = np.where(ts_ok, ts, np.nan)
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        eval_ts = np.where(ts_ok, ts - float(interval) + 1.0, np.nan)

    keep = decision_ok
    has_arm = keep & (in_age | zero_slope_arm)
    armed_ts = np.full(n, np.nan)
    armed_ts[has_arm] = eval_ts[arm[has_arm]]

    def masked(values, mask=keep):
        return np.where(mask, values, np.nan)

    values = {
        "mir_range": masked(rng),
        "mir_ema1": masked(stages["ema1"]),
        "mir_ema2": masked(stages["ema2"]),
        "mir_ratio": masked(ratio),
        "mir_mass_index": masked(mass),
        "mir_prev_mass_index": masked(prev_mass),
        "mir_trend_sma": masked(sma),
        "mir_trend_slope": masked(slope),
        "mir_upper_cross": masked(up.astype("float64")),
        "mir_lower_cross": masked(down.astype("float64")),
        "mir_bulge_armed_ts_ms": armed_ts,
        "mir_bulge_age": masked(age.astype("float64"), has_arm),
        "mir_bulge_slope": masked(arm_slope, has_arm),
        "mir_bulge_direction": masked(bulge_dir.astype("float64")),
        "mir_bulge_completed": masked(completed.astype("float64")),
        "mir_tested_side": masked(tested.astype("float64")),
        "mir_confirmed": masked(confirmed.astype("float64")),
        "mir_filtered": masked(((tested != 0) & ~confirmed).astype("float64")),
        "mir_close": masked(c),
        "mir_confirm_high": masked(confirm_high),
        "mir_confirm_low": masked(confirm_low),
    }
    return _attach(result, values, decision_ok, reason, signal, params, interval, eval_ts)
