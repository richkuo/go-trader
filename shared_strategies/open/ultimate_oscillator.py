import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

from donchian_breakout import donchian_breakout_core

HISTORY_LIMIT = 200
REQUIRED_COLUMNS = ("high", "low", "close")
WINDOW_NAMES = ("short_window", "mid_window", "long_window")
WINDOW_WEIGHTS = (4.0, 2.0, 1.0)

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_breakout": 0,
    "unconfirmed_long": 2,
    "unconfirmed_short": 3,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "nonpositive_input": 12,
    "inconsistent_candle": 13,
    "timestamp_order": 14,
    "cadence_gap": 15,
    "zero_range": 16,
    "missing_columns": 17,
    "missing_timestamps": 18,
    "nonfinite_computed": 19,
    "timestamp_missing": 20,
}


def _validate_integer(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"ultimate_oscillator: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"ultimate_oscillator: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < 1:
        raise ValueError(f"ultimate_oscillator: {name} must be at least 1, got {value!r}")
    return as_int


def _validate_threshold(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"ultimate_oscillator: {name} must be a finite number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"ultimate_oscillator: {name} must be a finite number, got {value!r}")
    return as_float


def required_history_bars(long_window: int, breakout_lookback: int) -> int:
    return max(int(long_window) + 1, int(breakout_lookback) + 2)


def validate_uo_params(short_window, mid_window, long_window,
                       bullish_threshold, bearish_threshold, breakout_lookback) -> tuple:
    short = _validate_integer("short_window", short_window)
    mid = _validate_integer("mid_window", mid_window)
    long_ = _validate_integer("long_window", long_window)
    lookback = _validate_integer("breakout_lookback", breakout_lookback)
    if not short < mid < long_:
        raise ValueError(
            "ultimate_oscillator: windows must satisfy short_window < mid_window < long_window, "
            f"got {short}, {mid}, {long_}")
    bullish = _validate_threshold("bullish_threshold", bullish_threshold)
    bearish = _validate_threshold("bearish_threshold", bearish_threshold)
    if not 0.0 < bearish < bullish < 100.0:
        raise ValueError(
            "ultimate_oscillator: thresholds must satisfy 0 < bearish_threshold < "
            f"bullish_threshold < 100, got {bearish} and {bullish}")
    need = required_history_bars(long_, lookback)
    if need >= HISTORY_LIMIT:
        raise ValueError(
            f"ultimate_oscillator: required history {need} bars must stay below {HISTORY_LIMIT}")
    return short, mid, long_, bullish, bearish, lookback


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


def _window_sum(values: np.ndarray, window: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window:
        return out
    count = n - window + 1
    acc = np.zeros(count)
    with np.errstate(over="ignore", invalid="ignore"):
        for j in range(window):
            acc = acc + values[j:j + count]
    out[window - 1:] = acc
    return out


def _span_steps(ts: np.ndarray, need: int) -> tuple:
    n = len(ts)
    increasing = np.zeros(n, dtype=bool)
    uniform = np.zeros(n, dtype=bool)
    if n < need:
        return increasing, uniform
    steps = np.full(n, np.nan)
    with np.errstate(invalid="ignore"):
        steps[1:] = ts[1:] - ts[:-1]
    view = sliding_window_view(steps[1:], need - 1)
    finite = np.isfinite(view).all(axis=1)
    with np.errstate(invalid="ignore"):
        positive = finite & (view > 0).all(axis=1)
        equal = positive & (view.max(axis=1) == view.min(axis=1))
    increasing[need - 1:] = positive
    uniform[need - 1:] = equal
    return increasing, uniform


def _numeric(result: pd.DataFrame, col: str) -> np.ndarray:
    return pd.to_numeric(result[col], errors="coerce").to_numpy(dtype="float64")


def _diagnostic_columns() -> tuple:
    cols = ["uo", "uo_buying_pressure", "uo_true_range", "donchian_upper", "donchian_lower",
            "uo_eval_ts_ms"]
    for name in WINDOW_NAMES:
        tag = name.split("_")[0]
        cols += [f"uo_bp_sum_{tag}", f"uo_tr_sum_{tag}", f"uo_avg_{tag}"]
    return tuple(cols)


def _hold_all(result: pd.DataFrame, reason: str, code: str, bullish: float, bearish: float,
              ts) -> pd.DataFrame:
    n = len(result)
    for col in _diagnostic_columns():
        result[col] = np.full(n, np.nan)
    if ts is not None:
        result["uo_eval_ts_ms"] = ts
    result["uo_bullish_threshold"] = np.full(n, bullish)
    result["uo_bearish_threshold"] = np.full(n, bearish)
    result["uo_raw_event"] = np.zeros(n, dtype=np.int64)
    result["uo_valid"] = np.zeros(n, dtype=bool)
    result["uo_reason"] = np.full(n, reason, dtype=object)
    result["uo_reason_code"] = np.full(n, REASON_CODES[code], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def ultimate_oscillator_core(
    df: pd.DataFrame,
    short_window: int = 7,
    mid_window: int = 14,
    long_window: int = 28,
    bullish_threshold: float = 70.0,
    bearish_threshold: float = 30.0,
    breakout_lookback: int = 20,
) -> pd.DataFrame:
    short, mid, long_, bullish, bearish, lookback = validate_uo_params(
        short_window, mid_window, long_window, bullish_threshold, bearish_threshold,
        breakout_lookback)
    windows = (short, mid, long_)
    need = required_history_bars(long_, lookback)
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _hold_all(result, "missing_columns:" + ",".join(missing), "missing_columns",
                         bullish, bearish, None)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, "missing_timestamps", "missing_timestamps", bullish, bearish, None)

    high = _numeric(result, "high")
    low = _numeric(result, "low")
    close = _numeric(result, "close")

    finite = np.isfinite(high) & np.isfinite(low) & np.isfinite(close)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ((high <= 0) | (low <= 0) | (close <= 0))
        inconsistent = finite & ~nonpositive & ((low > close) | (close > high))
    ts_missing = ~np.isfinite(ts)

    row_reason = np.full(n, "", dtype=object)
    row_reason[ts_missing] = "timestamp_missing"
    row_reason[inconsistent] = "inconsistent_candle"
    row_reason[nonpositive] = "nonpositive_input"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~nonpositive & ~inconsistent & ~ts_missing

    hi = np.where(row_ok, high, np.nan)
    lo = np.where(row_ok, low, np.nan)
    cl = np.where(row_ok, close, np.nan)

    prev_cl = np.full(n, np.nan)
    if n > 1:
        prev_cl[1:] = cl[:-1]
    pair_ok = row_ok & np.isfinite(prev_cl)
    with np.errstate(over="ignore", invalid="ignore"):
        floor = np.minimum(lo, prev_cl)
        ceiling = np.maximum(hi, prev_cl)
        bp = cl - floor
        tr = ceiling - floor
    bp = np.where(pair_ok, bp, np.nan)
    tr = np.where(pair_ok, tr, np.nan)
    bp_zero = np.where(pair_ok, bp, 0.0)
    tr_zero = np.where(pair_ok, tr, 0.0)

    span_rows_ok = _trailing_all(row_ok, need)
    increasing, uniform = _span_steps(ts, need)
    span_ok = span_rows_ok & increasing & uniform

    bp_sums, tr_sums, avgs = [], [], []
    computed_ok = span_ok & np.isfinite(bp) & np.isfinite(tr)
    zero_range = np.zeros(n, dtype=bool)
    for w in windows:
        bp_sum = _window_sum(bp_zero, w)
        tr_sum = _window_sum(tr_zero, w)
        with np.errstate(divide="ignore", invalid="ignore", over="ignore"):
            avg = bp_sum / tr_sum
        sums_finite = np.isfinite(bp_sum) & np.isfinite(tr_sum)
        zero_range |= span_ok & sums_finite & (tr_sum == 0.0)
        computed_ok &= sums_finite & np.isfinite(avg)
        bp_sums.append(bp_sum)
        tr_sums.append(tr_sum)
        avgs.append(avg)
    with np.errstate(over="ignore", invalid="ignore"):
        weighted = (WINDOW_WEIGHTS[0] * avgs[0] + WINDOW_WEIGHTS[1] * avgs[1]
                    + WINDOW_WEIGHTS[2] * avgs[2])
        uo = 100.0 * weighted / sum(WINDOW_WEIGHTS)
    computed_ok &= ~zero_range & np.isfinite(uo)
    decision_ok = span_ok & computed_ok

    channels = donchian_breakout_core(
        pd.DataFrame({"high": hi, "low": lo, "close": cl}),
        entry_period=lookback, exit_period=lookback)
    upper = channels["donchian_upper"].to_numpy(dtype="float64")
    lower = channels["donchian_lower"].to_numpy(dtype="float64")
    raw_event = channels["signal"].to_numpy(dtype=np.int64)
    raw_event = np.where(span_ok, raw_event, 0)

    with np.errstate(invalid="ignore"):
        long_entry = decision_ok & (raw_event == 1) & (uo > bullish)
        short_entry = decision_ok & (raw_event == -1) & (uo < bearish)
    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_breakout", dtype=object)
    reason[decision_ok & (raw_event == 1) & ~long_entry] = "unconfirmed_long"
    reason[decision_ok & (raw_event == -1) & ~short_entry] = "unconfirmed_short"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    positions = np.arange(n)
    last_bad = np.maximum.accumulate(np.where(row_reason != "", positions, -1)) if n else positions
    for i in np.flatnonzero(~decision_ok):
        if last_bad[i] >= 0 and last_bad[i] >= i - need + 1:
            reason[i] = row_reason[last_bad[i]]
        elif i + 1 < need:
            reason[i] = "insufficient_history"
        elif not increasing[i]:
            reason[i] = "timestamp_order"
        elif not uniform[i]:
            reason[i] = "cadence_gap"
        elif zero_range[i]:
            reason[i] = "zero_range"
        else:
            reason[i] = "nonfinite_computed"

    result["uo"] = np.where(decision_ok, uo, np.nan)
    result["uo_buying_pressure"] = bp
    result["uo_true_range"] = tr
    for name, bp_sum, tr_sum, avg in zip(WINDOW_NAMES, bp_sums, tr_sums, avgs):
        tag = name.split("_")[0]
        result[f"uo_bp_sum_{tag}"] = np.where(span_ok, bp_sum, np.nan)
        result[f"uo_tr_sum_{tag}"] = np.where(span_ok, tr_sum, np.nan)
        result[f"uo_avg_{tag}"] = np.where(decision_ok, avg, np.nan)
    result["uo_bullish_threshold"] = np.full(n, bullish)
    result["uo_bearish_threshold"] = np.full(n, bearish)
    result["donchian_upper"] = np.where(span_ok, upper, np.nan)
    result["donchian_lower"] = np.where(span_ok, lower, np.nan)
    result["uo_raw_event"] = raw_event
    result["uo_eval_ts_ms"] = ts
    result["uo_valid"] = decision_ok
    result["uo_reason"] = reason
    result["uo_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
