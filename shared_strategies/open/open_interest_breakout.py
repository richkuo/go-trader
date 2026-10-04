import hashlib
import json
import math
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

STRATEGY = "open_interest_breakout"
OBSERVATION_KIND = "open_interest"
ACCEPTED_UNITS = ("base",)
ACCEPTED_TIME_BASES = ("receipt", "archive_timestamp")
REQUIRED_COLUMNS = ("high", "low", "close")

PARAM_BOUNDS = {
    "price_lookback": (2, 200),
    "oi_lookback": (1, 96),
    "max_observation_age_ms": (1_000, 3_600_000),
    "observation_cadence_ms": (1_000, 3_600_000),
    "max_gap_ms": (1_000, 86_400_000),
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_breakout": 0,
    "oi_below_threshold": 2,
    "breakout_continuation": 3,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "inconsistent_hlc": 13,
    "timestamp_order": 14,
    "missing_columns": 16,
    "bar_spacing": 17,
    "bar_not_closed": 18,
    "observations_unavailable": 20,
    "unsupported_observations": 21,
    "observation_order": 22,
    "observation_duplicate": 23,
    "unaligned_endpoint": 24,
    "insufficient_observation_history": 25,
    "stale_endpoint": 26,
    "observation_gap": 27,
    "session_change": 28,
    "insufficient_coverage": 29,
    "max_gap_exceeded": 30,
    "nonpositive_open_interest": 31,
    "nonfinite_open_interest": 32,
}

DIAGNOSTIC_COLUMNS = (
    "oib_upper", "oib_lower", "oib_breakout_state", "oib_endpoint_ms", "oib_cutoff_ms",
    "oib_oi_current", "oib_oi_prior", "oib_oi_current_ms", "oib_oi_prior_ms",
    "oib_oi_current_age_ms", "oib_oi_prior_age_ms", "oib_oi_change", "oib_threshold",
    "oib_coverage", "oib_max_gap_ms", "oib_oi_valid", "oib_valid", "oib_reason",
    "oib_reason_code", "oib_carried", "oib_source", "oib_time_basis", "oib_window_sha256",
)


def _int_param(name, value):
    lo, hi = PARAM_BOUNDS[name]
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{STRATEGY}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def _float_param(name, value, lo, hi, lo_inclusive=True, hi_inclusive=False):
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be a number, got {value!r}")
    v = float(value)
    ok = math.isfinite(v)
    ok = ok and (v >= lo if lo_inclusive else v > lo)
    ok = ok and (v <= hi if hi_inclusive else v < hi)
    if not ok:
        lb = "[" if lo_inclusive else "("
        rb = "]" if hi_inclusive else ")"
        raise ValueError(f"{STRATEGY}: {name} must be finite and in {lb}{lo}, {hi}{rb}, got {value!r}")
    return v


def validate_oib_params(price_lookback, oi_lookback, oi_change_threshold, max_observation_age_ms,
                        observation_cadence_ms, min_coverage, max_gap_ms) -> dict:
    out = {
        "price_lookback": _int_param("price_lookback", price_lookback),
        "oi_lookback": _int_param("oi_lookback", oi_lookback),
        "oi_change_threshold": _float_param("oi_change_threshold", oi_change_threshold, 0.0, 1.0),
        "max_observation_age_ms": _int_param("max_observation_age_ms", max_observation_age_ms),
        "observation_cadence_ms": _int_param("observation_cadence_ms", observation_cadence_ms),
        "min_coverage": _float_param("min_coverage", min_coverage, 0.0, 1.0, lo_inclusive=False, hi_inclusive=True),
        "max_gap_ms": _int_param("max_gap_ms", max_gap_ms),
    }
    if out["max_gap_ms"] < out["observation_cadence_ms"]:
        raise ValueError(f"{STRATEGY}: max_gap_ms {out['max_gap_ms']} is below observation_cadence_ms "
                         f"{out['observation_cadence_ms']}")
    if out["max_observation_age_ms"] > out["max_gap_ms"]:
        raise ValueError(f"{STRATEGY}: max_observation_age_ms {out['max_observation_age_ms']} is above "
                         f"max_gap_ms {out['max_gap_ms']}")
    return out


def required_history_bars(price_lookback: int, oi_lookback: int) -> int:
    return max(int(price_lookback) + 2, int(oi_lookback) + 1)


def _timestamps_ms(df: pd.DataFrame):
    if "timestamp" in df.columns:
        return pd.to_numeric(df["timestamp"], errors="coerce").to_numpy(dtype="float64")
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
    return None


def _window_all(flags: np.ndarray, window: int, end_offset: int) -> np.ndarray:
    n = len(flags)
    out = np.zeros(n, dtype=bool)
    if window <= 0 or n < window + end_offset:
        return out
    views = sliding_window_view(flags, window).all(axis=1)
    start = window - 1 + end_offset
    out[start:] = views[: n - start]
    return out


def _window_reduce(values: np.ndarray, window: int, end_offset: int, op) -> np.ndarray:
    n = len(values)
    out = np.full(n, np.nan)
    if n < window + end_offset:
        return out
    views = op(sliding_window_view(values, window), axis=1)
    start = window - 1 + end_offset
    out[start:] = views[: n - start]
    return out


class _Series:
    def __init__(self, error=None, detail=""):
        self.error = error
        self.detail = detail
        self.t = np.zeros(0, dtype=np.int64)
        self.v = np.zeros(0)
        self.session = np.zeros(0, dtype=np.int64)
        self.gaps = []
        self.source = ""
        self.time_basis = ""
        self.cadence = 0
        self.cutoff = None
        self.offset = None
        self.interval = None


def _int_or_none(value):
    if value is None or isinstance(value, bool):
        return None
    try:
        f = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(f) or not f.is_integer():
        return None
    return int(f)


def parse_observations(obs, cadence_ms: int) -> _Series:
    if obs is None:
        return _Series("observations_unavailable", "no open-interest observations were supplied")
    if not isinstance(obs, dict):
        return _Series("unsupported_observations", "observations must be an object")
    if not obs.get("available", False):
        return _Series("observations_unavailable", str(obs.get("reason") or "observations marked unavailable"))
    series = _Series()
    series.source = str(obs.get("source") or "")
    series.time_basis = str(obs.get("time_basis") or "")
    if obs.get("kind") != OBSERVATION_KIND:
        return _Series("unsupported_observations", f"kind {obs.get('kind')!r} is not {OBSERVATION_KIND!r}")
    if obs.get("units") not in ACCEPTED_UNITS:
        return _Series("unsupported_observations", f"units {obs.get('units')!r} are not one of {ACCEPTED_UNITS}")
    if series.time_basis not in ACCEPTED_TIME_BASES:
        return _Series("unsupported_observations", f"time basis {series.time_basis!r} is not one of {ACCEPTED_TIME_BASES}")
    if not series.source:
        return _Series("unsupported_observations", "observations carry no source identity")
    series.cadence = _int_or_none(obs.get("cadence_ms"))
    if series.cadence != int(cadence_ms):
        return _Series("unsupported_observations",
                       f"observation cadence {obs.get('cadence_ms')!r} differs from the specified {cadence_ms}")
    series.offset = _int_or_none(obs.get("bar_endpoint_offset_ms"))
    series.interval = _int_or_none(obs.get("bar_interval_ms"))
    if series.offset is None or series.interval is None or series.interval <= 0 or series.offset <= 0:
        return _Series("unsupported_observations", "bar_interval_ms and bar_endpoint_offset_ms must be positive integers")
    if obs.get("cutoff_ms") is not None:
        series.cutoff = _int_or_none(obs.get("cutoff_ms"))
        if series.cutoff is None:
            return _Series("unsupported_observations", f"cutoff_ms {obs.get('cutoff_ms')!r} is not an integer")
    samples = obs.get("samples")
    if not isinstance(samples, list):
        return _Series("unsupported_observations", "samples must be a list")
    t = np.empty(len(samples), dtype=np.int64)
    v = np.empty(len(samples))
    sess = np.empty(len(samples), dtype=np.int64)
    seq = np.empty(len(samples), dtype=np.int64)
    for i, s in enumerate(samples):
        if not isinstance(s, dict):
            return _Series("unsupported_observations", f"sample {i} is not an object")
        ti, si, qi = _int_or_none(s.get("recv_ms")), _int_or_none(s.get("session")), _int_or_none(s.get("seq"))
        if ti is None or si is None or qi is None:
            return _Series("unsupported_observations", f"sample {i} lacks an integer recv_ms, session or seq")
        try:
            vi = float(s.get("value"))
        except (TypeError, ValueError):
            vi = float("nan")
        t[i], v[i], sess[i], seq[i] = ti, vi, si, qi
    if len(t) > 1:
        if bool((np.diff(t) < 0).any()):
            return _Series("observation_order", "samples are not in non-decreasing time order")
        pairs = set()
        for si, qi in zip(sess.tolist(), seq.tolist()):
            if (si, qi) in pairs:
                return _Series("observation_duplicate", f"session {si} sequence {qi} appears twice")
            pairs.add((si, qi))
        same = sess[1:] == sess[:-1]
        if bool((same & (seq[1:] <= seq[:-1])).any()):
            return _Series("observation_order", "sequence numbers are not increasing within a session")
    gaps = []
    for i, g in enumerate(obs.get("gaps") or []):
        if not isinstance(g, dict):
            return _Series("unsupported_observations", f"gap {i} is not an object")
        start = _int_or_none(g.get("start_ms"))
        end = None if g.get("end_ms") is None else _int_or_none(g.get("end_ms"))
        detected = start if g.get("detected_ms") is None else _int_or_none(g.get("detected_ms"))
        if (start is None or detected is None or (g.get("end_ms") is not None and end is None)
                or (end is not None and end < start) or detected < start):
            return _Series("unsupported_observations", f"gap {i} is malformed")
        gaps.append((start, end, detected, str(g.get("reason") or "gap")))
    series.t, series.v, series.session, series.gaps = t, v, sess, gaps
    return series


def _empty_columns(result: pd.DataFrame, threshold: float, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in DIAGNOSTIC_COLUMNS:
        result[col] = np.full(n, np.nan)
    result["oib_threshold"] = np.full(n, threshold)
    result["oib_oi_valid"] = np.zeros(n, dtype=bool)
    result["oib_valid"] = np.zeros(n, dtype=bool)
    result["oib_carried"] = np.zeros(n, dtype=bool)
    result["oib_reason"] = np.full(n, reason, dtype=object)
    result["oib_reason_code"] = np.full(n, REASON_CODES[reason], dtype=np.int64)
    result["oib_source"] = np.full(n, "", dtype=object)
    result["oib_time_basis"] = np.full(n, "", dtype=object)
    result["oib_window_sha256"] = np.full(n, "", dtype=object)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def _endpoint_index(t: np.ndarray, endpoints: np.ndarray) -> np.ndarray:
    return np.searchsorted(t, endpoints, side="right") - 1


def _window_sha256(series: _Series, q: int, c: int, gaps: list) -> str:
    doc = {
        "samples": [[int(series.t[k]), float(series.v[k]), int(series.session[k])] for k in range(q, c + 1)],
        "gaps": [[g[0], g[1], g[2], g[3]] for g in gaps],
    }
    return hashlib.sha256(json.dumps(doc, separators=(",", ":")).encode("utf-8")).hexdigest()


def _open_interest_state(series: _Series, endpoints: np.ndarray, prior_endpoints: np.ndarray,
                         rows_ok: np.ndarray, p: dict, hash_rows: np.ndarray):
    n = len(endpoints)
    reason = np.full(n, "", dtype=object)
    window_sha = np.full(n, "", dtype=object)
    cur = np.full(n, np.nan)
    prior = np.full(n, np.nan)
    cur_t = np.full(n, np.nan)
    prior_t = np.full(n, np.nan)
    coverage = np.full(n, np.nan)
    max_gap = np.full(n, np.nan)
    if series.error:
        reason[:] = series.error
        return reason, cur, prior, cur_t, prior_t, coverage, max_gap, window_sha
    cadence = series.cadence
    if series.interval % cadence != 0:
        reason[:] = "unaligned_endpoint"
        return reason, cur, prior, cur_t, prior_t, coverage, max_gap, window_sha
    t, v, sess = series.t, series.v, series.session
    ci = _endpoint_index(t, endpoints)
    pi = _endpoint_index(t, prior_endpoints)
    buckets = np.unique(-(-t // cadence) * cadence) if len(t) else np.zeros(0, dtype=np.int64)
    for i in range(n):
        if not rows_ok[i]:
            continue
        e, pe = int(endpoints[i]), int(prior_endpoints[i])
        if e % cadence or pe % cadence:
            reason[i] = "unaligned_endpoint"
            continue
        c, q = int(ci[i]), int(pi[i])
        if q < 0:
            reason[i] = "insufficient_observation_history"
            continue
        cur_t[i], prior_t[i] = t[c], t[q]
        cur[i], prior[i] = v[c], v[q]
        window_gaps = [g for g in series.gaps
                       if g[0] < e and g[2] <= e and (g[1] is None or g[1] > t[q])]
        if hash_rows[i]:
            window_sha[i] = _window_sha256(series, q, c, window_gaps)
        if window_gaps:
            reason[i] = "observation_gap"
            continue
        if e - t[c] > p["max_observation_age_ms"] or pe - t[q] > p["max_observation_age_ms"]:
            reason[i] = "stale_endpoint"
            continue
        if bool((sess[q:c + 1] != sess[c]).any()):
            reason[i] = "session_change"
            continue
        expected = (e - pe) // cadence
        present = int(np.searchsorted(buckets, e, side="right") - np.searchsorted(buckets, pe, side="right"))
        coverage[i] = present / expected if expected > 0 else np.nan
        times = t[q:c + 1]
        diffs = np.diff(np.concatenate([times, [e]]))
        max_gap[i] = float(diffs.max()) if len(diffs) else 0.0
        if not coverage[i] >= p["min_coverage"]:
            reason[i] = "insufficient_coverage"
            continue
        if max_gap[i] > p["max_gap_ms"]:
            reason[i] = "max_gap_exceeded"
            continue
        if not (math.isfinite(cur[i]) and math.isfinite(prior[i])):
            reason[i] = "nonfinite_open_interest"
            continue
        if cur[i] <= 0 or prior[i] <= 0:
            reason[i] = "nonpositive_open_interest"
            continue
    return reason, cur, prior, cur_t, prior_t, coverage, max_gap, window_sha


def open_interest_breakout_core(
    df: pd.DataFrame,
    price_lookback: int = 20,
    oi_lookback: int = 4,
    oi_change_threshold: float = 0.002,
    max_observation_age_ms: int = 120_000,
    observation_cadence_ms: int = 60_000,
    min_coverage: float = 0.95,
    max_gap_ms: int = 300_000,
    open_interest_observations=None,
) -> pd.DataFrame:
    p = validate_oib_params(price_lookback, oi_lookback, oi_change_threshold, max_observation_age_ms,
                            observation_cadence_ms, min_coverage, max_gap_ms)
    result = df.copy()
    n = len(result)
    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _empty_columns(result, p["oi_change_threshold"], "missing_columns")

    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    finite = np.isfinite(high) & np.isfinite(low) & np.isfinite(close)
    with np.errstate(invalid="ignore"):
        inconsistent = finite & ((high < low) | (close > high) | (close < low))

    series = parse_observations(open_interest_observations, p["observation_cadence_ms"])
    ts = _timestamps_ms(result)
    bad_ts = np.zeros(n, dtype=bool) if ts is not None else np.ones(n, dtype=bool)
    spacing = np.zeros(n, dtype=bool)
    if ts is not None:
        bad_ts = ~np.isfinite(ts)
        if n > 1:
            with np.errstate(invalid="ignore"):
                bad_ts[1:] |= ~(ts[1:] > ts[:-1])
                if series.interval:
                    spacing[1:] = ~bad_ts[1:] & ~bad_ts[:-1] & (ts[1:] - ts[:-1] != series.interval)

    row_reason = np.full(n, "", dtype=object)
    row_reason[spacing] = "bar_spacing"
    row_reason[bad_ts] = "timestamp_order"
    row_reason[inconsistent] = "inconsistent_hlc"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~inconsistent & ~bad_ts & ~spacing

    lookback = p["price_lookback"]
    prior_ok = _window_all(row_ok, lookback, 1)
    upper = _window_reduce(np.where(row_ok, high, -np.inf), lookback, 1, np.max)
    lower = _window_reduce(np.where(row_ok, low, np.inf), lookback, 1, np.min)
    state_ok = prior_ok & row_ok
    upper = np.where(state_ok, upper, np.nan)
    lower = np.where(state_ok, lower, np.nan)
    state = np.full(n, np.nan)
    with np.errstate(invalid="ignore"):
        up = state_ok & (close > upper)
        down = state_ok & (close < lower)
    state[state_ok] = 0.0
    state[up] = 1.0
    state[down] = -1.0
    prev_state_ok = np.zeros(n, dtype=bool)
    prev_up = np.zeros(n, dtype=bool)
    prev_down = np.zeros(n, dtype=bool)
    if n > 1:
        prev_state_ok[1:] = state_ok[:-1]
        prev_up[1:] = up[:-1]
        prev_down[1:] = down[:-1]
    price_ok = state_ok & prev_state_ok & _window_all(row_ok, p["oi_lookback"] + 1, 0)

    endpoints = np.full(n, np.nan)
    if ts is not None and series.offset is not None:
        endpoints = ts + series.offset
    closed = np.ones(n, dtype=bool)
    if series.cutoff is not None:
        with np.errstate(invalid="ignore"):
            closed = endpoints <= series.cutoff
    interval = series.interval or 0
    prior_endpoints = endpoints - p["oi_lookback"] * interval
    eval_rows = price_ok & closed & np.isfinite(endpoints)
    new_up = price_ok & closed & up & ~prev_up
    new_down = price_ok & closed & down & ~prev_down
    oi_reason, cur, prior, cur_t, prior_t, coverage, max_gap, window_sha = _open_interest_state(
        series, np.nan_to_num(endpoints).astype(np.int64), np.nan_to_num(prior_endpoints).astype(np.int64),
        eval_rows, p, new_up | new_down)
    oi_valid = eval_rows & (oi_reason == "")
    with np.errstate(invalid="ignore", divide="ignore"):
        change = np.where(oi_valid, cur / prior - 1.0, np.nan)
    confirm = oi_valid & (change > p["oi_change_threshold"])
    long_entry = new_up & confirm
    short_entry = new_down & confirm
    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_breakout", dtype=object)
    reason[price_ok & closed & ((up & prev_up) | (down & prev_down))] = "breakout_continuation"
    breakout = new_up | new_down
    oi_bad = breakout & ~oi_valid
    reason[oi_bad] = np.where(oi_reason[oi_bad] == "", "observations_unavailable", oi_reason[oi_bad])
    reason[breakout & oi_valid & ~confirm] = "oi_below_threshold"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(lookback, p["oi_lookback"])
    for i in np.flatnonzero(~price_ok):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        lo = max(0, i - need + 1)
        prior_defects = [r for r in row_reason[lo:i] if r]
        reason[i] = prior_defects[-1] if prior_defects else "insufficient_history"
    not_closed = price_ok & ~closed
    reason[not_closed] = "bar_not_closed"

    valid = price_ok & closed
    diag = {
        "oib_upper": upper, "oib_lower": lower, "oib_breakout_state": state,
        "oib_endpoint_ms": endpoints, "oib_oi_current": cur, "oib_oi_prior": prior,
        "oib_oi_current_ms": cur_t, "oib_oi_prior_ms": prior_t, "oib_oi_change": change,
        "oib_coverage": coverage, "oib_max_gap_ms": max_gap,
    }
    with np.errstate(invalid="ignore"):
        diag["oib_oi_current_age_ms"] = endpoints - cur_t
        diag["oib_oi_prior_age_ms"] = prior_endpoints - prior_t

    carried = np.zeros(n, dtype=bool)
    if series.cutoff is not None and n >= 2 and not closed[-1] and closed[-2]:
        age = series.cutoff - endpoints[-2]
        if valid[-2] and 0 <= age <= p["max_observation_age_ms"]:
            signal[-1] = signal[-2]
            reason[-1] = reason[-2]
            for col in diag:
                diag[col][-1] = diag[col][-2]
            oi_valid[-1] = oi_valid[-2]
            window_sha[-1] = window_sha[-2]
            valid[-1] = True
            carried[-1] = True

    for col, values in diag.items():
        result[col] = values
    result["oib_cutoff_ms"] = np.full(n, float(series.cutoff) if series.cutoff is not None else np.nan)
    result["oib_threshold"] = np.full(n, p["oi_change_threshold"])
    result["oib_oi_valid"] = oi_valid
    result["oib_valid"] = valid
    result["oib_carried"] = carried
    result["oib_reason"] = reason
    result["oib_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["oib_source"] = np.full(n, series.source if not series.error else f"{series.error}: {series.detail}", dtype=object)
    result["oib_time_basis"] = np.full(n, series.time_basis, dtype=object)
    result["oib_window_sha256"] = window_sha
    result["signal"] = signal
    return result
