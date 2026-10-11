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

NAME = "ease_of_movement_breakout"
PARAM_MIN = 2
PARAM_MAX = 100
REQUIRED_COLUMNS = ("high", "low", "close", "volume")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_breakout": 0,
    "indicator_disagrees": 2,
    "zero_indicator": 3,
    "breakout_continuation": 4,
    "insufficient_history": 10,
    "missing_cadence": 11,
    "cadence_gap": 12,
    "timestamp_order": 13,
    "missing_timestamps": 14,
    "nonfinite_input": 15,
    "invalid_volume": 16,
    "inconsistent_range": 17,
    "nonfinite_derived": 18,
    "missing_columns": 19,
}

DIAGNOSTIC_COLUMNS = (
    "eom_eval_ts_ms",
    "eom_interval_ms",
    "eom_mid_change",
    "eom_raw",
    "eom_smoothed",
    "eom_prior_high",
    "eom_prior_low",
    "eom_setup",
)


def _validate_param(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < PARAM_MIN or as_int > PARAM_MAX:
        raise ValueError(f"{NAME}: {name} must be in [{PARAM_MIN}, {PARAM_MAX}], got {value!r}")
    return as_int


def validate_eom_params(eom_period, breakout_window) -> tuple:
    return _validate_param("eom_period", eom_period), _validate_param("breakout_window", breakout_window)


def required_history_bars(eom_period: int, breakout_window: int) -> int:
    return max(int(eom_period) + 1, int(breakout_window) + 2)


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


def _window_count(flags: np.ndarray, window: int) -> np.ndarray:
    n = len(flags)
    out = np.zeros(n, dtype=np.int64)
    if n < window:
        return out
    cs = np.concatenate(([0], np.cumsum(flags.astype(np.int64))))
    out[window - 1:] = cs[window:] - cs[:n - window + 1]
    return out


def _trailing_reduce(values: np.ndarray, window: int, end_offset: int, op) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window + end_offset:
        return out
    reduced = op(sliding_window_view(values, window), axis=1)
    start = window - 1 + end_offset
    out[start:] = reduced[: n - start]
    return out


def _hold_all(result: pd.DataFrame, reason: str, code: str, interval) -> pd.DataFrame:
    n = len(result)
    for col in DIAGNOSTIC_COLUMNS:
        result[col] = np.full(n, np.nan)
    if interval is not None:
        result["eom_interval_ms"] = np.full(n, float(interval))
    result["eom_valid"] = np.zeros(n, dtype=bool)
    result["eom_reason"] = np.full(n, reason, dtype=object)
    result["eom_reason_code"] = np.full(n, REASON_CODES[code], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def _last_defect(reasons: np.ndarray, lo: int, hi: int):
    if hi < lo:
        return None
    seg = reasons[lo:hi + 1]
    hits = np.flatnonzero(seg != "")
    if len(hits) == 0:
        return None
    pos = lo + int(hits[-1])
    return pos, reasons[pos]


def ease_of_movement_breakout_core(
    df: pd.DataFrame,
    eom_period: int = 14,
    breakout_window: int = 20,
) -> pd.DataFrame:
    period, window = validate_eom_params(eom_period, breakout_window)
    result = df.copy()
    n = len(result)
    interval, kind = frame_cadence(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _hold_all(result, "missing_columns:" + ",".join(missing), "missing_columns", interval)
    if interval is None:
        return _hold_all(result, "missing_cadence", "missing_cadence", None)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, "missing_timestamps", "missing_timestamps", interval)

    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    volume = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")

    ts_ok = np.isfinite(ts)
    price_finite = np.isfinite(high) & np.isfinite(low) & np.isfinite(close)
    with np.errstate(invalid="ignore"):
        bad_range = price_finite & ((high < low) | (close > high) | (close < low))
        vol_nonpositive = np.isfinite(volume) & (volume <= 0)

    price_reason = np.full(n, "", dtype=object)
    price_reason[~ts_ok] = "missing_timestamps"
    price_reason[bad_range] = "inconsistent_range"
    price_reason[~price_finite] = "nonfinite_input"
    price_ok = price_finite & ~bad_range & ts_ok

    vol_reason = np.full(n, "", dtype=object)
    vol_reason[vol_nonpositive] = "invalid_volume"
    vol_reason[~np.isfinite(volume)] = "nonfinite_input"
    vol_ok = np.isfinite(volume) & ~vol_nonpositive

    edge_ok = np.zeros(n, dtype=bool)
    edge_reason = np.full(n, "", dtype=object)
    if n > 1:
        both = ts_ok[1:] & ts_ok[:-1]
        with np.errstate(invalid="ignore"):
            step = ts[1:] - ts[:-1]
        exact = both & (step == float(interval))
        edge_ok[1:] = exact
        tail_reason = np.full(n - 1, "", dtype=object)
        with np.errstate(invalid="ignore"):
            tail_reason[both & (step != float(interval))] = "cadence_gap"
            tail_reason[both & (step <= 0)] = "timestamp_order"
        tail_reason[~both] = "missing_timestamps"
        edge_reason[1:] = tail_reason

    safe_high = np.where(price_ok, high, 0.0)
    safe_low = np.where(price_ok, low, 0.0)
    safe_volume = np.where(vol_ok, volume, 1.0)
    with np.errstate(over="ignore", invalid="ignore"):
        midpoint = (safe_high + safe_low) / 2.0
        span = safe_high - safe_low

    obs_inputs_ok = np.zeros(n, dtype=bool)
    mid_change = np.full(n, np.nan)
    raw = np.full(n, np.nan)
    if n > 1:
        obs_inputs_ok[1:] = price_ok[1:] & price_ok[:-1] & vol_ok[1:] & edge_ok[1:]
        with np.errstate(over="ignore", invalid="ignore"):
            change = midpoint[1:] - midpoint[:-1]
            value = change * span[1:] / safe_volume[1:]
        mid_change[1:] = change
        raw[1:] = value + 0.0
    obs_ok = obs_inputs_ok & np.isfinite(raw) & np.isfinite(mid_change)
    derived_reason = np.full(n, "", dtype=object)
    derived_reason[obs_inputs_ok & ~obs_ok] = "nonfinite_derived"

    obs_window_ok = _window_count(obs_ok, period) == period
    with np.errstate(over="ignore", invalid="ignore"):
        smoothed = _trailing_reduce(np.where(obs_ok, raw, 0.0), period, 0, np.sum) / float(period)
    smooth_ok = obs_window_ok & np.isfinite(smoothed)
    smooth_derived_bad = obs_window_ok & ~np.isfinite(smoothed)
    smoothed = np.where(smooth_ok, smoothed + 0.0, np.nan)

    rows_ok = _window_count(price_ok, window + 1) == window + 1
    edges_ok = _window_count(edge_ok, window) == window
    state_ok = np.zeros(n, dtype=bool)
    if n > 0:
        state_ok[1:] = rows_ok[1:] & edges_ok[1:]
    upper = _trailing_reduce(np.where(price_ok, high, -np.inf), window, 1, np.max)
    lower = _trailing_reduce(np.where(price_ok, low, np.inf), window, 1, np.min)
    upper = np.where(state_ok, upper, np.nan)
    lower = np.where(state_ok, lower, np.nan)
    with np.errstate(invalid="ignore"):
        up = state_ok & (close > upper)
        down = state_ok & (close < lower)
    setup = np.full(n, np.nan)
    setup[state_ok] = 0.0
    setup[up] = 1.0
    setup[down] = -1.0

    prev_ok = np.zeros(n, dtype=bool)
    prev_up = np.zeros(n, dtype=bool)
    prev_down = np.zeros(n, dtype=bool)
    if n > 1:
        prev_ok[1:] = state_ok[:-1]
        prev_up[1:] = up[:-1]
        prev_down[1:] = down[:-1]

    decision_ok = state_ok & prev_ok & smooth_ok
    new_up = decision_ok & up & ~prev_up
    new_down = decision_ok & down & ~prev_down
    with np.errstate(invalid="ignore"):
        long_entry = new_up & (smoothed > 0.0)
        short_entry = new_down & (smoothed < 0.0)
        zero_at_breakout = (new_up | new_down) & (smoothed == 0.0)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_breakout", dtype=object)
    reason[decision_ok & ((up & prev_up) | (down & prev_down))] = "breakout_continuation"
    reason[(new_up | new_down) & ~long_entry & ~short_entry] = "indicator_disagrees"
    reason[zero_at_breakout] = "zero_indicator"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(period, window)
    for i in np.flatnonzero(~decision_ok):
        row_lo = max(0, i - need + 1)
        found = []
        for pos_reason in (
            _last_defect(price_reason, row_lo, i),
            _last_defect(edge_reason, max(1, row_lo + 1), i),
            _last_defect(vol_reason, max(0, i - period + 1), i),
            _last_defect(derived_reason, max(1, i - period + 1), i),
        ):
            if pos_reason is not None:
                found.append(pos_reason)
        if found:
            reason[i] = max(found, key=lambda pr: pr[0])[1]
        elif i < need - 1:
            reason[i] = "insufficient_history"
        elif smooth_derived_bad[i]:
            reason[i] = "nonfinite_derived"
        else:
            reason[i] = "insufficient_history"

    eval_ts = np.full(n, np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        eval_ts = np.where(ts_ok, ts, np.nan)
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        eval_ts = np.where(ts_ok, ts - float(interval) + 1.0, np.nan)

    result["eom_eval_ts_ms"] = eval_ts
    result["eom_interval_ms"] = np.full(n, float(interval))
    result["eom_mid_change"] = np.where(obs_ok, mid_change + 0.0, np.nan)
    result["eom_raw"] = np.where(obs_ok, raw, np.nan)
    result["eom_smoothed"] = smoothed
    result["eom_prior_high"] = upper
    result["eom_prior_low"] = lower
    result["eom_setup"] = setup
    result["eom_valid"] = decision_ok
    result["eom_reason"] = reason
    result["eom_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
