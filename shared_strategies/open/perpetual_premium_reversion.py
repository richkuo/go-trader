import hashlib
import json
import math
from decimal import Decimal, InvalidOperation, localcontext
from numbers import Real

import numpy as np
import pandas as pd

STRATEGY = "perpetual_premium_reversion"
PREMIUM_KIND = "perp_premium"
PREMIUM_CONTRACT = {
    "source": "hyperliquid_ws_activeAssetCtx+bbo",
    "contract": "perp-premium-quote-oracle/1639-v1",
    "time_basis": "receipt_cutoff_latest_component",
    "oracle_time_basis": "receipt_only_no_source_time",
    "sample_ms": 60_000,
    "max_future_skew_ms": 5_000,
}
SAMPLE_MS = PREMIUM_CONTRACT["sample_ms"]
TIMESTAMP_KIND_ATTR = "bar_timestamp_kind"
INTERVAL_ATTR = "bar_interval_ms"
NATIVE_CLOSE_KIND = "native_close_inclusive"
UNESTABLISHABLE_PARAMS = {
    "max_oracle_source_age_ms": "the venue asset context carries no source time, so the age of the underlying "
                                "oracle update cannot be established; oracle freshness is the receipt age only "
                                "(max_oracle_age_ms)",
    "require_oracle_event_time": "the venue asset context carries no source time; no oracle event time exists",
}

DEFAULT_PARAMS = {
    "lookback_samples": 30,
    "entry_z": 2.0,
    "turn_span": 1,
    "variance_floor": 1e-10,
    "max_spread_bps": 5.0,
    "max_quote_age_ms": 5_000,
    "max_oracle_age_ms": 5_000,
    "max_skew_ms": 3_000,
    "max_abs_premium": 0.01,
    "max_mark_dislocation": 0.005,
    "expected_interval_ms": 60_000,
}

INT_BOUNDS = {
    "lookback_samples": (10, 720),
    "turn_span": (1, 5),
    "max_quote_age_ms": (100, 60_000),
    "max_oracle_age_ms": (100, 60_000),
    "max_skew_ms": (0, 60_000),
    "expected_interval_ms": (60_000, 3_600_000),
}

FLOAT_BOUNDS = {
    "entry_z": (0.0, 10.0, False),
    "variance_floor": (0.0, 1e-4, False),
    "max_spread_bps": (0.0, 1000.0, False),
    "max_abs_premium": (0.0, 0.5, False),
    "max_mark_dislocation": (0.0, 0.5, False),
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_extreme": 0,
    "no_turn": 2,
    "wrong_premium_sign": 3,
    "insufficient_samples": 10,
    "nonfinite_input": 11,
    "inconsistent_ohlc": 12,
    "timestamp_order": 13,
    "missing_columns": 14,
    "bar_not_closed": 16,
    "off_grid_decision": 17,
    "premium_unavailable": 20,
    "unsupported_premium": 21,
    "cadence_mismatch": 25,
    "sample_missing": 30,
    "sample_not_sampled": 31,
    "sample_invalidated": 32,
    "stale_quote": 33,
    "stale_oracle": 34,
    "component_skew": 35,
    "wide_spread": 36,
    "premium_dislocation": 37,
    "mark_dislocation": 38,
    "session_change": 39,
    "variance_floor": 40,
    "malformed_sample": 41,
}

EVIDENCE_COLUMNS = (
    "ppr_sample_ms", "ppr_bid", "ppr_ask", "ppr_mid", "ppr_oracle", "ppr_mark", "ppr_spread_bps",
    "ppr_quote_event_ms", "ppr_quote_recv_ms", "ppr_context_recv_ms", "ppr_session", "ppr_premium",
    "ppr_extreme_premium", "ppr_mean", "ppr_std", "ppr_z_extreme", "ppr_z_current",
)
TEXT_COLUMNS = ("ppr_side", "ppr_window_sha256")


def _int_param(name, value):
    lo, hi = INT_BOUNDS[name]
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{STRATEGY}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def _float_param(name, value):
    lo, hi, lo_inclusive = FLOAT_BOUNDS[name]
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be a number, got {value!r}")
    v = float(value)
    if not math.isfinite(v) or v > hi or v < lo or (v == lo and not lo_inclusive):
        raise ValueError(f"{STRATEGY}: {name} must be finite and in ({lo}, {hi}], got {value!r}")
    return v


def validate_premium_params(lookback_samples, entry_z, turn_span, variance_floor, max_spread_bps,
                            max_quote_age_ms, max_oracle_age_ms, max_skew_ms, max_abs_premium,
                            max_mark_dislocation, expected_interval_ms) -> dict:
    out = {name: _int_param(name, value) for name, value in (
        ("lookback_samples", lookback_samples), ("turn_span", turn_span), ("max_quote_age_ms", max_quote_age_ms),
        ("max_oracle_age_ms", max_oracle_age_ms), ("max_skew_ms", max_skew_ms),
        ("expected_interval_ms", expected_interval_ms))}
    out.update({name: _float_param(name, value) for name, value in (
        ("entry_z", entry_z), ("variance_floor", variance_floor), ("max_spread_bps", max_spread_bps),
        ("max_abs_premium", max_abs_premium), ("max_mark_dislocation", max_mark_dislocation))})
    if out["expected_interval_ms"] % SAMPLE_MS:
        raise ValueError(f"{STRATEGY}: expected_interval_ms {out['expected_interval_ms']} is not a multiple of the "
                         f"{SAMPLE_MS} ms premium sample grid")
    if out["turn_span"] >= out["lookback_samples"]:
        raise ValueError(f"{STRATEGY}: turn_span {out['turn_span']} must be below lookback_samples "
                         f"{out['lookback_samples']}")
    return out


def premium_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{STRATEGY}: params must be an object, got {params!r}")
    for name, why in UNESTABLISHABLE_PARAMS.items():
        if name in params:
            raise ValueError(f"{STRATEGY}: {name} is refused: {why}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS) - {"premium_observations"})
    if unknown:
        raise ValueError(f"{STRATEGY}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **{k: v for k, v in params.items() if k in DEFAULT_PARAMS}}
    validate_premium_params(**merged)


def _int_or_none(value):
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, (int, np.integer)):
        return int(value)
    if isinstance(value, float) and math.isfinite(value) and value.is_integer():
        return int(value)
    return None


def _positive_decimal(text):
    if not isinstance(text, str) or not text:
        return None
    whole, dot, frac = text.partition(".")
    if not whole.isdigit() or (dot and (not frac.isdigit() or frac.endswith("0"))):
        return None
    if len(whole) > 1 and whole.startswith("0"):
        return None
    try:
        value = Decimal(text)
    except InvalidOperation:
        return None
    return value if value > 0 else None


class PremiumSeries:
    def __init__(self, error=None, detail=""):
        self.error = error
        self.detail = detail
        self.samples = {}
        self.cutoff = None
        self.bar_interval = None


def _parse_quote(q):
    if not isinstance(q, dict):
        return None
    bid, ask = _positive_decimal(q.get("bid")), _positive_decimal(q.get("ask"))
    bid_sz, ask_sz = _positive_decimal(q.get("bid_sz")), _positive_decimal(q.get("ask_sz"))
    ints = {k: _int_or_none(q.get(k)) for k in ("event_ms", "recv_ms", "session", "seq", "bid_n", "ask_n")}
    if None in (bid, ask, bid_sz, ask_sz) or any(v is None for v in ints.values()) or bid >= ask:
        return None
    return {"bid": bid, "ask": ask, "bid_sz": bid_sz, "ask_sz": ask_sz, **ints}


def _parse_context(c):
    if not isinstance(c, dict):
        return None
    oracle, mark = _positive_decimal(c.get("oracle")), _positive_decimal(c.get("mark"))
    mid_raw = c.get("mid")
    mid = None if mid_raw is None else _positive_decimal(mid_raw)
    ints = {k: _int_or_none(c.get(k)) for k in ("recv_ms", "session", "seq")}
    if None in (oracle, mark) or (mid_raw is not None and mid is None) or any(v is None for v in ints.values()):
        return None
    if c.get("oracle_event_ms") is not None:
        return None
    return {"oracle": oracle, "mark": mark, "mid": mid, **ints}


def parse_premium_observations(obs) -> PremiumSeries:
    if obs is None:
        return PremiumSeries("premium_unavailable", "no premium observations were supplied")
    if not isinstance(obs, dict):
        return PremiumSeries("unsupported_premium", "premium observations must be an object")
    if not obs.get("available", False):
        return PremiumSeries("premium_unavailable", str(obs.get("reason") or "premium observations marked unavailable"))
    if obs.get("kind") != PREMIUM_KIND:
        return PremiumSeries("unsupported_premium", f"kind {obs.get('kind')!r} is not {PREMIUM_KIND!r}")
    for key, want in PREMIUM_CONTRACT.items():
        if obs.get(key) != want:
            return PremiumSeries("unsupported_premium", f"{key} {obs.get(key)!r} is not the frozen contract value {want!r}")
    series = PremiumSeries()
    series.bar_interval = _int_or_none(obs.get("bar_interval_ms"))
    if obs.get("cutoff_ms") is not None:
        series.cutoff = _int_or_none(obs.get("cutoff_ms"))
        if series.cutoff is None:
            return PremiumSeries("unsupported_premium", f"cutoff_ms {obs.get('cutoff_ms')!r} is not an integer")
    samples = obs.get("samples")
    if not isinstance(samples, list):
        return PremiumSeries("unsupported_premium", "samples must be a list")
    last_t = None
    for i, smp in enumerate(samples):
        if not isinstance(smp, dict):
            return PremiumSeries("unsupported_premium", f"sample {i} is not an object")
        t = _int_or_none(smp.get("t"))
        finalized = _int_or_none(smp.get("finalized_ms"))
        invalid_raw = smp.get("invalid_ms")
        invalid = None if invalid_raw is None else _int_or_none(invalid_raw)
        status = smp.get("status")
        if t is None or t % SAMPLE_MS or finalized is None or finalized <= t or (invalid_raw is not None and invalid is None):
            return PremiumSeries("unsupported_premium", f"sample {i} has malformed times")
        if last_t is not None and t <= last_t:
            return PremiumSeries("unsupported_premium", f"sample {i} at {t} is not after {last_t}")
        last_t = t
        if status not in ("sampled", "missing", "invalid"):
            return PremiumSeries("unsupported_premium", f"sample {i} has status {status!r}")
        quote = _parse_quote(smp.get("quote")) if smp.get("quote") is not None else None
        context = _parse_context(smp.get("context")) if smp.get("context") is not None else None
        bad = (smp.get("quote") is not None and quote is None) or (smp.get("context") is not None and context is None)
        if status == "sampled" and (quote is None or context is None):
            bad = True
        if not bad and quote is not None and (quote["recv_ms"] > t or quote["event_ms"] > quote["recv_ms"] + PREMIUM_CONTRACT["max_future_skew_ms"]):
            bad = True
        if not bad and context is not None and context["recv_ms"] > t:
            bad = True
        series.samples[t] = {"status": status, "reason": smp.get("reason") or "", "quote": quote, "context": context,
                             "invalid": invalid, "malformed": bad}
    return series


def _open_times_ms(df: pd.DataFrame, interval: int):
    if "timestamp" in df.columns:
        raw = pd.to_numeric(df["timestamp"], errors="coerce").to_numpy(dtype="float64")
    elif isinstance(df.index, pd.DatetimeIndex):
        idx = df.index
        if idx.tz is not None:
            idx = idx.tz_convert("UTC").tz_localize(None)
        ns_per_unit = {"s": 1_000_000_000, "ms": 1_000_000, "us": 1_000, "ns": 1}[idx.unit]
        missing = np.asarray(idx.isna())
        values = np.where(missing, 0, idx.asi8)
        raw = ((values * ns_per_unit) // 1_000_000).astype("float64")
        raw[missing] = np.nan
    else:
        return None
    kind = df.attrs.get(TIMESTAMP_KIND_ATTR)
    if kind is None:
        finite = raw[np.isfinite(raw)]
        if len(finite) and bool((np.mod(finite, interval) == interval - 1).all()):
            kind = NATIVE_CLOSE_KIND
    if kind == NATIVE_CLOSE_KIND:
        raw = raw - interval + 1
    return raw


def evaluate_sample(entry, t: int, decision_ms: int, p: dict):
    if entry is None:
        return None, "sample_missing"
    if entry["malformed"]:
        return None, "malformed_sample"
    if entry["status"] != "sampled":
        return None, "sample_not_sampled"
    if entry["invalid"] is not None and entry["invalid"] <= decision_ms:
        return None, "sample_invalidated"
    q, c = entry["quote"], entry["context"]
    if t - q["event_ms"] > p["max_quote_age_ms"] or t - q["recv_ms"] > p["max_quote_age_ms"]:
        return None, "stale_quote"
    if t - c["recv_ms"] > p["max_oracle_age_ms"]:
        return None, "stale_oracle"
    if abs(q["recv_ms"] - c["recv_ms"]) > p["max_skew_ms"]:
        return None, "component_skew"
    if q["session"] != c["session"]:
        return None, "session_change"
    with localcontext() as ctx:
        ctx.prec = 40
        mid = (q["bid"] + q["ask"]) / 2
        spread_bps = float((q["ask"] - q["bid"]) / mid * 10_000)
        premium = float(mid / c["oracle"] - 1)
        mark_dislocation = float(abs(mid / c["mark"] - 1))
    if spread_bps > p["max_spread_bps"]:
        return None, "wide_spread"
    if abs(premium) > p["max_abs_premium"]:
        return None, "premium_dislocation"
    if mark_dislocation > p["max_mark_dislocation"]:
        return None, "mark_dislocation"
    return {
        "t": t, "premium": premium, "mid": float(mid), "bid": float(q["bid"]), "ask": float(q["ask"]),
        "oracle": float(c["oracle"]), "mark": float(c["mark"]), "spread_bps": spread_bps,
        "quote_event_ms": q["event_ms"], "quote_recv_ms": q["recv_ms"], "context_recv_ms": c["recv_ms"],
        "session": q["session"], "quote_seq": q["seq"], "context_seq": c["seq"],
        "key": [t, str(q["bid"]), str(q["ask"]), str(c["oracle"]), q["event_ms"], q["recv_ms"], c["recv_ms"], q["session"]],
    }, ""


def premium_decision(series: PremiumSeries, decision_ms: int, p: dict):
    if decision_ms % SAMPLE_MS:
        return 0, "off_grid_decision", None
    lookback, span = p["lookback_samples"], p["turn_span"]
    accepted = []
    for k in range(lookback, -1, -1):
        t = decision_ms - k * SAMPLE_MS
        view, why = evaluate_sample(series.samples.get(t), t, decision_ms, p)
        if view is None:
            return 0, why, None
        accepted.append(view)
    if len({v["session"] for v in accepted}) != 1:
        return 0, "session_change", None
    history = [v["premium"] for v in accepted[:-1]]
    mean = math.fsum(history) / lookback
    var = math.fsum((x - mean) ** 2 for x in history) / lookback
    current = accepted[-1]
    extreme = accepted[-1 - span]
    std = math.sqrt(var) if var > 0 else 0.0
    ev = {
        "current": current, "extreme": extreme, "mean": mean, "std": std,
        "sha256": hashlib.sha256(json.dumps([v["key"] for v in accepted], separators=(",", ":")).encode("utf-8")).hexdigest(),
    }
    if not var > p["variance_floor"]:
        return 0, "variance_floor", ev
    dist = [v["premium"] - mean for v in accepted[-1 - span:]]
    z_ext = dist[0] / std
    ev["z_extreme"], ev["z_current"] = z_ext, dist[-1] / std
    if z_ext > p["entry_z"]:
        side = -1
    elif z_ext < -p["entry_z"]:
        side = 1
    else:
        return 0, "no_extreme", ev
    if side == -1:
        if not (extreme["premium"] > 0 and current["premium"] > 0):
            return 0, "wrong_premium_sign", ev
        turned = all(dist[j + 1] < dist[j] for j in range(span)) and dist[-1] > 0
    else:
        if not (extreme["premium"] < 0 and current["premium"] < 0):
            return 0, "wrong_premium_sign", ev
        turned = all(dist[j + 1] > dist[j] for j in range(span)) and dist[-1] < 0
    if not turned:
        return 0, "no_turn", ev
    ev["side"] = "short" if side == -1 else "long"
    return side, "entry_short" if side == -1 else "entry_long", ev


def _empty(result: pd.DataFrame, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in EVIDENCE_COLUMNS:
        result[col] = np.full(n, np.nan)
    for col in TEXT_COLUMNS:
        result[col] = np.full(n, "", dtype=object)
    result["ppr_deadline_ms"] = np.full(n, np.nan)
    result["ppr_carried"] = np.zeros(n, dtype=bool)
    result["ppr_reason"] = np.full(n, reason, dtype=object)
    result["ppr_reason_code"] = np.full(n, REASON_CODES[reason], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def perpetual_premium_reversion_core(
    df: pd.DataFrame,
    lookback_samples: int = 30,
    entry_z: float = 2.0,
    turn_span: int = 1,
    variance_floor: float = 1e-10,
    max_spread_bps: float = 5.0,
    max_quote_age_ms: int = 5_000,
    max_oracle_age_ms: int = 5_000,
    max_skew_ms: int = 3_000,
    max_abs_premium: float = 0.01,
    max_mark_dislocation: float = 0.005,
    expected_interval_ms: int = 60_000,
    premium_observations=None,
) -> pd.DataFrame:
    p = validate_premium_params(lookback_samples, entry_z, turn_span, variance_floor, max_spread_bps,
                                max_quote_age_ms, max_oracle_age_ms, max_skew_ms, max_abs_premium,
                                max_mark_dislocation, expected_interval_ms)
    result = df.copy()
    n = len(result)
    for col in ("open", "high", "low", "close"):
        if col not in result.columns:
            return _empty(result, "missing_columns")
    interval = p["expected_interval_ms"]
    declared = result.attrs.get(INTERVAL_ATTR)
    if declared is not None and _int_or_none(declared) != interval:
        return _empty(result, "cadence_mismatch")
    series = parse_premium_observations(premium_observations)
    if not series.error and series.bar_interval is not None and series.bar_interval != interval:
        series = PremiumSeries("unsupported_premium",
                               f"bar_interval_ms {series.bar_interval} differs from expected_interval_ms {interval}")
    o = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    h = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    c = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    ts = _open_times_ms(result, interval)
    if ts is None:
        return _empty(result, "timestamp_order")

    signal = np.zeros(n, dtype=np.int64)
    reason = np.full(n, "no_extreme", dtype=object)
    deadline = np.full(n, np.nan)
    ev_cols = {col: np.full(n, np.nan) for col in EVIDENCE_COLUMNS}
    text_cols = {col: np.full(n, "", dtype=object) for col in TEXT_COLUMNS}
    for i in range(n):
        if not (np.isfinite(o[i]) and np.isfinite(h[i]) and np.isfinite(lo[i]) and np.isfinite(c[i]) and np.isfinite(ts[i])):
            reason[i] = "nonfinite_input"
            continue
        if h[i] < lo[i] or o[i] > h[i] or o[i] < lo[i] or c[i] > h[i] or c[i] < lo[i] or lo[i] <= 0:
            reason[i] = "inconsistent_ohlc"
            continue
        t = int(ts[i])
        if t % interval or (i > 0 and np.isfinite(ts[i - 1]) and not ts[i] > ts[i - 1]):
            reason[i] = "timestamp_order"
            continue
        d = t + interval
        deadline[i] = d
        if series.cutoff is not None and d > series.cutoff:
            reason[i] = "bar_not_closed"
            continue
        if series.error:
            reason[i] = series.error
            continue
        sig, why, ev = premium_decision(series, d, p)
        signal[i] = sig
        reason[i] = why
        if ev is None:
            continue
        cur, ext = ev["current"], ev["extreme"]
        ev_cols["ppr_sample_ms"][i] = cur["t"]
        ev_cols["ppr_bid"][i] = cur["bid"]
        ev_cols["ppr_ask"][i] = cur["ask"]
        ev_cols["ppr_mid"][i] = cur["mid"]
        ev_cols["ppr_oracle"][i] = cur["oracle"]
        ev_cols["ppr_mark"][i] = cur["mark"]
        ev_cols["ppr_spread_bps"][i] = cur["spread_bps"]
        ev_cols["ppr_quote_event_ms"][i] = cur["quote_event_ms"]
        ev_cols["ppr_quote_recv_ms"][i] = cur["quote_recv_ms"]
        ev_cols["ppr_context_recv_ms"][i] = cur["context_recv_ms"]
        ev_cols["ppr_session"][i] = cur["session"]
        ev_cols["ppr_premium"][i] = cur["premium"]
        ev_cols["ppr_extreme_premium"][i] = ext["premium"]
        ev_cols["ppr_mean"][i] = ev["mean"]
        ev_cols["ppr_std"][i] = ev["std"]
        ev_cols["ppr_z_extreme"][i] = ev.get("z_extreme", np.nan)
        ev_cols["ppr_z_current"][i] = ev.get("z_current", np.nan)
        text_cols["ppr_side"][i] = ev.get("side", "")
        text_cols["ppr_window_sha256"][i] = ev["sha256"]

    carried = np.zeros(n, dtype=bool)
    if series.cutoff is not None and n >= 2 and reason[-1] == "bar_not_closed" and np.isfinite(deadline[-2]):
        age = series.cutoff - deadline[-2]
        if 0 <= age < interval:
            signal[-1] = signal[-2]
            reason[-1] = reason[-2]
            deadline[-1] = deadline[-2]
            for col in EVIDENCE_COLUMNS:
                ev_cols[col][-1] = ev_cols[col][-2]
            for col in TEXT_COLUMNS:
                text_cols[col][-1] = text_cols[col][-2]
            carried[-1] = True

    for col in EVIDENCE_COLUMNS:
        result[col] = ev_cols[col]
    for col in TEXT_COLUMNS:
        result[col] = text_cols[col]
    result["ppr_deadline_ms"] = deadline
    result["ppr_carried"] = carried
    result["ppr_reason"] = reason
    result["ppr_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
