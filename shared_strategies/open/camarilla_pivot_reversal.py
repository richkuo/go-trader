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

NAME = "camarilla_pivot_reversal"
DAY_MS = 86_400_000
HOUR_MS = 3_600_000
LEVEL_COEFFICIENT = 1.1
INNER_DIVISOR = 4.0
OUTER_DIVISOR = 2.0
SUPPORTED_INTERVALS_MS = {
    900_000: "15m",
    1_800_000: "30m",
    3_600_000: "1h",
    7_200_000: "2h",
    14_400_000: "4h",
    28_800_000: "8h",
    43_200_000: "12h",
}
DEFAULT_PARAMS = {"expected_interval_ms": HOUR_MS}
REQUIRED_COLUMNS = ("open", "high", "low", "close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "level_equal": 2,
    "long_opposite_jump": 3,
    "short_opposite_jump": 4,
    "long_outer_breach": 5,
    "short_outer_breach": 6,
    "first_bar_of_day": 7,
    "insufficient_history": 10,
    "missing_timestamp": 11,
    "duplicate_timestamp": 12,
    "descending_timestamp": 13,
    "received_out_of_order": 14,
    "cadence_gap": 15,
    "grid_misaligned": 16,
    "nonfinite_price": 17,
    "nonpositive_price": 18,
    "inconsistent_candle": 19,
    "prior_day_missing": 20,
    "prior_day_incomplete": 21,
    "zero_range": 22,
    "nonfinite_derived": 23,
    "missing_columns": 24,
    "missing_timestamps": 25,
    "undeclared_timing": 26,
    "timing_contradiction": 27,
    "decision_time_missing": 28,
    "decision_bar_stale": 29,
    "decision_bar_future": 30,
    "decision_boundary_mismatch": 31,
}

ROW_REASONS = ("", "missing_timestamp", "grid_misaligned", "received_out_of_order", "nonfinite_price",
               "nonpositive_price", "inconsistent_candle")
EDGE_REASONS = ("", "missing_timestamp", "duplicate_timestamp", "descending_timestamp", "cadence_gap")
ORDER_ROW_REASONS = ("missing_timestamp", "grid_misaligned", "received_out_of_order")
ORDER_EDGE_REASONS = ("missing_timestamp", "duplicate_timestamp", "descending_timestamp")

VALUE_COLUMNS = (
    "cpr_eval_ts_ms", "cpr_prev_ts_ms", "cpr_day_open_ms", "cpr_prior_day_open_ms", "cpr_bar_in_day",
    "cpr_prior_high", "cpr_prior_low", "cpr_prior_close", "cpr_range", "cpr_s3", "cpr_r3", "cpr_s4", "cpr_r4",
    "cpr_prev_close", "cpr_close", "cpr_prev_low", "cpr_low", "cpr_prev_high", "cpr_high",
    "cpr_decision_cutoff_ms", "cpr_decision_boundary_ms",
)


def validate_camarilla_params(expected_interval_ms) -> int:
    value = expected_interval_ms
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: expected_interval_ms must be an integer number of milliseconds, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: expected_interval_ms must be an integer number of milliseconds, got {value!r}")
    as_int = int(as_float)
    if as_int not in SUPPORTED_INTERVALS_MS:
        supported = ", ".join(f"{ms} ({tf})" for ms, tf in SUPPORTED_INTERVALS_MS.items())
        raise ValueError(f"{NAME}: expected_interval_ms={value!r} is not a supported interval; supported: {supported}")
    return as_int


def validate_camarilla_param_dict(params: dict) -> int:
    merged = {**DEFAULT_PARAMS, **(params or {})}
    return validate_camarilla_params(merged["expected_interval_ms"])


def camarilla_levels(high: float, low: float, close: float) -> dict:
    rng = high - low
    inner = LEVEL_COEFFICIENT * rng / INNER_DIVISOR
    outer = LEVEL_COEFFICIENT * rng / OUTER_DIVISOR
    return {"range": rng, "s3": close - inner, "r3": close + inner, "s4": close - outer, "r4": close + outer}


def _absolute_ms(df: pd.DataFrame):
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


def _frame_timing(df: pd.DataFrame, interval: int):
    declared = df.attrs.get(FRAME_INTERVAL_ATTR)
    kind = df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR)
    if declared is None or kind is None:
        return None, "undeclared_timing"
    if (isinstance(declared, (bool, np.bool_)) or not isinstance(declared, (int, np.integer))
            or int(declared) != interval):
        return None, "timing_contradiction"
    if kind not in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE):
        return None, "timing_contradiction"
    ms = _absolute_ms(df)
    if ms is None:
        return None, "missing_timestamps"
    open_ms = ms.copy()
    if kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        open_ms = ms - float(interval) + 1.0
    return open_ms, None


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


def _last_flag_index(flags: np.ndarray) -> np.ndarray:
    pos = np.where(flags, np.arange(len(flags)), -1)
    return np.maximum.accumulate(pos) if len(pos) else pos


def _hold_all(result: pd.DataFrame, interval: int, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in VALUE_COLUMNS:
        result[col] = np.full(n, np.nan)
    result["cpr_interval_ms"] = np.full(n, float(interval))
    result["cpr_valid"] = np.zeros(n, dtype="float64")
    result["cpr_reason"] = np.full(n, reason, dtype=object)
    result["cpr_reason_code"] = np.full(n, float(REASON_CODES[reason]))
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def _hold_reason(i, row_bad, row_reason, edge_reason, timed, day, day_any, bar_in_day, seg_id, seg_first,
                 seg_count, seg_day, seg_bad_row, seg_last_bad, seg_clean_start, per_day) -> str:
    if row_bad[i]:
        return ROW_REASONS[row_reason[i]]
    if i == 0:
        return "insufficient_history"
    if edge_reason[i]:
        return EDGE_REASONS[edge_reason[i]]
    if not timed[i - 1]:
        return ROW_REASONS[row_reason[i - 1]]
    if day[i] != day[i - 1]:
        return "first_bar_of_day"
    if row_bad[i - 1]:
        return ROW_REASONS[row_reason[i - 1]]
    ki = int(seg_id[i])
    if int(seg_first[ki]) > i - 1:
        return "first_bar_of_day"
    if not seg_clean_start[ki]:
        first = int(seg_first[ki])
        return EDGE_REASONS[edge_reason[first]] or ROW_REASONS[row_reason[first]] or "prior_day_missing"
    if ki == 0:
        return "insufficient_history"
    pi = ki - 1
    first = int(seg_first[pi])
    if seg_day[pi] < 0:
        return ROW_REASONS[row_reason[first]] or "prior_day_missing"
    if seg_day[pi] != day[i] - 1:
        return "prior_day_missing"
    if seg_bad_row[pi]:
        return ROW_REASONS[row_reason[int(seg_last_bad[pi])]]
    if not seg_clean_start[pi]:
        return EDGE_REASONS[edge_reason[first]] or "prior_day_incomplete"
    if seg_count[pi] != per_day:
        before = first - 1
        if before >= 0 and row_bad[before] and day_any[before] == day[i] - 1:
            return ROW_REASONS[row_reason[before]]
        if before < 0 and bar_in_day[first] > 0:
            return "insufficient_history"
        return "prior_day_incomplete"
    return "prior_day_incomplete"


def camarilla_pivot_reversal_core(df: pd.DataFrame, expected_interval_ms: int = HOUR_MS) -> pd.DataFrame:
    interval = validate_camarilla_params(expected_interval_ms)
    per_day = DAY_MS // interval
    result = df.copy()
    n = len(result)

    if any(c not in result.columns for c in REQUIRED_COLUMNS):
        return _hold_all(result, interval, "missing_columns")
    open_ms, frame_reason = _frame_timing(result, interval)
    if frame_reason:
        return _hold_all(result, interval, frame_reason)
    if n == 0:
        return _hold_all(result, interval, "insufficient_history")

    o = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    h = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    c = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")

    ts_ok = np.isfinite(open_ms)
    aligned = np.zeros(n, dtype=bool)
    aligned[ts_ok] = np.mod(open_ms[ts_ok], float(interval)) == 0.0
    received = result.attrs.get(FRAME_RECEIVED_ORDER_DEFECTS_ATTR) or ()
    raw_ms = _absolute_ms(result)
    flagged = np.zeros(n, dtype=bool)
    if len(received) and raw_ms is not None:
        flagged = np.isin(raw_ms, np.asarray(list(received), dtype="float64"))

    finite = np.isfinite(o) & np.isfinite(h) & np.isfinite(lo) & np.isfinite(c)
    with np.errstate(invalid="ignore"):
        positive = finite & (o > 0.0) & (h > 0.0) & (lo > 0.0) & (c > 0.0)
        consistent = positive & (h >= lo) & (h >= o) & (h >= c) & (lo <= o) & (lo <= c)

    row_reason = np.zeros(n, dtype=np.int64)
    row_reason[positive & ~consistent] = ROW_REASONS.index("inconsistent_candle")
    row_reason[finite & ~positive] = ROW_REASONS.index("nonpositive_price")
    row_reason[~finite] = ROW_REASONS.index("nonfinite_price")
    row_reason[flagged] = ROW_REASONS.index("received_out_of_order")
    row_reason[ts_ok & ~aligned] = ROW_REASONS.index("grid_misaligned")
    row_reason[~ts_ok] = ROW_REASONS.index("missing_timestamp")
    row_bad = row_reason > 0
    timed = ts_ok & aligned & ~flagged

    edge_reason = np.zeros(n, dtype=np.int64)
    if n > 1:
        both = ts_ok[1:] & ts_ok[:-1]
        with np.errstate(invalid="ignore"):
            step = open_ms[1:] - open_ms[:-1]
        er = np.zeros(n - 1, dtype=np.int64)
        er[both & (step != float(interval))] = EDGE_REASONS.index("cadence_gap")
        er[both & (step < 0)] = EDGE_REASONS.index("descending_timestamp")
        er[both & (step == 0)] = EDGE_REASONS.index("duplicate_timestamp")
        er[~both] = EDGE_REASONS.index("missing_timestamp")
        edge_reason[1:] = er
    order_edge = np.isin(edge_reason, [EDGE_REASONS.index(r) for r in ORDER_EDGE_REASONS])

    day = np.full(n, -1, dtype=np.int64)
    bar_in_day = np.full(n, -1, dtype=np.int64)
    whole = np.where(ts_ok, open_ms, 0.0).astype(np.int64)
    day_any = np.where(ts_ok, whole // DAY_MS, -1)
    day[timed] = whole[timed] // DAY_MS
    bar_in_day[timed] = (whole[timed] % DAY_MS) // interval

    prev_day = np.concatenate([[-2], day[:-1]])
    new_segment = ~timed | order_edge | (day != prev_day)
    new_segment[0] = True
    seg_id = np.cumsum(new_segment.astype(np.int64)) - 1
    seg_first = np.flatnonzero(new_segment)
    seg_count = np.diff(np.concatenate([seg_first, [n]]))
    seg_last = seg_first + seg_count - 1
    seg_high = np.maximum.reduceat(np.where(row_bad, -np.inf, h), seg_first)
    seg_low = np.minimum.reduceat(np.where(row_bad, np.inf, lo), seg_first)
    seg_bad_row = np.add.reduceat(row_bad.astype(np.int64), seg_first) > 0
    seg_last_bad = _last_flag_index(row_bad)[seg_last]
    seg_day = day[seg_first]
    seg_clean_start = timed[seg_first] & ~order_edge[seg_first]

    idx = np.arange(n)
    k = seg_id
    pair_in_segment = (idx - 1) >= seg_first[k]
    pair_edge_ok = np.zeros(n, dtype=bool)
    pair_edge_ok[1:] = edge_reason[1:] == 0
    prev_row_ok = np.zeros(n, dtype=bool)
    prev_row_ok[1:] = ~row_bad[:-1]
    pair_ok = timed & ~row_bad & pair_in_segment & pair_edge_ok & prev_row_ok

    has_prior = k >= 1
    pk = np.where(has_prior, k - 1, 0)
    prior_complete = (has_prior & seg_clean_start[k] & (seg_day[pk] == day - 1) & (seg_count[pk] == per_day)
                      & ~seg_bad_row[pk] & seg_clean_start[pk])
    structural = pair_ok & prior_complete

    prior_high = np.where(structural, seg_high[pk], np.nan)
    prior_low = np.where(structural, seg_low[pk], np.nan)
    prior_close = np.where(structural, c[seg_last[pk]], np.nan)
    with np.errstate(over="ignore", invalid="ignore"):
        rng = prior_high - prior_low
        inner = LEVEL_COEFFICIENT * rng / INNER_DIVISOR
        outer = LEVEL_COEFFICIENT * rng / OUTER_DIVISOR
        s3 = prior_close - inner
        r3 = prior_close + inner
        s4 = prior_close - outer
        r4 = prior_close + outer
    derived_finite = (np.isfinite(rng) & np.isfinite(s3) & np.isfinite(r3) & np.isfinite(s4) & np.isfinite(r4))
    zero_range = structural & derived_finite & (rng <= 0.0)
    nonfinite_derived = structural & ~derived_finite
    valid = structural & derived_finite & ~zero_range

    prev_c = np.concatenate([[np.nan], c[:-1]])
    prev_l = np.concatenate([[np.nan], lo[:-1]])
    prev_h = np.concatenate([[np.nan], h[:-1]])
    prev_ts = np.concatenate([[np.nan], open_ms[:-1]])

    with np.errstate(invalid="ignore"):
        long_setup = valid & (prev_c <= s3) & (c > s3)
        short_setup = valid & (prev_c >= r3) & (c < r3)
        long_inner = long_setup & (c < r3)
        short_inner = short_setup & (c > s3)
        long_outer = (prev_l > s4) & (lo > s4)
        short_outer = (prev_h < r4) & (h < r4)
        long_entry = long_inner & long_outer
        short_entry = short_inner & short_outer
        level_equal = valid & (((prev_c <= s3) & (c == s3)) | ((prev_c >= r3) & (c == r3)))

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[level_equal] = "level_equal"
    reason[long_setup & ~long_inner] = "long_opposite_jump"
    reason[short_setup & ~short_inner] = "short_opposite_jump"
    reason[long_inner & ~long_outer] = "long_outer_breach"
    reason[short_inner & ~short_outer] = "short_outer_breach"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"
    reason[zero_range] = "zero_range"
    reason[nonfinite_derived] = "nonfinite_derived"

    for i in np.flatnonzero(~structural):
        reason[i] = _hold_reason(int(i), row_bad, row_reason, edge_reason, timed, day, day_any, bar_in_day,
                                 seg_id, seg_first, seg_count, seg_day, seg_bad_row, seg_last_bad,
                                 seg_clean_start, per_day)

    cutoff_col = np.full(n, np.nan)
    boundary_col = np.full(n, np.nan)
    cutoff, declared_boundary, runtime = _decision_time(result.attrs)
    valid_out = valid.copy()
    if runtime:
        i = n - 1
        if cutoff is not None:
            cutoff_col[i] = float(cutoff)
        if declared_boundary is not None:
            boundary_col[i] = float(declared_boundary)
        if valid_out[i]:
            row_boundary = int(open_ms[i]) + interval if np.isfinite(open_ms[i]) else None
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
                valid_out[i] = False
                signal[i] = 0
                reason[i] = held

    day_open = np.where(timed, (day * DAY_MS).astype("float64"), np.nan)
    result["cpr_eval_ts_ms"] = np.where(ts_ok, open_ms, np.nan)
    result["cpr_prev_ts_ms"] = prev_ts
    result["cpr_day_open_ms"] = day_open
    result["cpr_prior_day_open_ms"] = np.where(structural, day_open - float(DAY_MS), np.nan)
    result["cpr_bar_in_day"] = np.where(timed, bar_in_day.astype("float64"), np.nan)
    result["cpr_prior_high"] = prior_high
    result["cpr_prior_low"] = prior_low
    result["cpr_prior_close"] = prior_close
    result["cpr_range"] = np.where(structural, rng, np.nan)
    result["cpr_s3"] = np.where(structural, s3, np.nan)
    result["cpr_r3"] = np.where(structural, r3, np.nan)
    result["cpr_s4"] = np.where(structural, s4, np.nan)
    result["cpr_r4"] = np.where(structural, r4, np.nan)
    result["cpr_prev_close"] = prev_c
    result["cpr_close"] = c
    result["cpr_prev_low"] = prev_l
    result["cpr_low"] = lo
    result["cpr_prev_high"] = prev_h
    result["cpr_high"] = h
    result["cpr_decision_cutoff_ms"] = cutoff_col
    result["cpr_decision_boundary_ms"] = boundary_col
    result["cpr_interval_ms"] = np.full(n, float(interval))
    result["cpr_valid"] = valid_out.astype("float64")
    result["cpr_reason"] = reason
    result["cpr_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result
