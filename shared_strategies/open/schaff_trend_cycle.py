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

NAME = "schaff_trend_cycle"
FAST_MIN, FAST_MAX = 2, 32
SLOW_MIN, SLOW_MAX = 3, 64
CYCLE_MIN, CYCLE_MAX = 2, 16
SMOOTHING_MIN, SMOOTHING_MAX = 0.25, 1.0
REPLAY_BARS = 192
REQUIRED_HISTORY = REPLAY_BARS + 1
REQUIRED_COLUMNS = ("open", "high", "low", "close", "volume")
CHUNK_ROWS = 4096

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "slope_disagrees": 2,
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
    "zero_range": 20,
    "nonfinite_derived": 21,
    "decision_time_missing": 22,
    "decision_bar_stale": 23,
    "decision_bar_future": 24,
    "decision_boundary_mismatch": 25,
}

VALUE_COLUMNS = (
    "stc_eval_ts_ms", "stc_value", "stc_prev_value", "stc_macd", "stc_prev_macd", "stc_macd_slope",
    "stc_stoch1_raw", "stc_stoch1", "stc_stoch2_raw", "stc_decision_cutoff_ms", "stc_decision_boundary_ms",
)

FLAG_COLUMNS = ("stc_value_valid", "stc_prev_valid", "stc_valid")


def _period(name: str, value, lo: int, hi: int) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{NAME}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def _number(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"{NAME}: {name} must be finite, got {value!r}")
    return as_float


def validate_stc_params(fast_period, slow_period, cycle_length, smoothing, lower_threshold,
                        upper_threshold) -> tuple:
    fast = _period("fast_period", fast_period, FAST_MIN, FAST_MAX)
    slow = _period("slow_period", slow_period, SLOW_MIN, SLOW_MAX)
    if fast >= slow:
        raise ValueError(f"{NAME}: fast_period must be below slow_period, got {fast} and {slow}")
    cycle = _period("cycle_length", cycle_length, CYCLE_MIN, CYCLE_MAX)
    smooth = _number("smoothing", smoothing)
    if smooth < SMOOTHING_MIN or smooth > SMOOTHING_MAX:
        raise ValueError(f"{NAME}: smoothing must be in [{SMOOTHING_MIN}, {SMOOTHING_MAX}], got {smoothing!r}")
    lower = _number("lower_threshold", lower_threshold)
    upper = _number("upper_threshold", upper_threshold)
    if not (0.0 < lower < upper < 100.0):
        raise ValueError(f"{NAME}: thresholds need 0 < lower_threshold < upper_threshold < 100, "
                         f"got {lower_threshold!r} and {upper_threshold!r}")
    return fast, slow, cycle, smooth, lower, upper


def stc_param_validator(params: dict) -> None:
    params = params or {}
    validate_stc_params(params.get("fast_period"), params.get("slow_period"), params.get("cycle_length"),
                        params.get("smoothing"), params.get("lower_threshold"), params.get("upper_threshold"))


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
    if n < length:
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


def _endpoints(close: np.ndarray, ends: np.ndarray, fast: int, slow: int, cycle: int, smooth: float) -> dict:
    m = len(ends)
    out = {key: np.full(m, np.nan) for key in ("macd", "stoch1_raw", "stoch1", "stoch2_raw", "value")}
    out["zero_range"] = np.zeros(m, dtype=bool)
    if m == 0:
        return out
    alpha_fast = 2.0 / (fast + 1)
    alpha_slow = 2.0 / (slow + 1)
    keep_fast = 1.0 - alpha_fast
    keep_slow = 1.0 - alpha_slow
    last = REPLAY_BARS - 1
    j0 = slow - 1
    j1 = j0 + cycle - 1
    j2 = j1 + cycle - 1
    offsets = np.arange(-last, 1)
    for start in range(0, m, CHUNK_ROWS):
        rows = ends[start:start + CHUNK_ROWS]
        window = close[rows[:, None] + offsets]
        k = len(rows)
        ema_fast = window[:, 0].copy()
        ema_slow = window[:, 0].copy()
        macd = np.empty((k, REPLAY_BARS))
        macd[:, 0] = ema_fast - ema_slow
        for j in range(1, REPLAY_BARS):
            ema_fast = alpha_fast * window[:, j] + keep_fast * ema_fast
            ema_slow = alpha_slow * window[:, j] + keep_slow * ema_slow
            macd[:, j] = ema_fast - ema_slow
        zero = np.zeros(k, dtype=bool)
        stage1 = np.empty((k, REPLAY_BARS))
        raw1 = np.zeros(k)
        with np.errstate(invalid="ignore", divide="ignore", over="ignore"):
            for j in range(j1, REPLAY_BARS):
                span = macd[:, j - cycle + 1:j + 1]
                lo = span.min(axis=1)
                width = span.max(axis=1) - lo
                flat = ~(width > 0)
                zero |= flat
                raw1 = np.where(flat, 0.0, 100.0 * (macd[:, j] - lo) / np.where(flat, 1.0, width))
                if j == j1:
                    stage1[:, j] = raw1
                else:
                    stage1[:, j] = stage1[:, j - 1] + smooth * (raw1 - stage1[:, j - 1])
            raw2 = np.zeros(k)
            final = np.zeros(k)
            for j in range(j2, REPLAY_BARS):
                span = stage1[:, j - cycle + 1:j + 1]
                lo = span.min(axis=1)
                width = span.max(axis=1) - lo
                flat = ~(width > 0)
                zero |= flat
                raw2 = np.where(flat, 0.0, 100.0 * (stage1[:, j] - lo) / np.where(flat, 1.0, width))
                if j == j2:
                    final = raw2
                else:
                    final = final + smooth * (raw2 - final)
        sl = slice(start, start + k)
        out["macd"][sl] = macd[:, last]
        out["stoch1_raw"][sl] = raw1
        out["stoch1"][sl] = stage1[:, last]
        out["stoch2_raw"][sl] = raw2
        out["value"][sl] = final
        out["zero_range"][sl] = zero
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


def _hold_all(result: pd.DataFrame, reason: str, params: tuple, interval) -> pd.DataFrame:
    n = len(result)
    for col in VALUE_COLUMNS:
        result[col] = np.full(n, np.nan)
    for col in FLAG_COLUMNS:
        result[col] = np.zeros(n, dtype="float64")
    _attach_params(result, params, interval)
    result["stc_reason"] = np.full(n, reason, dtype=object)
    result["stc_reason_code"] = np.full(n, float(REASON_CODES[reason]))
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def _attach_params(result: pd.DataFrame, params: tuple, interval) -> None:
    n = len(result)
    fast, slow, cycle, smooth, lower, upper = params
    result["stc_interval_ms"] = np.full(n, float(interval) if interval else np.nan)
    result["stc_fast_period"] = np.full(n, float(fast))
    result["stc_slow_period"] = np.full(n, float(slow))
    result["stc_cycle_length"] = np.full(n, float(cycle))
    result["stc_smoothing"] = np.full(n, smooth)
    result["stc_lower_threshold"] = np.full(n, lower)
    result["stc_upper_threshold"] = np.full(n, upper)
    result["stc_replay_bars"] = np.full(n, float(REPLAY_BARS))
    result["stc_required_history"] = np.full(n, float(REQUIRED_HISTORY))


def schaff_trend_cycle_core(
    df: pd.DataFrame,
    fast_period: int = 23,
    slow_period: int = 50,
    cycle_length: int = 10,
    smoothing: float = 0.5,
    lower_threshold: float = 25.0,
    upper_threshold: float = 75.0,
) -> pd.DataFrame:
    params = validate_stc_params(fast_period, slow_period, cycle_length, smoothing, lower_threshold,
                                 upper_threshold)
    fast, slow, cycle, smooth, lower, upper = params
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
        edge_reason[1:] = tail
    edge_bad = edge_reason != ""

    rows_clear = _window_clear(row_bad, REPLAY_BARS)
    edges_clear = np.zeros(n, dtype=bool)
    if n >= REPLAY_BARS:
        edges_clear[REPLAY_BARS - 1:] = _window_clear(edge_bad, REPLAY_BARS - 1)[REPLAY_BARS - 1:]
    inputs_ok = rows_clear & edges_clear

    ends = np.flatnonzero(inputs_ok)
    safe_close = np.where(row_bad, 1.0, close)
    computed = _endpoints(safe_close, ends, fast, slow, cycle, smooth)
    stc = np.full(n, np.nan)
    macd = np.full(n, np.nan)
    stoch1_raw = np.full(n, np.nan)
    stoch1 = np.full(n, np.nan)
    stoch2_raw = np.full(n, np.nan)
    zero_range = np.zeros(n, dtype=bool)
    stc[ends] = computed["value"]
    macd[ends] = computed["macd"]
    stoch1_raw[ends] = computed["stoch1_raw"]
    stoch1[ends] = computed["stoch1"]
    stoch2_raw[ends] = computed["stoch2_raw"]
    zero_range[ends] = computed["zero_range"]
    derived_finite = (np.isfinite(stc) & np.isfinite(macd) & np.isfinite(stoch1_raw) & np.isfinite(stoch1)
                      & np.isfinite(stoch2_raw))
    nonfinite_derived = inputs_ok & ~zero_range & ~derived_finite
    value_ok = inputs_ok & ~zero_range & derived_finite

    prev_ok = np.zeros(n, dtype=bool)
    prev_stc = np.full(n, np.nan)
    prev_macd = np.full(n, np.nan)
    if n > 1:
        prev_ok[1:] = value_ok[:-1]
        prev_stc[1:] = stc[:-1]
        prev_macd[1:] = macd[:-1]
    decision_ok = value_ok & prev_ok

    with np.errstate(invalid="ignore"):
        slope = macd - prev_macd
        crossed_up = decision_ok & (prev_stc <= lower) & (stc > lower)
        crossed_down = decision_ok & (prev_stc >= upper) & (stc < upper)
        long_entry = crossed_up & (macd > prev_macd)
        short_entry = crossed_down & (macd < prev_macd)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1
    reason = np.full(n, "no_cross", dtype=object)
    reason[(crossed_up & ~long_entry) | (crossed_down & ~short_entry)] = "slope_disagrees"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    for i in np.flatnonzero(~decision_ok):
        lo = max(0, i - REQUIRED_HISTORY + 1)
        found = [pr for pr in (_last_defect(row_reason, lo, i), _last_defect(edge_reason, max(1, lo + 1), i))
                 if pr is not None]
        if found:
            reason[i] = max(found, key=lambda pr: pr[0])[1]
        elif i < REQUIRED_HISTORY - 1:
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

    result["stc_eval_ts_ms"] = eval_ts
    result["stc_value"] = np.where(value_ok, stc, np.nan)
    result["stc_prev_value"] = np.where(prev_ok, prev_stc, np.nan)
    result["stc_macd"] = np.where(value_ok, macd, np.nan)
    result["stc_prev_macd"] = np.where(prev_ok, prev_macd, np.nan)
    result["stc_macd_slope"] = np.where(value_ok & prev_ok, slope, np.nan)
    result["stc_stoch1_raw"] = np.where(value_ok, stoch1_raw, np.nan)
    result["stc_stoch1"] = np.where(value_ok, stoch1, np.nan)
    result["stc_stoch2_raw"] = np.where(value_ok, stoch2_raw, np.nan)
    result["stc_decision_cutoff_ms"] = cutoff_col
    result["stc_decision_boundary_ms"] = boundary_col
    result["stc_value_valid"] = value_ok.astype("float64")
    result["stc_prev_valid"] = prev_ok.astype("float64")
    result["stc_valid"] = decision_ok.astype("float64")
    _attach_params(result, params, interval)
    result["stc_reason"] = reason
    result["stc_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result
