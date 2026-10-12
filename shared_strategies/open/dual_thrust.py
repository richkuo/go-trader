import math
import os
import sys
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

_TOOLS_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "shared_tools"))
if _TOOLS_DIR not in sys.path:
    sys.path.append(_TOOLS_DIR)

from closed_bar import (
    FRAME_INTERVAL_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)

NAME = "dual_thrust"
DAY_MS = 86_400_000
HOUR_MS = 3_600_000
LOOKBACK_MIN = 1
LOOKBACK_MAX = 199
COEFFICIENT_MAX = 2.0
MAX_WINDOW_BARS = 200
DEFAULT_PARAMS = {
    "lookback_sessions": 4,
    "k_long": 0.5,
    "k_short": 0.5,
    "session_start_hour": 0,
    "expected_interval_ms": HOUR_MS,
}
PARAM_NAMES = tuple(DEFAULT_PARAMS)
REQUIRED_COLUMNS = ("open", "high", "low", "close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "inside_range": 0,
    "threshold_equal": 2,
    "long_consumed": 3,
    "short_consumed": 4,
    "insufficient_history": 10,
    "missing_timestamp": 11,
    "duplicate_timestamp": 12,
    "descending_timestamp": 13,
    "cadence_gap": 14,
    "grid_misaligned": 15,
    "nonfinite_price": 16,
    "nonpositive_price": 17,
    "inconsistent_candle": 18,
    "zero_range": 19,
    "nonfinite_derived": 20,
    "missing_columns": 21,
    "missing_timestamps": 22,
    "timing_contradiction": 23,
    "undeclared_timing": 24,
}

ROW_REASONS = ("", "missing_timestamp", "grid_misaligned", "nonfinite_price", "nonpositive_price",
               "inconsistent_candle")
STEP_REASONS = ("", "missing_timestamp", "duplicate_timestamp", "descending_timestamp", "cadence_gap")

NUMERIC_COLUMNS = (
    "dt_eval_ts_ms", "dt_open", "dt_high", "dt_low", "dt_close", "dt_session_open_ms", "dt_bar_in_session",
    "dt_lookback_sessions", "dt_k_long", "dt_k_short", "dt_session_start_hour", "dt_interval_ms",
    "dt_hh", "dt_lc", "dt_hc", "dt_ll", "dt_range", "dt_anchor", "dt_upper", "dt_lower",
    "dt_long_consumed", "dt_short_consumed",
)
RESULT_COLUMNS = NUMERIC_COLUMNS + ("dt_valid", "dt_reason", "dt_reason_code", "signal")


def _integral(name: str, value, low: int, high) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < low or (high is not None and as_int > high):
        bound = f"[{low}, {high}]" if high is not None else f">= {low}"
        raise ValueError(f"{NAME}: {name} must be in {bound}, got {value!r}")
    return as_int


def _coefficient(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not (0.0 < as_float <= COEFFICIENT_MAX):
        raise ValueError(
            f"{NAME}: {name} must be finite, above 0 and at most {COEFFICIENT_MAX:g}, got {value!r}")
    return as_float


def bars_per_session(expected_interval_ms: int) -> int:
    return DAY_MS // int(expected_interval_ms)


def required_window_bars(lookback_sessions: int, expected_interval_ms: int) -> int:
    return (int(lookback_sessions) + 1) * bars_per_session(expected_interval_ms)


def validate_dual_thrust_params(lookback_sessions, k_long, k_short, session_start_hour,
                                expected_interval_ms) -> tuple:
    lookback = _integral("lookback_sessions", lookback_sessions, LOOKBACK_MIN, LOOKBACK_MAX)
    kl = _coefficient("k_long", k_long)
    ks = _coefficient("k_short", k_short)
    hour = _integral("session_start_hour", session_start_hour, 0, 23)
    interval = _integral("expected_interval_ms", expected_interval_ms, 1, DAY_MS)
    if DAY_MS % interval:
        raise ValueError(
            f"{NAME}: expected_interval_ms={interval} must divide a 24-hour UTC day ({DAY_MS} ms) exactly")
    if (hour * HOUR_MS) % interval:
        raise ValueError(
            f"{NAME}: session_start_hour={hour} is not on the {interval} ms candle grid; a candle would "
            f"cross the session boundary")
    span = required_window_bars(lookback, interval)
    if span > MAX_WINDOW_BARS:
        raise ValueError(
            f"{NAME}: (lookback_sessions + 1) x bars per session = ({lookback} + 1) x "
            f"{bars_per_session(interval)} = {span} exceeds the {MAX_WINDOW_BARS}-bar check window")
    return lookback, kl, ks, hour, interval


def validate_dual_thrust_param_dict(params: dict) -> tuple:
    merged = {**DEFAULT_PARAMS, **(params or {})}
    return validate_dual_thrust_params(*(merged[k] for k in PARAM_NAMES))


def _absolute_ms(df: pd.DataFrame):
    if isinstance(df.index, pd.DatetimeIndex):
        idx = df.index
        aware = idx.tz is not None
        if aware:
            idx = idx.tz_convert("UTC").tz_localize(None)
        ns_per_unit = {"s": 1_000_000_000, "ms": 1_000_000, "us": 1_000, "ns": 1}[idx.unit]
        missing = np.asarray(idx.isna())
        raw = np.where(missing, 0, idx.asi8)
        values = ((raw * ns_per_unit) // 1_000_000).astype("float64")
        values[missing] = np.nan
        return values, aware
    if "timestamp" in df.columns:
        return pd.to_numeric(df["timestamp"], errors="coerce").to_numpy(dtype="float64"), True
    return None, False


def _frame_open_ms(df: pd.DataFrame, interval: int):
    declared = df.attrs.get(FRAME_INTERVAL_ATTR)
    if declared is not None:
        if (isinstance(declared, (bool, np.bool_)) or not isinstance(declared, (int, np.integer))
                or int(declared) != interval):
            return None, None, "timing_contradiction"
    kind = df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR)
    if kind is not None and kind not in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE):
        return None, None, "timing_contradiction"
    ms, aware = _absolute_ms(df)
    if ms is None:
        return None, None, "missing_timestamps"
    finite = np.isfinite(ms)
    misaligned = np.zeros(len(ms), dtype=bool)
    open_ms = np.full(len(ms), np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        open_ms[finite] = ms[finite]
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        open_ms[finite] = ms[finite] - float(interval) + 1.0
    elif aware:
        rem = np.zeros(len(ms))
        rem[finite] = np.mod(ms[finite], float(interval))
        as_open = finite & (rem == 0.0)
        as_close = finite & ~as_open & (rem == float(interval - 1))
        open_ms[as_open] = ms[as_open]
        open_ms[as_close] = ms[as_close] - float(interval) + 1.0
        misaligned = finite & ~as_open & ~as_close
        open_ms[misaligned] = ms[misaligned]
    else:
        return None, None, "undeclared_timing"
    with np.errstate(invalid="ignore"):
        misaligned |= finite & (np.mod(open_ms, float(interval)) != 0.0)
    return open_ms, misaligned, None


def _last_flag_index(flags: np.ndarray) -> np.ndarray:
    pos = np.where(flags, np.arange(len(flags)), -1)
    return np.maximum.accumulate(pos) if len(pos) else pos


def _param_columns(result: pd.DataFrame, params: tuple) -> None:
    n = len(result)
    lookback, kl, ks, hour, interval = params
    result["dt_lookback_sessions"] = np.full(n, float(lookback))
    result["dt_k_long"] = np.full(n, kl)
    result["dt_k_short"] = np.full(n, ks)
    result["dt_session_start_hour"] = np.full(n, float(hour))
    result["dt_interval_ms"] = np.full(n, float(interval))


def _hold_all(result: pd.DataFrame, params: tuple, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in NUMERIC_COLUMNS:
        result[col] = np.full(n, np.nan)
    _param_columns(result, params)
    result["dt_valid"] = np.zeros(n, dtype=bool)
    result["dt_reason"] = np.full(n, reason, dtype=object)
    result["dt_reason_code"] = np.full(n, REASON_CODES[reason], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def dual_thrust_core(
    df: pd.DataFrame,
    lookback_sessions: int = 4,
    k_long: float = 0.5,
    k_short: float = 0.5,
    session_start_hour: int = 0,
    expected_interval_ms: int = HOUR_MS,
) -> pd.DataFrame:
    params = validate_dual_thrust_params(lookback_sessions, k_long, k_short, session_start_hour,
                                         expected_interval_ms)
    lookback, kl, ks, hour, interval = params
    per_session = bars_per_session(interval)
    history = lookback * per_session
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _hold_all(result, params, "missing_columns")
    open_ms, misaligned, frame_reason = _frame_open_ms(result, interval)
    if frame_reason:
        return _hold_all(result, params, frame_reason)

    o = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    h = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo_px = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    c = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")

    ts_ok = np.isfinite(open_ms)
    finite = np.isfinite(o) & np.isfinite(h) & np.isfinite(lo_px) & np.isfinite(c)
    with np.errstate(invalid="ignore"):
        positive = finite & (o > 0.0) & (h > 0.0) & (lo_px > 0.0) & (c > 0.0)
        consistent = positive & (h >= lo_px) & (h >= o) & (h >= c) & (lo_px <= o) & (lo_px <= c)

    row_reason = np.zeros(n, dtype=np.int64)
    row_reason[positive & ~consistent] = ROW_REASONS.index("inconsistent_candle")
    row_reason[finite & ~positive] = ROW_REASONS.index("nonpositive_price")
    row_reason[~finite] = ROW_REASONS.index("nonfinite_price")
    row_reason[misaligned] = ROW_REASONS.index("grid_misaligned")
    row_reason[~ts_ok] = ROW_REASONS.index("missing_timestamp")
    row_bad = row_reason > 0

    step_reason = np.zeros(n, dtype=np.int64)
    if n > 1:
        both = ts_ok[1:] & ts_ok[:-1]
        with np.errstate(invalid="ignore"):
            step = open_ms[1:] - open_ms[:-1]
        sr = np.zeros(n - 1, dtype=np.int64)
        sr[both & (step != float(interval))] = STEP_REASONS.index("cadence_gap")
        sr[both & (step < 0)] = STEP_REASONS.index("descending_timestamp")
        sr[both & (step == 0)] = STEP_REASONS.index("duplicate_timestamp")
        sr[~both] = STEP_REASONS.index("missing_timestamp")
        step_reason[1:] = sr
    step_bad = step_reason > 0

    idx = np.arange(n)
    clean = ~row_bad
    open_int = np.where(clean, np.nan_to_num(open_ms, nan=0.0), 0.0).astype(np.int64)
    bar_in_session = np.zeros(n, dtype=np.int64)
    bar_in_session[clean] = ((open_int[clean] - hour * HOUR_MS) % DAY_MS) // interval
    session_open_ms = open_int - bar_in_session * interval
    session_row = idx - bar_in_session
    need_from = session_row - history

    last_row_bad = _last_flag_index(row_bad)
    last_step_bad = _last_flag_index(step_bad)
    structural = clean & (need_from >= 0) & (last_row_bad < need_from) & (last_step_bad <= need_from)

    hh = np.full(n, np.nan)
    ll = np.full(n, np.nan)
    hc = np.full(n, np.nan)
    lc = np.full(n, np.nan)
    if structural.any():
        windows_high = np.max(sliding_window_view(np.where(clean, h, -np.inf), history), axis=1)
        windows_low = np.min(sliding_window_view(np.where(clean, lo_px, np.inf), history), axis=1)
        safe_close = np.where(clean, c, np.nan)
        for k in np.unique(session_row[structural]):
            rows = structural & (session_row == k)
            closes = safe_close[k - 1 - np.arange(lookback) * per_session]
            hh[rows] = windows_high[k - history]
            ll[rows] = windows_low[k - history]
            hc[rows] = np.max(closes)
            lc[rows] = np.min(closes)

    with np.errstate(over="ignore", invalid="ignore"):
        rng = np.maximum(hh - lc, hc - ll)
        anchor = np.where(structural, o[np.clip(session_row, 0, None)], np.nan)
        upper = anchor + kl * rng
        lower = anchor - ks * rng
    derived_finite = np.isfinite(rng) & np.isfinite(upper) & np.isfinite(lower)
    zero_range = structural & derived_finite & (rng == 0.0)
    nonfinite_derived = structural & ~derived_finite
    valid = structural & derived_finite & ~zero_range

    with np.errstate(invalid="ignore"):
        long_hit = valid & (c > upper)
        short_hit = valid & (c < lower)
        equal = valid & ((c == upper) | (c == lower))
    long_cum = np.cumsum(long_hit.astype(np.int64))
    short_cum = np.cumsum(short_hit.astype(np.int64))
    start = np.clip(session_row - 1, 0, None)
    long_base = np.where(session_row > 0, long_cum[start], 0)
    short_base = np.where(session_row > 0, short_cum[start], 0)
    long_count = np.where(valid, long_cum - long_base, 0)
    short_count = np.where(valid, short_cum - short_base, 0)
    long_entry = long_hit & (long_count == 1)
    short_entry = short_hit & (short_count == 1)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "inside_range", dtype=object)
    reason[equal] = "threshold_equal"
    reason[long_hit & ~long_entry] = "long_consumed"
    reason[short_hit & ~short_entry] = "short_consumed"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"
    reason[zero_range] = "zero_range"
    reason[nonfinite_derived] = "nonfinite_derived"

    for i in np.flatnonzero(~structural):
        if row_bad[i]:
            reason[i] = ROW_REASONS[row_reason[i]]
            continue
        lo_row = max(int(need_from[i]), 0)
        r = int(last_row_bad[i])
        s = int(last_step_bad[i])
        r_in = r >= lo_row
        s_in = s > lo_row
        if r_in and (not s_in or r >= s):
            reason[i] = ROW_REASONS[row_reason[r]]
        elif s_in:
            reason[i] = STEP_REASONS[step_reason[s]]
        else:
            reason[i] = "insufficient_history"

    result["dt_eval_ts_ms"] = open_ms
    result["dt_open"] = o
    result["dt_high"] = h
    result["dt_low"] = lo_px
    result["dt_close"] = c
    result["dt_session_open_ms"] = np.where(clean, session_open_ms.astype("float64"), np.nan)
    result["dt_bar_in_session"] = np.where(clean, bar_in_session.astype("float64"), np.nan)
    _param_columns(result, params)
    result["dt_hh"] = hh
    result["dt_lc"] = lc
    result["dt_hc"] = hc
    result["dt_ll"] = ll
    result["dt_range"] = np.where(structural, rng, np.nan)
    result["dt_anchor"] = anchor
    result["dt_upper"] = np.where(structural, upper, np.nan)
    result["dt_lower"] = np.where(structural, lower, np.nan)
    result["dt_long_consumed"] = np.where(valid, (long_count > 0).astype("float64"), np.nan)
    result["dt_short_consumed"] = np.where(valid, (short_count > 0).astype("float64"), np.nan)
    result["dt_valid"] = valid
    result["dt_reason"] = reason
    result["dt_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
