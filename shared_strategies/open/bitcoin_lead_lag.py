import hashlib
import json
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
from lead_lag_inputs import BASIS_SEALED, CONTEXT_KIND, LEAD_LAG_STRATEGY, LEADER_COIN, support_bars

STRATEGY = LEAD_LAG_STRATEGY
CONTEXT_PARAM = "leader_context"
SIGNAL_MODES = ("lead_lag", "follower_momentum", "btc_trend_filter")
SUPPORTED_INTERVALS_MS = (900_000, 3_600_000, 14_400_000)
PROPOSED_SHORT_NAME = "btclag"
PROPOSED_DIRECTION = "both"

DEFAULT_PARAMS = {
    "lag_bars": 1,
    "horizon_bars": 4,
    "training_bars": 240,
    "impulse_z": 2.0,
    "min_r2": 0.01,
    "min_edge_bps": 5.0,
    "cost_bps": 20.0,
    "max_data_age_ms": 300_000,
    "expected_interval_ms": 3_600_000,
    "signal_mode": "lead_lag",
}

INT_BOUNDS = {
    "lag_bars": (1, 6),
    "horizon_bars": (1, 12),
    "training_bars": (48, 720),
    "max_data_age_ms": (60_000, 86_400_000),
    "expected_interval_ms": (900_000, 14_400_000),
}

FLOAT_BOUNDS = {
    "impulse_z": (0.5, 6.0, True),
    "min_r2": (0.0, 1.0, False),
    "min_edge_bps": (0.0, 500.0, True),
    "cost_bps": (0.0, 500.0, True),
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_event": 0,
    "event_active": 2,
    "event_consumed": 3,
    "conflicting_events": 4,
    "insufficient_history": 10,
    "missing_columns": 11,
    "cadence_mismatch": 12,
    "missing_timestamps": 13,
    "nonfinite_input": 14,
    "timestamp_order": 15,
    "bar_spacing": 16,
    "leader_bar_missing": 17,
    "leader_unavailable": 18,
    "follower_is_leader": 19,
    "context_mismatch": 20,
    "decision_boundary_mismatch": 21,
    "event_stale": 22,
    "event_future": 23,
    "receipt_unknown": 24,
    "receipt_future": 25,
    "receipt_stale": 26,
}

TEXT_COLUMNS = ("bll_reason", "bll_event_id", "bll_input_reason", "bll_support_sha256", "bll_mode")

NUMERIC_COLUMNS = (
    "bll_event_open_ms", "bll_event_return", "bll_event_sigma", "bll_event_z", "bll_direction",
    "bll_forecast_start_ms", "bll_forecast_end_ms", "bll_slope", "bll_intercept", "bll_r2",
    "bll_slope_first", "bll_slope_second", "bll_train_first_ms", "bll_train_last_ms", "bll_train_pairs",
    "bll_forecast", "bll_completed", "bll_remaining_bps", "bll_threshold_bps", "bll_btc_trend",
    "bll_decision_open_ms", "bll_follower_close", "bll_leader_close", "bll_support_first_ms",
    "bll_support_bars", "bll_cutoff_ms", "bll_boundary_ms", "bll_event_age_ms",
)


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
    lo, hi, hi_inclusive = FLOAT_BOUNDS[name]
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be a number, got {value!r}")
    as_float = float(value)
    upper_ok = as_float <= hi if hi_inclusive else as_float < hi
    if not math.isfinite(as_float) or as_float < lo or not upper_ok:
        bound = "]" if hi_inclusive else ")"
        raise ValueError(f"{STRATEGY}: {name} must be finite and in [{lo}, {hi}{bound}, got {value!r}")
    return as_float


def required_history_bars(lag_bars: int, horizon_bars: int, training_bars: int) -> int:
    return support_bars(lag_bars, horizon_bars, training_bars)


def validate_lead_lag_params(lag_bars, horizon_bars, training_bars, impulse_z, min_r2, min_edge_bps, cost_bps,
                             max_data_age_ms, expected_interval_ms, signal_mode) -> dict:
    out = {name: _int_param(name, value) for name, value in (
        ("lag_bars", lag_bars), ("horizon_bars", horizon_bars), ("training_bars", training_bars),
        ("max_data_age_ms", max_data_age_ms), ("expected_interval_ms", expected_interval_ms))}
    for name, value in (("impulse_z", impulse_z), ("min_r2", min_r2), ("min_edge_bps", min_edge_bps),
                        ("cost_bps", cost_bps)):
        out[name] = _float_param(name, value)
    if out["expected_interval_ms"] not in SUPPORTED_INTERVALS_MS:
        raise ValueError(f"{STRATEGY}: expected_interval_ms {out['expected_interval_ms']} is not a supported cadence "
                         f"{list(SUPPORTED_INTERVALS_MS)} (15m, 1h, 4h)")
    if not isinstance(signal_mode, str) or signal_mode not in SIGNAL_MODES:
        raise ValueError(f"{STRATEGY}: signal_mode must be one of {list(SIGNAL_MODES)}, got {signal_mode!r}")
    out["signal_mode"] = signal_mode
    if out["training_bars"] % 2:
        raise ValueError(f"{STRATEGY}: training_bars must be even so the two stability halves are equal, "
                         f"got {out['training_bars']}")
    out["support_bars"] = required_history_bars(out["lag_bars"], out["horizon_bars"], out["training_bars"])
    return out


def lead_lag_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{STRATEGY}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS) - {CONTEXT_PARAM})
    if unknown:
        raise ValueError(f"{STRATEGY}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **{k: v for k, v in params.items() if k in DEFAULT_PARAMS}}
    validate_lead_lag_params(**merged)


def merged_params(params) -> dict:
    merged = {**DEFAULT_PARAMS, **{k: v for k, v in (params or {}).items() if k in DEFAULT_PARAMS}}
    return validate_lead_lag_params(**merged)


def _int_or_none(value):
    if value is None or isinstance(value, (bool, np.bool_)):
        return None
    if isinstance(value, (int, np.integer)):
        return int(value)
    if isinstance(value, (float, np.floating)) and math.isfinite(float(value)) and float(value).is_integer():
        return int(value)
    return None


def frame_open_times_ms(df: pd.DataFrame, interval: int):
    if isinstance(df.index, pd.DatetimeIndex):
        idx = df.index
        if idx.tz is not None:
            idx = idx.tz_convert("UTC").tz_localize(None)
        ns_per_unit = {"s": 1_000_000_000, "ms": 1_000_000, "us": 1_000, "ns": 1}[idx.unit]
        missing = np.asarray(idx.isna())
        raw = np.where(missing, 0, idx.asi8)
        values = ((raw * ns_per_unit) // 1_000_000).astype("float64")
        values[missing] = np.nan
    elif "timestamp" in df.columns:
        values = pd.to_numeric(df["timestamp"], errors="coerce").to_numpy(dtype="float64")
    else:
        return None
    kind = df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR)
    if kind not in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE):
        finite = values[np.isfinite(values)]
        kind = TIMESTAMP_KIND_NATIVE_CLOSE if (
            len(finite) and bool((np.mod(finite, interval) == interval - 1).all())) else TIMESTAMP_KIND_OPEN
    if kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        values = values - interval + 1
    return values


def _context_leader(ctx, follower: str):
    if ctx is None:
        return None, "leader_unavailable", "no leader context was supplied; leader rows come only from supplied inputs"
    if not isinstance(ctx, dict) or ctx.get("kind") != CONTEXT_KIND:
        return None, "leader_unavailable", f"leader context is not a {CONTEXT_KIND} object"
    if not ctx.get("available"):
        code = str(ctx.get("hold_reason") or "leader_unavailable")
        if code == "follower_is_leader":
            return None, code, str(ctx.get("detail") or "")
        return None, "leader_unavailable", f"{code}: {ctx.get('detail') or ''}"
    if ctx.get("leader") != LEADER_COIN:
        return None, "context_mismatch", f"leader {ctx.get('leader')!r} is not {LEADER_COIN}"
    if follower and ctx.get("follower") not in (None, follower):
        return None, "context_mismatch", f"context follower {ctx.get('follower')!r} is not {follower!r}"
    opens = ctx.get("leader_open_ms")
    closes = ctx.get("leader_close")
    if not isinstance(opens, (list, tuple)) or not isinstance(closes, (list, tuple)) or len(opens) != len(closes):
        return None, "context_mismatch", "leader rows are malformed"
    table = {}
    prev = None
    for k, (t, c) in enumerate(zip(opens, closes)):
        t = _int_or_none(t)
        if t is None or (prev is not None and t <= prev):
            return None, "context_mismatch", f"leader row {k} has a missing, duplicate or reordered opening time"
        prev = t
        try:
            value = float(c)
        except (TypeError, ValueError):
            continue
        if math.isfinite(value) and value > 0:
            table[t] = value
    return table, "", ""


def _runs(valid: np.ndarray, opens: np.ndarray, interval: int) -> np.ndarray:
    n = len(valid)
    start = np.full(n, -1, dtype=np.int64)
    for k in range(n):
        if not valid[k]:
            continue
        if k > 0 and valid[k - 1] and start[k - 1] >= 0 and opens[k] - opens[k - 1] == interval:
            start[k] = start[k - 1]
        else:
            start[k] = k
    return start


def _support_sha256(opens, follower, leader, lo, hi) -> str:
    rows = [[int(opens[k]), float(follower[k]), float(leader[k])] for k in range(lo, hi + 1)]
    return hashlib.sha256(json.dumps(rows, separators=(",", ":")).encode("utf-8")).hexdigest()


def _hold_all(result: pd.DataFrame, reason: str, p: dict, detail: str = "") -> pd.DataFrame:
    n = len(result)
    for col in NUMERIC_COLUMNS:
        result[col] = np.full(n, np.nan)
    for col in TEXT_COLUMNS:
        result[col] = np.full(n, "", dtype=object)
    result["bll_mode"] = np.full(n, p.get("signal_mode", ""), dtype=object)
    result["bll_reason"] = np.full(n, reason, dtype=object)
    result["bll_input_reason"] = np.full(n, detail, dtype=object)
    result["bll_reason_code"] = np.full(n, REASON_CODES[reason], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


class _Fits:
    def __init__(self, rl, rf, y, run_start, lag, horizon, training):
        self.rl = rl
        self.rf = rf
        self.y = y
        self.run_start = run_start
        self.lag = lag
        self.horizon = horizon
        self.training = training
        self.cache = {}

    def train_range(self, d):
        last = d - self.lag - self.horizon + 1
        first = last - self.training + 1
        return first, last

    def clean(self, lo, hi):
        if lo < 0 or hi >= len(self.run_start):
            return False
        start = self.run_start[hi]
        return start >= 0 and start <= lo

    def fit(self, d):
        if d in self.cache:
            return self.cache[d]
        first, last = self.train_range(d)
        out = None
        if self.clean(first - 1, d):
            x = self.rl[first:last + 1]
            y = self.y[first:last + 1]
            f = self.rf[first:last + 1]
            out = _ols(x, y, f, self.training)
            if out is not None:
                out["first"], out["last"] = first, last
        self.cache[d] = out
        return out


def _ols(x, y, f, training):
    if len(x) != training or not (np.isfinite(x).all() and np.isfinite(y).all() and np.isfinite(f).all()):
        return None
    sigma_x = float(np.std(x))
    sigma_f = float(np.std(f))
    coef = _slope(x, y)
    if coef is None:
        return None
    slope, intercept, r2 = coef
    half = training // 2
    first = _slope(x[:half], y[:half])
    second = _slope(x[half:], y[half:])
    return {
        "slope": slope, "intercept": intercept, "r2": r2,
        "slope_first": first[0] if first else float("nan"),
        "slope_second": second[0] if second else float("nan"),
        "sigma_x": sigma_x, "sigma_f": sigma_f,
    }


def _slope(x, y):
    mx = float(np.mean(x))
    my = float(np.mean(y))
    dx = x - mx
    dy = y - my
    sxx = float(np.dot(dx, dx))
    if not sxx > 0:
        return None
    sxy = float(np.dot(dx, dy))
    syy = float(np.dot(dy, dy))
    slope = sxy / sxx
    intercept = my - slope * mx
    r2 = (sxy * sxy) / (sxx * syy) if syy > 0 else 0.0
    if not (math.isfinite(slope) and math.isfinite(intercept) and math.isfinite(r2)):
        return None
    return slope, intercept, r2


def bitcoin_lead_lag_core(
    df: pd.DataFrame,
    lag_bars: int = 1,
    horizon_bars: int = 4,
    training_bars: int = 240,
    impulse_z: float = 2.0,
    min_r2: float = 0.01,
    min_edge_bps: float = 5.0,
    cost_bps: float = 20.0,
    max_data_age_ms: int = 300_000,
    expected_interval_ms: int = 3_600_000,
    signal_mode: str = "lead_lag",
    leader_context=None,
) -> pd.DataFrame:
    p = validate_lead_lag_params(lag_bars, horizon_bars, training_bars, impulse_z, min_r2, min_edge_bps, cost_bps,
                                 max_data_age_ms, expected_interval_ms, signal_mode)
    result = df.copy()
    n = len(result)
    interval = p["expected_interval_ms"]
    if "close" not in result.columns:
        return _hold_all(result, "missing_columns", p)
    declared = result.attrs.get(FRAME_INTERVAL_ATTR)
    if declared is not None and _int_or_none(declared) != interval:
        return _hold_all(result, "cadence_mismatch", p, f"frame cadence {declared!r} != {interval}")
    opens = frame_open_times_ms(result, interval)
    if opens is None:
        return _hold_all(result, "missing_timestamps", p)
    follower = str((leader_context or {}).get("follower") or "") if isinstance(leader_context, dict) else ""
    if follower == LEADER_COIN:
        return _hold_all(result, "follower_is_leader", p, "the follower is the BTC leader")
    table, why, detail = _context_leader(leader_context, follower)
    if table is None:
        return _hold_all(result, why, p, detail)

    L, H, T, S = p["lag_bars"], p["horizon_bars"], p["training_bars"], p["support_bars"]
    z, mode = p["impulse_z"], p["signal_mode"]
    threshold_bps = p["cost_bps"] + p["min_edge_bps"]
    fclose = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    lclose = np.full(n, np.nan)
    row_reason = np.full(n, "", dtype=object)
    prev = None
    for k in range(n):
        t = opens[k]
        if not np.isfinite(t):
            row_reason[k] = "missing_timestamps"
            continue
        if t % interval:
            row_reason[k] = "bar_spacing"
            continue
        if prev is not None and not t > prev:
            row_reason[k] = "timestamp_order"
        prev = t if prev is None else max(prev, t)
        if row_reason[k]:
            continue
        if not (np.isfinite(fclose[k]) and fclose[k] > 0):
            row_reason[k] = "nonfinite_input"
            continue
        lv = table.get(int(t))
        if lv is None:
            row_reason[k] = "leader_bar_missing"
            continue
        lclose[k] = lv
    valid = row_reason == ""
    run_start = _runs(valid, np.nan_to_num(opens, nan=-1.0).astype(np.int64), interval)

    with np.errstate(invalid="ignore", divide="ignore"):
        lf = np.where(valid, np.log(np.where(valid, fclose, 1.0)), np.nan)
        ll = np.where(valid, np.log(np.where(valid, lclose, 1.0)), np.nan)
    rl = np.full(n, np.nan)
    rf = np.full(n, np.nan)
    for k in range(1, n):
        if run_start[k] >= 0 and run_start[k] <= k - 1:
            rl[k] = ll[k] - ll[k - 1]
            rf[k] = lf[k] - lf[k - 1]
    y = np.full(n, np.nan)
    for j in range(n):
        s, e = j + L - 1, j + L + H - 1
        if e < n and 0 <= run_start[e] <= s:
            y[j] = lf[e] - lf[s]
    fits = _Fits(rl, rf, y, run_start, L, H, T)

    def impulse(j):
        fj = fits.fit(j)
        if fj is None or not np.isfinite(rl[j]) or not np.isfinite(rf[j]):
            return None
        if mode == "lead_lag":
            sigma, move = fj["sigma_x"], rl[j]
        else:
            sigma, move = fj["sigma_f"], rf[j]
        if not sigma > 0 or move == 0 or not abs(move) >= z * sigma:
            return None
        direction = 1 if move > 0 else -1
        trend = float("nan")
        if mode == "btc_trend_filter":
            if not fits.clean(j - H, j):
                return None
            trend = float(np.sum(rl[j - H + 1:j + 1]))
            if not (trend > 0 if direction > 0 else trend < 0):
                return None
        return {"j": j, "move": float(move), "sigma": float(sigma), "direction": direction, "trend": trend}

    def qualifies(ev, d):
        j = ev["j"]
        if mode != "lead_lag":
            return (d == j), None
        fd = fits.fit(d)
        if fd is None:
            return False, None
        s, e = j + L - 1, j + L + H - 1
        if not (j <= d <= e - 1):
            return False, None
        slope, intercept, r2 = fd["slope"], fd["intercept"], fd["r2"]
        if not (slope > 0 and fd["slope_first"] > 0 and fd["slope_second"] > 0 and r2 >= p["min_r2"]):
            return False, None
        forecast = intercept + slope * ev["move"]
        completed = (lf[d] - lf[s]) if d > s else 0.0
        remaining_bps = (forecast - completed) * 10_000.0
        info = {"fit": fd, "forecast": forecast, "completed": completed, "remaining_bps": remaining_bps}
        if not (ev["direction"] * forecast > 0):
            return False, info
        return ev["direction"] * remaining_bps > threshold_bps, info

    span = (L + H - 1) if mode == "lead_lag" else 1
    events = {}

    def event_at(j):
        if j not in events:
            events[j] = impulse(j) if j >= 0 else None
        return events[j]

    first_cache = {}

    def first_qualifying(ev, upto):
        j = ev["j"]
        key = j
        state = first_cache.get(key)
        if state is None:
            state = {"next": j, "found": None, "info": None}
            first_cache[key] = state
        last = min(upto, j + span - 1)
        while state["found"] is None and state["next"] <= last:
            d = state["next"]
            ok, info = qualifies(ev, d)
            if ok:
                state["found"], state["info"] = d, info
            state["next"] = d + 1
        if state["found"] is not None and state["found"] <= upto:
            return state["found"], state["info"]
        return None, None

    signal = np.zeros(n, dtype=np.int64)
    reason = np.full(n, "no_event", dtype=object)
    cols = {c: np.full(n, np.nan) for c in NUMERIC_COLUMNS}
    text = {c: np.full(n, "", dtype=object) for c in TEXT_COLUMNS}
    text["bll_mode"][:] = mode
    timeframe_label = {900_000: "15m", 3_600_000: "1h", 14_400_000: "4h"}[interval]

    for i in range(n):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        cols["bll_decision_open_ms"][i] = opens[i]
        cols["bll_follower_close"][i] = fclose[i]
        cols["bll_leader_close"][i] = lclose[i]
        lo = i - S + 1
        if not fits.clean(lo, i):
            reason[i] = "insufficient_history"
            continue
        cols["bll_support_first_ms"][i] = opens[lo]
        cols["bll_support_bars"][i] = S
        active = []
        for j in range(i - span + 1, i + 1):
            ev = event_at(j)
            if ev is not None:
                active.append(ev)
        if not active:
            continue
        firing = []
        consumed = False
        for ev in active:
            d, info = first_qualifying(ev, i)
            if d == i:
                firing.append((ev, info))
            elif d is not None:
                consumed = True
        if not firing:
            reason[i] = "event_consumed" if consumed else "event_active"
            latest = active[-1]
            cols["bll_event_open_ms"][i] = opens[latest["j"]]
            continue
        directions = {ev["direction"] for ev, _ in firing}
        if len(directions) > 1:
            reason[i] = "conflicting_events"
            continue
        ev, info = max(firing, key=lambda pair: pair[0]["j"])
        j = ev["j"]
        signal[i] = ev["direction"]
        reason[i] = "entry_long" if ev["direction"] > 0 else "entry_short"
        source = LEADER_COIN if mode == "lead_lag" else (follower or "follower")
        text["bll_event_id"][i] = (f"{source}:{timeframe_label}:{int(opens[j])}:"
                                   f"{'up' if ev['direction'] > 0 else 'down'}:{mode}")
        cols["bll_event_open_ms"][i] = opens[j]
        cols["bll_event_return"][i] = ev["move"]
        cols["bll_event_sigma"][i] = ev["sigma"]
        cols["bll_event_z"][i] = abs(ev["move"]) / ev["sigma"]
        cols["bll_direction"][i] = ev["direction"]
        cols["bll_btc_trend"][i] = ev["trend"]
        fd = fits.fit(i)
        if info is not None:
            fd = info["fit"]
            cols["bll_forecast"][i] = info["forecast"]
            cols["bll_completed"][i] = info["completed"]
            cols["bll_remaining_bps"][i] = info["remaining_bps"]
            cols["bll_forecast_start_ms"][i] = opens[j + L - 1]
            cols["bll_forecast_end_ms"][i] = opens[j] + (L + H - 1) * interval
        if fd is not None:
            cols["bll_slope"][i] = fd["slope"]
            cols["bll_intercept"][i] = fd["intercept"]
            cols["bll_r2"][i] = fd["r2"]
            cols["bll_slope_first"][i] = fd["slope_first"]
            cols["bll_slope_second"][i] = fd["slope_second"]
            cols["bll_train_first_ms"][i] = opens[fd["first"]]
            cols["bll_train_last_ms"][i] = opens[fd["last"]]
            cols["bll_train_pairs"][i] = T
        cols["bll_threshold_bps"][i] = threshold_bps
        text["bll_support_sha256"][i] = _support_sha256(opens, fclose, lclose, lo, i)

    if isinstance(leader_context, dict) and leader_context.get("basis") == BASIS_SEALED and n > 0:
        _sealed_gate(leader_context, p, opens, interval, signal, reason, cols, text)

    for col, values in cols.items():
        result[col] = values
    for col, values in text.items():
        result[col] = values
    result["bll_reason"] = reason
    result["bll_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result


def _sealed_gate(ctx, p, opens, interval, signal, reason, cols, text):
    i = len(opens) - 1
    cutoff = _int_or_none(ctx.get("cutoff_ms"))
    boundary = _int_or_none(ctx.get("boundary_ms"))
    sealed = _int_or_none(ctx.get("sealed_at_ms"))
    limit = p["max_data_age_ms"]
    held = ""
    detail = ""
    if cutoff is not None:
        cols["bll_cutoff_ms"][i] = cutoff
    if boundary is not None:
        cols["bll_boundary_ms"][i] = boundary
    row_boundary = int(opens[i]) + interval if np.isfinite(opens[i]) else None
    if cutoff is None or boundary is None or row_boundary != boundary:
        held = "decision_boundary_mismatch"
        detail = f"decision row boundary {row_boundary} != sealed boundary {boundary}"
    else:
        age = cutoff - boundary
        cols["bll_event_age_ms"][i] = age
        if age < 0:
            held, detail = "event_future", f"boundary {boundary} is after the cutoff {cutoff}"
        elif age > limit:
            held, detail = "event_stale", f"closed bar is {age} ms old, over max_data_age_ms {limit}"
        else:
            for role in ("leader", "follower"):
                recv = _int_or_none(ctx.get(f"{role}_last_recv_at_ms"))
                if recv is None or sealed is None:
                    held, detail = "receipt_unknown", f"{role} frame carries no receipt time"
                elif recv > sealed:
                    held, detail = "receipt_future", f"{role} receipt {recv} is after the seal {sealed}"
                elif sealed - recv > limit:
                    held, detail = "receipt_stale", f"{role} receipt is {sealed - recv} ms old, over {limit}"
                if held:
                    break
    if held:
        signal[i] = 0
        reason[i] = held
        text["bll_input_reason"][i] = detail
