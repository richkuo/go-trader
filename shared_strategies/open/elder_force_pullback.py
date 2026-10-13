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
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)
from indicators_core import fixed_length_ema

NAME = "elder_force_pullback"
REQUIRED_COLUMNS = ("open", "high", "low", "close", "volume")
MAX_REQUIRED_HISTORY = 199
MAX_RECURSION_LENGTH = 197
RECURSION_MULTIPLE = 10

DEFAULT_PARAMS = {
    "force_period": 13,
    "trend_period": 50,
    "slope_span": 5,
    "recursion_length": 150,
    "volume_weighting": 1,
}

INT_MINIMUMS = {
    "force_period": 2,
    "trend_period": 2,
    "slope_span": 1,
    "recursion_length": 1,
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "zero_force": 2,
    "trend_unconfirmed": 3,
    "volume_ineligible": 4,
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
    "nonfinite_derived": 21,
}

VALUE_COLUMNS = (
    "efp_close", "efp_prev_close", "efp_volume", "efp_prev_volume", "efp_weight",
    "efp_raw_force", "efp_prev_raw_force", "efp_seed_force", "efp_prev_seed_force",
    "efp_force", "efp_prev_force",
    "efp_trend_sma", "efp_trend_sma_lag", "efp_prev_trend_sma", "efp_prev_trend_sma_lag",
    "efp_uptrend", "efp_prev_uptrend", "efp_downtrend", "efp_prev_downtrend",
    "efp_cross", "efp_volume_eligible",
)

PARAM_COLUMNS = ("force_period", "trend_period", "slope_span", "recursion_length", "volume_weighting",
                 "required_history")


def _int_param(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    return int(as_float)


def required_history_bars(trend_period: int, slope_span: int, recursion_length: int) -> int:
    return max(int(recursion_length) + 2, int(trend_period) + int(slope_span) + 1)


def validate_efp_params(force_period, trend_period, slope_span, recursion_length, volume_weighting) -> dict:
    raw = {"force_period": force_period, "trend_period": trend_period, "slope_span": slope_span,
           "recursion_length": recursion_length}
    out = {}
    for key, value in raw.items():
        as_int = _int_param(key, value)
        if as_int < INT_MINIMUMS[key]:
            raise ValueError(f"{NAME}: {key} must be >= {INT_MINIMUMS[key]}, got {value!r}")
        out[key] = as_int
    flag = _int_param("volume_weighting", volume_weighting)
    if flag not in (0, 1):
        raise ValueError(f"{NAME}: volume_weighting must be 0 or 1, got {volume_weighting!r}")
    out["volume_weighting"] = flag
    if out["recursion_length"] < RECURSION_MULTIPLE * out["force_period"]:
        raise ValueError(f"{NAME}: recursion_length must be >= {RECURSION_MULTIPLE} x force_period "
                         f"({RECURSION_MULTIPLE * out['force_period']}), got {recursion_length!r}")
    if out["recursion_length"] > MAX_RECURSION_LENGTH:
        raise ValueError(f"{NAME}: recursion_length must be <= {MAX_RECURSION_LENGTH}, got {recursion_length!r}")
    need = required_history_bars(out["trend_period"], out["slope_span"], out["recursion_length"])
    if need > MAX_REQUIRED_HISTORY:
        raise ValueError(
            f"{NAME}: required history max(recursion_length + 2, trend_period + slope_span + 1) = {need} rows "
            f"exceeds {MAX_REQUIRED_HISTORY}, the closed portion of a 200-row check frame")
    out["required_history"] = need
    return out


def efp_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{NAME}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS))
    if unknown:
        raise ValueError(f"{NAME}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **params}
    validate_efp_params(**merged)


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
    if length <= 0:
        out[:] = True
        return out
    if n < length:
        return out
    csum = np.concatenate(([0], np.cumsum(bad.astype(np.int64))))
    ends = np.arange(length - 1, n)
    out[ends] = (csum[ends + 1] - csum[ends + 1 - length]) == 0
    return out


def _last_defect(reasons: np.ndarray, lo: int, hi: int):
    lo = max(lo, 0)
    if hi < lo:
        return None
    hits = np.flatnonzero(reasons[lo:hi + 1] != "")
    if len(hits) == 0:
        return None
    pos = lo + int(hits[-1])
    return pos, reasons[pos]


def _trailing_mean(values: np.ndarray, length: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < length:
        return out
    k = np.arange(length - 1, n)
    with np.errstate(over="ignore", invalid="ignore"):
        total = values[k - length + 1].copy()
        for j in range(1, length):
            total = total + values[k - length + 1 + j]
        out[k] = total / float(length)
    return out


def _lag(values: np.ndarray, by: int) -> np.ndarray:
    out = np.full(len(values), np.nan)
    if len(values) > by:
        out[by:] = values[:-by]
    return out


def _attach(result: pd.DataFrame, values: dict, valid: np.ndarray, reason: np.ndarray, signal: np.ndarray,
            params: dict, interval, eval_ts: np.ndarray) -> pd.DataFrame:
    n = len(result)
    result["efp_eval_ts_ms"] = eval_ts
    for col in VALUE_COLUMNS:
        result[col] = values[col]
    result["efp_interval_ms"] = np.full(n, float(interval) if interval is not None else np.nan)
    for key in PARAM_COLUMNS:
        result[f"efp_{key}"] = np.full(n, float(params[key]))
    result["efp_valid"] = valid.astype("float64")
    result["efp_reason"] = reason
    result["efp_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result


def _hold_all(result: pd.DataFrame, reason: str, params: dict, interval) -> pd.DataFrame:
    n = len(result)
    values = {col: np.full(n, np.nan) for col in VALUE_COLUMNS}
    return _attach(result, values, np.zeros(n, dtype=bool), np.full(n, reason, dtype=object),
                   np.zeros(n, dtype=np.int64), params, interval, np.full(n, np.nan))


def elder_force_pullback_core(
    df: pd.DataFrame,
    force_period: int = 13,
    trend_period: int = 50,
    slope_span: int = 5,
    recursion_length: int = 150,
    volume_weighting: int = 1,
) -> pd.DataFrame:
    params = validate_efp_params(force_period, trend_period, slope_span, recursion_length, volume_weighting)
    period = params["force_period"]
    trend = params["trend_period"]
    span = params["slope_span"]
    length = params["recursion_length"]
    weighted = params["volume_weighting"] == 1
    need = params["required_history"]

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
        tail[~both] = "missing_timestamps"
        edge_reason[1:] = tail
    edge_bad = edge_reason != ""

    c = np.where(row_bad, np.nan, close)
    v = np.where(row_bad, np.nan, volume)
    weight = v if weighted else np.where(row_bad, np.nan, 1.0)
    prev_c = _lag(c, 1)
    with np.errstate(over="ignore", invalid="ignore"):
        raw_force = (c - prev_c) * weight
    force = fixed_length_ema(raw_force, period, length)
    prev_force = _lag(force, 1)
    seed_force = _lag(raw_force, length - 1)
    prev_seed_force = _lag(raw_force, length)

    sma = _trailing_mean(c, trend)
    sma_lag = _lag(sma, span)
    prev_sma = _lag(sma, 1)
    prev_sma_lag = _lag(sma, span + 1)
    prev_v = _lag(v, 1)

    with np.errstate(invalid="ignore"):
        up_now = (c > sma) & (sma > sma_lag)
        up_prev = (prev_c > prev_sma) & (prev_sma > prev_sma_lag)
        down_now = (c < sma) & (sma < sma_lag)
        down_prev = (prev_c < prev_sma) & (prev_sma < prev_sma_lag)
        cross_up = (prev_force < 0.0) & (force > 0.0)
        cross_down = (prev_force > 0.0) & (force < 0.0)
        zero_force = (force == 0.0) | (prev_force == 0.0)
        volume_ok = (v > 0.0) & (prev_v > 0.0)

    rows_ok = _window_clear(row_bad, need)
    edges_ok = _window_clear(edge_bad, need - 1)
    window_ok = rows_ok & edges_ok
    if n:
        window_ok[:min(need - 1, n)] = False
    derived_ok = (np.isfinite(force) & np.isfinite(prev_force) & np.isfinite(sma) & np.isfinite(sma_lag)
                  & np.isfinite(prev_sma) & np.isfinite(prev_sma_lag) & np.isfinite(c) & np.isfinite(prev_c))
    decision_ok = window_ok & derived_ok

    trend_long = up_now & up_prev
    trend_short = down_now & down_prev
    long_entry = decision_ok & cross_up & trend_long & volume_ok
    short_entry = decision_ok & cross_down & trend_short & volume_ok
    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    crossed = cross_up | cross_down
    trend_ok = (cross_up & trend_long) | (cross_down & trend_short)
    reason = np.full(n, "no_cross", dtype=object)
    reason[~crossed & zero_force] = "zero_force"
    reason[crossed & ~trend_ok] = "trend_unconfirmed"
    reason[crossed & trend_ok & ~volume_ok] = "volume_ineligible"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"
    for i in np.flatnonzero(~decision_ok):
        lo = i - need + 1
        found = [hit for hit in (_last_defect(row_reason, lo, i), _last_defect(edge_reason, lo + 1, i))
                 if hit is not None]
        if found:
            reason[i] = max(found, key=lambda hit: hit[0])[1]
        elif i < need - 1:
            reason[i] = "insufficient_history"
        else:
            reason[i] = "nonfinite_derived"

    eval_ts = np.full(n, np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        eval_ts = np.where(ts_ok, ts, np.nan)
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        eval_ts = np.where(ts_ok, ts - float(interval) + 1.0, np.nan)

    keep = decision_ok

    def masked(values):
        return np.where(keep, values, np.nan)

    cross = np.zeros(n)
    cross[cross_up] = 1.0
    cross[cross_down] = -1.0
    values = {
        "efp_close": masked(c),
        "efp_prev_close": masked(prev_c),
        "efp_volume": masked(v),
        "efp_prev_volume": masked(prev_v),
        "efp_weight": masked(weight),
        "efp_raw_force": masked(raw_force),
        "efp_prev_raw_force": masked(_lag(raw_force, 1)),
        "efp_seed_force": masked(seed_force),
        "efp_prev_seed_force": masked(prev_seed_force),
        "efp_force": masked(force),
        "efp_prev_force": masked(prev_force),
        "efp_trend_sma": masked(sma),
        "efp_trend_sma_lag": masked(sma_lag),
        "efp_prev_trend_sma": masked(prev_sma),
        "efp_prev_trend_sma_lag": masked(prev_sma_lag),
        "efp_uptrend": masked(up_now.astype("float64")),
        "efp_prev_uptrend": masked(up_prev.astype("float64")),
        "efp_downtrend": masked(down_now.astype("float64")),
        "efp_prev_downtrend": masked(down_prev.astype("float64")),
        "efp_cross": masked(cross),
        "efp_volume_eligible": masked(volume_ok.astype("float64")),
    }
    return _attach(result, values, decision_ok, reason, signal, params, interval, eval_ts)
