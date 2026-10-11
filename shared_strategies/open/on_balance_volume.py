import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

SPAN_MIN = 1
SPAN_MAX = 10
SEPARATION_MIN = 1
SEPARATION_MAX = 100
EXPIRY_MIN = 1
EXPIRY_MAX = 50
MAX_SUPPORT_BARS = 200
REQUIRED_COLUMNS = ("open", "high", "low", "close", "volume")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_setup": 0,
    "setup_armed": 2,
    "awaiting_reclaim": 3,
    "opposite_events": 4,
    "no_pivot_pair": 5,
    "no_price_divergence": 6,
    "volume_not_confirmed": 7,
    "zero_volume_span": 8,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "invalid_price": 12,
    "inconsistent_ohlc": 13,
    "timestamp_order": 14,
    "cadence_gap": 15,
    "invalid_volume": 16,
    "missing_columns": 17,
    "missing_timestamps": 18,
}

STATE_NONE = 0
STATE_ARMED = 2
STATE_AWAITING = 3
STATE_EMITTED = 1
STATE_CONSUMED = 9
STATE_UNOBSERVABLE = 19

SIDE_FIELDS = (
    "state", "pivot1_ts_ms", "pivot1_price", "pivot2_ts_ms", "pivot2_price",
    "obv1", "obv2", "signed_sum", "abs_sum", "divergence", "reclaim",
    "confirm_ts_ms", "expiry_ts_ms",
)


def _as_int(name, value, lo, hi):
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"on_balance_volume_divergence: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"on_balance_volume_divergence: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"on_balance_volume_divergence: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def support_bars(left_span, right_span, max_separation, setup_expiry) -> int:
    return int(left_span) + int(max_separation) + int(right_span) + int(setup_expiry) + 1


def validate_obv_params(left_span, right_span, min_separation, max_separation,
                        volume_threshold, setup_expiry, volume_test) -> dict:
    left = _as_int("left_span", left_span, SPAN_MIN, SPAN_MAX)
    right = _as_int("right_span", right_span, SPAN_MIN, SPAN_MAX)
    min_sep = _as_int("min_separation", min_separation, SEPARATION_MIN, SEPARATION_MAX)
    max_sep = _as_int("max_separation", max_separation, SEPARATION_MIN, SEPARATION_MAX)
    if min_sep > max_sep:
        raise ValueError(
            "on_balance_volume_divergence: min_separation must be <= max_separation, "
            f"got {min_separation!r} > {max_separation!r}")
    expiry = _as_int("setup_expiry", setup_expiry, EXPIRY_MIN, EXPIRY_MAX)
    if isinstance(volume_threshold, (bool, np.bool_)) or not isinstance(volume_threshold, Real):
        raise ValueError(
            f"on_balance_volume_divergence: volume_threshold must be a number, got {volume_threshold!r}")
    threshold = float(volume_threshold)
    if not math.isfinite(threshold) or threshold < 0.0 or threshold > 1.0:
        raise ValueError(
            f"on_balance_volume_divergence: volume_threshold must be finite and in [0, 1], got {volume_threshold!r}")
    if not isinstance(volume_test, (bool, np.bool_)):
        raise ValueError(
            f"on_balance_volume_divergence: volume_test must be a boolean, got {volume_test!r}")
    support = support_bars(left, right, max_sep, expiry)
    if support > MAX_SUPPORT_BARS:
        raise ValueError(
            "on_balance_volume_divergence: left_span + max_separation + right_span + setup_expiry + 1 "
            f"must be <= {MAX_SUPPORT_BARS}, got {support}")
    return {
        "left_span": left, "right_span": right, "min_separation": min_sep,
        "max_separation": max_sep, "volume_threshold": threshold,
        "setup_expiry": expiry, "volume_test": bool(volume_test), "support": support,
    }


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


def _last_true_index(flags: np.ndarray) -> np.ndarray:
    idx = np.where(flags, np.arange(len(flags)), -1)
    return np.maximum.accumulate(idx) if len(idx) else idx


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


def _bounded_sums(segment: np.ndarray):
    scale = float(np.max(np.abs(segment))) if len(segment) else 0.0
    if not (scale > 0.0):
        return 0.0, 0.0, None
    scaled = segment / scale
    signed = math.fsum(scaled.tolist())
    absolute = math.fsum(np.abs(scaled).tolist())
    divergence = signed / absolute
    with np.errstate(over="ignore", invalid="ignore"):
        signed_raw = signed * scale
        abs_raw = absolute * scale
    return (signed_raw if math.isfinite(signed_raw) else math.nan,
            abs_raw if math.isfinite(abs_raw) else math.nan,
            divergence)


def _side_setups(pivot_flags, prices, highs, lows, close, signed_volume, row_ok, p, lows_side):
    n = len(close)
    left, right = p["left_span"], p["right_span"]
    min_sep, max_sep = p["min_separation"], p["max_separation"]
    expiry, threshold, volume_test = p["setup_expiry"], p["volume_threshold"], p["volume_test"]
    pivots = np.flatnonzero(pivot_flags)
    setups = []
    for idx, q in enumerate(pivots):
        c = int(q) + right
        rec = {"q": int(q), "c": c, "p": None, "code": STATE_NONE, "armed": False,
               "signed_sum": math.nan, "abs_sum": math.nan, "divergence": math.nan,
               "reclaim": math.nan, "emit": -1}
        j = int(np.searchsorted(pivots, q - min_sep, side="right")) - 1
        if j < 0 or j >= idx or pivots[j] < q - max_sep:
            rec["code"] = REASON_CODES["no_pivot_pair"]
            setups.append(rec)
            continue
        pv = int(pivots[j])
        rec["p"] = pv
        if not row_ok[pv:int(q) + 1].all():
            rec["code"] = STATE_UNOBSERVABLE
            setups.append(rec)
            continue
        signed_raw, abs_raw, divergence = _bounded_sums(signed_volume[pv + 1:int(q) + 1])
        rec["signed_sum"], rec["abs_sum"] = signed_raw, abs_raw
        if divergence is not None:
            rec["divergence"] = divergence
        price_div = prices[q] < prices[pv] if lows_side else prices[q] > prices[pv]
        if not price_div:
            rec["code"] = REASON_CODES["no_price_divergence"]
        elif divergence is None:
            rec["code"] = REASON_CODES["zero_volume_span"]
        else:
            confirmed = divergence > threshold if lows_side else divergence < -threshold
            if volume_test and not confirmed:
                rec["code"] = REASON_CODES["volume_not_confirmed"]
            else:
                rec["armed"] = True
                rec["code"] = STATE_ARMED
        if rec["armed"]:
            span = slice(int(q), c + 1)
            rec["reclaim"] = float(highs[span].max()) if lows_side else float(lows[span].min())
            nxt = int(pivots[idx + 1]) + right if idx + 1 < len(pivots) else n
            last = min(c + expiry, nxt - 1, n - 1)
            if last >= c + 1:
                window = close[c + 1:last + 1]
                with np.errstate(invalid="ignore"):
                    hits = window > rec["reclaim"] if lows_side else window < rec["reclaim"]
                if hits.any():
                    rec["emit"] = c + 1 + int(np.argmax(hits))
        setups.append(rec)
    return setups


def _side_rows(setups, n, ts, cadence, prices, obv, p):
    expiry = p["setup_expiry"]
    cols = {f: np.full(n, np.nan) for f in SIDE_FIELDS}
    cols["state"] = np.zeros(n, dtype=np.int64)
    emit = np.zeros(n, dtype=bool)
    armed_now = np.zeros(n, dtype=bool)
    awaiting = np.zeros(n, dtype=bool)
    failure = np.zeros(n, dtype=np.int64)
    if not setups:
        return cols, emit, armed_now, awaiting, failure
    confirms = np.array([s["c"] for s in setups], dtype=np.int64)
    rows = np.arange(n)
    which = np.searchsorted(confirms, rows, side="right") - 1
    for t in np.flatnonzero((which >= 0)):
        s = setups[which[t]]
        if t - s["c"] > expiry:
            continue
        if s["armed"]:
            if s["emit"] == t:
                state = STATE_EMITTED
                emit[t] = True
            elif 0 <= s["emit"] < t:
                state = STATE_CONSUMED
            elif t == s["c"]:
                state = STATE_ARMED
                armed_now[t] = True
            else:
                state = STATE_AWAITING
                awaiting[t] = True
        else:
            state = s["code"]
            if t == s["c"] and state != STATE_UNOBSERVABLE:
                failure[t] = state
        cols["state"][t] = state
        q, pv = s["q"], s["p"]
        cols["pivot2_ts_ms"][t] = ts[q]
        cols["pivot2_price"][t] = prices[q]
        cols["obv2"][t] = obv[q]
        if pv is not None:
            cols["pivot1_ts_ms"][t] = ts[pv]
            cols["pivot1_price"][t] = prices[pv]
            cols["obv1"][t] = obv[pv]
        cols["signed_sum"][t] = s["signed_sum"]
        cols["abs_sum"][t] = s["abs_sum"]
        cols["divergence"][t] = s["divergence"]
        cols["reclaim"][t] = s["reclaim"]
        cols["confirm_ts_ms"][t] = ts[s["c"]]
        cols["expiry_ts_ms"][t] = ts[s["c"]] + expiry * cadence[t]
    return cols, emit, armed_now, awaiting, failure


def _write_columns(result, n, *, obv, valid, support_start, ts, reason, sides, signal, p):
    result["obv"] = obv
    for side, cols in sides.items():
        for field in SIDE_FIELDS:
            result[f"obvd_{side}_{field}"] = cols[field]
    result["obvd_support_bars"] = np.full(n, p["support"], dtype=np.int64)
    result["obvd_support_start_ts_ms"] = support_start
    result["obvd_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["obvd_volume_test"] = np.full(n, p["volume_test"], dtype=bool)
    result["obvd_valid"] = valid
    result["obvd_reason"] = reason
    result["obvd_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result


def _hold_all(result, p, reason, ts):
    n = len(result)
    empty = {f: np.full(n, np.nan) for f in SIDE_FIELDS}
    empty["state"] = np.zeros(n, dtype=np.int64)
    sides = {"long": empty, "short": {k: v.copy() for k, v in empty.items()}}
    return _write_columns(result, n, obv=np.full(n, np.nan), valid=np.zeros(n, dtype=bool),
                          support_start=np.full(n, np.nan), ts=ts,
                          reason=np.full(n, reason, dtype=object), sides=sides,
                          signal=np.zeros(n, dtype=np.int64), p=p)


def on_balance_volume_divergence_core(
    df: pd.DataFrame,
    left_span: int = 3,
    right_span: int = 3,
    min_separation: int = 5,
    max_separation: int = 40,
    volume_threshold: float = 0.10,
    setup_expiry: int = 12,
    volume_test: bool = True,
) -> pd.DataFrame:
    p = validate_obv_params(left_span, right_span, min_separation, max_separation,
                            volume_threshold, setup_expiry, volume_test)
    result = df.copy()
    n = len(result)
    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _hold_all(result, p, "missing_columns", None)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, p, "missing_timestamps", None)

    op = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    hi = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    cl = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    vol = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(op) & np.isfinite(hi) & np.isfinite(lo) & np.isfinite(cl)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ((op <= 0) | (hi <= 0) | (lo <= 0) | (cl <= 0))
        inconsistent = finite & ~nonpositive & ((hi < lo) | (op > hi) | (op < lo) | (cl > hi) | (cl < lo))
        bad_volume = ~np.isfinite(vol) | (vol < 0)
    bad_ts = ~np.isfinite(ts)

    row_reason = np.full(n, "", dtype=object)
    row_reason[bad_ts] = "timestamp_order"
    row_reason[bad_volume] = "invalid_volume"
    row_reason[inconsistent] = "inconsistent_ohlc"
    row_reason[nonpositive] = "invalid_price"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~nonpositive & ~inconsistent & ~bad_volume & ~bad_ts

    steps = np.full(n, np.nan)
    if n > 1:
        steps[1:] = ts[1:] - ts[:-1]
    with np.errstate(invalid="ignore"):
        step_bad = ~(steps > 0)
    step_bad[0] = False
    change = np.zeros(n, dtype=bool)
    if n > 2:
        with np.errstate(invalid="ignore"):
            change[2:] = ~(steps[2:] == steps[1:-1])

    H = p["support"]
    rows = np.arange(n)
    start = rows - H + 1
    last_bad_row = _last_true_index(row_reason != "")
    last_bad_step = _last_true_index(step_bad)
    last_change = _last_true_index(change)
    full = start >= 0
    window_rows_ok = last_bad_row < np.maximum(start, 0)
    window_steps_ok = last_bad_step < np.maximum(start, 0) + 1
    window_cadence_ok = last_change < np.maximum(start, 0) + 2
    valid = full & window_rows_ok & window_steps_ok & window_cadence_ok

    prev_close = np.concatenate([[np.nan], cl[:-1]]) if n else cl
    with np.errstate(invalid="ignore"):
        direction = np.sign(cl - prev_close)
        signed_volume = np.where(np.isfinite(direction), direction * vol, np.nan)
    prev_ok = np.concatenate([[False], row_ok[:-1]]) if n else row_ok
    segment_id = np.cumsum(row_ok & ~prev_ok)
    contrib = np.where(row_ok & prev_ok, signed_volume, 0.0)
    with np.errstate(over="ignore", invalid="ignore"):
        obv = np.array(pd.Series(contrib).groupby(segment_id).cumsum(), dtype="float64")
    obv[~row_ok | ~np.isfinite(obv)] = np.nan

    cadence = np.full(n, np.nan)
    if n > 1:
        cadence[1:] = steps[1:]
        cadence[0] = steps[1]

    low_pivots = _strict_pivots(lo, row_ok, p["left_span"], p["right_span"], lows=True)
    high_pivots = _strict_pivots(hi, row_ok, p["left_span"], p["right_span"], lows=False)
    long_setups = _side_setups(low_pivots, lo, hi, lo, cl, signed_volume, row_ok, p, True)
    short_setups = _side_setups(high_pivots, hi, hi, lo, cl, signed_volume, row_ok, p, False)
    long_cols, long_emit, long_armed, long_wait, long_fail = _side_rows(long_setups, n, ts, cadence, lo, obv, p)
    short_cols, short_emit, short_armed, short_wait, short_fail = _side_rows(short_setups, n, ts, cadence, hi, obv, p)

    reason = np.full(n, "no_setup", dtype=object)
    for code_name, code in REASON_CODES.items():
        if code in (5, 6, 7, 8):
            reason[(long_fail == code) | ((short_fail == code) & (long_fail == 0))] = code_name
    reason[long_wait | short_wait] = "awaiting_reclaim"
    reason[long_armed | short_armed] = "setup_armed"
    both = long_emit & short_emit
    reason[long_emit & ~short_emit] = "entry_long"
    reason[short_emit & ~long_emit] = "entry_short"
    reason[both] = "opposite_events"

    signal = np.zeros(n, dtype=np.int64)
    signal[long_emit & ~short_emit] = 1
    signal[short_emit & ~long_emit] = -1

    for t in np.flatnonzero(~valid):
        lo_row = max(0, t - H + 1)
        bad_rows = [r for r in row_reason[lo_row:t + 1] if r]
        if bad_rows:
            reason[t] = bad_rows[-1]
        elif step_bad[lo_row + 1:t + 1].any():
            reason[t] = "timestamp_order"
        elif change[lo_row + 2:t + 1].any():
            reason[t] = "cadence_gap"
        else:
            reason[t] = "insufficient_history"
    signal[~valid] = 0

    support_start = np.full(n, np.nan)
    if n:
        support_start[full] = ts[start[full]]

    return _write_columns(result, n, obv=obv, valid=valid, support_start=support_start, ts=ts,
                          reason=reason, sides={"long": long_cols, "short": short_cols},
                          signal=signal, p=p)
