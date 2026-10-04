import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

PERIOD_MIN = 2
PERIOD_MAX = 100
SMOOTH_BARS = 4
REQUIRED_COLUMNS = ("open", "high", "low", "close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "zero_line_filter": 2,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "inconsistent_ohlc": 13,
    "timestamp_order": 14,
    "cadence_gap": 15,
    "zero_range_window": 16,
    "missing_columns": 17,
    "missing_timestamps": 18,
}


def validate_rvi_params(period, zero_line_filter) -> tuple:
    if isinstance(period, (bool, np.bool_)) or not isinstance(period, Real):
        raise ValueError(f"relative_vigor_index: period must be an integer, got {period!r}")
    as_float = float(period)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"relative_vigor_index: period must be an integer, got {period!r}")
    as_int = int(as_float)
    if as_int < PERIOD_MIN or as_int > PERIOD_MAX:
        raise ValueError(
            f"relative_vigor_index: period must be in [{PERIOD_MIN}, {PERIOD_MAX}], got {period!r}"
        )
    if not isinstance(zero_line_filter, (bool, np.bool_)):
        raise ValueError(
            f"relative_vigor_index: zero_line_filter must be a boolean, got {zero_line_filter!r}"
        )
    return as_int, bool(zero_line_filter)


def index_history_bars(period: int) -> int:
    return int(period) + SMOOTH_BARS - 1


def signal_line_history_bars(period: int) -> int:
    return index_history_bars(period) + SMOOTH_BARS - 1


def required_history_bars(period: int) -> int:
    return signal_line_history_bars(period) + 1


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


def _trailing_all(flags: np.ndarray, window: int) -> np.ndarray:
    n = len(flags)
    out = np.zeros(n, dtype=bool)
    if n < window:
        return out
    out[window - 1:] = sliding_window_view(flags, window).all(axis=1)
    return out


def _trailing_sum(values: np.ndarray, window: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window:
        return out
    out[window - 1:] = sliding_window_view(values, window).sum(axis=1)
    return out


def _uniform_steps(steps: np.ndarray, bars: int) -> np.ndarray:
    n = len(steps)
    out = np.zeros(n, dtype=bool)
    count = bars - 1
    if count < 1 or n < bars:
        return out
    view = sliding_window_view(steps[1:], count)
    finite = np.isfinite(view).all(axis=1)
    with np.errstate(invalid="ignore"):
        equal = (view.max(axis=1) == view.min(axis=1))
    out[bars - 1:] = finite & equal
    return out


def _weighted4(values: np.ndarray) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < SMOOTH_BARS:
        return out
    out[3:] = (values[3:] + 2.0 * values[2:-1] + 2.0 * values[1:-2] + values[:-3]) / 6.0
    return out


def _hold_all(result: pd.DataFrame, zero_line_filter: bool, reason: str, code: str,
              ts) -> pd.DataFrame:
    n = len(result)
    for col in ("rvi", "rvi_signal", "rvi_diff", "rvi_prev_diff", "rvi_num_mean", "rvi_den_mean"):
        result[col] = np.full(n, np.nan)
    result["rvi_zero_line_filter"] = np.full(n, zero_line_filter, dtype=bool)
    result["rvi_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["rvi_valid"] = np.zeros(n, dtype=bool)
    result["rvi_reason"] = np.full(n, reason, dtype=object)
    result["rvi_reason_code"] = np.full(n, REASON_CODES[code], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def relative_vigor_index_core(
    df: pd.DataFrame,
    period: int = 10,
    zero_line_filter: bool = True,
) -> pd.DataFrame:
    period, zero_line_filter = validate_rvi_params(period, zero_line_filter)
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _hold_all(result, zero_line_filter, "missing_columns:" + ",".join(missing),
                         "missing_columns", None)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, zero_line_filter, "missing_timestamps", "missing_timestamps", None)

    op = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    hi = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    cl = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(op) & np.isfinite(hi) & np.isfinite(lo) & np.isfinite(cl)
    with np.errstate(invalid="ignore"):
        inconsistent = finite & ((hi < lo) | (op > hi) | (op < lo) | (cl > hi) | (cl < lo))

    bad_ts = ~np.isfinite(ts)
    steps = np.full(n, np.nan)
    if n > 1:
        with np.errstate(invalid="ignore"):
            bad_ts[1:] |= ~(ts[1:] > ts[:-1])
            steps[1:] = ts[1:] - ts[:-1]

    row_reason = np.full(n, "", dtype=object)
    row_reason[bad_ts] = "timestamp_order"
    row_reason[inconsistent] = "inconsistent_ohlc"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~inconsistent & ~bad_ts

    num = np.where(row_ok, cl - op, 0.0)
    den = np.where(row_ok, hi - lo, 0.0)
    smooth_num = _weighted4(num)
    smooth_den = _weighted4(den)

    index_bars = index_history_bars(period)
    signal_bars = signal_line_history_bars(period)
    need = required_history_bars(period)

    index_rows_ok = _trailing_all(row_ok, index_bars) & _uniform_steps(steps, index_bars)
    num_mean = _trailing_sum(np.nan_to_num(smooth_num), period) / period
    den_mean = _trailing_sum(np.nan_to_num(smooth_den), period) / period
    zero_range = index_rows_ok & ~(den_mean > 0)
    index_ok = index_rows_ok & ~zero_range
    rvi = np.full(n, np.nan)
    rvi[index_ok] = num_mean[index_ok] / den_mean[index_ok]

    signal_ok = (_trailing_all(index_ok, SMOOTH_BARS)
                 & _uniform_steps(steps, signal_bars))
    signal_line = np.full(n, np.nan)
    smoothed_index = _weighted4(np.where(index_ok, rvi, 0.0))
    signal_line[signal_ok] = smoothed_index[signal_ok]

    diff = np.full(n, np.nan)
    diff[signal_ok] = rvi[signal_ok] - signal_line[signal_ok]
    prev_diff = np.full(n, np.nan)
    decision_ok = np.zeros(n, dtype=bool)
    if n > 1:
        prev_diff[1:] = diff[:-1]
        decision_ok[1:] = signal_ok[1:] & signal_ok[:-1]
    prev_diff[~decision_ok] = np.nan

    with np.errstate(invalid="ignore"):
        long_cross = decision_ok & (prev_diff <= 0.0) & (diff > 0.0)
        short_cross = decision_ok & (prev_diff >= 0.0) & (diff < 0.0)
        if zero_line_filter:
            long_entry = long_cross & (rvi > 0.0)
            short_entry = short_cross & (rvi < 0.0)
        else:
            long_entry = long_cross
            short_entry = short_cross

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[(long_cross & ~long_entry) | (short_cross & ~short_entry)] = "zero_line_filter"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    span_uniform = _uniform_steps(steps, need)
    for i in np.flatnonzero(~decision_ok):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        start = max(0, i - need + 1)
        prior = [r for r in row_reason[start:i] if r]
        if prior:
            reason[i] = prior[-1]
        elif i + 1 >= need and not span_uniform[i]:
            reason[i] = "cadence_gap"
        elif i + 1 < need:
            if i >= 1 and not np.all(steps[1:i + 1] == steps[i]):
                reason[i] = "cadence_gap"
            else:
                reason[i] = "insufficient_history"
        elif zero_range[max(0, i - SMOOTH_BARS):i + 1].any():
            reason[i] = "zero_range_window"
        else:
            reason[i] = "insufficient_history"

    result["rvi"] = rvi
    result["rvi_signal"] = signal_line
    result["rvi_diff"] = diff
    result["rvi_prev_diff"] = prev_diff
    result["rvi_num_mean"] = np.where(index_ok, num_mean, np.nan)
    result["rvi_den_mean"] = np.where(index_rows_ok, den_mean, np.nan)
    result["rvi_zero_line_filter"] = np.full(n, zero_line_filter, dtype=bool)
    result["rvi_eval_ts_ms"] = ts
    result["rvi_valid"] = decision_ok
    result["rvi_reason"] = reason
    result["rvi_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
