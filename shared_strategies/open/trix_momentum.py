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
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)

NAME = "trix_momentum"
REQUIRED_COLUMNS = ("open", "high", "low", "close")
MAX_RECURSION_LENGTH = 197
MIN_RECURSION_MULTIPLE = 6
CHUNK_ROWS = 4096

DEFAULT_PARAMS = {"period": 18, "signal_period": 9, "recursion_length": 120}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "zero_line_disagrees": 2,
    "insufficient_history": 10,
    "missing_columns": 11,
    "missing_cadence": 12,
    "missing_timestamps": 13,
    "nonfinite_input": 14,
    "nonpositive_price": 15,
    "invalid_candle_geometry": 16,
    "timestamp_order": 17,
    "cadence_gap": 18,
    "invalid_denominator": 19,
    "nonfinite_derived": 20,
}

VALUE_COLUMNS = (
    "trix_eval_ts_ms",
    "trix_close",
    "trix_prev_close",
    "trix_seed_close",
    "trix_prev_seed_close",
    "trix_ema3",
    "trix_ema3_base",
    "trix_prev_ema3",
    "trix_prev_ema3_base",
    "trix_value",
    "trix_prev_value",
    "trix_signal_line",
    "trix_prev_signal_line",
    "trix_diff",
    "trix_prev_diff",
)


def _integer(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    return int(as_float)


def required_history_bars(recursion_length: int) -> int:
    return int(recursion_length) + 2


def validate_trix_params(period, signal_period, recursion_length) -> tuple:
    p = _integer("period", period)
    s = _integer("signal_period", signal_period)
    length = _integer("recursion_length", recursion_length)
    if p < 1:
        raise ValueError(f"{NAME}: period must be at least 1, got {period!r}")
    if s < 1:
        raise ValueError(f"{NAME}: signal_period must be at least 1, got {signal_period!r}")
    if length > MAX_RECURSION_LENGTH:
        raise ValueError(f"{NAME}: recursion_length must be at most {MAX_RECURSION_LENGTH} so recursion_length + 2 "
                         f"fits the 199 closed rows of a 200-row check frame, got {recursion_length!r}")
    if length < MIN_RECURSION_MULTIPLE * p:
        raise ValueError(f"{NAME}: recursion_length must be at least {MIN_RECURSION_MULTIPLE} * period = "
                         f"{MIN_RECURSION_MULTIPLE * p}, got {recursion_length!r}")
    if s >= length:
        raise ValueError(f"{NAME}: signal_period must be below recursion_length, got {s} >= {length}")
    return p, s, length


def trix_param_validator(params: dict) -> None:
    merged = {**DEFAULT_PARAMS, **(params or {})}
    validate_trix_params(merged["period"], merged["signal_period"], merged["recursion_length"])


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


def _endpoints(close: np.ndarray, ends: np.ndarray, period: int, signal_period: int, length: int) -> dict:
    m = len(ends)
    keys = ("ema3", "ema3_base", "value", "signal_line")
    out = {key: np.full(m, np.nan) for key in keys}
    out["bad_denominator"] = np.zeros(m, dtype=bool)
    out["nonfinite"] = np.zeros(m, dtype=bool)
    if m == 0:
        return out
    alpha = 2.0 / (period + 1.0)
    beta = 2.0 / (signal_period + 1.0)
    offsets = np.arange(-length, 1)
    with np.errstate(over="ignore", invalid="ignore", divide="ignore", under="ignore"):
        for start in range(0, m, CHUNK_ROWS):
            rows = ends[start:start + CHUNK_ROWS]
            window = close[rows[:, None] + offsets]
            k = len(rows)
            e1 = window[:, 0].copy()
            e2 = e1.copy()
            e3 = e1.copy()
            base = e3.copy()
            line = np.full(k, np.nan)
            osc = np.full(k, np.nan)
            bad_den = np.zeros(k, dtype=bool)
            nonfinite = np.zeros(k, dtype=bool)
            for j in range(1, length + 1):
                base = e3
                e1 = e1 + alpha * (window[:, j] - e1)
                e2 = e2 + alpha * (e1 - e2)
                e3 = e3 + alpha * (e2 - e3)
                den_ok = np.isfinite(base) & (base != 0.0)
                bad_den |= ~den_ok
                osc = 100.0 * (e3 - base) / np.where(den_ok, base, 1.0)
                if j == 1:
                    line = osc.copy()
                else:
                    line = line + beta * (osc - line)
                nonfinite |= ~(np.isfinite(e1) & np.isfinite(e2) & np.isfinite(e3) & np.isfinite(osc)
                               & np.isfinite(line))
            sl = slice(start, start + k)
            out["ema3"][sl] = e3
            out["ema3_base"][sl] = base
            out["value"][sl] = osc
            out["signal_line"][sl] = line
            out["bad_denominator"][sl] = bad_den
            out["nonfinite"][sl] = nonfinite & ~bad_den
    return out


def _attach(result: pd.DataFrame, values: dict, value_ok: np.ndarray, decision_ok: np.ndarray, reason: np.ndarray,
            signal: np.ndarray, params: tuple, interval) -> pd.DataFrame:
    n = len(result)
    p, s, length = params
    for col in VALUE_COLUMNS:
        result[col] = values[col]
    result["trix_interval_ms"] = np.full(n, float(interval) if interval is not None else np.nan)
    result["trix_period"] = np.full(n, float(p))
    result["trix_signal_period"] = np.full(n, float(s))
    result["trix_recursion_length"] = np.full(n, float(length))
    result["trix_required_history"] = np.full(n, float(required_history_bars(length)))
    result["trix_value_valid"] = value_ok.astype("float64")
    result["trix_valid"] = decision_ok.astype("float64")
    result["trix_reason"] = reason
    result["trix_reason_code"] = np.array([float(REASON_CODES[r.split(":")[0]]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result


def _hold_all(result: pd.DataFrame, reason: str, params: tuple, interval) -> pd.DataFrame:
    n = len(result)
    values = {col: np.full(n, np.nan) for col in VALUE_COLUMNS}
    none = np.zeros(n, dtype=bool)
    return _attach(result, values, none, none, np.full(n, reason, dtype=object), np.zeros(n, dtype=np.int64),
                   params, interval)


def trix_momentum_core(
    df: pd.DataFrame,
    period: int = 18,
    signal_period: int = 9,
    recursion_length: int = 120,
) -> pd.DataFrame:
    params = validate_trix_params(period, signal_period, recursion_length)
    p, s, length = params
    result = df.copy()
    n = len(result)
    interval, kind = frame_cadence(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _hold_all(result, "missing_columns:" + ",".join(missing), params, interval)
    if interval is None:
        return _hold_all(result, "missing_cadence", params, None)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, "missing_timestamps", params, interval)

    open_ = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")

    ts_ok = np.isfinite(ts)
    finite = np.isfinite(open_) & np.isfinite(high) & np.isfinite(low) & np.isfinite(close)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ((open_ <= 0) | (high <= 0) | (low <= 0) | (close <= 0))
        geometry = finite & ~nonpositive & (
            (high < low) | (open_ > high) | (open_ < low) | (close > high) | (close < low))

    row_reason = np.full(n, "", dtype=object)
    row_reason[geometry] = "invalid_candle_geometry"
    row_reason[nonpositive] = "nonpositive_price"
    row_reason[~finite] = "nonfinite_input"
    row_reason[~ts_ok] = "missing_timestamps"
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

    run_rows = length + 1
    rows_clear = _window_clear(row_bad, run_rows)
    edges_clear = np.zeros(n, dtype=bool)
    if n >= run_rows:
        edges_clear[run_rows - 1:] = _window_clear(edge_bad, length)[run_rows - 1:]
    inputs_ok = rows_clear & edges_clear

    ends = np.flatnonzero(inputs_ok)
    safe_close = np.where(row_bad, 1.0, close)
    computed = _endpoints(safe_close, ends, p, s, length)
    ema3 = np.full(n, np.nan)
    ema3_base = np.full(n, np.nan)
    value = np.full(n, np.nan)
    line = np.full(n, np.nan)
    bad_den = np.zeros(n, dtype=bool)
    nonfinite = np.zeros(n, dtype=bool)
    ema3[ends] = computed["ema3"]
    ema3_base[ends] = computed["ema3_base"]
    value[ends] = computed["value"]
    line[ends] = computed["signal_line"]
    bad_den[ends] = computed["bad_denominator"]
    nonfinite[ends] = computed["nonfinite"]
    value_ok = inputs_ok & ~bad_den & ~nonfinite

    derived_reason = np.full(n, "", dtype=object)
    derived_reason[bad_den] = "invalid_denominator"
    derived_reason[nonfinite] = "nonfinite_derived"

    prev_ok = np.zeros(n, dtype=bool)
    prev_value = np.full(n, np.nan)
    prev_line = np.full(n, np.nan)
    prev_ema3 = np.full(n, np.nan)
    prev_ema3_base = np.full(n, np.nan)
    prev_close = np.full(n, np.nan)
    seed_close = np.full(n, np.nan)
    prev_seed_close = np.full(n, np.nan)
    c = np.where(row_bad, np.nan, close)
    if n > 1:
        prev_ok[1:] = value_ok[:-1]
        prev_value[1:] = value[:-1]
        prev_line[1:] = line[:-1]
        prev_ema3[1:] = ema3[:-1]
        prev_ema3_base[1:] = ema3_base[:-1]
        prev_close[1:] = c[:-1]
    if n > length:
        seed_close[length:] = c[:-length]
    if n > length + 1:
        prev_seed_close[length + 1:] = c[:-(length + 1)]
    decision_ok = value_ok & prev_ok

    with np.errstate(invalid="ignore", over="ignore"):
        diff = value - line
        prev_diff = prev_value - prev_line
        cross_up = decision_ok & (diff > 0.0) & (prev_diff <= 0.0)
        cross_down = decision_ok & (diff < 0.0) & (prev_diff >= 0.0)
        long_entry = cross_up & (value > 0.0)
        short_entry = cross_down & (value < 0.0)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[(cross_up & ~long_entry) | (cross_down & ~short_entry)] = "zero_line_disagrees"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"
    need = required_history_bars(length)
    for i in np.flatnonzero(~decision_ok):
        lo = i - need + 1
        found = [hit for hit in (
            _last_defect(row_reason, lo, i),
            _last_defect(edge_reason, lo + 1, i),
        ) if hit is not None]
        if found:
            reason[i] = max(found, key=lambda hit: hit[0])[1]
        elif i < need - 1:
            reason[i] = "insufficient_history"
        else:
            derived = [hit for hit in (_last_defect(derived_reason, i - 1, i),) if hit is not None]
            reason[i] = derived[0][1] if derived else "nonfinite_derived"

    eval_ts = np.full(n, np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        eval_ts = np.where(ts_ok, ts, np.nan)
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        eval_ts = np.where(ts_ok, ts - float(interval) + 1.0, np.nan)

    values = {
        "trix_eval_ts_ms": eval_ts,
        "trix_close": np.where(value_ok, c, np.nan),
        "trix_prev_close": np.where(prev_ok, prev_close, np.nan),
        "trix_seed_close": np.where(value_ok, seed_close, np.nan),
        "trix_prev_seed_close": np.where(prev_ok, prev_seed_close, np.nan),
        "trix_ema3": np.where(value_ok, ema3, np.nan),
        "trix_ema3_base": np.where(value_ok, ema3_base, np.nan),
        "trix_prev_ema3": np.where(prev_ok, prev_ema3, np.nan),
        "trix_prev_ema3_base": np.where(prev_ok, prev_ema3_base, np.nan),
        "trix_value": np.where(value_ok, value, np.nan),
        "trix_prev_value": np.where(prev_ok, prev_value, np.nan),
        "trix_signal_line": np.where(value_ok, line, np.nan),
        "trix_prev_signal_line": np.where(prev_ok, prev_line, np.nan),
        "trix_diff": np.where(value_ok, diff, np.nan),
        "trix_prev_diff": np.where(prev_ok, prev_diff, np.nan),
    }
    return _attach(result, values, value_ok, decision_ok, reason, signal, params, interval)
