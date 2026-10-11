import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

PERIOD_MIN = 1
PERIOD_MAX = 198
THRESHOLD_LOW = 0.0
THRESHOLD_HIGH = 100.0
DEFAULT_PERIOD = 20
DEFAULT_THRESHOLD = 25.0
DEFAULT_EXPECTED_INTERVAL_MS = 3_600_000

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "threshold_boundary": 2,
    "flat_price": 3,
    "insufficient_history": 10,
    "nonfinite_close": 11,
    "nonpositive_close": 12,
    "missing_timestamp": 13,
    "duplicate_timestamp": 14,
    "descending_timestamp": 15,
    "cadence_gap": 16,
    "nonfinite_oscillator": 17,
    "missing_columns": 18,
    "missing_timestamps": 19,
}

ROW_REASONS = ("", "nonfinite_close", "nonpositive_close", "missing_timestamp")
STEP_REASONS = ("", "missing_timestamp", "duplicate_timestamp", "descending_timestamp", "cadence_gap")

RESULT_COLUMNS = (
    "cmo_close", "cmo_eval_ts_ms", "cmo_gain_sum", "cmo_loss_sum", "cmo", "cmo_prev",
    "cmo_valid", "cmo_reason", "cmo_reason_code",
)


def _integral(name: str, value, low, high) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"chande_momentum: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"chande_momentum: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < low or (high is not None and as_int > high):
        bound = f"[{low}, {high}]" if high is not None else f">= {low}"
        raise ValueError(f"chande_momentum: {name} must be {bound}, got {value!r}")
    return as_int


def validate_cmo_params(period, threshold, expected_interval_ms) -> tuple:
    period = _integral("period", period, PERIOD_MIN, PERIOD_MAX)
    if isinstance(threshold, (bool, np.bool_)) or not isinstance(threshold, Real):
        raise ValueError(f"chande_momentum: threshold must be a number, got {threshold!r}")
    thr = float(threshold)
    if not math.isfinite(thr) or not (THRESHOLD_LOW < thr < THRESHOLD_HIGH):
        raise ValueError(
            f"chande_momentum: threshold must be finite and strictly between "
            f"{THRESHOLD_LOW:g} and {THRESHOLD_HIGH:g}, got {threshold!r}"
        )
    interval = _integral("expected_interval_ms", expected_interval_ms, 1, None)
    return period, thr, interval


def oscillator_history_bars(period: int) -> int:
    return int(period) + 1


def required_history_bars(period: int) -> int:
    return int(period) + 2


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


def _ordered_window_sums(values: np.ndarray, window: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window:
        return out
    view = sliding_window_view(values, window)
    acc = np.zeros(view.shape[0])
    for k in range(window):
        acc = acc + view[:, k]
    out[window - 1:] = acc
    return out


def _last_flag_index(flags: np.ndarray) -> np.ndarray:
    pos = np.where(flags, np.arange(len(flags)), -1)
    return np.maximum.accumulate(pos) if len(pos) else pos


def _hold_all(result: pd.DataFrame, reason: str, code: str, ts) -> pd.DataFrame:
    n = len(result)
    for col in ("cmo_close", "cmo_gain_sum", "cmo_loss_sum", "cmo", "cmo_prev"):
        result[col] = np.full(n, np.nan)
    result["cmo_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["cmo_valid"] = np.zeros(n, dtype=bool)
    result["cmo_reason"] = np.full(n, reason, dtype=object)
    result["cmo_reason_code"] = np.full(n, REASON_CODES[code], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def chande_momentum_core(
    df: pd.DataFrame,
    period: int = DEFAULT_PERIOD,
    threshold: float = DEFAULT_THRESHOLD,
    expected_interval_ms: int = DEFAULT_EXPECTED_INTERVAL_MS,
) -> pd.DataFrame:
    period, threshold, interval = validate_cmo_params(period, threshold, expected_interval_ms)
    result = df.copy()
    n = len(result)

    if "close" not in result.columns:
        return _hold_all(result, "missing_columns:close", "missing_columns", None)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, "missing_timestamps", "missing_timestamps", None)

    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    finite = np.isfinite(close)
    with np.errstate(invalid="ignore"):
        positive = finite & (close > 0.0)
    ts_ok = np.isfinite(ts)

    row_reason = np.zeros(n, dtype=np.int64)
    row_reason[~ts_ok] = ROW_REASONS.index("missing_timestamp")
    row_reason[finite & ~positive] = ROW_REASONS.index("nonpositive_close")
    row_reason[~finite] = ROW_REASONS.index("nonfinite_close")
    row_bad = row_reason > 0

    step_reason = np.zeros(n, dtype=np.int64)
    if n > 1:
        prev_ts, cur_ts = ts[:-1], ts[1:]
        both = np.isfinite(prev_ts) & np.isfinite(cur_ts)
        with np.errstate(invalid="ignore"):
            step = cur_ts - prev_ts
        sr = np.zeros(n - 1, dtype=np.int64)
        sr[both & (step != float(interval))] = STEP_REASONS.index("cadence_gap")
        sr[both & (step < 0)] = STEP_REASONS.index("descending_timestamp")
        sr[both & (step == 0)] = STEP_REASONS.index("duplicate_timestamp")
        sr[~both] = STEP_REASONS.index("missing_timestamp")
        step_reason[1:] = sr
    step_bad = step_reason > 0

    idx = np.arange(n)
    last_row_bad = _last_flag_index(row_bad)
    last_step_bad = _last_flag_index(step_bad)
    osc_ok = (idx >= period) & (last_row_bad < idx - period) & (last_step_bad < idx - period + 1)
    decision_ok = np.zeros(n, dtype=bool)
    if n > 1:
        decision_ok[1:] = osc_ok[1:] & osc_ok[:-1]

    safe_close = np.where(positive, close, 0.0)
    diff = np.zeros(n)
    if n > 1:
        diff[1:] = safe_close[1:] - safe_close[:-1]
    gains_step = np.where(diff > 0.0, diff, 0.0)
    losses_step = np.where(diff < 0.0, -diff, 0.0)
    gain_sum = np.full(n, np.nan)
    loss_sum = np.full(n, np.nan)
    if n > period:
        gain_sum[period:] = _ordered_window_sums(gains_step[1:], period)[period - 1:]
        loss_sum[period:] = _ordered_window_sums(losses_step[1:], period)[period - 1:]
    gain_sum[~osc_ok] = np.nan
    loss_sum[~osc_ok] = np.nan

    denom = gain_sum + loss_sum
    flat = osc_ok & (denom == 0.0)
    cmo = np.full(n, np.nan)
    live = osc_ok & ~flat
    with np.errstate(invalid="ignore", divide="ignore", over="ignore"):
        cmo[live] = 100.0 * (gain_sum[live] - loss_sum[live]) / denom[live]
    cmo[flat] = 0.0
    nonfinite_osc = osc_ok & ~np.isfinite(cmo)
    osc_ok = osc_ok & ~nonfinite_osc
    cmo[~osc_ok] = np.nan
    if n > 1:
        decision_ok[1:] = osc_ok[1:] & osc_ok[:-1]

    prev = np.full(n, np.nan)
    if n > 1:
        prev[1:] = cmo[:-1]

    with np.errstate(invalid="ignore"):
        long_entry = decision_ok & (cmo > threshold) & (prev <= threshold)
        short_entry = decision_ok & (cmo < -threshold) & (prev >= -threshold)
        boundary = decision_ok & ((cmo == threshold) | (cmo == -threshold))

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[decision_ok & flat] = "flat_price"
    reason[boundary] = "threshold_boundary"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    span_first_row = idx - period - 1
    span_first_step = idx - period
    for i in np.flatnonzero(~decision_ok):
        r = last_row_bad[i]
        s = last_step_bad[i]
        r_in = r >= 0 and r >= span_first_row[i]
        s_in = s >= 0 and s >= span_first_step[i]
        if r_in and (not s_in or r >= s):
            reason[i] = ROW_REASONS[row_reason[r]]
        elif s_in:
            reason[i] = STEP_REASONS[step_reason[s]]
        elif i >= 1 and (nonfinite_osc[i] or nonfinite_osc[i - 1]):
            reason[i] = "nonfinite_oscillator"
        else:
            reason[i] = "insufficient_history"

    result["cmo_close"] = np.where(finite, close, np.nan)
    result["cmo_eval_ts_ms"] = ts
    result["cmo_gain_sum"] = gain_sum
    result["cmo_loss_sum"] = loss_sum
    result["cmo"] = cmo
    result["cmo_prev"] = prev
    result["cmo_valid"] = decision_ok
    result["cmo_reason"] = reason
    result["cmo_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
