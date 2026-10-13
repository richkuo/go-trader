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

NAME = "true_strength_index"
REQUIRED_COLUMNS = ("open", "high", "low", "close")
MAX_RECURSION_LENGTH = 198
MIN_RECURSION_MULTIPLE = 6
MIN_PERIOD = 2
CHUNK_ROWS = 4096

DEFAULT_PARAMS = {"slow_period": 25, "fast_period": 13, "signal_period": 7, "recursion_length": 180}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "centerline_disagrees": 2,
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
    "tsi_eval_ts_ms",
    "tsi_close",
    "tsi_prev_close",
    "tsi_seed_close",
    "tsi_prev_seed_close",
    "tsi_first_change",
    "tsi_prev_first_change",
    "tsi_slow_change",
    "tsi_prev_slow_change",
    "tsi_double_change",
    "tsi_prev_double_change",
    "tsi_slow_abs",
    "tsi_prev_slow_abs",
    "tsi_double_abs",
    "tsi_prev_double_abs",
    "tsi_signal_seed_change",
    "tsi_prev_signal_seed_change",
    "tsi_value",
    "tsi_prev_value",
    "tsi_signal_line",
    "tsi_prev_signal_line",
    "tsi_diff",
    "tsi_prev_diff",
)

_ENDPOINT_KEYS = ("slow_change", "double_change", "slow_abs", "double_abs", "signal_seed_change", "value",
                  "signal_line")


def _integer(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    return int(as_float)


def required_history_bars(recursion_length: int) -> int:
    return int(recursion_length) + 1


def validate_tsi_params(slow_period, fast_period, signal_period, recursion_length) -> tuple:
    slow = _integer("slow_period", slow_period)
    fast = _integer("fast_period", fast_period)
    sig = _integer("signal_period", signal_period)
    length = _integer("recursion_length", recursion_length)
    for key, value, raw in (("slow_period", slow, slow_period), ("fast_period", fast, fast_period),
                            ("signal_period", sig, signal_period)):
        if value < MIN_PERIOD:
            raise ValueError(f"{NAME}: {key} must be at least {MIN_PERIOD}, got {raw!r}")
    if fast >= slow:
        raise ValueError(f"{NAME}: fast_period must be below slow_period, got {fast} >= {slow}")
    if length > MAX_RECURSION_LENGTH:
        raise ValueError(f"{NAME}: recursion_length must be at most {MAX_RECURSION_LENGTH} so recursion_length + 1 "
                         f"fits the 199 closed rows of a 200-row check frame, got {recursion_length!r}")
    if length < MIN_RECURSION_MULTIPLE * slow:
        raise ValueError(f"{NAME}: recursion_length must be at least {MIN_RECURSION_MULTIPLE} * slow_period = "
                         f"{MIN_RECURSION_MULTIPLE * slow}, got {recursion_length!r}")
    if sig >= length:
        raise ValueError(f"{NAME}: signal_period must be below recursion_length, got {sig} >= {length}")
    return slow, fast, sig, length


def tsi_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{NAME}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS))
    if unknown:
        raise ValueError(f"{NAME}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **params}
    validate_tsi_params(merged["slow_period"], merged["fast_period"], merged["signal_period"],
                        merged["recursion_length"])


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


def endpoint_values(close: np.ndarray, ends: np.ndarray, slow_period: int, fast_period: int, signal_period: int,
                    length: int) -> dict:
    m = len(ends)
    out = {key: np.full(m, np.nan) for key in _ENDPOINT_KEYS}
    out["bad_denominator"] = np.zeros(m, dtype=bool)
    out["nonfinite"] = np.zeros(m, dtype=bool)
    if m == 0:
        return out
    a_slow = 2.0 / (slow_period + 1.0)
    a_fast = 2.0 / (fast_period + 1.0)
    a_sig = 2.0 / (signal_period + 1.0)
    offsets = np.arange(-(length - 1), 1)
    with np.errstate(over="ignore", invalid="ignore", divide="ignore", under="ignore"):
        for start in range(0, m, CHUNK_ROWS):
            rows = ends[start:start + CHUNK_ROWS]
            window = close[rows[:, None] + offsets]
            change = window[:, 1:] - window[:, :-1]
            k = len(rows)
            first = change[:, 0]
            slow = first.copy()
            fast = first.copy()
            slow_abs = np.abs(first)
            fast_abs = slow_abs.copy()
            line = np.full(k, np.nan)
            line_set = np.zeros(k, dtype=bool)
            seed_at = np.full(k, np.nan)
            nonfinite = np.zeros(k, dtype=bool)
            tsi = np.full(k, np.nan)
            for j in range(change.shape[1]):
                if j > 0:
                    d = change[:, j]
                    slow = slow + a_slow * (d - slow)
                    fast = fast + a_fast * (slow - fast)
                    slow_abs = slow_abs + a_slow * (np.abs(d) - slow_abs)
                    fast_abs = fast_abs + a_fast * (slow_abs - fast_abs)
                den_ok = np.isfinite(fast_abs) & (fast_abs > 0.0)
                tsi = np.where(den_ok, 100.0 * (fast / np.where(den_ok, fast_abs, 1.0)), np.nan)
                tsi_ok = np.isfinite(tsi)
                seeding = ~line_set & tsi_ok
                line = np.where(line_set, line + a_sig * (tsi - line), np.where(tsi_ok, tsi, np.nan))
                seed_at[seeding] = float(j)
                line_set |= tsi_ok
                nonfinite |= ~(np.isfinite(slow) & np.isfinite(fast) & np.isfinite(slow_abs) & np.isfinite(fast_abs))
            bad_den = ~(np.isfinite(fast_abs) & (fast_abs > 0.0))
            nonfinite |= ~(np.isfinite(tsi) & np.isfinite(line))
            sl = slice(start, start + k)
            out["slow_change"][sl] = slow
            out["double_change"][sl] = fast
            out["slow_abs"][sl] = slow_abs
            out["double_abs"][sl] = fast_abs
            out["signal_seed_change"][sl] = seed_at
            out["value"][sl] = tsi
            out["signal_line"][sl] = line
            out["bad_denominator"][sl] = bad_den
            out["nonfinite"][sl] = nonfinite & ~bad_den
    return out


def _attach(result: pd.DataFrame, values: dict, value_ok: np.ndarray, decision_ok: np.ndarray, reason: np.ndarray,
            signal: np.ndarray, params: tuple, interval) -> pd.DataFrame:
    n = len(result)
    slow, fast, sig, length = params
    for col in VALUE_COLUMNS:
        result[col] = values[col]
    result["tsi_interval_ms"] = np.full(n, float(interval) if interval is not None else np.nan)
    result["tsi_slow_period"] = np.full(n, float(slow))
    result["tsi_fast_period"] = np.full(n, float(fast))
    result["tsi_signal_period"] = np.full(n, float(sig))
    result["tsi_recursion_length"] = np.full(n, float(length))
    result["tsi_required_history"] = np.full(n, float(required_history_bars(length)))
    result["tsi_value_valid"] = value_ok.astype("float64")
    result["tsi_valid"] = decision_ok.astype("float64")
    result["tsi_reason"] = reason
    result["tsi_reason_code"] = np.array([float(REASON_CODES[r.split(":")[0]]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result


def _hold_all(result: pd.DataFrame, reason: str, params: tuple, interval) -> pd.DataFrame:
    n = len(result)
    values = {col: np.full(n, np.nan) for col in VALUE_COLUMNS}
    none = np.zeros(n, dtype=bool)
    return _attach(result, values, none, none, np.full(n, reason, dtype=object), np.zeros(n, dtype=np.int64),
                   params, interval)


def true_strength_index_core(
    df: pd.DataFrame,
    slow_period: int = 25,
    fast_period: int = 13,
    signal_period: int = 7,
    recursion_length: int = 180,
) -> pd.DataFrame:
    params = validate_tsi_params(slow_period, fast_period, signal_period, recursion_length)
    slow, fast, sig, length = params
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

    rows_clear = _window_clear(row_bad, length)
    edges_clear = np.zeros(n, dtype=bool)
    if n >= length:
        edges_clear[length - 1:] = _window_clear(edge_bad, length - 1)[length - 1:]
    inputs_ok = rows_clear & edges_clear

    ends = np.flatnonzero(inputs_ok)
    safe_close = np.where(row_bad, 1.0, close)
    computed = endpoint_values(safe_close, ends, slow, fast, sig, length)
    endpoint = {key: np.full(n, np.nan) for key in _ENDPOINT_KEYS}
    for key in _ENDPOINT_KEYS:
        endpoint[key][ends] = computed[key]
    bad_den = np.zeros(n, dtype=bool)
    nonfinite = np.zeros(n, dtype=bool)
    bad_den[ends] = computed["bad_denominator"]
    nonfinite[ends] = computed["nonfinite"]
    value_ok = inputs_ok & ~bad_den & ~nonfinite

    derived_reason = np.full(n, "", dtype=object)
    derived_reason[bad_den] = "invalid_denominator"
    derived_reason[nonfinite] = "nonfinite_derived"

    c = np.where(row_bad, np.nan, close)
    seed_close = np.full(n, np.nan)
    first_change = np.full(n, np.nan)
    if n >= length:
        seed_close[length - 1:] = c[:n - length + 1]
        first_change[length - 1:] = c[1:n - length + 2] - c[:n - length + 1]

    def lagged(values):
        out = np.full(n, np.nan)
        if n > 1:
            out[1:] = values[:-1]
        return out

    prev_ok = np.zeros(n, dtype=bool)
    if n > 1:
        prev_ok[1:] = value_ok[:-1]
    decision_ok = value_ok & prev_ok

    value = endpoint["value"]
    line = endpoint["signal_line"]
    prev_value = lagged(value)
    prev_line = lagged(line)
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
    reason[(cross_up & ~long_entry) | (cross_down & ~short_entry)] = "centerline_disagrees"
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

    def cur(values):
        return np.where(value_ok, values, np.nan)

    def prev(values):
        return np.where(prev_ok, lagged(values), np.nan)

    values = {
        "tsi_eval_ts_ms": eval_ts,
        "tsi_close": cur(c),
        "tsi_prev_close": prev(c),
        "tsi_seed_close": cur(seed_close),
        "tsi_prev_seed_close": prev(seed_close),
        "tsi_first_change": cur(first_change),
        "tsi_prev_first_change": prev(first_change),
        "tsi_slow_change": cur(endpoint["slow_change"]),
        "tsi_prev_slow_change": prev(endpoint["slow_change"]),
        "tsi_double_change": cur(endpoint["double_change"]),
        "tsi_prev_double_change": prev(endpoint["double_change"]),
        "tsi_slow_abs": cur(endpoint["slow_abs"]),
        "tsi_prev_slow_abs": prev(endpoint["slow_abs"]),
        "tsi_double_abs": cur(endpoint["double_abs"]),
        "tsi_prev_double_abs": prev(endpoint["double_abs"]),
        "tsi_signal_seed_change": cur(endpoint["signal_seed_change"]),
        "tsi_prev_signal_seed_change": prev(endpoint["signal_seed_change"]),
        "tsi_value": cur(value),
        "tsi_prev_value": np.where(prev_ok, prev_value, np.nan),
        "tsi_signal_line": cur(line),
        "tsi_prev_signal_line": np.where(prev_ok, prev_line, np.nan),
        "tsi_diff": cur(diff),
        "tsi_prev_diff": np.where(prev_ok, prev_diff, np.nan),
    }
    return _attach(result, values, value_ok, decision_ok, reason, signal, params, interval)
