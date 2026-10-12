import math
import os
import sys
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

_TOOLS_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "shared_tools"))
if _TOOLS_DIR not in sys.path:
    sys.path.append(_TOOLS_DIR)

from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)

NAME = "klinger_divergence"
FAST_MIN, FAST_MAX = 2, 120
SLOW_MIN, SLOW_MAX = 3, 150
SIGNAL_MIN, SIGNAL_MAX = 2, 150
REPLAY_MIN, REPLAY_MAX = 4, 190
SPAN_MIN, SPAN_MAX = 1, 10
SEPARATION_MIN, SEPARATION_MAX = 1, 100
EXPIRY_MIN, EXPIRY_MAX = 1, 50
MAX_SUPPORT_BARS = 200
REQUIRED_COLUMNS = ("open", "high", "low", "close", "volume")
CHUNK_ROWS = 2048

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_setup": 0,
    "setup_armed": 2,
    "awaiting_cross": 3,
    "setup_consumed": 4,
    "opposite_events": 5,
    "no_pivot_pair": 6,
    "no_divergence": 7,
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
    "zero_cm": 20,
    "nonfinite_derived": 21,
    "decision_time_missing": 22,
    "decision_bar_stale": 23,
    "decision_bar_future": 24,
    "decision_boundary_mismatch": 25,
}

STATE_NONE = 0
STATE_EMITTED = 1
STATE_ARMED = 2
STATE_AWAITING = 3
STATE_CONSUMED = 4
STATE_NO_PAIR = 6
STATE_NO_DIVERGENCE = 7
STATE_UNOBSERVABLE = 9

ENDPOINT_COLUMNS = (
    "klinger_trend", "klinger_dm", "klinger_cm", "klinger_volume_force", "klinger_fast_ema",
    "klinger_slow_ema", "klinger_oscillator", "klinger_signal_line",
)

SIDE_FIELDS = (
    "state", "armed", "pivot1_ts_ms", "pivot1_price", "pivot1_oscillator", "pivot2_ts_ms", "pivot2_price",
    "pivot2_oscillator", "confirm_ts_ms", "expiry_ts_ms",
)

PARAM_NAMES = (
    "fast_period", "slow_period", "signal_period", "replay_bars", "left_span", "right_span",
    "min_separation", "max_separation", "setup_expiry", "divergence_test",
)


def _integer(name: str, value, lo: int, hi: int) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{NAME}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def support_bars(replay_bars, left_span, max_separation, right_span, setup_expiry) -> int:
    return int(replay_bars) + int(left_span) + int(max_separation) + int(right_span) + int(setup_expiry) + 2


def validate_klinger_params(fast_period, slow_period, signal_period, replay_bars, left_span, right_span,
                            min_separation, max_separation, setup_expiry, divergence_test) -> dict:
    fast = _integer("fast_period", fast_period, FAST_MIN, FAST_MAX)
    slow = _integer("slow_period", slow_period, SLOW_MIN, SLOW_MAX)
    signal = _integer("signal_period", signal_period, SIGNAL_MIN, SIGNAL_MAX)
    replay = _integer("replay_bars", replay_bars, REPLAY_MIN, REPLAY_MAX)
    if fast >= slow:
        raise ValueError(f"{NAME}: fast_period must be below slow_period, got {fast} and {slow}")
    if slow >= replay:
        raise ValueError(f"{NAME}: slow_period must be below replay_bars, got {slow} and {replay}")
    if signal >= replay:
        raise ValueError(f"{NAME}: signal_period must be below replay_bars, got {signal} and {replay}")
    left = _integer("left_span", left_span, SPAN_MIN, SPAN_MAX)
    right = _integer("right_span", right_span, SPAN_MIN, SPAN_MAX)
    min_sep = _integer("min_separation", min_separation, SEPARATION_MIN, SEPARATION_MAX)
    max_sep = _integer("max_separation", max_separation, SEPARATION_MIN, SEPARATION_MAX)
    if min_sep > max_sep:
        raise ValueError(f"{NAME}: min_separation must be <= max_separation, got {min_sep} and {max_sep}")
    expiry = _integer("setup_expiry", setup_expiry, EXPIRY_MIN, EXPIRY_MAX)
    if not isinstance(divergence_test, (bool, np.bool_)):
        raise ValueError(f"{NAME}: divergence_test must be a boolean, got {divergence_test!r}")
    support = support_bars(replay, left, max_sep, right, expiry)
    if support > MAX_SUPPORT_BARS:
        raise ValueError(
            f"{NAME}: replay_bars + left_span + max_separation + right_span + setup_expiry + 2 must be "
            f"<= {MAX_SUPPORT_BARS}, got {support}")
    return {
        "fast_period": fast, "slow_period": slow, "signal_period": signal, "replay_bars": replay,
        "left_span": left, "right_span": right, "min_separation": min_sep, "max_separation": max_sep,
        "setup_expiry": expiry, "divergence_test": bool(divergence_test), "support": support,
    }


def klinger_param_validator(params: dict) -> None:
    params = params or {}
    validate_klinger_params(*(params.get(k) for k in PARAM_NAMES))


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
    csum = np.concatenate([[0], np.cumsum(bad.astype(np.int64))])
    ends = np.arange(length - 1, n)
    out[ends] = (csum[ends + 1] - csum[ends + 1 - length]) == 0
    return out


def _last_defect(reasons: np.ndarray, lo: int, hi: int):
    if hi < lo:
        return None
    hits = np.flatnonzero(reasons[lo:hi + 1] != "")
    if len(hits) == 0:
        return None
    pos = lo + int(hits[-1])
    return pos, reasons[pos]


def replay_endpoints(high: np.ndarray, low: np.ndarray, close: np.ndarray, volume: np.ndarray, ends: np.ndarray,
                     fast: int, slow: int, signal: int, replay: int) -> dict:
    m = len(ends)
    out = {key: np.full(m, np.nan) for key in ENDPOINT_COLUMNS}
    out["zero_cm"] = np.zeros(m, dtype=bool)
    if m == 0:
        return out
    alpha_fast = 2.0 / (fast + 1)
    alpha_slow = 2.0 / (slow + 1)
    alpha_signal = 2.0 / (signal + 1)
    offsets = np.arange(-replay, 1)
    hlc_all = (high + low) + close
    dm_all = high - low
    with np.errstate(invalid="ignore", divide="ignore", over="ignore"):
        for start in range(0, m, CHUNK_ROWS):
            rows = ends[start:start + CHUNK_ROWS]
            idx = rows[:, None] + offsets
            dm_w = dm_all[idx]
            hlc_w = hlc_all[idx]
            vol_w = volume[idx]
            k = len(rows)
            trend = np.full(k, -1.0)
            dm = dm_w[:, 0]
            cm = dm.copy()
            bad = ~(cm > 0)
            zero = bad.copy()
            ratio = np.where(bad, 0.0, dm / np.where(bad, 1.0, cm))
            vf = vol_w[:, 0] * 2.0 * (ratio - 1.0) * trend * 100.0
            fast_ema = vf.copy()
            slow_ema = vf.copy()
            osc = fast_ema - slow_ema
            sig_line = osc.copy()
            for j in range(1, replay + 1):
                new_trend = np.where(hlc_w[:, j] > hlc_w[:, j - 1], 1.0, -1.0)
                dm = dm_w[:, j]
                cm = np.where(new_trend == trend, cm + dm, dm_w[:, j - 1] + dm)
                trend = new_trend
                bad = ~(cm > 0)
                zero |= bad
                ratio = np.where(bad, 0.0, dm / np.where(bad, 1.0, cm))
                vf = vol_w[:, j] * 2.0 * (ratio - 1.0) * trend * 100.0
                fast_ema = alpha_fast * vf + (1.0 - alpha_fast) * fast_ema
                slow_ema = alpha_slow * vf + (1.0 - alpha_slow) * slow_ema
                osc = fast_ema - slow_ema
                sig_line = alpha_signal * osc + (1.0 - alpha_signal) * sig_line
            sl = slice(start, start + k)
            out["klinger_trend"][sl] = trend
            out["klinger_dm"][sl] = dm
            out["klinger_cm"][sl] = cm
            out["klinger_volume_force"][sl] = vf
            out["klinger_fast_ema"][sl] = fast_ema
            out["klinger_slow_ema"][sl] = slow_ema
            out["klinger_oscillator"][sl] = osc
            out["klinger_signal_line"][sl] = sig_line
            out["zero_cm"][sl] = zero
    return out


def _strict_pivots(values: np.ndarray, row_ok: np.ndarray, left: int, right: int, lows: bool) -> np.ndarray:
    n = len(values)
    width = left + right + 1
    out = np.zeros(n, dtype=bool)
    if n < width:
        return out
    win = sliding_window_view(values, width)
    ok = sliding_window_view(row_ok, width).all(axis=1)
    centre = win[:, left]
    with np.errstate(invalid="ignore"):
        if lows:
            strict = (centre < win[:, :left].min(axis=1)) & (centre < win[:, left + 1:].min(axis=1))
        else:
            strict = (centre > win[:, :left].max(axis=1)) & (centre > win[:, left + 1:].max(axis=1))
    out[left:n - right] = ok & strict
    return out


def _side_setups(pivot_flags, prices, osc, osc_ok, cross, p, lows_side, n):
    right = p["right_span"]
    min_sep, max_sep = p["min_separation"], p["max_separation"]
    expiry = p["setup_expiry"]
    want = 1 if lows_side else -1
    pivots = np.flatnonzero(pivot_flags)
    setups = []
    for idx, q in enumerate(pivots):
        q = int(q)
        c = q + right
        rec = {"q": q, "c": c, "p": None, "state": STATE_NO_PAIR, "armed": False, "emit": -1}
        j = int(np.searchsorted(pivots, q - min_sep, side="right")) - 1
        if j >= 0 and int(pivots[j]) >= q - max_sep:
            pv = int(pivots[j])
            rec["p"] = pv
            if not (osc_ok[pv] and osc_ok[q]):
                rec["state"] = STATE_UNOBSERVABLE
            else:
                if p["divergence_test"]:
                    if lows_side:
                        diverges = prices[q] < prices[pv] and osc[q] > osc[pv]
                    else:
                        diverges = prices[q] > prices[pv] and osc[q] < osc[pv]
                else:
                    diverges = True
                if diverges:
                    rec["armed"] = True
                    rec["state"] = STATE_ARMED
                else:
                    rec["state"] = STATE_NO_DIVERGENCE
        if rec["armed"]:
            nxt = int(pivots[idx + 1]) + right if idx + 1 < len(pivots) else n
            last = min(c + expiry, nxt - 1, n - 1)
            if last >= c + 1:
                hits = cross[c + 1:last + 1] == want
                if hits.any():
                    rec["emit"] = c + 1 + int(np.argmax(hits))
        setups.append(rec)
    return setups


def _side_rows(setups, n, eval_ts, interval, prices, osc, expiry):
    cols = {f: np.full(n, np.nan) for f in SIDE_FIELDS}
    cols["state"] = np.zeros(n, dtype=np.int64)
    cols["armed"] = np.zeros(n, dtype="float64")
    if not setups:
        return cols
    confirms = np.array([s["c"] for s in setups], dtype=np.int64)
    which = np.searchsorted(confirms, np.arange(n), side="right") - 1
    for t in np.flatnonzero(which >= 0):
        s = setups[which[t]]
        c = s["c"]
        if t - c > expiry:
            continue
        if s["armed"]:
            if s["emit"] == t:
                state = STATE_EMITTED
            elif c < s["emit"] < t:
                state = STATE_CONSUMED
            elif t == c:
                state = STATE_ARMED
            else:
                state = STATE_AWAITING
            cols["armed"][t] = 1.0
        else:
            state = s["state"]
        cols["state"][t] = state
        q, pv = s["q"], s["p"]
        cols["pivot2_ts_ms"][t] = eval_ts[q]
        cols["pivot2_price"][t] = prices[q]
        cols["pivot2_oscillator"][t] = osc[q]
        if pv is not None:
            cols["pivot1_ts_ms"][t] = eval_ts[pv]
            cols["pivot1_price"][t] = prices[pv]
            cols["pivot1_oscillator"][t] = osc[pv]
        cols["confirm_ts_ms"][t] = eval_ts[c]
        cols["expiry_ts_ms"][t] = eval_ts[c] + float(expiry * interval)
    return cols


def _decision_time(attrs: dict):
    if FRAME_DECISION_TIME_ATTR not in attrs:
        return None, None, False
    raw = attrs.get(FRAME_DECISION_TIME_ATTR)
    cutoff = boundary = None
    if isinstance(raw, dict):
        for key in ("cutoff_ms", "boundary_ms"):
            value = raw.get(key)
            if isinstance(value, (int, np.integer)) and not isinstance(value, (bool, np.bool_)) and int(value) > 0:
                if key == "cutoff_ms":
                    cutoff = int(value)
                else:
                    boundary = int(value)
    return cutoff, boundary, True


def _attach_params(result: pd.DataFrame, p: dict, interval) -> None:
    n = len(result)
    result["klinger_interval_ms"] = np.full(n, float(interval) if interval else np.nan)
    for key in PARAM_NAMES:
        result[f"klinger_{key}"] = np.full(n, float(p[key]))
    result["klinger_support_bars"] = np.full(n, float(p["support"]))


def _write(result, *, p, interval, eval_ts, replay_start, support_start, endpoint, endpoint_ok, prev_osc,
           prev_sig, cross, sides, valid, cutoff_col, boundary_col, reason, signal):
    n = len(result)
    result["klinger_eval_ts_ms"] = eval_ts
    result["klinger_replay_start_ts_ms"] = replay_start
    result["klinger_support_start_ts_ms"] = support_start
    for col in ENDPOINT_COLUMNS:
        result[col] = endpoint[col]
    result["klinger_prev_oscillator"] = prev_osc
    result["klinger_prev_signal_line"] = prev_sig
    result["klinger_cross"] = cross
    result["klinger_endpoint_valid"] = endpoint_ok.astype("float64")
    for side, cols in sides.items():
        for field in SIDE_FIELDS:
            result[f"klinger_{side}_{field}"] = cols[field] if field != "state" else cols[field].astype("float64")
    result["klinger_valid"] = valid.astype("float64")
    result["klinger_decision_cutoff_ms"] = cutoff_col
    result["klinger_decision_boundary_ms"] = boundary_col
    _attach_params(result, p, interval)
    result["klinger_reason"] = reason
    result["klinger_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result


def _empty_sides(n):
    sides = {}
    for side in ("long", "short"):
        cols = {f: np.full(n, np.nan) for f in SIDE_FIELDS}
        cols["state"] = np.zeros(n, dtype=np.int64)
        cols["armed"] = np.zeros(n, dtype="float64")
        sides[side] = cols
    return sides


def _hold_all(result: pd.DataFrame, reason: str, p: dict, interval) -> pd.DataFrame:
    n = len(result)
    nan = np.full(n, np.nan)
    endpoint = {col: nan.copy() for col in ENDPOINT_COLUMNS}
    return _write(result, p=p, interval=interval, eval_ts=nan.copy(), replay_start=nan.copy(),
                  support_start=nan.copy(), endpoint=endpoint, endpoint_ok=np.zeros(n, dtype=bool),
                  prev_osc=nan.copy(), prev_sig=nan.copy(), cross=nan.copy(), sides=_empty_sides(n),
                  valid=np.zeros(n, dtype=bool), cutoff_col=nan.copy(), boundary_col=nan.copy(),
                  reason=np.full(n, reason, dtype=object), signal=np.zeros(n, dtype=np.int64))


def klinger_divergence_core(
    df: pd.DataFrame,
    fast_period: int = 34,
    slow_period: int = 55,
    signal_period: int = 13,
    replay_bars: int = 112,
    left_span: int = 3,
    right_span: int = 3,
    min_separation: int = 5,
    max_separation: int = 40,
    setup_expiry: int = 12,
    divergence_test: bool = True,
) -> pd.DataFrame:
    p = validate_klinger_params(fast_period, slow_period, signal_period, replay_bars, left_span, right_span,
                                min_separation, max_separation, setup_expiry, divergence_test)
    replay = p["replay_bars"]
    support = p["support"]
    result = df.copy()
    n = len(result)
    interval, kind = frame_cadence(result)

    if any(c not in result.columns for c in REQUIRED_COLUMNS):
        return _hold_all(result, "missing_columns", p, interval)
    if interval is None:
        return _hold_all(result, "missing_cadence", p, None)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, "missing_timestamps", p, interval)

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
    row_ok = ~row_bad

    edge_reason = np.full(n, "", dtype=object)
    if n > 1:
        both = ts_ok[1:] & ts_ok[:-1]
        with np.errstate(invalid="ignore"):
            step = ts[1:] - ts[:-1]
        tail = np.full(n - 1, "", dtype=object)
        with np.errstate(invalid="ignore"):
            tail[both & (step != float(interval))] = "cadence_gap"
            tail[both & (step <= 0)] = "timestamp_order"
        edge_reason[1:] = tail
    edge_bad = edge_reason != ""

    safe_high = np.where(row_bad, 1.0, high)
    safe_low = np.where(row_bad, 1.0, low)
    safe_close = np.where(row_bad, 1.0, close)
    safe_volume = np.where(row_bad, 0.0, volume)

    endpoint_inputs_ok = _window_clear(row_bad, replay + 1) & _window_clear(edge_bad, replay)
    endpoint_inputs_ok[:replay] = False
    ends = np.flatnonzero(endpoint_inputs_ok)
    computed = replay_endpoints(safe_high, safe_low, safe_close, safe_volume, ends, p["fast_period"],
                                p["slow_period"], p["signal_period"], replay)
    endpoint = {col: np.full(n, np.nan) for col in ENDPOINT_COLUMNS}
    zero_cm = np.zeros(n, dtype=bool)
    for col in ENDPOINT_COLUMNS:
        endpoint[col][ends] = computed[col]
    zero_cm[ends] = computed["zero_cm"]
    derived_finite = np.ones(n, dtype=bool)
    for col in ENDPOINT_COLUMNS:
        derived_finite &= np.isfinite(endpoint[col])
    endpoint_ok = endpoint_inputs_ok & ~zero_cm & derived_finite
    endpoint_zero = endpoint_inputs_ok & zero_cm
    endpoint_nonfinite = endpoint_inputs_ok & ~zero_cm & ~derived_finite
    for col in ENDPOINT_COLUMNS:
        endpoint[col] = np.where(endpoint_ok, endpoint[col], np.nan)

    osc = endpoint["klinger_oscillator"]
    sig_line = endpoint["klinger_signal_line"]
    prev_ok = np.zeros(n, dtype=bool)
    prev_osc = np.full(n, np.nan)
    prev_sig = np.full(n, np.nan)
    if n > 1:
        prev_ok[1:] = endpoint_ok[:-1]
        prev_osc[1:] = osc[:-1]
        prev_sig[1:] = sig_line[:-1]
    cross_ok = endpoint_ok & prev_ok
    cross = np.full(n, np.nan)
    with np.errstate(invalid="ignore"):
        up = cross_ok & (prev_osc <= prev_sig) & (osc > sig_line)
        down = cross_ok & (prev_osc >= prev_sig) & (osc < sig_line)
    cross[cross_ok] = 0.0
    cross[up] = 1.0
    cross[down] = -1.0
    cross_dir = np.where(np.isfinite(cross), cross, 0.0).astype(np.int64)

    low_pivots = _strict_pivots(safe_low, row_ok, p["left_span"], p["right_span"], lows=True)
    high_pivots = _strict_pivots(safe_high, row_ok, p["left_span"], p["right_span"], lows=False)

    eval_ts = np.full(n, np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        eval_ts = np.where(ts_ok, ts, np.nan)
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        eval_ts = np.where(ts_ok, ts - float(interval) + 1.0, np.nan)

    long_setups = _side_setups(low_pivots, safe_low, osc, endpoint_ok, cross_dir, p, True, n)
    short_setups = _side_setups(high_pivots, safe_high, osc, endpoint_ok, cross_dir, p, False, n)
    sides = {
        "long": _side_rows(long_setups, n, eval_ts, interval, safe_low, osc, p["setup_expiry"]),
        "short": _side_rows(short_setups, n, eval_ts, interval, safe_high, osc, p["setup_expiry"]),
    }
    long_state = sides["long"]["state"]
    short_state = sides["short"]["state"]

    support_ok = _window_clear(row_bad, support) & _window_clear(edge_bad, support - 1)
    endpoints_clear = _window_clear(~endpoint_ok, support - replay)
    valid = support_ok & endpoints_clear
    valid[:support - 1] = False

    long_emit = long_state == STATE_EMITTED
    short_emit = short_state == STATE_EMITTED
    reason = np.full(n, "no_setup", dtype=object)
    for state, name in ((STATE_NO_PAIR, "no_pivot_pair"), (STATE_NO_DIVERGENCE, "no_divergence")):
        reason[short_state == state] = name
    for state, name in ((STATE_NO_PAIR, "no_pivot_pair"), (STATE_NO_DIVERGENCE, "no_divergence")):
        reason[long_state == state] = name
    reason[(long_state == STATE_CONSUMED) | (short_state == STATE_CONSUMED)] = "setup_consumed"
    reason[(long_state == STATE_AWAITING) | (short_state == STATE_AWAITING)] = "awaiting_cross"
    reason[(long_state == STATE_ARMED) | (short_state == STATE_ARMED)] = "setup_armed"
    reason[long_emit & ~short_emit] = "entry_long"
    reason[short_emit & ~long_emit] = "entry_short"
    reason[long_emit & short_emit] = "opposite_events"

    signal = np.zeros(n, dtype=np.int64)
    signal[long_emit & ~short_emit] = 1
    signal[short_emit & ~long_emit] = -1

    for i in np.flatnonzero(~valid):
        lo = max(0, i - support + 1)
        found = [pr for pr in (_last_defect(row_reason, lo, i), _last_defect(edge_reason, max(1, lo + 1), i))
                 if pr is not None]
        if found:
            reason[i] = max(found, key=lambda pr: pr[0])[1]
        elif i < support - 1:
            reason[i] = "insufficient_history"
        elif endpoint_zero[i - support + replay + 1:i + 1].any():
            reason[i] = "zero_cm"
        else:
            reason[i] = "nonfinite_derived"
    signal[~valid] = 0
    for side in ("long", "short"):
        cols = sides[side]
        cols["state"] = np.where(valid, cols["state"], 0)
        for field in SIDE_FIELDS:
            if field == "state":
                continue
            fill = 0.0 if field == "armed" else np.nan
            cols[field] = np.where(valid, cols[field], fill)

    cutoff_col = np.full(n, np.nan)
    boundary_col = np.full(n, np.nan)
    cutoff, declared_boundary, runtime = _decision_time(result.attrs)
    if runtime and n > 0:
        i = n - 1
        if cutoff is not None:
            cutoff_col[i] = float(cutoff)
        if declared_boundary is not None:
            boundary_col[i] = float(declared_boundary)
        if valid[i]:
            row_boundary = None
            if kind == TIMESTAMP_KIND_OPEN:
                row_boundary = int(ts[i]) + interval
            elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
                row_boundary = int(ts[i]) + 1
            held = ""
            if cutoff is None or declared_boundary is None or row_boundary is None:
                held = "decision_time_missing"
            elif row_boundary != declared_boundary:
                held = "decision_boundary_mismatch"
            elif cutoff - row_boundary < 0:
                held = "decision_bar_future"
            elif cutoff - row_boundary >= interval:
                held = "decision_bar_stale"
            if held:
                valid[i] = False
                signal[i] = 0
                reason[i] = held

    replay_start = np.full(n, np.nan)
    support_start = np.full(n, np.nan)
    if n > replay:
        replay_start[replay:] = eval_ts[:n - replay]
    if n >= support:
        support_start[support - 1:] = eval_ts[:n - support + 1]

    return _write(result, p=p, interval=interval, eval_ts=eval_ts, replay_start=replay_start,
                  support_start=support_start, endpoint=endpoint, endpoint_ok=endpoint_ok,
                  prev_osc=np.where(prev_ok, prev_osc, np.nan), prev_sig=np.where(prev_ok, prev_sig, np.nan),
                  cross=cross, sides=sides, valid=valid, cutoff_col=cutoff_col, boundary_col=boundary_col,
                  reason=reason, signal=signal)
