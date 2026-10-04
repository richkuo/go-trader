import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

from indicators_core import true_range_series

PERIOD_MIN = 2
PERIOD_MAX = 100
REQUIRED_COLUMNS = ("high", "low", "close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "separation_not_met": 2,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "inconsistent_hlc": 12,
    "timestamp_order": 13,
    "zero_true_range": 14,
    "missing_columns": 15,
}


def _validate_period(value) -> int:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"vortex_trend: period must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"vortex_trend: period must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < PERIOD_MIN or as_int > PERIOD_MAX:
        raise ValueError(
            f"vortex_trend: period must be in [{PERIOD_MIN}, {PERIOD_MAX}], got {value!r}"
        )
    return as_int


def _validate_separation(value) -> float:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"vortex_trend: min_separation must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or as_float < 0.0 or as_float >= 1.0:
        raise ValueError(
            f"vortex_trend: min_separation must be finite and in [0, 1), got {value!r}"
        )
    return as_float


def validate_vortex_params(period, min_separation) -> tuple:
    return _validate_period(period), _validate_separation(min_separation)


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


def _window_all(flags: np.ndarray, window: int) -> np.ndarray:
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
    out[window - 1:] = sliding_window_view(values, window).sum(axis=1)
    return out


def _empty_result(result: pd.DataFrame, separation: float, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in ("vortex_vi_plus", "vortex_vi_minus", "vortex_diff", "vortex_prev_diff",
                "vortex_tr_sum", "vortex_eval_ts_ms"):
        result[col] = np.full(n, np.nan)
    result["vortex_separation"] = np.full(n, separation)
    result["vortex_valid"] = np.zeros(n, dtype=bool)
    result["vortex_reason"] = np.full(n, reason, dtype=object)
    result["vortex_reason_code"] = np.full(n, REASON_CODES["missing_columns"], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def vortex_trend_core(
    df: pd.DataFrame,
    period: int = 14,
    min_separation: float = 0.0,
) -> pd.DataFrame:
    period, min_separation = validate_vortex_params(period, min_separation)
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _empty_result(result, min_separation, "missing_columns:" + ",".join(missing))

    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(high) & np.isfinite(low) & np.isfinite(close)
    with np.errstate(invalid="ignore"):
        inconsistent = finite & ((high < low) | (close > high) | (close < low))

    ts = _timestamps_ms(result)
    bad_ts = np.zeros(n, dtype=bool)
    if ts is not None:
        bad_ts = ~np.isfinite(ts)
        if n > 1:
            with np.errstate(invalid="ignore"):
                bad_ts[1:] |= ~(ts[1:] > ts[:-1])

    row_reason = np.full(n, "", dtype=object)
    row_reason[bad_ts] = "timestamp_order"
    row_reason[inconsistent] = "inconsistent_hlc"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~inconsistent & ~bad_ts

    pair_ok = np.zeros(n, dtype=bool)
    if n > 1:
        pair_ok[1:] = row_ok[1:] & row_ok[:-1]

    prev_high = np.full(n, np.nan)
    prev_low = np.full(n, np.nan)
    prev_high[1:] = high[:-1]
    prev_low[1:] = low[:-1]
    tr = true_range_series(
        pd.Series(high), pd.Series(low), pd.Series(close)
    ).to_numpy(dtype="float64")
    vm_plus = np.where(pair_ok, np.abs(high - prev_low), 0.0)
    vm_minus = np.where(pair_ok, np.abs(low - prev_high), 0.0)
    tr = np.where(pair_ok, tr, 0.0)

    window_ok = _window_all(pair_ok, period)
    tr_sum = _window_sum(tr, period)
    plus_sum = _window_sum(vm_plus, period)
    minus_sum = _window_sum(vm_minus, period)
    zero_tr = window_ok & ~(tr_sum > 0)
    vi_ok = window_ok & ~zero_tr

    vi_plus = np.full(n, np.nan)
    vi_minus = np.full(n, np.nan)
    vi_plus[vi_ok] = plus_sum[vi_ok] / tr_sum[vi_ok]
    vi_minus[vi_ok] = minus_sum[vi_ok] / tr_sum[vi_ok]
    diff = vi_plus - vi_minus
    tr_sum_out = np.where(window_ok, tr_sum, np.nan)

    prev_diff = np.full(n, np.nan)
    prev_ok = np.zeros(n, dtype=bool)
    if n > 1:
        prev_diff[1:] = diff[:-1]
        prev_ok[1:] = vi_ok[:-1]
    decision_ok = vi_ok & prev_ok

    with np.errstate(invalid="ignore"):
        long_entry = decision_ok & (diff > min_separation) & (prev_diff <= min_separation)
        short_entry = decision_ok & (diff < -min_separation) & (prev_diff >= -min_separation)
        sign_flip = decision_ok & (
            ((diff > 0) & (prev_diff <= 0)) | ((diff < 0) & (prev_diff >= 0))
        )

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[sign_flip & ~long_entry & ~short_entry] = "separation_not_met"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(period)
    window_defect = np.full(n, "", dtype=object)
    for i in np.flatnonzero(~decision_ok):
        if row_reason[i]:
            window_defect[i] = row_reason[i]
            continue
        lo = max(0, i - need + 1)
        prior = [r for r in row_reason[lo:i] if r]
        if prior:
            window_defect[i] = prior[-1]
        elif zero_tr[i] or (i > 0 and zero_tr[i - 1]):
            window_defect[i] = "zero_true_range"
        else:
            window_defect[i] = "insufficient_history"
    invalid = ~decision_ok
    reason[invalid] = window_defect[invalid]

    result["vortex_vi_plus"] = vi_plus
    result["vortex_vi_minus"] = vi_minus
    result["vortex_diff"] = diff
    result["vortex_prev_diff"] = np.where(decision_ok, prev_diff, np.nan)
    result["vortex_tr_sum"] = tr_sum_out
    result["vortex_separation"] = np.full(n, min_separation)
    result["vortex_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["vortex_valid"] = decision_ok
    result["vortex_reason"] = reason
    result["vortex_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
