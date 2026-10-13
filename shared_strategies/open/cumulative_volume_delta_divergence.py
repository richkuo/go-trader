import hashlib
import json
import math
from decimal import Decimal, InvalidOperation, localcontext
from numbers import Real

import numpy as np
import pandas as pd

STRATEGY = "cumulative_volume_delta_divergence"
TRADE_FLOW_KIND = "trade_flow"
TRADE_FLOW_CONTRACT = {
    "source": "hyperliquid_ws_trades",
    "units": "base",
    "time_basis": "event_time_receipt_cutoff",
    "identity": "venue+namespace+coin+tid",
    "interval_ms": 60_000,
    "reorder_grace_ms": 5_000,
    "max_event_gap_ms": 30_000,
    "decimal_scale": 8,
}
REQUIRED_SIDE_EVIDENCE = "hyperliquid-trades-side-aggressor/1638-v1"
UNVERIFIED_SIDE_EVIDENCE = "unverified"
SESSION_HOURS = (1, 2, 3, 4, 6, 8, 12, 24)
TIMESTAMP_KIND_ATTR = "bar_timestamp_kind"
INTERVAL_ATTR = "bar_interval_ms"
NATIVE_CLOSE_KIND = "native_close_inclusive"

DEFAULT_PARAMS = {
    "left_span": 3,
    "right_span": 3,
    "min_separation": 3,
    "max_separation": 36,
    "delta_threshold": 0.05,
    "setup_expiry": 6,
    "session_hours": 4,
    "expected_interval_ms": 300_000,
    "delta_confirmation": True,
}

INT_BOUNDS = {
    "left_span": (1, 10),
    "right_span": (1, 10),
    "min_separation": (1, 96),
    "max_separation": (2, 288),
    "setup_expiry": (1, 96),
    "session_hours": (1, 24),
    "expected_interval_ms": (60_000, 3_600_000),
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_setup": 0,
    "setup_active": 2,
    "armed_long": 3,
    "armed_short": 4,
    "delta_not_confirmed": 5,
    "opposing_entries": 6,
    "setup_cleared_flow": 7,
    "setup_expired": 8,
    "superseded": 9,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "inconsistent_ohlc": 12,
    "timestamp_order": 13,
    "missing_columns": 14,
    "bar_spacing": 15,
    "bar_not_closed": 16,
    "incomplete_session_start": 17,
    "incomplete_session_candles": 18,
    "trade_flow_unavailable": 20,
    "unsupported_trade_flow": 21,
    "side_unverified": 22,
    "flow_unavailable": 23,
    "zero_flow_volume": 24,
    "cadence_mismatch": 25,
}

EVIDENCE_COLUMNS = (
    "cvd_side", "cvd_setup_id", "cvd_p1_ms", "cvd_p2_ms", "cvd_confirm_ms", "cvd_p1_price", "cvd_p2_price",
    "cvd_p1_delta", "cvd_p2_delta", "cvd_delta_change", "cvd_total_volume", "cvd_ratio", "cvd_reclaim",
    "cvd_session_ms", "cvd_flow_bars", "cvd_flow_sha256",
)


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


def validate_cvd_params(left_span, right_span, min_separation, max_separation, delta_threshold,
                        setup_expiry, session_hours, expected_interval_ms, delta_confirmation) -> dict:
    out = {name: _int_param(name, value) for name, value in (
        ("left_span", left_span), ("right_span", right_span), ("min_separation", min_separation),
        ("max_separation", max_separation), ("setup_expiry", setup_expiry), ("session_hours", session_hours),
        ("expected_interval_ms", expected_interval_ms))}
    if isinstance(delta_threshold, bool) or not isinstance(delta_threshold, Real):
        raise ValueError(f"{STRATEGY}: delta_threshold must be a number, got {delta_threshold!r}")
    threshold = float(delta_threshold)
    if not math.isfinite(threshold) or threshold < 0 or threshold >= 1:
        raise ValueError(f"{STRATEGY}: delta_threshold must be finite and in [0, 1), got {delta_threshold!r}")
    out["delta_threshold"] = threshold
    if not isinstance(delta_confirmation, (bool, np.bool_)):
        raise ValueError(f"{STRATEGY}: delta_confirmation must be true or false, got {delta_confirmation!r}")
    out["delta_confirmation"] = bool(delta_confirmation)
    if out["session_hours"] not in SESSION_HOURS:
        raise ValueError(f"{STRATEGY}: session_hours must divide 24 (one of {SESSION_HOURS}), got {session_hours!r}")
    interval = out["expected_interval_ms"]
    flow = TRADE_FLOW_CONTRACT["interval_ms"]
    if interval % flow:
        raise ValueError(f"{STRATEGY}: expected_interval_ms {interval} is not a multiple of the {flow} ms trade-flow interval")
    session_ms = out["session_hours"] * 3_600_000
    if session_ms % interval:
        raise ValueError(f"{STRATEGY}: expected_interval_ms {interval} does not divide the {session_ms} ms session")
    if TRADE_FLOW_CONTRACT["reorder_grace_ms"] >= interval:
        raise ValueError(f"{STRATEGY}: the trade-flow reorder grace must be shorter than one bar")
    if out["min_separation"] > out["max_separation"]:
        raise ValueError(f"{STRATEGY}: min_separation {out['min_separation']} is above max_separation {out['max_separation']}")
    bars = session_ms // interval
    span = out["left_span"] + out["right_span"] + out["min_separation"] + 1
    if span > bars:
        raise ValueError(f"{STRATEGY}: left_span + right_span + min_separation + 1 = {span} bars cannot fit one "
                         f"{bars}-bar session")
    return out


def cvd_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{STRATEGY}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS) - {"trade_flow_observations"})
    if unknown:
        raise ValueError(f"{STRATEGY}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **{k: v for k, v in params.items() if k in DEFAULT_PARAMS}}
    validate_cvd_params(**merged)


def _int_or_none(value):
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, (int, np.integer)):
        return int(value)
    if isinstance(value, float) and math.isfinite(value) and value.is_integer():
        return int(value)
    return None


def _canonical_decimal(text):
    if not isinstance(text, str) or not text:
        return None
    whole, dot, frac = text.partition(".")
    if not whole.isdigit() or (dot and (not frac.isdigit() or frac.endswith("0"))):
        return None
    if len(whole) > 1 and whole.startswith("0"):
        return None
    if len(frac) > TRADE_FLOW_CONTRACT["decimal_scale"]:
        return None
    try:
        return Decimal(text)
    except InvalidOperation:
        return None


class TradeFlow:
    def __init__(self, error=None, detail=""):
        self.error = error
        self.detail = detail
        self.intervals = {}
        self.cutoff = None
        self.source = ""
        self.bar_interval = None


def parse_trade_flow(obs) -> TradeFlow:
    if obs is None:
        return TradeFlow("trade_flow_unavailable", "no trade-flow observations were supplied")
    if not isinstance(obs, dict):
        return TradeFlow("unsupported_trade_flow", "trade-flow observations must be an object")
    if not obs.get("available", False):
        return TradeFlow("trade_flow_unavailable", str(obs.get("reason") or "trade flow marked unavailable"))
    if obs.get("kind") != TRADE_FLOW_KIND:
        return TradeFlow("unsupported_trade_flow", f"kind {obs.get('kind')!r} is not {TRADE_FLOW_KIND!r}")
    for key, want in TRADE_FLOW_CONTRACT.items():
        if obs.get(key) != want:
            return TradeFlow("unsupported_trade_flow", f"{key} {obs.get(key)!r} is not the frozen contract value {want!r}")
    evidence = obs.get("side_evidence")
    if evidence == UNVERIFIED_SIDE_EVIDENCE or evidence != REQUIRED_SIDE_EVIDENCE:
        return TradeFlow("side_unverified", f"side evidence {evidence!r} is not {REQUIRED_SIDE_EVIDENCE!r}; "
                                             "aggressor direction is unverified")
    flow = TradeFlow()
    flow.source = str(obs.get("source"))
    flow.bar_interval = _int_or_none(obs.get("bar_interval_ms"))
    if obs.get("cutoff_ms") is not None:
        flow.cutoff = _int_or_none(obs.get("cutoff_ms"))
        if flow.cutoff is None:
            return TradeFlow("unsupported_trade_flow", f"cutoff_ms {obs.get('cutoff_ms')!r} is not an integer")
    intervals = obs.get("intervals")
    if not isinstance(intervals, list):
        return TradeFlow("unsupported_trade_flow", "intervals must be a list")
    step = TRADE_FLOW_CONTRACT["interval_ms"]
    grace = TRADE_FLOW_CONTRACT["reorder_grace_ms"]
    for i, iv in enumerate(intervals):
        if not isinstance(iv, dict):
            return TradeFlow("unsupported_trade_flow", f"interval {i} is not an object")
        start, end = _int_or_none(iv.get("start_ms")), _int_or_none(iv.get("end_ms"))
        finalized, trades = _int_or_none(iv.get("finalized_ms")), _int_or_none(iv.get("trades"))
        invalid = iv.get("invalid_ms")
        invalid_ms = None if invalid is None else _int_or_none(invalid)
        buy, sell, total = (_canonical_decimal(iv.get(k)) for k in ("buy", "sell", "total"))
        status = iv.get("status")
        if (start is None or end is None or start % step or end != start + step or finalized is None
                or trades is None or trades < 0 or (invalid is not None and invalid_ms is None)):
            return TradeFlow("unsupported_trade_flow", f"interval {i} has malformed times or tally")
        if buy is None or sell is None or total is None or buy + sell != total:
            return TradeFlow("unsupported_trade_flow", f"interval {i} volumes are not canonical decimals with buy + sell = total")
        if (trades == 0) != (total == 0):
            return TradeFlow("unsupported_trade_flow", f"interval {i} tally {trades} disagrees with its volume {total}")
        if finalized < start:
            return TradeFlow("unsupported_trade_flow", f"interval {i} finalized at {finalized} before its start {start}")
        if status == "complete":
            if finalized < end + grace:
                return TradeFlow("unsupported_trade_flow", f"interval {i} completed before its reorder grace")
            if flow.cutoff is not None and invalid_ms is not None and invalid_ms <= flow.cutoff:
                return TradeFlow("unsupported_trade_flow", f"interval {i} is complete but invalidated before the cutoff")
        elif status == "invalid":
            if invalid_ms is None:
                return TradeFlow("unsupported_trade_flow", f"interval {i} is invalid without an invalidation time")
        else:
            return TradeFlow("unsupported_trade_flow", f"interval {i} has status {status!r}")
        if start in flow.intervals:
            return TradeFlow("unsupported_trade_flow", f"interval {start} appears twice")
        flow.intervals[start] = {
            "buy": buy, "sell": sell, "total": total, "trades": trades, "finalized": finalized,
            "invalid": invalid_ms, "hash": str(iv.get("records_hash") or ""),
        }
    return flow


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
    if df.attrs.get(TIMESTAMP_KIND_ATTR) == NATIVE_CLOSE_KIND:
        raw = raw - interval + 1
    return raw


def _bar_flow(flow: TradeFlow, open_ms: int, interval: int):
    step = TRADE_FLOW_CONTRACT["interval_ms"]
    delta = Decimal(0)
    total = Decimal(0)
    avail_from = 0
    avail_until = None
    parts = []
    for start in range(open_ms, open_ms + interval, step):
        iv = flow.intervals.get(start)
        if iv is None:
            return None
        delta += iv["buy"] - iv["sell"]
        total += iv["total"]
        avail_from = max(avail_from, iv["finalized"])
        if iv["invalid"] is not None:
            avail_until = iv["invalid"] if avail_until is None else min(avail_until, iv["invalid"])
        parts.append([start, str(iv["buy"]), str(iv["sell"]), str(iv["total"]), iv["trades"], iv["hash"]])
    return {"delta": delta, "total": total, "from": avail_from, "until": avail_until, "parts": parts}


def _flow_sha256(parts: list) -> str:
    return hashlib.sha256(json.dumps(parts, separators=(",", ":")).encode("utf-8")).hexdigest()


def _empty(result: pd.DataFrame, reason: str, threshold: float) -> pd.DataFrame:
    n = len(result)
    for col in EVIDENCE_COLUMNS:
        result[col] = np.full(n, np.nan) if col not in ("cvd_side", "cvd_setup_id", "cvd_flow_sha256") else np.full(n, "", dtype=object)
    result["cvd_threshold"] = np.full(n, threshold)
    result["cvd_deadline_ms"] = np.full(n, np.nan)
    result["cvd_flow_available"] = np.zeros(n, dtype=bool)
    result["cvd_carried"] = np.zeros(n, dtype=bool)
    result["cvd_reason"] = np.full(n, reason, dtype=object)
    result["cvd_reason_code"] = np.full(n, REASON_CODES[reason], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def cumulative_volume_delta_divergence_core(
    df: pd.DataFrame,
    left_span: int = 3,
    right_span: int = 3,
    min_separation: int = 3,
    max_separation: int = 36,
    delta_threshold: float = 0.05,
    setup_expiry: int = 6,
    session_hours: int = 4,
    expected_interval_ms: int = 300_000,
    delta_confirmation: bool = True,
    trade_flow_observations=None,
) -> pd.DataFrame:
    p = validate_cvd_params(left_span, right_span, min_separation, max_separation, delta_threshold,
                            setup_expiry, session_hours, expected_interval_ms, delta_confirmation)
    result = df.copy()
    n = len(result)
    threshold = p["delta_threshold"]
    for col in ("open", "high", "low", "close", "volume"):
        if col not in result.columns:
            return _empty(result, "missing_columns", threshold)
    interval = p["expected_interval_ms"]
    declared = result.attrs.get(INTERVAL_ATTR)
    if declared is not None and _int_or_none(declared) != interval:
        return _empty(result, "cadence_mismatch", threshold)
    flow = parse_trade_flow(trade_flow_observations)
    if not flow.error and flow.bar_interval is not None and flow.bar_interval != interval:
        flow = TradeFlow("unsupported_trade_flow", f"bar_interval_ms {flow.bar_interval} differs from expected_interval_ms {interval}")

    o = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    h = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    c = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    v = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")
    ts = _open_times_ms(result, interval)
    if ts is None:
        return _empty(result, "timestamp_order", threshold)

    finite = np.isfinite(o) & np.isfinite(h) & np.isfinite(lo) & np.isfinite(c) & np.isfinite(v) & np.isfinite(ts)
    with np.errstate(invalid="ignore"):
        inconsistent = finite & ((h < lo) | (o > h) | (o < lo) | (c > h) | (c < lo) | (lo <= 0) | (v < 0))
    row_reason = [""] * n
    for i in range(n):
        if not finite[i]:
            row_reason[i] = "nonfinite_input"
        elif inconsistent[i]:
            row_reason[i] = "inconsistent_ohlc"
        elif ts[i] % interval:
            row_reason[i] = "timestamp_order"
        elif i > 0 and np.isfinite(ts[i - 1]) and not ts[i] > ts[i - 1]:
            row_reason[i] = "timestamp_order"
    session_ms = p["session_hours"] * 3_600_000
    L, R, E = p["left_span"], p["right_span"], p["setup_expiry"]

    bar_flow = [None] * n
    bar_static = [""] * n
    if not flow.error:
        for i in range(n):
            if row_reason[i]:
                continue
            bf = _bar_flow(flow, int(ts[i]), interval)
            if bf is None:
                bar_static[i] = "missing_interval"
                continue
            if float(bf["total"]) != float(v[i]):
                bar_static[i] = "candle_volume_mismatch"
            bar_flow[i] = bf

    signal = np.zeros(n, dtype=np.int64)
    reason = np.full(n, "no_setup", dtype=object)
    deadline = np.full(n, np.nan)
    flow_available = np.zeros(n, dtype=bool)
    ev = {col: [None] * n for col in EVIDENCE_COLUMNS}

    def avail(j, at_ms):
        bf = bar_flow[j]
        if bf is None or bar_static[j]:
            return False
        return bf["from"] <= at_ms and (bf["until"] is None or at_ms < bf["until"])

    def put(i, setup):
        for col in EVIDENCE_COLUMNS:
            ev[col][i] = setup[col]

    session_id = None
    session_first = None
    session_broken = False
    lows, highs = [], []
    setups = {}
    cum = {}
    prev_ts = None
    for i in range(n):
        if row_reason[i]:
            reason[i] = row_reason[i]
            if session_id is not None:
                session_broken = True
                setups = {}
            prev_ts = None
            continue
        t = int(ts[i])
        d = t + interval
        deadline[i] = d
        sid = t // session_ms
        spacing_break = prev_ts is not None and t - prev_ts != interval and sid == session_id
        prev_ts = t
        if sid != session_id:
            session_id, session_first = sid, i
            session_broken = t != sid * session_ms
            lows, highs, setups, cum = [], [], {}, {}
            running = Decimal(0)
            for j in range(i, n):
                if row_reason[j] or int(ts[j]) // session_ms != sid or bar_flow[j] is None or bar_static[j]:
                    break
                running += bar_flow[j]["delta"]
                cum[j] = running
        if spacing_break:
            session_broken = True
            setups = {}
        if flow.cutoff is not None and d > flow.cutoff:
            reason[i] = "bar_not_closed"
            continue
        if session_broken:
            reason[i] = "incomplete_session_start" if int(ts[session_first]) != sid * session_ms else "incomplete_session_candles"
            continue
        if flow.error:
            reason[i] = flow.error
            continue
        first_gap = None
        for j in range(session_first, i + 1):
            if not avail(j, d):
                first_gap = j
                break
        flow_available[i] = first_gap is None or first_gap >= i
        events = []
        for side in list(setups):
            s = setups[side]
            if first_gap is not None and first_gap <= s["_p2"]:
                del setups[side]
                events.append("setup_cleared_flow")
            elif i > s["_c"] + E:
                del setups[side]
                events.append("setup_expired")
        long_hit = "long" in setups and c[i] > setups["long"]["cvd_reclaim"]
        short_hit = "short" in setups and c[i] < setups["short"]["cvd_reclaim"]
        if long_hit and short_hit:
            setups = {}
            events.append("opposing_entries")
        elif long_hit or short_hit:
            side = "long" if long_hit else "short"
            put(i, setups.pop(side))
            signal[i] = 1 if long_hit else -1
            events.append("entry_long" if long_hit else "entry_short")
        pivot = i - R
        if pivot - L >= session_first:
            window = range(pivot - L, pivot + R + 1)
            for kind, series, better in (("low", lo, lambda a, b: a < b), ("high", h, lambda a, b: a > b)):
                if not all(better(series[pivot], series[k]) for k in window if k != pivot):
                    continue
                book = lows if kind == "low" else highs
                prior = book[-1] if book else None
                book.append(pivot)
                if prior is None:
                    continue
                sep = pivot - prior
                if sep < p["min_separation"] or sep > p["max_separation"]:
                    continue
                if not better(series[pivot], series[prior]):
                    continue
                if first_gap is not None and first_gap <= pivot:
                    events.append("flow_unavailable")
                    continue
                total = Decimal(0)
                for k in range(prior + 1, pivot + 1):
                    total += bar_flow[k]["total"]
                if total == 0:
                    events.append("zero_flow_volume")
                    continue
                change = cum[pivot] - cum[prior]
                with localcontext() as ctx:
                    ctx.prec = 40
                    ratio = float(change / total)
                side = "long" if kind == "low" else "short"
                confirmed = ratio > threshold if side == "long" else ratio < -threshold
                if p["delta_confirmation"] and not confirmed:
                    events.append("delta_not_confirmed")
                    continue
                parts = []
                for k in range(session_first, pivot + 1):
                    parts.extend(bar_flow[k]["parts"])
                if side in setups:
                    events.append("superseded")
                setups[side] = {
                    "_c": i, "_p2": pivot,
                    "cvd_side": side, "cvd_setup_id": f"{side}:{t}",
                    "cvd_p1_ms": float(ts[prior]), "cvd_p2_ms": float(ts[pivot]), "cvd_confirm_ms": float(t),
                    "cvd_p1_price": float(series[prior]), "cvd_p2_price": float(series[pivot]),
                    "cvd_p1_delta": float(cum[prior]), "cvd_p2_delta": float(cum[pivot]),
                    "cvd_delta_change": float(change), "cvd_total_volume": float(total), "cvd_ratio": ratio,
                    "cvd_reclaim": float(h[i]) if side == "long" else float(lo[i]),
                    "cvd_session_ms": float(sid * session_ms), "cvd_flow_bars": float(pivot - session_first + 1),
                    "cvd_flow_sha256": _flow_sha256(parts),
                }
                events.append("armed_" + side)
                if signal[i] == 0 and not ev["cvd_side"][i]:
                    put(i, setups[side])
        for want in ("entry_long", "entry_short", "opposing_entries", "armed_long", "armed_short",
                     "setup_cleared_flow", "setup_expired", "superseded", "delta_not_confirmed",
                     "zero_flow_volume", "flow_unavailable"):
            if want in events:
                reason[i] = want
                break
        else:
            if setups:
                reason[i] = "setup_active"

    carried = np.zeros(n, dtype=bool)
    if flow.cutoff is not None and n >= 2 and reason[-1] == "bar_not_closed" and np.isfinite(deadline[-2]):
        age = flow.cutoff - deadline[-2]
        if 0 <= age < interval:
            signal[-1] = signal[-2]
            reason[-1] = reason[-2]
            deadline[-1] = deadline[-2]
            flow_available[-1] = flow_available[-2]
            for col in EVIDENCE_COLUMNS:
                ev[col][-1] = ev[col][-2]
            carried[-1] = True

    for col in EVIDENCE_COLUMNS:
        if col in ("cvd_side", "cvd_setup_id", "cvd_flow_sha256"):
            result[col] = np.array([x if x is not None else "" for x in ev[col]], dtype=object)
        else:
            result[col] = np.array([x if x is not None else np.nan for x in ev[col]], dtype="float64")
    result["cvd_threshold"] = np.full(n, threshold)
    result["cvd_deadline_ms"] = deadline
    result["cvd_flow_available"] = flow_available
    result["cvd_carried"] = carried
    result["cvd_reason"] = reason
    result["cvd_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
