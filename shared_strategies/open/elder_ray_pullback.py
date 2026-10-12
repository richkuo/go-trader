import math
from numbers import Real

import numpy as np
import pandas as pd

NAME = "elder_ray_pullback"
EMA_PERIOD_MIN = 2
EMA_PERIOD_MAX = 32
SLOPE_SPAN_MIN = 1
SLOPE_SPAN_MAX = 8
PULLBACK_WINDOW_MIN = 1
PULLBACK_WINDOW_MAX = 16
DEFAULT_EMA_PERIOD = 13
DEFAULT_SLOPE_SPAN = 3
DEFAULT_PULLBACK_WINDOW = 8
RECURRENCE_HORIZON = 64
REQUIRED_COLUMNS = ("open", "high", "low", "close", "volume")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_setup": 0,
    "pullback_long": 2,
    "pullback_short": -2,
    "setup_expired": 3,
    "trend_cleared": 4,
    "insufficient_history": 10,
    "missing_columns": 11,
    "missing_timestamps": 12,
    "nonfinite_input": 13,
    "nonpositive_price": 14,
    "invalid_candle_geometry": 15,
    "negative_volume": 16,
    "timestamp_order": 17,
    "irregular_cadence": 18,
    "arithmetic_overflow": 19,
}

VALUE_COLUMNS = (
    "erp_ema", "erp_ema_prior", "erp_slope", "erp_bull_power", "erp_bear_power",
    "erp_setup_side", "erp_setup_age", "erp_high", "erp_low", "erp_close", "erp_eval_ts_ms",
)

FLAG_COLUMNS = ("erp_ema_valid", "erp_slope_valid", "erp_valid")


def _validate_int(key: str, value, low: int, high: int) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {key} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {key} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < low or as_int > high:
        raise ValueError(f"{NAME}: {key} must be in [{low}, {high}], got {value!r}")
    return as_int


def validate_elder_ray_params(ema_period, slope_span, pullback_window) -> tuple:
    return (
        _validate_int("ema_period", ema_period, EMA_PERIOD_MIN, EMA_PERIOD_MAX),
        _validate_int("slope_span", slope_span, SLOPE_SPAN_MIN, SLOPE_SPAN_MAX),
        _validate_int("pullback_window", pullback_window, PULLBACK_WINDOW_MIN, PULLBACK_WINDOW_MAX),
    )


def validate_elder_ray_param_dict(params) -> tuple:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{NAME}: params must be an object, got {params!r}")
    return validate_elder_ray_params(
        params.get("ema_period", DEFAULT_EMA_PERIOD),
        params.get("slope_span", DEFAULT_SLOPE_SPAN),
        params.get("pullback_window", DEFAULT_PULLBACK_WINDOW),
    )


def required_history_bars(slope_span: int, pullback_window: int) -> int:
    return RECURRENCE_HORIZON + int(slope_span) + int(pullback_window) + 1


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
    if length <= 0:
        out[:] = True
        return out
    if n < length:
        return out
    csum = np.concatenate([[0], np.cumsum(bad.astype(np.int64))])
    ends = np.arange(length - 1, n)
    out[ends] = (csum[ends + 1] - csum[ends + 1 - length]) == 0
    return out


def _span_clear(row_bad: np.ndarray, order_bad: np.ndarray, cadence_bad: np.ndarray, span: int) -> np.ndarray:
    n = len(row_bad)
    ok = _window_clear(row_bad, span)
    order_ok = np.zeros(n, dtype=bool)
    cadence_ok = np.zeros(n, dtype=bool)
    if span >= 2:
        order_ok[span - 1:] = _window_clear(order_bad, span - 1)[span - 1:]
    if span >= 3:
        cadence_ok[span - 1:] = _window_clear(cadence_bad, span - 2)[span - 1:]
    return ok & order_ok & cadence_ok


def finite_ema(close: np.ndarray, ema_period: int) -> np.ndarray:
    n = len(close)
    out = np.zeros(n, dtype="float64")
    if n < RECURRENCE_HORIZON:
        return out
    alpha = 2.0 / (ema_period + 1.0)
    beta = 1.0 - alpha
    m = n - RECURRENCE_HORIZON + 1
    with np.errstate(over="ignore", invalid="ignore"):
        e = close[0:m].copy()
        for j in range(1, RECURRENCE_HORIZON):
            e = alpha * close[j:j + m] + beta * e
    out[RECURRENCE_HORIZON - 1:] = e
    return out


def _capped_run(cond: np.ndarray, cap: int) -> np.ndarray:
    n = len(cond)
    idx = np.arange(n)
    last_false = np.maximum.accumulate(np.where(~cond, idx, -1)) if n else idx
    return np.minimum(idx - last_false, cap)


def _attach(result: pd.DataFrame, values: dict, flags: dict, reason: np.ndarray, signal: np.ndarray,
            ema_period: int, slope_span: int, pullback_window: int) -> pd.DataFrame:
    n = len(result)
    for col in VALUE_COLUMNS:
        result[col] = values[col]
    for col in FLAG_COLUMNS:
        result[col] = flags[col].astype("float64")
    result["erp_ema_period"] = np.full(n, float(ema_period))
    result["erp_slope_span"] = np.full(n, float(slope_span))
    result["erp_pullback_window"] = np.full(n, float(pullback_window))
    result["erp_horizon"] = np.full(n, float(RECURRENCE_HORIZON))
    result["erp_required_history"] = np.full(n, float(required_history_bars(slope_span, pullback_window)))
    result["erp_reason"] = reason
    result["erp_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result


def elder_ray_pullback_core(df: pd.DataFrame, ema_period: int = DEFAULT_EMA_PERIOD,
                            slope_span: int = DEFAULT_SLOPE_SPAN,
                            pullback_window: int = DEFAULT_PULLBACK_WINDOW) -> pd.DataFrame:
    ema_period, slope_span, pullback_window = validate_elder_ray_params(ema_period, slope_span, pullback_window)
    result = df.copy()
    n = len(result)
    values = {col: np.zeros(n, dtype="float64") for col in VALUE_COLUMNS}
    values["erp_setup_age"] = np.full(n, -1.0)
    flags = {col: np.zeros(n, dtype=bool) for col in FLAG_COLUMNS}
    signal = np.zeros(n, dtype=np.int64)
    params = (ema_period, slope_span, pullback_window)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _attach(result, values, flags, np.full(n, "missing_columns", dtype=object), signal, *params)

    ts = _timestamps_ms(result)
    if ts is None:
        return _attach(result, values, flags, np.full(n, "missing_timestamps", dtype=object), signal, *params)

    open_ = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    volume = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")

    ts_missing = ~np.isfinite(ts)
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
    row_reason[ts_missing] = "missing_timestamps"
    row_bad = row_reason != ""

    diff = np.full(n, np.nan)
    order_bad = np.zeros(n, dtype=bool)
    cadence_bad = np.zeros(n, dtype=bool)
    diff_known = np.zeros(n, dtype=bool)
    if n > 1:
        with np.errstate(invalid="ignore"):
            diff[1:] = ts[1:] - ts[:-1]
        diff_known[1:] = ~ts_missing[1:] & ~ts_missing[:-1]
        with np.errstate(invalid="ignore"):
            order_bad[1:] = diff_known[1:] & ~(diff[1:] > 0)
    if n > 2:
        with np.errstate(invalid="ignore"):
            cadence_bad[2:] = diff_known[2:] & diff_known[1:-1] & ~(diff[2:] == diff[1:-1])

    clean_close = np.where(row_bad, 0.0, close)
    clean_high = np.where(row_bad, 0.0, high)
    clean_low = np.where(row_bad, 0.0, low)

    ema_ok = _span_clear(row_bad, order_bad, cadence_bad, RECURRENCE_HORIZON)
    ema = finite_ema(clean_close, ema_period)
    ema_prior = np.zeros(n, dtype="float64")
    prior_ok = np.zeros(n, dtype=bool)
    if n > slope_span:
        ema_prior[slope_span:] = ema[:-slope_span]
        prior_ok[slope_span:] = ema_ok[:-slope_span]
    structural = ema_ok & prior_ok
    with np.errstate(over="ignore", invalid="ignore"):
        slope = ema - ema_prior
        bull = clean_high - ema
        bear = clean_low - ema
    ema_finite = np.isfinite(ema)
    computed_finite = (ema_finite & np.isfinite(ema_prior) & np.isfinite(slope)
                       & np.isfinite(bull) & np.isfinite(bear))
    overflow_bad = structural & ~computed_finite
    usable = structural & computed_finite

    need = required_history_bars(slope_span, pullback_window)
    decision_ok = (_span_clear(row_bad, order_bad, cadence_bad, need)
                   & _window_clear(overflow_bad, pullback_window + 2))

    with np.errstate(invalid="ignore"):
        rising = usable & (slope > 0)
        falling = usable & (slope < 0)
        long_cond = rising & (bear < 0)
        short_cond = falling & (bull > 0)
        long_recovery = rising & (bear >= 0)
        short_recovery = falling & (bull <= 0)

    cap = pullback_window + 1
    long_run = _capped_run(long_cond, cap)
    short_run = _capped_run(short_cond, cap)
    prev_long = np.zeros(n, dtype=np.int64)
    prev_short = np.zeros(n, dtype=np.int64)
    if n > 1:
        prev_long[1:] = long_run[:-1]
        prev_short[1:] = short_run[:-1]

    long_live = (prev_long >= 1) & (prev_long <= pullback_window)
    short_live = (prev_short >= 1) & (prev_short <= pullback_window)
    long_entry = decision_ok & long_recovery & long_live
    short_entry = decision_ok & short_recovery & short_live
    long_armed = decision_ok & long_cond & (prev_long <= pullback_window)
    short_armed = decision_ok & short_cond & (prev_short <= pullback_window)
    expired = decision_ok & ((prev_long > pullback_window) | (prev_short > pullback_window))
    cleared = decision_ok & (long_live | short_live)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_setup", dtype=object)
    reason[cleared] = "trend_cleared"
    reason[expired] = "setup_expired"
    reason[short_armed] = "pullback_short"
    reason[long_armed] = "pullback_long"
    reason[short_entry] = "entry_short"
    reason[long_entry] = "entry_long"
    for i in np.flatnonzero(~decision_ok):
        lo = max(0, i - need + 1)
        defects = np.flatnonzero(row_bad[lo:i + 1])
        if len(defects):
            reason[i] = row_reason[lo + defects[-1]]
        elif order_bad[lo + 1:i + 1].any():
            reason[i] = "timestamp_order"
        elif cadence_bad[lo + 2:i + 1].any():
            reason[i] = "irregular_cadence"
        elif overflow_bad[max(0, i - pullback_window - 1):i + 1].any():
            reason[i] = "arithmetic_overflow"
        else:
            reason[i] = "insufficient_history"

    side = np.zeros(n, dtype="float64")
    age = np.full(n, -1.0)
    side[long_armed | long_entry] = 1.0
    side[short_armed | short_entry] = -1.0
    age[long_armed | long_entry] = prev_long[long_armed | long_entry].astype("float64")
    age[short_armed | short_entry] = prev_short[short_armed | short_entry].astype("float64")

    ema_valid = ema_ok & ema_finite
    values["erp_ema"] = np.where(ema_valid, ema, 0.0)
    values["erp_ema_prior"] = np.where(usable, ema_prior, 0.0)
    values["erp_slope"] = np.where(usable, slope, 0.0)
    values["erp_bull_power"] = np.where(usable, bull, 0.0)
    values["erp_bear_power"] = np.where(usable, bear, 0.0)
    values["erp_setup_side"] = side
    values["erp_setup_age"] = age
    values["erp_high"] = clean_high
    values["erp_low"] = clean_low
    values["erp_close"] = clean_close
    values["erp_eval_ts_ms"] = np.where(ts_missing, 0.0, ts)
    flags["erp_ema_valid"] = ema_valid
    flags["erp_slope_valid"] = usable
    flags["erp_valid"] = decision_ok
    return _attach(result, values, flags, reason, signal, *params)
