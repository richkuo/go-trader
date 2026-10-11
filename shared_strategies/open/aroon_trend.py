import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

NAME = "aroon_trend"
PERIOD_MIN = 2
PERIOD_MAX = 198
STRENGTH_MIN = 0.0
STRENGTH_MAX = 100.0
DEFAULT_PERIOD = 25
DEFAULT_STRENGTH = 70.0
REQUIRED_COLUMNS = ("high", "low", "close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "strength_not_met": 2,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "inconsistent_hlc": 12,
    "timestamp_order": 13,
    "flat_range": 14,
    "missing_columns": 15,
}

OUTPUT_FLOAT_COLUMNS = (
    "aroon_up",
    "aroon_down",
    "aroon_prev_up",
    "aroon_prev_down",
    "aroon_high_age",
    "aroon_low_age",
    "aroon_eval_ts_ms",
)


def _validate_period(value) -> int:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: period must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: period must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < PERIOD_MIN or as_int > PERIOD_MAX:
        raise ValueError(f"{NAME}: period must be in [{PERIOD_MIN}, {PERIOD_MAX}], got {value!r}")
    return as_int


def _validate_strength(value) -> float:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: strength must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or as_float < STRENGTH_MIN or as_float > STRENGTH_MAX:
        raise ValueError(
            f"{NAME}: strength must be finite and in [{STRENGTH_MIN:g}, {STRENGTH_MAX:g}], got {value!r}"
        )
    return as_float


def validate_aroon_params(period, strength) -> tuple:
    return _validate_period(period), _validate_strength(strength)


def validate_aroon_param_dict(params) -> tuple:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{NAME}: params must be an object, got {params!r}")
    return validate_aroon_params(
        params.get("period", DEFAULT_PERIOD),
        params.get("strength", DEFAULT_STRENGTH),
    )


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


def _empty_result(result: pd.DataFrame, period: int, strength: float, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in OUTPUT_FLOAT_COLUMNS:
        result[col] = np.full(n, np.nan)
    result["aroon_period"] = np.full(n, period, dtype=np.int64)
    result["aroon_strength"] = np.full(n, strength)
    result["aroon_valid"] = np.zeros(n, dtype=bool)
    result["aroon_reason"] = np.full(n, reason, dtype=object)
    result["aroon_reason_code"] = np.full(n, REASON_CODES["missing_columns"], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def aroon_trend_core(
    df: pd.DataFrame,
    period: int = DEFAULT_PERIOD,
    strength: float = DEFAULT_STRENGTH,
) -> pd.DataFrame:
    period, strength = validate_aroon_params(period, strength)
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _empty_result(result, period, strength, "missing_columns:" + ",".join(missing))

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

    span = period + 1
    up = np.full(n, np.nan)
    down = np.full(n, np.nan)
    high_age = np.full(n, np.nan)
    low_age = np.full(n, np.nan)
    window_ok = np.zeros(n, dtype=bool)
    flat = np.zeros(n, dtype=bool)
    if n >= span:
        safe_high = np.where(row_ok, high, 0.0)
        safe_low = np.where(row_ok, low, 0.0)
        hw = sliding_window_view(safe_high, span)[:, ::-1]
        lw = sliding_window_view(safe_low, span)[:, ::-1]
        ok_w = sliding_window_view(row_ok, span).all(axis=1)
        h_age = hw.argmax(axis=1)
        l_age = lw.argmin(axis=1)
        flat_w = ok_w & (hw.max(axis=1) == lw.min(axis=1))
        ind_w = ok_w & ~flat_w
        sl = slice(span - 1, n)
        window_ok[sl] = ok_w
        flat[sl] = flat_w
        high_age[sl] = np.where(ind_w, h_age, np.nan)
        low_age[sl] = np.where(ind_w, l_age, np.nan)
        up[sl] = np.where(ind_w, 100.0 * (period - h_age) / period, np.nan)
        down[sl] = np.where(ind_w, 100.0 * (period - l_age) / period, np.nan)
    ind_ok = window_ok & ~flat

    prev_up = np.full(n, np.nan)
    prev_down = np.full(n, np.nan)
    prev_ok = np.zeros(n, dtype=bool)
    if n > 1:
        prev_up[1:] = up[:-1]
        prev_down[1:] = down[:-1]
        prev_ok[1:] = ind_ok[:-1]
    decision_ok = ind_ok & prev_ok

    with np.errstate(invalid="ignore"):
        cross_up = decision_ok & (prev_up <= prev_down) & (up > down)
        cross_down = decision_ok & (prev_down <= prev_up) & (down > up)
        long_entry = cross_up & (up > strength)
        short_entry = cross_down & (down > strength)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[(cross_up | cross_down) & ~long_entry & ~short_entry] = "strength_not_met"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(period)
    for i in np.flatnonzero(~decision_ok):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        lo = max(0, i - need + 1)
        prior = [r for r in row_reason[lo:i] if r]
        if prior:
            reason[i] = prior[-1]
        elif flat[i] or (i > 0 and flat[i - 1]):
            reason[i] = "flat_range"
        else:
            reason[i] = "insufficient_history"

    result["aroon_up"] = up
    result["aroon_down"] = down
    result["aroon_prev_up"] = np.where(decision_ok, prev_up, np.nan)
    result["aroon_prev_down"] = np.where(decision_ok, prev_down, np.nan)
    result["aroon_high_age"] = high_age
    result["aroon_low_age"] = low_age
    result["aroon_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["aroon_period"] = np.full(n, period, dtype=np.int64)
    result["aroon_strength"] = np.full(n, strength)
    result["aroon_valid"] = decision_ok
    result["aroon_reason"] = reason
    result["aroon_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
