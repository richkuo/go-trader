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
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)

NAME = "adaptive_moving_average"
REQUIRED_COLUMNS = ("open", "high", "low", "close", "volume")
MAX_REQUIRED_HISTORY = 199

PARAM_DOMAINS = {
    "efficiency_period": (1, 64),
    "fast_period": (1, 64),
    "slow_period": (2, 256),
    "slope_span": (1, 64),
    "recursion_length": (1, 197),
}

DEFAULT_PARAMS = {
    "efficiency_period": 10,
    "fast_period": 2,
    "slow_period": 30,
    "slope_span": 3,
    "recursion_length": 120,
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "slope_disagrees": 2,
    "insufficient_history": 10,
    "missing_cadence": 11,
    "cadence_gap": 12,
    "timestamp_order": 13,
    "missing_timestamps": 14,
    "nonfinite_input": 15,
    "nonpositive_price": 16,
    "invalid_candle_geometry": 17,
    "invalid_volume": 18,
    "nonfinite_derived": 19,
    "missing_columns": 20,
}

VALUE_COLUMNS = (
    "ama_eval_ts_ms",
    "ama_close",
    "ama_prev_close",
    "ama_seed_close",
    "ama_efficiency",
    "ama_smoothing",
    "ama_value",
    "ama_prev_value",
    "ama_slope_ref",
    "ama_slope",
)


def _validate_int(name: str, value) -> int:
    lo, hi = PARAM_DOMAINS[name]
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{NAME}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def required_history_bars(efficiency_period: int, recursion_length: int, slope_span: int) -> int:
    return int(recursion_length) + int(efficiency_period) + max(int(slope_span), 1)


def validate_ama_params(efficiency_period, fast_period, slow_period, slope_span, recursion_length) -> tuple:
    e = _validate_int("efficiency_period", efficiency_period)
    f = _validate_int("fast_period", fast_period)
    s = _validate_int("slow_period", slow_period)
    span = _validate_int("slope_span", slope_span)
    length = _validate_int("recursion_length", recursion_length)
    if f >= s:
        raise ValueError(f"{NAME}: fast_period must be less than slow_period, got {f} >= {s}")
    need = required_history_bars(e, length, span)
    if need > MAX_REQUIRED_HISTORY:
        raise ValueError(
            f"{NAME}: recursion_length + efficiency_period + max(slope_span, 1) = {need} rows exceeds "
            f"{MAX_REQUIRED_HISTORY}, the closed portion of a 200-row check frame")
    return e, f, s, span, length


def ama_param_validator(params: dict) -> None:
    merged = {**DEFAULT_PARAMS, **(params or {})}
    validate_ama_params(merged["efficiency_period"], merged["fast_period"], merged["slow_period"],
                        merged["slope_span"], merged["recursion_length"])


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


def _attach(result: pd.DataFrame, values: dict, valid: np.ndarray, reason: np.ndarray, signal: np.ndarray,
            params: tuple, interval) -> pd.DataFrame:
    n = len(result)
    e, f, s, span, length = params
    for col in VALUE_COLUMNS:
        result[col] = values[col]
    result["ama_interval_ms"] = np.full(n, float(interval) if interval is not None else np.nan)
    result["ama_efficiency_period"] = np.full(n, float(e))
    result["ama_fast_period"] = np.full(n, float(f))
    result["ama_slow_period"] = np.full(n, float(s))
    result["ama_slope_span"] = np.full(n, float(span))
    result["ama_recursion_length"] = np.full(n, float(length))
    result["ama_required_history"] = np.full(n, float(required_history_bars(e, length, span)))
    result["ama_valid"] = valid.astype("float64")
    result["ama_reason"] = reason
    result["ama_reason_code"] = np.array([float(REASON_CODES[r.split(":")[0]]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result


def _hold_all(result: pd.DataFrame, reason: str, params: tuple, interval) -> pd.DataFrame:
    n = len(result)
    values = {col: np.full(n, np.nan) for col in VALUE_COLUMNS}
    return _attach(result, values, np.zeros(n, dtype=bool), np.full(n, reason, dtype=object),
                   np.zeros(n, dtype=np.int64), params, interval)


def adaptive_moving_average_core(
    df: pd.DataFrame,
    efficiency_period: int = 10,
    fast_period: int = 2,
    slow_period: int = 30,
    slope_span: int = 3,
    recursion_length: int = 120,
) -> pd.DataFrame:
    params = validate_ama_params(efficiency_period, fast_period, slow_period, slope_span, recursion_length)
    e, f, s, span, length = params
    result = df.copy()
    n = len(result)
    interval, kind = frame_cadence(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _hold_all(result, "missing_columns:" + ",".join(missing), params, interval)
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
        bad_volume = finite & ~nonpositive & ~geometry & (volume < 0)

    row_reason = np.full(n, "", dtype=object)
    row_reason[bad_volume] = "invalid_volume"
    row_reason[geometry] = "invalid_candle_geometry"
    row_reason[nonpositive] = "nonpositive_price"
    row_reason[~finite] = "nonfinite_input"
    row_reason[~ts_ok] = "missing_timestamps"
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

    c = np.where(row_bad, np.nan, close)
    fast_sc = 2.0 / (f + 1.0)
    slow_sc = 2.0 / (s + 1.0)
    change = np.full(n, np.nan)
    noise = np.full(n, np.nan)
    efficiency = np.full(n, np.nan)
    smoothing = np.full(n, np.nan)
    with np.errstate(over="ignore", invalid="ignore", divide="ignore"):
        absdiff = np.full(n, np.nan)
        if n > 1:
            absdiff[1:] = np.abs(c[1:] - c[:-1])
        if n > e:
            k = np.arange(e, n)
            change[k] = np.abs(c[k] - c[k - e])
            total = absdiff[k - e + 1].copy()
            for j in range(e - 2, -1, -1):
                total = total + absdiff[k - j]
            noise[k] = total
            ratio = np.where(total > 0.0, change[k] / np.where(total > 0.0, total, 1.0), 0.0)
            ratio = np.where(np.isfinite(total) & np.isfinite(change[k]), ratio, np.nan)
            efficiency[k] = ratio
            root = ratio * (fast_sc - slow_sc) + slow_sc
            smoothing[k] = root * root

    derived_reason = np.full(n, "", dtype=object)
    inputs_clean = np.zeros(n, dtype=bool)
    if n > e:
        inputs_clean[e:] = _window_clear(row_bad, 0, e + 1)[e:] & _window_clear(edge_bad, 0, e)[e:]
    derived_bad_k = inputs_clean & ~(np.isfinite(change) & np.isfinite(noise) & np.isfinite(efficiency)
                                     & np.isfinite(smoothing))
    derived_reason[derived_bad_k] = "nonfinite_derived"

    ama = np.full(n, np.nan)
    first_value = length + e - 1
    if n > first_value:
        t = np.arange(first_value, n)
        with np.errstate(over="ignore", invalid="ignore"):
            a = c[t - length].copy()
            for j in range(1, length + 1):
                kk = t - length + j
                a = a + smoothing[kk] * (c[kk] - a)
        ama[t] = a

    need = required_history_bars(e, length, span)
    prev_ama = np.full(n, np.nan)
    ref_ama = np.full(n, np.nan)
    prev_close = np.full(n, np.nan)
    seed_close = np.full(n, np.nan)
    if n > 1:
        prev_ama[1:] = ama[:-1]
        prev_close[1:] = c[:-1]
    if n > span:
        ref_ama[span:] = ama[:-span]
    if n > length:
        seed_close[length:] = c[:-length]
    with np.errstate(over="ignore", invalid="ignore"):
        slope = ama - ref_ama

    rows_ok = _window_clear(row_bad, 0, need)
    edges_ok = _window_clear(edge_bad, 0, need - 1)
    derived_ok = _window_clear(derived_bad_k, 0, span + length)
    values_finite = (np.isfinite(ama) & np.isfinite(prev_ama) & np.isfinite(ref_ama) & np.isfinite(slope)
                     & np.isfinite(c) & np.isfinite(prev_close))
    window_ok = rows_ok & edges_ok
    if n:
        window_ok[:min(need - 1, n)] = False
    decision_ok = window_ok & derived_ok & values_finite

    with np.errstate(invalid="ignore"):
        cross_up = decision_ok & (c > ama) & (prev_close <= prev_ama)
        cross_down = decision_ok & (c < ama) & (prev_close >= prev_ama)
        long_entry = cross_up & (slope > 0.0)
        short_entry = cross_down & (slope < 0.0)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[(cross_up & ~long_entry) | (cross_down & ~short_entry)] = "slope_disagrees"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"
    for i in np.flatnonzero(~decision_ok):
        lo = i - need + 1
        found = [hit for hit in (
            _last_defect(row_reason, lo, i),
            _last_defect(edge_reason, lo + 1, i),
            _last_defect(derived_reason, i - span - length + 1, i),
        ) if hit is not None]
        if found:
            reason[i] = max(found, key=lambda hit: hit[0])[1]
        elif i < need - 1:
            reason[i] = "insufficient_history"
        else:
            reason[i] = "nonfinite_derived"

    eval_ts = np.full(n, np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        eval_ts = np.where(ts_ok, ts, np.nan)
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        eval_ts = np.where(ts_ok, ts - float(interval) + 1.0, np.nan)

    keep = decision_ok
    values = {
        "ama_eval_ts_ms": eval_ts,
        "ama_close": np.where(keep, c, np.nan),
        "ama_prev_close": np.where(keep, prev_close, np.nan),
        "ama_seed_close": np.where(keep, seed_close, np.nan),
        "ama_efficiency": np.where(keep, efficiency, np.nan),
        "ama_smoothing": np.where(keep, smoothing, np.nan),
        "ama_value": np.where(keep, ama, np.nan),
        "ama_prev_value": np.where(keep, prev_ama, np.nan),
        "ama_slope_ref": np.where(keep, ref_ama, np.nan),
        "ama_slope": np.where(keep, slope, np.nan),
    }
    return _attach(result, values, decision_ok, reason, signal, params, interval)
