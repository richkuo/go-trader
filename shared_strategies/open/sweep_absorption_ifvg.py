import hashlib
import json
import math
import os
import sys
from decimal import Decimal, localcontext
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

_OPEN_DIR = os.path.dirname(os.path.abspath(__file__))
_TOOLS_DIR = os.path.abspath(os.path.join(_OPEN_DIR, "..", "..", "shared_tools"))
for _path in (_TOOLS_DIR, _OPEN_DIR):
    if _path not in sys.path:
        sys.path.append(_path)

from closed_bar import FRAME_DECISION_TIME_ATTR, FRAME_INTERVAL_ATTR
from cumulative_volume_delta_divergence import TRADE_FLOW_CONTRACT, _open_times_ms, parse_trade_flow

STRATEGY = "sweep_absorption_ifvg"
HOUR_MS = 3_600_000
DAY_MS = 24 * HOUR_MS
ATR_PERIOD = 14
HTF_INTERVAL_MS = HOUR_MS
HTF_TIMEFRAME = "1h"
HTF_LEFT = 2
HTF_RIGHT = 2
HTF_SEARCH_MS = 24 * HOUR_MS
MAX_DECISION_BARS = 200
FLOW_RETENTION_CAP_MS = 26 * HOUR_MS
ALIGNMENT_ROWS = 1
HTF_SLACK_ROWS = 2
FLOW_MINUTE_MS = TRADE_FLOW_CONTRACT["interval_ms"]
HTF_KIND = "htf_candles"
NATIVE_CLOSE_KIND = "native_close_inclusive"
OPEN_KIND = "open"

DEFAULT_PARAMS = {
    "session_start_hour_utc": 0,
    "session_end_hour_utc": 8,
    "sweep_window": 24,
    "max_sweep_bars": 6,
    "sweep_atr_mult": 0.1,
    "absorption_threshold": 0.05,
    "absorption_filter": True,
    "gap_window": 12,
    "min_fvg_atr": 0.25,
    "confirm_window": 6,
    "min_displacement_atr": 0.5,
    "setup_expiry": 12,
    "entry_level": 0.5,
    "expected_interval_ms": 300_000,
}

INT_BOUNDS = {
    "session_start_hour_utc": (0, 23),
    "session_end_hour_utc": (0, 23),
    "sweep_window": (1, 96),
    "max_sweep_bars": (1, 24),
    "gap_window": (1, 48),
    "confirm_window": (1, 48),
    "setup_expiry": (1, 96),
    "expected_interval_ms": (60_000, 1_800_000),
}

FLOAT_BOUNDS = {
    "sweep_atr_mult": (0.0, 5.0, False),
    "absorption_threshold": (0.0, 1.0, True),
    "min_fvg_atr": (0.0, 10.0, False),
    "min_displacement_atr": (0.0, 10.0, False),
    "entry_level": (0.0, 1.0, False),
}

INPUT_PARAMS = ("trade_flow_observations", "htf_candles")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_setup": 0,
    "armed_long": 2,
    "armed_short": 3,
    "setup_active": 4,
    "awaiting_sweep": 5,
    "awaiting_reclaim": 6,
    "awaiting_displacement": 7,
    "in_session": 8,
    "opposing_entries": 9,
    "sweep_not_reclaimed": 20,
    "no_gap": 21,
    "displacement_weak": 22,
    "no_displacement": 23,
    "absorption_new_delta_extreme": 24,
    "absorption_failed": 25,
    "zero_flow_volume": 26,
    "flow_unavailable": 27,
    "flow_volume_mismatch": 28,
    "htf_bias_rejected": 29,
    "setup_expired": 30,
    "wick_broken": 31,
    "retest_failed": 32,
    "setup_cleared_flow": 33,
    "session_reset": 34,
    "insufficient_history": 40,
    "nonfinite_input": 41,
    "inconsistent_ohlc": 42,
    "timestamp_order": 43,
    "missing_columns": 44,
    "bar_spacing": 45,
    "cadence_mismatch": 46,
    "incomplete_session": 47,
    "bar_not_closed": 48,
    "trade_flow_unavailable": 50,
    "unsupported_trade_flow": 51,
    "side_unverified": 52,
    "htf_unavailable": 53,
    "decision_time_missing": 60,
    "decision_bar_stale": 61,
    "decision_bar_future": 62,
    "decision_boundary_mismatch": 63,
}

TERMINAL_REASONS = ("sweep_not_reclaimed", "no_gap", "displacement_weak", "no_displacement",
                    "absorption_new_delta_extreme", "absorption_failed", "zero_flow_volume", "flow_unavailable",
                    "flow_volume_mismatch", "htf_bias_rejected", "setup_expired", "wick_broken", "retest_failed",
                    "setup_cleared_flow")

EVENT_PRIORITY = ("entry_long", "entry_short", "opposing_entries", "armed_long", "armed_short",
                  "setup_cleared_flow", "wick_broken", "retest_failed", "setup_expired",
                  "absorption_new_delta_extreme", "absorption_failed", "zero_flow_volume", "flow_volume_mismatch",
                  "flow_unavailable", "htf_bias_rejected", "displacement_weak", "no_displacement", "no_gap",
                  "sweep_not_reclaimed", "session_reset")

TEXT_COLUMNS = ("sai_side", "sai_setup_id", "sai_flow_sha256")
NUMERIC_COLUMNS = (
    "sai_session_ms", "sai_session_level", "sai_session_level_ms", "sai_sweep_start_ms", "sai_sweep_end_ms",
    "sai_sweep_wick", "sai_delta_session_level", "sai_delta_sweep", "sai_flow_volume", "sai_absorption_ratio",
    "sai_gap_bottom", "sai_gap_top", "sai_gap_ms", "sai_ce", "sai_entry_price", "sai_displacement_body",
    "sai_displacement_atr", "sai_confirm_ms", "sai_htf_bias", "sai_flow_minutes",
)
EVIDENCE_COLUMNS = TEXT_COLUMNS + NUMERIC_COLUMNS


def _int_param(name, value):
    lo, hi = INT_BOUNDS[name]
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{STRATEGY}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def _float_param(name, value):
    lo, hi, open_hi = FLOAT_BOUNDS[name]
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or as_float < lo or as_float > hi or (open_hi and as_float >= hi):
        bound = f"[{lo}, {hi})" if open_hi else f"[{lo}, {hi}]"
        raise ValueError(f"{STRATEGY}: {name} must be finite and in {bound}, got {value!r}")
    return as_float


def session_duration_ms(start_hour: int, end_hour: int) -> int:
    return ((end_hour - start_hour) % 24) * HOUR_MS


def support_requirements(p: dict) -> dict:
    interval = p["expected_interval_ms"]
    session_ms = session_duration_ms(p["session_start_hour_utc"], p["session_end_hour_utc"])
    session_bars = session_ms // interval
    prelude_bars = max(session_bars, p["gap_window"] + ATR_PERIOD)
    post_bars = p["sweep_window"] + p["max_sweep_bars"] + p["confirm_window"] + p["setup_expiry"] - 1
    decision_bars = prelude_bars + post_bars + ALIGNMENT_ROWS
    flow_window_ms = session_ms + post_bars * interval + interval + FLOW_MINUTE_MS
    htf_rows = HTF_SEARCH_MS // HTF_INTERVAL_MS + HTF_LEFT + -(-(post_bars * interval) // HTF_INTERVAL_MS) + HTF_SLACK_ROWS
    return {"session_ms": session_ms, "session_bars": session_bars, "prelude_bars": prelude_bars,
            "post_bars": post_bars, "decision_bars": decision_bars, "flow_window_ms": flow_window_ms,
            "htf_rows": htf_rows}


def validate_sai_params(session_start_hour_utc, session_end_hour_utc, sweep_window, max_sweep_bars, sweep_atr_mult,
                        absorption_threshold, absorption_filter, gap_window, min_fvg_atr, confirm_window,
                        min_displacement_atr, setup_expiry, entry_level, expected_interval_ms) -> dict:
    out = {name: _int_param(name, value) for name, value in (
        ("session_start_hour_utc", session_start_hour_utc), ("session_end_hour_utc", session_end_hour_utc),
        ("sweep_window", sweep_window), ("max_sweep_bars", max_sweep_bars), ("gap_window", gap_window),
        ("confirm_window", confirm_window), ("setup_expiry", setup_expiry),
        ("expected_interval_ms", expected_interval_ms))}
    for name, value in (("sweep_atr_mult", sweep_atr_mult), ("absorption_threshold", absorption_threshold),
                        ("min_fvg_atr", min_fvg_atr), ("min_displacement_atr", min_displacement_atr),
                        ("entry_level", entry_level)):
        out[name] = _float_param(name, value)
    if not isinstance(absorption_filter, (bool, np.bool_)):
        raise ValueError(f"{STRATEGY}: absorption_filter must be true or false, got {absorption_filter!r}")
    out["absorption_filter"] = bool(absorption_filter)
    if out["session_start_hour_utc"] == out["session_end_hour_utc"]:
        raise ValueError(f"{STRATEGY}: session_start_hour_utc equals session_end_hour_utc; the session would be empty")
    interval = out["expected_interval_ms"]
    if interval % FLOW_MINUTE_MS:
        raise ValueError(f"{STRATEGY}: expected_interval_ms {interval} is not a multiple of the "
                         f"{FLOW_MINUTE_MS} ms trade-flow interval")
    if HOUR_MS % interval:
        raise ValueError(f"{STRATEGY}: expected_interval_ms {interval} does not divide one hour, so UTC session "
                         f"boundaries would not align with bar opens")
    if TRADE_FLOW_CONTRACT["reorder_grace_ms"] >= interval:
        raise ValueError(f"{STRATEGY}: the trade-flow reorder grace must be shorter than one bar")
    support = support_requirements(out)
    if support["session_bars"] < 2:
        raise ValueError(f"{STRATEGY}: the session holds {support['session_bars']} bars; it needs at least 2")
    if support["decision_bars"] > MAX_DECISION_BARS:
        raise ValueError(f"{STRATEGY}: the support horizon needs {support['decision_bars']} decision bars "
                         f"(prelude {support['prelude_bars']} + post-session {support['post_bars']} + alignment "
                         f"{ALIGNMENT_ROWS}), over the {MAX_DECISION_BARS}-bar decision window")
    if support["flow_window_ms"] > FLOW_RETENTION_CAP_MS:
        raise ValueError(f"{STRATEGY}: the trade-flow window {support['flow_window_ms']} ms is over the "
                         f"{FLOW_RETENTION_CAP_MS} ms retention cap")
    if support["session_ms"] + support["post_bars"] * interval > DAY_MS:
        raise ValueError(f"{STRATEGY}: the session plus its post-session horizon spans more than 24 hours, so a setup "
                         f"would outlive the next session start")
    return out


def sai_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{STRATEGY}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS) - set(INPUT_PARAMS))
    if unknown:
        raise ValueError(f"{STRATEGY}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **{k: v for k, v in params.items() if k in DEFAULT_PARAMS}}
    validate_sai_params(**merged)


def support_for_params(params: dict) -> dict:
    merged = {**DEFAULT_PARAMS, **{k: v for k, v in (params or {}).items() if k in DEFAULT_PARAMS}}
    return support_requirements(validate_sai_params(**merged))


def _int_or_none(value):
    if value is None or isinstance(value, (bool, np.bool_)):
        return None
    if isinstance(value, (int, np.integer)):
        return int(value)
    if isinstance(value, (float, np.floating)) and math.isfinite(value) and float(value).is_integer():
        return int(value)
    return None


class HTFContext:
    def __init__(self, error="", detail=""):
        self.error = error
        self.detail = detail
        self.opens = np.zeros(0, dtype=np.int64)
        self.high = np.zeros(0)
        self.low = np.zeros(0)
        self.state = None


def parse_htf_candles(obs) -> HTFContext:
    if obs is None:
        return HTFContext("htf_unavailable", "no higher-timeframe candles were supplied")
    if not isinstance(obs, dict):
        return HTFContext("htf_unavailable", "higher-timeframe candles must be an object")
    if not obs.get("available", False):
        return HTFContext("htf_unavailable", str(obs.get("reason") or "higher-timeframe candles marked unavailable"))
    if obs.get("kind") != HTF_KIND:
        return HTFContext("htf_unavailable", f"kind {obs.get('kind')!r} is not {HTF_KIND!r}")
    if _int_or_none(obs.get("interval_ms")) != HTF_INTERVAL_MS:
        return HTFContext("htf_unavailable", f"interval_ms {obs.get('interval_ms')!r} is not {HTF_INTERVAL_MS}")
    kind = obs.get("timestamp_kind", OPEN_KIND)
    if kind not in (OPEN_KIND, NATIVE_CLOSE_KIND):
        return HTFContext("htf_unavailable", f"timestamp_kind {kind!r} is not supported")
    rows = obs.get("rows")
    if not isinstance(rows, list):
        return HTFContext("htf_unavailable", "rows must be a list")
    opens, highs, lows = [], [], []
    for i, row in enumerate(rows):
        if not isinstance(row, (list, tuple)) or len(row) < 5:
            return HTFContext("htf_unavailable", f"row {i} is malformed")
        stamp = _int_or_none(row[0])
        try:
            o, h, lo, c = (float(row[k]) for k in (1, 2, 3, 4))
        except (TypeError, ValueError):
            return HTFContext("htf_unavailable", f"row {i} prices are not numbers")
        if stamp is None or not all(math.isfinite(x) for x in (o, h, lo, c)):
            return HTFContext("htf_unavailable", f"row {i} is not finite")
        if h < lo or o > h or o < lo or c > h or c < lo or lo <= 0:
            return HTFContext("htf_unavailable", f"row {i} has inconsistent prices")
        open_ms = stamp - HTF_INTERVAL_MS + 1 if kind == NATIVE_CLOSE_KIND else stamp
        if open_ms % HTF_INTERVAL_MS:
            return HTFContext("htf_unavailable", f"row {i} opens at {open_ms}, off the {HTF_INTERVAL_MS} ms grid")
        if opens and open_ms <= opens[-1]:
            return HTFContext("htf_unavailable", f"row {i} is out of order")
        opens.append(open_ms)
        highs.append(h)
        lows.append(lo)
    ctx = HTFContext()
    ctx.opens = np.asarray(opens, dtype=np.int64)
    ctx.high = np.asarray(highs, dtype="float64")
    ctx.low = np.asarray(lows, dtype="float64")
    ctx.state = _htf_state_series(ctx)
    return ctx


def _pivots(values: np.ndarray, opens: np.ndarray, better) -> list:
    out = []
    n = len(values)
    for j in range(HTF_LEFT, n - HTF_RIGHT):
        span = range(j - HTF_LEFT, j + HTF_RIGHT + 1)
        if opens[j + HTF_RIGHT] - opens[j - HTF_LEFT] != (HTF_LEFT + HTF_RIGHT) * HTF_INTERVAL_MS:
            continue
        if all(better(values[j], values[k]) for k in span if k != j):
            out.append(j)
    return out


def _bias_at_close(ctx: HTFContext, j: int, low_pivots: list, high_pivots: list) -> tuple:
    close_ms = int(ctx.opens[j]) + HTF_INTERVAL_MS
    first = close_ms - HTF_SEARCH_MS - HTF_LEFT * HTF_INTERVAL_MS
    lo_idx = int(np.searchsorted(ctx.opens, first, side="left"))
    need = (close_ms - first) // HTF_INTERVAL_MS
    if j - lo_idx + 1 != need or int(ctx.opens[lo_idx]) != first:
        return (False, 0)
    search_from = close_ms - HTF_SEARCH_MS

    def last_two(pivots):
        usable = [p for p in pivots if int(ctx.opens[p]) >= search_from
                  and int(ctx.opens[p]) + (HTF_RIGHT + 1) * HTF_INTERVAL_MS <= close_ms]
        return usable[-2:]

    lows = last_two(low_pivots)
    highs = last_two(high_pivots)
    bias = 0
    long_ok = len(lows) == 2 and ctx.low[lows[1]] > ctx.low[lows[0]]
    short_ok = len(highs) == 2 and ctx.high[highs[1]] < ctx.high[highs[0]]
    if long_ok and not short_ok:
        bias = 1
    elif short_ok and not long_ok:
        bias = -1
    return (True, bias)


def _htf_state_series(ctx: HTFContext) -> pd.DataFrame:
    n = len(ctx.opens)
    if n == 0:
        return pd.DataFrame({"available": pd.Series(dtype="float64"), "bias": pd.Series(dtype="float64")})
    low_pivots = _pivots(ctx.low, ctx.opens, lambda a, b: a < b)
    high_pivots = _pivots(ctx.high, ctx.opens, lambda a, b: a > b)
    avail = np.zeros(n + 1)
    bias = np.zeros(n + 1)
    for j in range(n):
        ok, b = _bias_at_close(ctx, j, low_pivots, high_pivots)
        avail[j] = 1.0 if ok else 0.0
        bias[j] = float(b)
    index = np.append(ctx.opens, ctx.opens[-1] + HTF_INTERVAL_MS)
    frame = pd.DataFrame({"available": avail, "bias": bias}, index=index)
    return frame.shift(1)


def htf_bias_at(ctx: HTFContext, decision_ms: int):
    if ctx.error or ctx.state is None or len(ctx.opens) == 0:
        return None
    boundary = (int(decision_ms) // HTF_INTERVAL_MS) * HTF_INTERVAL_MS
    if boundary > int(ctx.opens[-1]) + HTF_INTERVAL_MS or boundary <= int(ctx.opens[0]):
        return None
    if boundary - HTF_INTERVAL_MS not in set(int(x) for x in ctx.opens):
        return None
    view = ctx.state.reindex(ctx.state.index.union(pd.Index([boundary]))).ffill()
    row = view.loc[boundary]
    if not np.isfinite(row["available"]) or row["available"] != 1.0:
        return None
    return int(row["bias"])


def _flow_sha256(parts: list) -> str:
    return hashlib.sha256(json.dumps(parts, separators=(",", ":")).encode("utf-8")).hexdigest()


def _true_range(h, lo, c):
    n = len(c)
    tr = np.full(n, np.nan)
    if n == 0:
        return tr
    prev = np.concatenate([[np.nan], c[:-1]])
    with np.errstate(invalid="ignore"):
        tr = np.fmax(h - lo, np.fmax(np.abs(h - prev), np.abs(lo - prev)))
    tr[0] = np.nan
    return tr


def _atr(tr):
    n = len(tr)
    out = np.full(n, np.nan)
    if n >= ATR_PERIOD:
        windows = sliding_window_view(tr, ATR_PERIOD)
        means = np.array([float(np.mean(w)) for w in windows])
        out[ATR_PERIOD - 1:] = means
    return out


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


class _Side:
    def __init__(self, side):
        self.side = side
        self.sign = 1 if side == "long" else -1
        self.state = "await_sweep"
        self.ev = {}
        self.b0 = None
        self.extreme = None
        self.extreme_i = None
        self.b_end = None
        self.armed = None
        self.minutes = None
        self.gap = None
        self.entry_price = None

    @property
    def done(self):
        return self.state == "done"

    @property
    def active(self):
        return self.state not in ("done",)


def _empty_result(result: pd.DataFrame, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in TEXT_COLUMNS:
        result[col] = np.full(n, "", dtype=object)
    for col in NUMERIC_COLUMNS:
        result[col] = np.full(n, np.nan)
    result["sai_deadline_ms"] = np.full(n, np.nan)
    result["sai_flow_available"] = np.zeros(n)
    result["sai_reason"] = np.full(n, reason, dtype=object)
    result["sai_reason_code"] = np.full(n, float(REASON_CODES[reason]))
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def sweep_absorption_ifvg_core(
    df: pd.DataFrame,
    session_start_hour_utc: int = 0,
    session_end_hour_utc: int = 8,
    sweep_window: int = 24,
    max_sweep_bars: int = 6,
    sweep_atr_mult: float = 0.1,
    absorption_threshold: float = 0.05,
    absorption_filter: bool = True,
    gap_window: int = 12,
    min_fvg_atr: float = 0.25,
    confirm_window: int = 6,
    min_displacement_atr: float = 0.5,
    setup_expiry: int = 12,
    entry_level: float = 0.5,
    expected_interval_ms: int = 300_000,
    trade_flow_observations=None,
    htf_candles=None,
) -> pd.DataFrame:
    p = validate_sai_params(session_start_hour_utc, session_end_hour_utc, sweep_window, max_sweep_bars,
                            sweep_atr_mult, absorption_threshold, absorption_filter, gap_window, min_fvg_atr,
                            confirm_window, min_displacement_atr, setup_expiry, entry_level, expected_interval_ms)
    support = support_requirements(p)
    result = df.copy()
    n = len(result)
    for col in ("open", "high", "low", "close", "volume"):
        if col not in result.columns:
            return _empty_result(result, "missing_columns")
    interval = p["expected_interval_ms"]
    declared = result.attrs.get(FRAME_INTERVAL_ATTR)
    if declared is not None and _int_or_none(declared) != interval:
        return _empty_result(result, "cadence_mismatch")
    ts = _open_times_ms(result, interval)
    if ts is None:
        return _empty_result(result, "timestamp_order")
    flow = parse_trade_flow(trade_flow_observations)
    if not flow.error and flow.bar_interval is not None and flow.bar_interval != interval:
        flow.error = "unsupported_trade_flow"
    htf = parse_htf_candles(htf_candles)
    coin = ""
    if isinstance(trade_flow_observations, dict):
        coin = str(trade_flow_observations.get("coin") or "")

    o = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    h = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    c = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    v = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(o) & np.isfinite(h) & np.isfinite(lo) & np.isfinite(c) & np.isfinite(v) & np.isfinite(ts)
    with np.errstate(invalid="ignore"):
        inconsistent = finite & ((h < lo) | (o > h) | (o < lo) | (c > h) | (c < lo) | (lo <= 0) | (v < 0))
    row_reason = [""] * n
    for i in range(n):
        if not finite[i]:
            row_reason[i] = "nonfinite_input"
        elif inconsistent[i]:
            row_reason[i] = "inconsistent_ohlc"
        elif int(ts[i]) % interval:
            row_reason[i] = "timestamp_order"
        elif i > 0 and np.isfinite(ts[i - 1]) and not ts[i] > ts[i - 1]:
            row_reason[i] = "timestamp_order"
    good = np.array([not r for r in row_reason], dtype=bool)
    contiguous_start = np.zeros(n, dtype=np.int64)
    for i in range(n):
        if not good[i]:
            contiguous_start[i] = i + 1
        elif i > 0 and good[i - 1] and int(ts[i]) - int(ts[i - 1]) == interval:
            contiguous_start[i] = contiguous_start[i - 1]
        else:
            contiguous_start[i] = i
    tr = _true_range(h, lo, c)
    for i in range(1, n):
        if not (good[i] and good[i - 1] and int(ts[i]) - int(ts[i - 1]) == interval):
            tr[i] = np.nan
    atr = _atr(tr)

    session_ms = support["session_ms"]
    post_bars = support["post_bars"]
    prelude_bars = support["prelude_bars"]
    start_off = p["session_start_hour_utc"] * HOUR_MS
    E = p["setup_expiry"]

    bar_cache = {}

    def bar_flow(i):
        if i in bar_cache:
            return bar_cache[i]
        open_ms = int(ts[i])
        out = None
        delta = Decimal(0)
        total = Decimal(0)
        minutes = []
        ok = True
        for start in range(open_ms, open_ms + interval, FLOW_MINUTE_MS):
            iv = flow.intervals.get(start)
            if iv is None:
                ok = False
                break
            delta += iv["buy"] - iv["sell"]
            total += iv["total"]
            minutes.append((start, iv))
        if ok:
            out = {"delta": delta, "total": total, "minutes": minutes,
                   "matches": float(total) == float(v[i])}
        bar_cache[i] = out
        return out

    def minute_available(iv, at_ms):
        return iv["finalized"] <= at_ms and (iv["invalid"] is None or at_ms < iv["invalid"])

    signal = np.zeros(n, dtype=np.int64)
    reason = np.full(n, "no_setup", dtype=object)
    deadline = np.full(n, np.nan)
    flow_available = np.zeros(n)
    ev_cols = {col: [None] * n for col in EVIDENCE_COLUMNS}

    def put(i, ev):
        for col in EVIDENCE_COLUMNS:
            if col in ev:
                ev_cols[col][i] = ev[col]

    ref_session = None
    sides = {}
    taint = ""
    session_info = None

    def setup_session(i, S):
        e_open = S + session_ms
        support_start = e_open - prelude_bars * interval
        first_index = i - (int(ts[i]) - support_start) // interval
        if first_index < 0 or contiguous_start[i] > first_index or int(ts[first_index]) != support_start:
            return None, "insufficient_history"
        s_from = i - (int(ts[i]) - S) // interval
        e_idx = s_from + session_ms // interval
        lows = lo[s_from:e_idx]
        highs = h[s_from:e_idx]
        low_k = int(np.argmin(lows))
        high_k = int(np.argmax(highs))
        return {"S": S, "e": e_idx, "s_from": s_from, "support_from": first_index,
                "long": {"level": float(lows[low_k]), "level_i": s_from + low_k},
                "short": {"level": float(highs[high_k]), "level_i": s_from + high_k}}, ""

    def gap_for(sd, b0):
        best = None
        for k in range(b0 - 1, b0 - p["gap_window"] - 1, -1):
            if k - 2 < 0:
                break
            a_k = atr[k]
            if not np.isfinite(a_k):
                continue
            if sd.sign > 0:
                bottom, top = h[k], lo[k - 2]
            else:
                bottom, top = h[k - 2], lo[k]
            if top > bottom and top - bottom >= p["min_fvg_atr"] * a_k:
                best = (k, float(bottom), float(top), float(a_k))
                break
        return best

    def evidence_minutes(S, b_end):
        minutes = []
        start_idx = session_info["s_from"]
        for j in range(start_idx, b_end + 1):
            bf = bar_flow(j)
            if bf is None:
                return None, "flow_unavailable"
            if not bf["matches"]:
                return None, "flow_volume_mismatch"
            minutes.extend(bf["minutes"])
        return minutes, ""

    def arm(sd, i, d):
        info = session_info
        level_i = info[sd.side]["level_i"]
        minutes, why = evidence_minutes(info["S"], sd.b_end)
        if why:
            return why
        if not all(minute_available(iv, d) for _, iv in minutes):
            return "flow_unavailable"
        cum = Decimal(0)
        cum_at = {}
        for j in range(info["s_from"], sd.b_end + 1):
            cum += bar_flow(j)["delta"]
            cum_at[j] = cum
        level_delta = cum_at[level_i]
        sweep_cums = [(cum_at[j], j) for j in range(sd.b0, sd.b_end + 1)]
        if sd.sign > 0:
            ext_delta, ext_j = min(sweep_cums, key=lambda t: (t[0], t[1]))
        else:
            ext_delta, ext_j = max(sweep_cums, key=lambda t: (t[0], -t[1]))
        total = Decimal(0)
        for j in range(level_i + 1, ext_j + 1):
            total += bar_flow(j)["total"]
        if total == 0:
            return "zero_flow_volume"
        change = (ext_delta - level_delta) * sd.sign
        with localcontext() as ctx:
            ctx.prec = 40
            ratio = float(change / total)
        if p["absorption_filter"]:
            if change <= 0:
                return "absorption_new_delta_extreme"
            if not ratio > p["absorption_threshold"]:
                return "absorption_failed"
        bias = htf_bias_at(htf, d)
        if bias is None:
            return "htf_unavailable"
        if bias != sd.sign:
            return "htf_bias_rejected"
        parts = [[start, str(iv["buy"]), str(iv["sell"]), str(iv["total"]), iv["trades"], iv["hash"]]
                 for start, iv in minutes]
        sd.minutes = [iv for _, iv in minutes]
        gap_k, bottom, top, _ = sd.gap
        depth = top - bottom
        entry_price = top - p["entry_level"] * depth if sd.sign > 0 else bottom + p["entry_level"] * depth
        sd.armed = i
        sd.entry_price = entry_price
        sd.ev = {
            "sai_side": sd.side,
            "sai_setup_id": f"{coin}:{info['S']}:{sd.side}:{int(ts[sd.b0])}:{int(ts[sd.b_end])}:{int(ts[gap_k])}",
            "sai_flow_sha256": _flow_sha256(parts),
            "sai_session_ms": float(info["S"]),
            "sai_session_level": float(info[sd.side]["level"]),
            "sai_session_level_ms": float(ts[level_i]),
            "sai_sweep_start_ms": float(ts[sd.b0]),
            "sai_sweep_end_ms": float(ts[sd.b_end]),
            "sai_sweep_wick": float(sd.extreme),
            "sai_delta_session_level": float(level_delta),
            "sai_delta_sweep": float(ext_delta),
            "sai_flow_volume": float(total),
            "sai_absorption_ratio": ratio,
            "sai_gap_bottom": bottom,
            "sai_gap_top": top,
            "sai_gap_ms": float(ts[gap_k]),
            "sai_ce": (bottom + top) / 2.0,
            "sai_entry_price": float(entry_price),
            "sai_displacement_body": float(abs(c[i] - o[i])),
            "sai_displacement_atr": float(atr[i]),
            "sai_confirm_ms": float(ts[i]),
            "sai_htf_bias": float(bias),
            "sai_flow_minutes": float(len(minutes)),
        }
        return ""

    def step_side(sd, i, j, d, events):
        L = session_info[sd.side]["level"]
        sign = sd.sign
        if sd.state == "await_sweep":
            if j >= p["sweep_window"]:
                sd.state = "done"
                return ""
            a_i = atr[i]
            if not np.isfinite(a_i):
                return "insufficient_history"
            beyond = (L - lo[i]) if sign > 0 else (h[i] - L)
            if beyond > 0 and beyond >= p["sweep_atr_mult"] * a_i:
                sd.state = "in_sweep"
                sd.b0 = i
                sd.extreme = lo[i] if sign > 0 else h[i]
            else:
                return ""
        if sd.state == "in_sweep":
            if i != sd.b0:
                if (sign > 0 and lo[i] < sd.extreme) or (sign < 0 and h[i] > sd.extreme):
                    sd.extreme = lo[i] if sign > 0 else h[i]
            reclaimed = c[i] > L if sign > 0 else c[i] < L
            if reclaimed:
                sd.b_end = i
                gap = gap_for(sd, sd.b0)
                if gap is None:
                    sd.state = "done"
                    events.append("no_gap")
                    return ""
                sd.gap = gap
                sd.state = "await_confirm"
                return ""
            if i - sd.b0 + 1 >= p["max_sweep_bars"]:
                sd.state = "done"
                events.append("sweep_not_reclaimed")
            return ""
        if sd.state == "await_confirm":
            if i - sd.b_end > p["confirm_window"]:
                sd.state = "done"
                events.append("no_displacement")
                return ""
            if (sign > 0 and c[i] < sd.extreme) or (sign < 0 and c[i] > sd.extreme):
                sd.state = "done"
                events.append("wick_broken")
                return ""
            _, bottom, top, _ = sd.gap
            crossed = c[i] > top if sign > 0 else c[i] < bottom
            if not crossed:
                if i - sd.b_end == p["confirm_window"]:
                    sd.state = "done"
                    events.append("no_displacement")
                return ""
            a_i = atr[i]
            if not np.isfinite(a_i):
                return "insufficient_history"
            body = (c[i] - o[i]) * sign
            if not (body > 0 and body >= p["min_displacement_atr"] * a_i):
                sd.state = "done"
                events.append("displacement_weak")
                return ""
            if flow.error:
                return flow.error
            why = arm(sd, i, d)
            if why == "htf_unavailable":
                return why
            if why:
                sd.state = "done"
                events.append(why)
                return ""
            sd.state = "armed"
            events.append("armed_" + sd.side)
            return ""
        if sd.state == "armed":
            if i - sd.armed > E:
                sd.state = "done"
                events.append("setup_expired")
                return ""
            if flow.error:
                return flow.error
            if not all(minute_available(iv, d) for iv in sd.minutes):
                sd.state = "done"
                events.append("setup_cleared_flow")
                return ""
            _, bottom, top, _ = sd.gap
            if (sign > 0 and c[i] < sd.extreme) or (sign < 0 and c[i] > sd.extreme):
                sd.state = "done"
                events.append("wick_broken")
                return ""
            if (sign > 0 and h[i] < bottom) or (sign < 0 and lo[i] > top):
                sd.state = "done"
                events.append("retest_failed")
                return ""
            touched = lo[i] <= sd.entry_price if sign > 0 else h[i] >= sd.entry_price
            if touched:
                sd.state = "done"
                events.append("entry_" + sd.side)
                return ""
            if i - sd.armed == E:
                sd.state = "done"
                events.append("setup_expired")
            return ""
        return ""

    for i in range(n):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        t = int(ts[i])
        d = t + interval
        deadline[i] = d
        if flow.cutoff is not None and d > flow.cutoff:
            reason[i] = "bar_not_closed"
            continue
        phase = (t - start_off) % DAY_MS
        if phase < session_ms:
            if ref_session is not None and any(sd.active for sd in sides.values()) and not taint:
                reason[i] = "session_reset"
            else:
                reason[i] = "in_session"
            ref_session, sides, taint, session_info = None, {}, "", None
            continue
        S = t - phase
        j = (t - (S + session_ms)) // interval
        if ref_session != S:
            ref_session = S
            taint = ""
            sides = {"long": _Side("long"), "short": _Side("short")}
            if j >= post_bars:
                for sd in sides.values():
                    sd.state = "done"
                session_info = None
            else:
                start_i = i - j
                if start_i < 0 or contiguous_start[i] > start_i:
                    taint = "insufficient_history"
                    session_info = None
                else:
                    session_info, why = setup_session(start_i, S)
                    if why:
                        taint = why
                if j > 0 and not taint:
                    taint = "insufficient_history"
        if j >= post_bars:
            for sd in sides.values():
                sd.state = "done"
        if taint:
            reason[i] = taint
            continue
        if all(sd.done for sd in sides.values()):
            reason[i] = "no_setup"
            continue
        if contiguous_start[i] > session_info["support_from"]:
            taint = "bar_spacing"
            reason[i] = taint
            continue
        events = []
        held = ""
        for side in ("long", "short"):
            sd = sides[side]
            if sd.done:
                continue
            why = step_side(sd, i, j, d, events)
            if why:
                held = why
        if held:
            taint = held
            reason[i] = held
            continue
        long_hit = "entry_long" in events
        short_hit = "entry_short" in events
        if long_hit and short_hit:
            events.append("opposing_entries")
            events.remove("entry_long")
            events.remove("entry_short")
        elif long_hit or short_hit:
            side = "long" if long_hit else "short"
            signal[i] = 1 if long_hit else -1
            put(i, sides[side].ev)
        if not ev_cols["sai_side"][i]:
            for side in ("long", "short"):
                if ("armed_" + side) in events:
                    put(i, sides[side].ev)
                    break
        for want in EVENT_PRIORITY:
            if want in events:
                reason[i] = want
                break
        else:
            states = [sd.state for sd in sides.values() if not sd.done]
            if "armed" in states:
                reason[i] = "setup_active"
                for side in ("long", "short"):
                    if sides[side].state == "armed" and not ev_cols["sai_side"][i]:
                        put(i, sides[side].ev)
            elif "await_confirm" in states:
                reason[i] = "awaiting_displacement"
            elif "in_sweep" in states:
                reason[i] = "awaiting_reclaim"
            elif "await_sweep" in states:
                reason[i] = "awaiting_sweep"
            else:
                reason[i] = "no_setup"
        if not flow.error:
            bf = bar_flow(i)
            flow_available[i] = 1.0 if (bf is not None and bf["matches"]) else 0.0

    cutoff, declared_boundary, runtime = _decision_time(result.attrs)
    if runtime and n > 0:
        i = n - 1
        held = ""
        if not row_reason[i]:
            row_boundary = int(ts[i]) + interval
            if cutoff is None or declared_boundary is None:
                held = "decision_time_missing"
            elif row_boundary != declared_boundary:
                held = "decision_boundary_mismatch"
            elif cutoff - row_boundary < 0:
                held = "decision_bar_future"
            elif cutoff - row_boundary >= interval:
                held = "decision_bar_stale"
        if held:
            signal[i] = 0
            reason[i] = held

    for col in TEXT_COLUMNS:
        result[col] = np.array([x if x is not None else "" for x in ev_cols[col]], dtype=object)
    for col in NUMERIC_COLUMNS:
        result[col] = np.array([x if x is not None else np.nan for x in ev_cols[col]], dtype="float64")
    result["sai_deadline_ms"] = deadline
    result["sai_flow_available"] = flow_available
    result["sai_reason"] = reason
    result["sai_reason_code"] = np.array([float(REASON_CODES[r]) for r in reason], dtype="float64")
    result["signal"] = signal
    return result
