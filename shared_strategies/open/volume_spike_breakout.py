import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

NAME = "volume_spike_breakout"
PREFIX = "vsb_"
WINDOW_MIN = 2
WINDOW_MAX = 100
DEFAULT_PARAMS = {"lookback": 20, "vol_period": 20, "vol_multiplier": 2.0, "min_body_frac": 0.0}
PRICE_COLUMNS = ("open", "high", "low", "close")
REQUIRED_COLUMNS = PRICE_COLUMNS + ("volume",)

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_breakout": 0,
    "volume_below_threshold": 2,
    "body_below_threshold": 3,
    "breakout_continuation": 4,
    "short_suppressed_spot": 5,
    "both_sides_qualified": 6,
    "insufficient_history": 10,
    "missing_columns": 11,
    "nonfinite_input": 12,
    "nonpositive_price": 13,
    "invalid_volume": 14,
    "inconsistent_ohlc": 15,
    "missing_timestamp": 16,
    "timestamp_order": 17,
    "nonfinite_derived": 18,
}

_ROW_DEFECTS = ("nonfinite_input", "nonpositive_price", "invalid_volume", "inconsistent_ohlc", "missing_timestamp")


def _validate_window(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < WINDOW_MIN or as_int > WINDOW_MAX:
        raise ValueError(f"{NAME}: {name} must be in [{WINDOW_MIN}, {WINDOW_MAX}], got {value!r}")
    return as_int


def _validate_number(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"{NAME}: {name} must be finite, got {value!r}")
    return as_float


def validate_vsb_params(lookback, vol_period, vol_multiplier, min_body_frac) -> tuple:
    lookback = _validate_window("lookback", lookback)
    vol_period = _validate_window("vol_period", vol_period)
    vol_multiplier = _validate_number("vol_multiplier", vol_multiplier)
    if vol_multiplier < 1.0:
        raise ValueError(f"{NAME}: vol_multiplier must be >= 1, got {vol_multiplier!r}")
    min_body_frac = _validate_number("min_body_frac", min_body_frac)
    if min_body_frac < 0.0 or min_body_frac >= 1.0:
        raise ValueError(f"{NAME}: min_body_frac must be in [0, 1), got {min_body_frac!r}")
    return lookback, vol_period, vol_multiplier, min_body_frac


def vsb_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{NAME}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS))
    if unknown:
        raise ValueError(f"{NAME}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **params}
    validate_vsb_params(merged["lookback"], merged["vol_period"], merged["vol_multiplier"], merged["min_body_frac"])


def required_history_bars(lookback: int, vol_period: int) -> int:
    return max(int(lookback), int(vol_period)) + 2


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


def _window(values: np.ndarray, window: int, end_offset: int, op) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window + end_offset:
        return out
    with np.errstate(invalid="ignore", over="ignore"):
        reduced = op(sliding_window_view(values, window), axis=1)
    start = window - 1 + end_offset
    out[start:] = reduced[: n - start]
    return out


def _prior_sum(values: np.ndarray, window: int) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window + 1:
        return out
    acc = np.zeros(n - window)
    with np.errstate(invalid="ignore", over="ignore"):
        for k in range(window):
            acc = acc + values[k:n - window + k]
    out[window:] = acc
    return out


def _last_index(flags: np.ndarray) -> np.ndarray:
    idx = np.where(flags, np.arange(len(flags)), -1)
    return np.maximum.accumulate(idx) if len(idx) else idx


def _columns(result: pd.DataFrame, n: int, lookback: int, vol_period: int, vol_multiplier: float,
             min_body_frac: float, allow_short: bool) -> dict:
    nan = np.full(n, np.nan)
    return {
        "vsb_range_high": nan.copy(),
        "vsb_range_low": nan.copy(),
        "vsb_base_volume": nan.copy(),
        "vsb_rel_vol": nan.copy(),
        "vsb_body_frac": nan.copy(),
        "vsb_long_qualified": nan.copy(),
        "vsb_short_qualified": nan.copy(),
        "vsb_prev_long_qualified": nan.copy(),
        "vsb_prev_short_qualified": nan.copy(),
        "vsb_lookback": np.full(n, float(lookback)),
        "vsb_vol_period": np.full(n, float(vol_period)),
        "vsb_vol_multiplier": np.full(n, float(vol_multiplier)),
        "vsb_min_body_frac": np.full(n, float(min_body_frac)),
        "vsb_required_history": np.full(n, float(required_history_bars(lookback, vol_period))),
        "vsb_short_entries": np.full(n, 1.0 if allow_short else 0.0),
    }


def _write(result: pd.DataFrame, cols: dict, valid: np.ndarray, reason: np.ndarray, signal: np.ndarray):
    for key, values in cols.items():
        result[key] = values
    result["vsb_valid"] = valid
    result["vsb_reason"] = reason
    result["vsb_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result


def _evaluate(df: pd.DataFrame, lookback, vol_period, vol_multiplier, min_body_frac, *, allow_short: bool,
              volume_predicate: bool) -> pd.DataFrame:
    lookback, vol_period, vol_multiplier, min_body_frac = validate_vsb_params(
        lookback, vol_period, vol_multiplier, min_body_frac)
    result = df.copy()
    n = len(result)
    cols = _columns(result, n, lookback, vol_period, vol_multiplier, min_body_frac, allow_short)
    signal = np.zeros(n, dtype=np.int64)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _write(result, cols, np.zeros(n, dtype=bool), np.full(n, "missing_columns", dtype=object), signal)

    o, h, l, c, v = (pd.to_numeric(result[col], errors="coerce").to_numpy(dtype="float64")
                     for col in REQUIRED_COLUMNS)
    finite = np.isfinite(o) & np.isfinite(h) & np.isfinite(l) & np.isfinite(c) & np.isfinite(v)
    with np.errstate(invalid="ignore"):
        nonpositive_price = finite & ((o <= 0) | (h <= 0) | (l <= 0) | (c <= 0))
        bad_volume = finite & (v <= 0)
        inconsistent = finite & ((h < l) | (o > h) | (o < l) | (c > h) | (c < l))

    ts = _timestamps_ms(result)
    missing_ts = np.zeros(n, dtype=bool)
    bad_step = np.zeros(n, dtype=bool)
    if ts is not None:
        missing_ts = ~np.isfinite(ts)
        if n > 1:
            both = np.isfinite(ts[1:]) & np.isfinite(ts[:-1])
            with np.errstate(invalid="ignore"):
                bad_step[1:] = both & ~(ts[1:] > ts[:-1])

    row_reason = np.full(n, "", dtype=object)
    row_reason[missing_ts] = "missing_timestamp"
    row_reason[inconsistent] = "inconsistent_ohlc"
    row_reason[bad_volume] = "invalid_volume"
    row_reason[nonpositive_price] = "nonpositive_price"
    row_reason[~finite] = "nonfinite_input"
    row_bad = row_reason != ""
    row_ok = ~row_bad

    span = max(lookback, vol_period)
    last_row_bad = _last_index(row_bad)
    last_step_bad = _last_index(bad_step)
    idx = np.arange(n)
    with np.errstate(invalid="ignore"):
        rows_ok = (idx - span >= 0) & (last_row_bad < idx - span) & (last_step_bad < idx - span + 1)

    safe_h = np.where(row_ok, h, np.nan)
    safe_l = np.where(row_ok, l, np.nan)
    safe_v = np.where(row_ok, v, np.nan)
    range_high = _window(safe_h, lookback, 1, np.max)
    range_low = _window(safe_l, lookback, 1, np.min)
    with np.errstate(invalid="ignore", over="ignore"):
        base_volume = _prior_sum(safe_v, vol_period) / float(vol_period)
    with np.errstate(invalid="ignore", divide="ignore", over="ignore"):
        rel_vol = v / base_volume
        bar_range = h - l
        body_frac = np.where(bar_range > 0, np.abs(c - o) / np.where(bar_range > 0, bar_range, 1.0), np.nan)

    derived_ok = (np.isfinite(range_high) & np.isfinite(range_low) & np.isfinite(base_volume)
                  & (base_volume > 0) & np.isfinite(rel_vol))
    cond_ok = rows_ok & derived_ok

    with np.errstate(invalid="ignore"):
        up = cond_ok & (c > range_high)
        down = cond_ok & (c < range_low)
        volume_ok = (rel_vol > vol_multiplier) if volume_predicate else np.ones(n, dtype=bool)
        if min_body_frac > 0.0:
            body_ok = np.isfinite(body_frac) & (body_frac >= min_body_frac)
        else:
            body_ok = np.ones(n, dtype=bool)
    long_q = up & volume_ok & body_ok
    short_q = down & volume_ok & body_ok

    prev_ok = np.zeros(n, dtype=bool)
    prev_long = np.zeros(n, dtype=bool)
    prev_short = np.zeros(n, dtype=bool)
    if n > 1:
        prev_ok[1:] = cond_ok[:-1]
        prev_long[1:] = long_q[:-1]
        prev_short[1:] = short_q[:-1]

    valid = cond_ok & prev_ok
    long_edge = valid & long_q & ~prev_long
    short_edge = valid & short_q & ~prev_short
    both = long_edge & short_edge
    long_entry = long_edge & ~both
    short_edge_only = short_edge & ~both

    signal[long_entry] = 1
    if allow_short:
        signal[short_edge_only] = -1

    reason = np.full(n, "no_breakout", dtype=object)
    reason[valid & (up | down) & ~volume_ok] = "volume_below_threshold"
    reason[valid & (up | down) & volume_ok & ~body_ok] = "body_below_threshold"
    reason[valid & ((long_q & prev_long) | (short_q & prev_short))] = "breakout_continuation"
    reason[long_entry] = "entry_long"
    reason[short_edge_only] = "entry_short" if allow_short else "short_suppressed_spot"
    reason[both] = "both_sides_qualified"

    invalid = np.flatnonzero(~valid)
    for i in invalid:
        start = i - span - 1
        row_at = last_row_bad[i]
        step_at = last_step_bad[i]
        row_in = row_at >= max(start, 0)
        step_in = step_at >= max(start + 1, 1)
        if row_in and (not step_in or row_at >= step_at):
            reason[i] = row_reason[row_at]
        elif step_in:
            reason[i] = "timestamp_order"
        elif start < 0:
            reason[i] = "insufficient_history"
        else:
            reason[i] = "nonfinite_derived"

    shown = cond_ok
    cols["vsb_range_high"] = np.where(shown, range_high, np.nan)
    cols["vsb_range_low"] = np.where(shown, range_low, np.nan)
    cols["vsb_base_volume"] = np.where(shown, base_volume, np.nan)
    cols["vsb_rel_vol"] = np.where(shown, rel_vol, np.nan)
    cols["vsb_body_frac"] = np.where(shown, body_frac, np.nan)
    cols["vsb_long_qualified"] = np.where(shown, long_q.astype(float), np.nan)
    cols["vsb_short_qualified"] = np.where(shown, short_q.astype(float), np.nan)
    cols["vsb_prev_long_qualified"] = np.where(prev_ok, prev_long.astype(float), np.nan)
    cols["vsb_prev_short_qualified"] = np.where(prev_ok, prev_short.astype(float), np.nan)
    return _write(result, cols, valid, reason, signal)


def volume_spike_breakout_core(df: pd.DataFrame, lookback=20, vol_period=20, vol_multiplier=2.0,
                               min_body_frac=0.0, *, allow_short: bool) -> pd.DataFrame:
    if not isinstance(allow_short, bool):
        raise ValueError(f"{NAME}: allow_short must be True or False, got {allow_short!r}")
    return _evaluate(df, lookback, vol_period, vol_multiplier, min_body_frac, allow_short=allow_short,
                     volume_predicate=True)


def volume_spike_breakout_futures(df: pd.DataFrame, lookback=20, vol_period=20, vol_multiplier=2.0,
                                  min_body_frac=0.0) -> pd.DataFrame:
    return volume_spike_breakout_core(df, lookback, vol_period, vol_multiplier, min_body_frac, allow_short=True)


def volume_spike_breakout_spot(df: pd.DataFrame, lookback=20, vol_period=20, vol_multiplier=2.0,
                               min_body_frac=0.0) -> pd.DataFrame:
    return volume_spike_breakout_core(df, lookback, vol_period, vol_multiplier, min_body_frac, allow_short=False)
