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
    FRAME_DECISION_TIME_ATTR,
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)

NAME = "stochastic_momentum_index"
MAX_ENDPOINT_BARS = 198
RECURSION_PER_PERIOD = 8
THRESHOLD_LIMIT = 100.0
SCALE = 200.0
REQUIRED_COLUMNS = ("open", "high", "low", "close")
CHUNK_ROWS = 4096

DEFAULT_PARAMS = {
    "range_period": 10,
    "smoothing_period": 3,
    "signal_period": 3,
    "recursion_length": 60,
    "oversold": -40.0,
    "overbought": 40.0,
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "level_cross_only": 2,
    "signal_cross_only": 3,
    "insufficient_history": 10,
    "missing_columns": 11,
    "missing_cadence": 12,
    "missing_timestamps": 13,
    "nonfinite_input": 14,
    "nonpositive_price": 15,
    "invalid_candle_geometry": 16,
    "timestamp_order": 18,
    "cadence_gap": 19,
    "zero_range": 20,
    "nonfinite_derived": 21,
    "decision_time_missing": 22,
    "decision_bar_stale": 23,
    "decision_bar_future": 24,
    "decision_boundary_mismatch": 25,
}

VALUE_COLUMNS = (
    "smi_eval_ts_ms", "smi_value", "smi_prev_value", "smi_signal_line", "smi_prev_signal_line", "smi_diff",
    "smi_prev_diff", "smi_relative_smoothed", "smi_range_smoothed", "smi_seed_relative", "smi_seed_range",
    "smi_prev_seed_relative", "smi_prev_seed_range", "smi_decision_cutoff_ms", "smi_decision_boundary_ms",
)

FLAG_COLUMNS = ("smi_value_valid", "smi_prev_valid", "smi_valid")


def _period(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a positive integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer() or as_float < 1:
        raise ValueError(f"{NAME}: {name} must be a positive integer, got {value!r}")
    return int(as_float)


def _threshold(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a finite number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"{NAME}: {name} must be a finite number, got {value!r}")
    return as_float


def validate_smi_params(range_period, smoothing_period, signal_period, recursion_length, oversold,
                        overbought) -> tuple:
    k = _period("range_period", range_period)
    smooth = _period("smoothing_period", smoothing_period)
    signal = _period("signal_period", signal_period)
    length = _period("recursion_length", recursion_length)
    low = _threshold("oversold", oversold)
    high = _threshold("overbought", overbought)
    if not (-THRESHOLD_LIMIT < low < high < THRESHOLD_LIMIT):
        raise ValueError(f"{NAME}: thresholds need -100 < oversold < overbought < 100, "
                         f"got {oversold!r} and {overbought!r}")
    if length < RECURSION_PER_PERIOD * max(smooth, signal):
        raise ValueError(f"{NAME}: recursion_length must be at least {RECURSION_PER_PERIOD} times the larger of "
                         f"smoothing_period and signal_period ({RECURSION_PER_PERIOD * max(smooth, signal)}), "
                         f"got {recursion_length!r}")
    if length + k > MAX_ENDPOINT_BARS:
        raise ValueError(f"{NAME}: recursion_length + range_period must be at most {MAX_ENDPOINT_BARS}, "
                         f"got {length} + {k} = {length + k}")
    return k, smooth, signal, length, low, high


def smi_param_validator(params: dict) -> None:
    params = params or {}
    validate_smi_params(params.get("range_period"), params.get("smoothing_period"), params.get("signal_period"),
                        params.get("recursion_length"), params.get("oversold"), params.get("overbought"))


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


def _window_clear(bad: np.ndarray, length: int) -> np.ndarray:
    n = len(bad)
    out = np.zeros(n, dtype=bool)
    if n < length or length < 1:
        return out
    csum = np.concatenate([[0], np.cumsum(bad.astype(np.int64))])
    ends = np.arange(length - 1, n)
    out[ends] = (csum[ends + 1] - csum[ends + 1 - length]) == 0
    return out


def _last_defect(reasons: np.ndarray, lo: int, hi: int):
    if hi < lo:
        return None
    hits = np.flatnonzero(reasons[lo:hi + 1] != "")
    if len(hits) == 0:
        return None
    pos = lo + int(hits[-1])
    return pos, reasons[pos]


def range_inputs(high: np.ndarray, low: np.ndarray, close: np.ndarray, k: int) -> tuple:
    n = len(close)
    relative = np.full(n, np.nan)
    width = np.full(n, np.nan)
    if n < k:
        return relative, width
    highest = np.lib.stride_tricks.sliding_window_view(high, k).max(axis=1)
    lowest = np.lib.stride_tricks.sliding_window_view(low, k).min(axis=1)
    midpoint = (highest + lowest) / 2.0
    relative[k - 1:] = close[k - 1:] - midpoint
    width[k - 1:] = highest - lowest
    return relative, width


def endpoint_replays(relative: np.ndarray, width: np.ndarray, ends: np.ndarray, smooth: int, signal: int,
                     length: int) -> dict:
    m = len(ends)
    keys = ("value", "signal_line", "relative_smoothed", "range_smoothed", "seed_relative", "seed_range")
    out = {key: np.full(m, np.nan) for key in keys}
    out["zero_range"] = np.zeros(m, dtype=bool)
    out["finite"] = np.zeros(m, dtype=bool)
    if m == 0:
        return out
    alpha = 2.0 / (smooth + 1)
    keep = 1.0 - alpha
    beta = 2.0 / (signal + 1)
    keep_beta = 1.0 - beta
    offsets = np.arange(-length, 1)
    with np.errstate(invalid="ignore", divide="ignore", over="ignore"):
        for start in range(0, m, CHUNK_ROWS):
            rows = ends[start:start + CHUNK_ROWS]
            rel = relative[rows[:, None] + offsets]
            rng = width[rows[:, None] + offsets]
            num1 = rel[:, 0].copy()
            num2 = rel[:, 0].copy()
            den1 = rng[:, 0].copy()
            den2 = rng[:, 0].copy()
            positive = den2 > 0
            osc = SCALE * num2 / den2
            line = osc.copy()
            finite = np.isfinite(num2) & np.isfinite(den2) & np.isfinite(osc)
            for j in range(1, length + 1):
                num1 = alpha * rel[:, j] + keep * num1
                num2 = alpha * num1 + keep * num2
                den1 = alpha * rng[:, j] + keep * den1
                den2 = alpha * den1 + keep * den2
                positive &= den2 > 0
                osc = SCALE * num2 / den2
                line = beta * osc + keep_beta * line
                finite &= np.isfinite(num2) & np.isfinite(den2) & np.isfinite(osc) & np.isfinite(line)
            sl = slice(start, start + len(rows))
            out["value"][sl] = osc
            out["signal_line"][sl] = line
            out["relative_smoothed"][sl] = num2
            out["range_smoothed"][sl] = den2
            out["seed_relative"][sl] = rel[:, 0]
            out["seed_range"][sl] = rng[:, 0]
            out["zero_range"][sl] = ~positive
            out["finite"][sl] = finite
    return out


def _decision_time(attrs: dict):
    if FRAME_DECISION_TIME_ATTR not in attrs:
        return None, None, False
    raw = attrs.get(FRAME_DECISION_TIME_ATTR)
    cutoff = boundary = None
    if isinstance(raw, dict):
        for key in ("cutoff_ms", "boundary_ms"):
            value = raw.get(key)
            if isinstance(value, (int, np.integer)) and not isinstance(value, (bool, np.bool_)) and int(value) > 0:
                if key == "cutoff_ms":
                    cutoff = int(value)
                else:
                    boundary = int(value)
    return cutoff, boundary, True


def _attach_params(result: pd.DataFrame, params: tuple, interval) -> None:
    n = len(result)
    k, smooth, signal, length, low, high = params
    result["smi_interval_ms"] = np.full(n, float(interval) if interval else np.nan)
    result["smi_range_period"] = np.full(n, float(k))
    result["smi_smoothing_period"] = np.full(n, float(smooth))
    result["smi_signal_period"] = np.full(n, float(signal))
    result["smi_recursion_length"] = np.full(n, float(length))
    result["smi_oversold"] = np.full(n, low)
    result["smi_overbought"] = np.full(n, high)
    result["smi_endpoint_bars"] = np.full(n, float(length + k))
    result["smi_required_history"] = np.full(n, float(length + k + 1))


def _hold_all(result: pd.DataFrame, reason: str, params: tuple, interval) -> pd.DataFrame:
    n = len(result)
    for col in VALUE_COLUMNS:
        result[col] = np.full(n, np.nan)
    for col in FLAG_COLUMNS:
        result[col] = np.zeros(n, dtype="float64")
    _attach_params(result, params, interval)
    result["smi_reason"] = np.full(n, reason, dtype=object)
    result["smi_reason_code"] = np.full(n, float(REASON_CODES[reason]))
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def stochastic_momentum_index_core(
    df: pd.DataFrame,
    range_period: int = 10,
    smoothing_period: int = 3,
    signal_period: int = 3,
    recursion_length: int = 60,
    oversold: float = -40.0,
    overbought: float = 40.0,
) -> pd.DataFrame:
    params = validate_smi_params(range_period, smoothing_period, signal_period, recursion_length, oversold,
                                 overbought)
    k, smooth, signal_n, length, low_level, high_level = params
    endpoint_bars = length + k
    required = endpoint_bars + 1
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

    ts_ok = np.isfinite(ts)
    finite = np.isfinite(open_) & np.isfinite(high) & np.isfinite(low) & np.isfinite(close)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ((open_ <= 0) | (high <= 0) | (low <= 0) | (close <= 0))
        geometry = finite & ~nonpositive & (
            (high < low) | (open_ > high) | (open_ < low) | (close > high) | (close < low))

    row_reason = np.full(n, "", dtype=object)
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
        edge_reason[1:] = tail
    edge_bad = edge_reason != ""

    rows_clear = _window_clear(row_bad, endpoint_bars)
    edges_clear = np.zeros(n, dtype=bool)
    if n >= endpoint_bars:
        edges_clear[endpoint_bars - 1:] = _window_clear(edge_bad, endpoint_bars - 1)[endpoint_bars - 1:]
    inputs_ok = rows_clear & edges_clear

    safe_high = np.where(row_bad, 1.0, high)
    safe_low = np.where(row_bad, 1.0, low)
    safe_close = np.where(row_bad, 1.0, close)
    relative, width = range_inputs(safe_high, safe_low, safe_close, k)
    ends = np.flatnonzero(inputs_ok)
    computed = endpoint_replays(relative, width, ends, smooth, signal_n, length)

    def _spread(key, fill=np.nan, dtype="float64"):
        out = np.full(n, fill, dtype=dtype)
        out[ends] = computed[key]
        return out

    value = _spread("value")
    line = _spread("signal_line")
    rel_smoothed = _spread("relative_smoothed")
    rng_smoothed = _spread("range_smoothed")
    seed_rel = _spread("seed_relative")
    seed_rng = _spread("seed_range")
    zero_range = _spread("zero_range", False, bool)
    derived_finite = _spread("finite", False, bool)
    nonfinite_derived = inputs_ok & ~zero_range & ~derived_finite
    value_ok = inputs_ok & ~zero_range & derived_finite
    with np.errstate(invalid="ignore"):
        diff = value - line

    prev_ok = np.zeros(n, dtype=bool)
    prev_value = np.full(n, np.nan)
    prev_line = np.full(n, np.nan)
    prev_diff = np.full(n, np.nan)
    prev_seed_rel = np.full(n, np.nan)
    prev_seed_rng = np.full(n, np.nan)
    if n > 1:
        prev_ok[1:] = value_ok[:-1]
        prev_value[1:] = value[:-1]
        prev_line[1:] = line[:-1]
        prev_diff[1:] = diff[:-1]
        prev_seed_rel[1:] = seed_rel[:-1]
        prev_seed_rng[1:] = seed_rng[:-1]
    decision_ok = value_ok & prev_ok

    with np.errstate(invalid="ignore"):
        level_up = decision_ok & (prev_value <= low_level) & (value > low_level)
        signal_up = decision_ok & (prev_diff <= 0.0) & (diff > 0.0)
        level_down = decision_ok & (prev_value >= high_level) & (value < high_level)
        signal_down = decision_ok & (prev_diff >= 0.0) & (diff < 0.0)
    long_entry = level_up & signal_up
    short_entry = level_down & signal_down

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1
    reason = np.full(n, "no_cross", dtype=object)
    reason[(signal_up & ~level_up) | (signal_down & ~level_down)] = "signal_cross_only"
    reason[(level_up & ~signal_up) | (level_down & ~signal_down)] = "level_cross_only"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    for i in np.flatnonzero(~decision_ok):
        lo = max(0, i - required + 1)
        found = [pr for pr in (_last_defect(row_reason, lo, i), _last_defect(edge_reason, max(1, lo + 1), i))
                 if pr is not None]
        if found:
            reason[i] = max(found, key=lambda pr: pr[0])[1]
        elif i < required - 1:
            reason[i] = "insufficient_history"
        elif nonfinite_derived[i] or (i > 0 and nonfinite_derived[i - 1]):
            reason[i] = "nonfinite_derived"
        else:
            reason[i] = "zero_range"

    cutoff_col = np.full(n, np.nan)
    boundary_col = np.full(n, np.nan)
    cutoff, declared_boundary, runtime = _decision_time(result.attrs)
    if runtime and n > 0:
        i = n - 1
        if cutoff is not None:
            cutoff_col[i] = float(cutoff)
        if declared_boundary is not None:
            boundary_col[i] = float(declared_boundary)
        if decision_ok[i]:
            row_boundary = None
            if kind == TIMESTAMP_KIND_OPEN:
                row_boundary = int(ts[i]) + interval
            elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
                row_boundary = int(ts[i]) + 1
            held = ""
            if cutoff is None or declared_boundary is None or row_boundary is None:
                held = "decision_time_missing"
            elif row_boundary != declared_boundary:
                held = "decision_boundary_mismatch"
            elif cutoff - row_boundary < 0:
                held = "decision_bar_future"
            elif cutoff - row_boundary >= interval:
                held = "decision_bar_stale"
            if held:
                decision_ok[i] = False
                signal[i] = 0
                reason[i] = held

    eval_ts = np.full(n, np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        eval_ts = np.where(ts_ok, ts, np.nan)
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        eval_ts = np.where(ts_ok, ts - float(interval) + 1.0, np.nan)

    result["smi_eval_ts_ms"] = eval_ts
    result["smi_value"] = np.where(value_ok, value, np.nan)
    result["smi_prev_value"] = np.where(prev_ok, prev_value, np.nan)
    result["smi_signal_line"] = np.where(value_ok, line, np.nan)
    result["smi_prev_signal_line"] = np.where(prev_ok, prev_line, np.nan)
    result["smi_diff"] = np.where(value_ok, diff, np.nan)
    result["smi_prev_diff"] = np.where(prev_ok, prev_diff, np.nan)
    result["smi_relative_smoothed"] = np.where(value_ok, rel_smoothed, np.nan)
    result["smi_range_smoothed"] = np.where(value_ok, rng_smoothed, np.nan)
    result["smi_seed_relative"] = np.where(value_ok, seed_rel, np.nan)
    result["smi_seed_range"] = np.where(value_ok, seed_rng, np.nan)
    result["smi_prev_seed_relative"] = np.where(prev_ok, prev_seed_rel, np.nan)
    result["smi_prev_seed_range"] = np.where(prev_ok, prev_seed_rng, np.nan)
    result["smi_decision_cutoff_ms"] = cutoff_col
    result["smi_decision_boundary_ms"] = boundary_col
    result["smi_value_valid"] = value_ok.astype("float64")
    result["smi_prev_valid"] = prev_ok.astype("float64")
    result["smi_valid"] = decision_ok.astype("float64")
    _attach_params(result, params, interval)
    result["smi_reason"] = reason
    result["smi_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result
