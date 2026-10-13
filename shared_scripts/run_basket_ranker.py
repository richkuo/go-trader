#!/usr/bin/env python3

import hashlib
import importlib.util
import json
import math
import os
import sys
import traceback
from types import MappingProxyType

_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
for _p in (os.path.join(_ROOT, "shared_tools"), os.path.join(_ROOT, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from closed_bar import ClosedBarHold, RULE_HYPERLIQUID_NATIVE, interval_ms_for, sealed_frame_timings, select_closed
from market_payload import (
    MarketPayloadError,
    market_decision_cutoff_ms,
    market_frame,
    market_frame_rows_with_timing,
    validate_market_payload,
)

REQUEST_VERSION = 2
MAX_LOOKBACK_BARS = 5000
ALLOCATION_CONTEXT_KEYS = frozenset({
    "gross_budget_usd", "max_coin_notional_usd", "max_transition_net_usd", "lookback_bars", "min_history_bars",
})
OUTPUT_KEYS = {"hold", "scores", "weights", "exclusions"}
WEIGHT_SUM_TOLERANCE = 1e-9


class RankerHold(Exception):
    pass


class RankerInvalid(Exception):
    pass


def _load_registry():
    path = os.path.join(_ROOT, "shared_strategies", "open", "registry.py")
    spec = importlib.util.spec_from_file_location("_basket_ranker_registry", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _finite_number(value) -> bool:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return math.isfinite(float(value))


def _int_field(req, key, minimum=None):
    value = req.get(key)
    if isinstance(value, bool) or not isinstance(value, int):
        raise RankerInvalid(f"request field {key!r} must be an integer, got {value!r}")
    if minimum is not None and value < minimum:
        raise RankerInvalid(f"request field {key!r} must be >= {minimum}, got {value}")
    return value


def parse_allocation_context(raw):
    if not isinstance(raw, dict):
        raise RankerInvalid("request field 'allocation_context' must be an object")
    keys = set(raw)
    missing = sorted(ALLOCATION_CONTEXT_KEYS - keys)
    unknown = sorted(keys - ALLOCATION_CONTEXT_KEYS)
    if missing or unknown:
        raise RankerInvalid(f"allocation_context has missing fields {missing} and unknown fields {unknown}")
    ctx = {}
    for key in ("gross_budget_usd", "max_coin_notional_usd", "max_transition_net_usd"):
        value = raw[key]
        if not _finite_number(value) or float(value) <= 0:
            raise RankerInvalid(f"allocation_context.{key} must be a finite positive number, got {value!r}")
        ctx[key] = float(value)
    for key in ("max_coin_notional_usd", "max_transition_net_usd"):
        if ctx[key] > ctx["gross_budget_usd"]:
            raise RankerInvalid(
                f"allocation_context.{key} {ctx[key]!r} exceeds gross_budget_usd {ctx['gross_budget_usd']!r}")
    lookback = raw["lookback_bars"]
    if isinstance(lookback, bool) or not isinstance(lookback, int) or not 2 <= lookback <= MAX_LOOKBACK_BARS:
        raise RankerInvalid(f"allocation_context.lookback_bars must be an integer in [2, {MAX_LOOKBACK_BARS}], got {lookback!r}")
    min_history = raw["min_history_bars"]
    if isinstance(min_history, bool) or not isinstance(min_history, int) or not 1 <= min_history <= lookback:
        raise RankerInvalid(f"allocation_context.min_history_bars must be an integer in [1, lookback_bars], got {min_history!r}")
    ctx["lookback_bars"] = lookback
    ctx["min_history_bars"] = min_history
    return MappingProxyType(ctx)


def parse_request(raw):
    try:
        req = json.loads(raw)
    except ValueError as e:
        raise RankerInvalid(f"request is not JSON: {e}")
    if not isinstance(req, dict):
        raise RankerInvalid("request must be a JSON object")
    if req.get("v") != REQUEST_VERSION:
        raise RankerInvalid(f"request version {req.get('v')!r} is not {REQUEST_VERSION}")
    legacy = sorted(k for k in ("lookback_bars", "min_history_bars") if k in req)
    if legacy:
        raise RankerInvalid(f"request fields {legacy} moved into allocation_context in request version {REQUEST_VERSION}")
    for key in ("basket_id", "ranker"):
        if not isinstance(req.get(key), str) or not req[key].strip():
            raise RankerInvalid(f"request field {key!r} must be a non-empty string")
    _int_field(req, "deadline_ms", 1)
    _int_field(req, "min_eligible", 1)
    if not isinstance(req.get("params"), dict):
        raise RankerInvalid("request field 'params' must be an object")
    expected = req.get("expected")
    if not isinstance(expected, dict):
        raise RankerInvalid("request field 'expected' must be an object")
    members = req.get("members")
    if not isinstance(members, list) or not members:
        raise RankerInvalid("request field 'members' must be a non-empty list")
    seen = set()
    for m in members:
        if not isinstance(m, dict) or not isinstance(m.get("symbol"), str) or not isinstance(m.get("timeframe"), str):
            raise RankerInvalid(f"member entry {m!r} is malformed")
        if m["symbol"] in seen:
            raise RankerInvalid(f"member {m['symbol']} is listed twice")
        seen.add(m["symbol"])
    if not isinstance(req.get("listings"), dict):
        raise RankerInvalid("request field 'listings' must be an object")
    if not isinstance(req.get("market"), dict):
        raise RankerInvalid("request field 'market' must be an object")
    return req, parse_allocation_context(req.get("allocation_context"))


def check_envelope(req):
    market = req["market"]
    expected = req["expected"]
    try:
        validate_market_payload(market, MarketPayloadError)
    except MarketPayloadError as e:
        raise RankerHold(f"the sealed market payload is malformed: {e}")
    if market.get("snapshot_id") != expected.get("snapshot_id"):
        raise RankerHold(f"the payload snapshot {market.get('snapshot_id')!r} is not the pending snapshot {expected.get('snapshot_id')!r}")
    if market.get("generation") != expected.get("generation"):
        raise RankerHold(f"the payload generation {market.get('generation')!r} is not the pending generation {expected.get('generation')!r}")
    cutoff = market_decision_cutoff_ms(market, MarketPayloadError)
    if cutoff <= 0 or cutoff != expected.get("cutoff_ms"):
        raise RankerHold(f"the payload decision cutoff {cutoff} is not the pending cutoff {expected.get('cutoff_ms')!r}")
    if market.get("feed_complete") is not True:
        raise RankerHold("the sealed market payload is not complete")
    return cutoff


def member_evidence(req, ctx, member, cutoff):
    market = req["market"]
    sym = member["symbol"]
    tf = member["timeframe"]
    deadline = req["deadline_ms"]
    try:
        interval = interval_ms_for(tf)
    except ClosedBarHold as e:
        raise RankerHold(f"{sym}: {e.reason}")
    fact = req["listings"].get(sym)
    if not isinstance(fact, dict):
        raise RankerHold(f"{sym}: no listing evidence is declared")
    try:
        listed = int(fact["listed_at_ms"])
        known = int(fact["known_at_ms"])
        delisted = int(fact.get("delisted_at_ms") or 0)
    except (KeyError, TypeError, ValueError):
        raise RankerHold(f"{sym}: the listing evidence is unreadable")
    if known > deadline:
        raise RankerHold(f"{sym}: the listing evidence was not known at the deadline")
    listing = {"symbol": sym, "listed_at_ms": listed, "known_at_ms": known}
    if listed > deadline:
        return {**listing, "status": "excluded", "reason": "not listed at the deadline"}, None
    if delisted and delisted <= deadline:
        return {**listing, "status": "excluded", "reason": "delisted at or before the deadline"}, None
    bars_since_listing = (deadline - listed) // interval
    if bars_since_listing < ctx["min_history_bars"]:
        return {**listing, "status": "excluded",
                "reason": f"listed {bars_since_listing} bars before the deadline, needs {ctx['min_history_bars']}"}, None
    try:
        frame = market_frame(market, sym, tf, MarketPayloadError)
    except MarketPayloadError as e:
        raise RankerHold(f"{sym}: {e}")
    if frame.get("stale"):
        raise RankerHold(f"{sym}: the sealed frame is stale ({frame.get('stale_reason') or 'no reason given'})")
    try:
        rows, timing = market_frame_rows_with_timing(market, sym, tf, MarketPayloadError)
        timings = sealed_frame_timings(rows, timing, tf)
        selection = select_closed(rows, timings, timeframe=tf, cutoff_ms=cutoff, rule=RULE_HYPERLIQUID_NATIVE,
                                  keep=ctx["lookback_bars"], min_rows=1)
    except (MarketPayloadError, ClosedBarHold, ValueError, TypeError) as e:
        raise RankerHold(f"{sym}: {getattr(e, 'reason', e)}")
    if selection.boundary_ms != deadline:
        raise RankerHold(f"{sym}: the decision candle closes at {selection.boundary_ms}, not at the rebalance deadline {deadline}")
    for i in range(1, len(selection.timings)):
        if selection.timings[i].open_ms - selection.timings[i - 1].open_ms != interval:
            raise RankerHold(f"{sym}: the closed history has a gap before the bar opened {selection.timings[i].open_ms}")
    needed = min(ctx["lookback_bars"], bars_since_listing)
    if len(selection.rows) < needed:
        raise RankerHold(f"{sym}: the sealed frame carries {len(selection.rows)} closed bars, needs {needed}")
    record = {
        **listing,
        "closed_bars": len(selection.rows),
        "boundary_ms": int(selection.boundary_ms),
        "bar_open_ms": int(selection.bar_open_ms),
        "input_sha256": selection.input_sha256,
        "mark": float(selection.rows[-1][4]),
    }
    return {**record, "status": "eligible", "reason": ""}, selection


def _frame(rows):
    import pandas as pd
    df = pd.DataFrame([[int(r[0])] + [float(x) for x in r[1:6]] for r in rows],
                      columns=["timestamp", "open", "high", "low", "close", "volume"])
    df.index = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    return df


def validate_output(result, eligible):
    if not isinstance(result, dict):
        raise RankerInvalid(f"ranker output must be an object, got {type(result).__name__}")
    unknown = sorted(set(result) - OUTPUT_KEYS)
    if unknown:
        raise RankerInvalid(f"ranker output has unknown keys {unknown}")
    hold = result.get("hold")
    if hold is not None and not isinstance(hold, str):
        raise RankerInvalid("ranker output 'hold' must be a string")
    if hold:
        if any(k in result for k in ("scores", "weights", "exclusions")):
            raise RankerInvalid("a ranker hold must not carry scores, weights or exclusions")
        raise RankerHold(f"the ranker held: {hold}")
    expected = set(eligible)
    for key in ("scores", "weights"):
        block = result.get(key)
        if not isinstance(block, dict):
            raise RankerInvalid(f"ranker output {key!r} must be an object")
        if set(block) != expected:
            raise RankerInvalid(f"ranker output {key!r} covers {sorted(block)}, the eligible set is {sorted(expected)}")
        for sym, value in block.items():
            if not _finite_number(value):
                raise RankerInvalid(f"ranker output {key}[{sym}] is not a finite number: {value!r}")
    exclusions = result.get("exclusions", {})
    if not isinstance(exclusions, dict):
        raise RankerInvalid("ranker output 'exclusions' must be an object")
    for sym, reason in exclusions.items():
        if sym not in expected:
            raise RankerInvalid(f"ranker exclusion {sym} is not an eligible member")
        if not isinstance(reason, str) or not reason.strip():
            raise RankerInvalid(f"ranker exclusion {sym} needs a reason")
        if float(result["weights"][sym]) != 0.0:
            raise RankerInvalid(f"ranker excluded {sym} but assigned it weight {result['weights'][sym]}")
    weights = {sym: float(v) for sym, v in result["weights"].items()}
    total = sum(abs(v) for v in weights.values())
    if total > 1.0 + WEIGHT_SUM_TOLERANCE:
        raise RankerInvalid(f"absolute target weights sum to {total}, above 1")
    if not any(v > 0 for v in weights.values()):
        raise RankerHold("the long side is empty")
    if not any(v < 0 for v in weights.values()):
        raise RankerHold("the short side is empty")
    scores = {sym: float(v) for sym, v in result["scores"].items()}
    return scores, weights, {sym: str(r) for sym, r in exclusions.items()}


def input_fingerprint(req, ctx, evidence):
    blob = json.dumps({
        "v": REQUEST_VERSION,
        "basket_id": req["basket_id"],
        "ranker": req["ranker"],
        "params": req["params"],
        "deadline_ms": req["deadline_ms"],
        "expected": req["expected"],
        "allocation_context": dict(ctx),
        "listings": req["listings"],
        "evidence": evidence,
    }, sort_keys=True, separators=(",", ":"), default=str)
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def admit_ranker(req, ctx):
    registry = _load_registry()
    try:
        entry = registry.get_basket_ranker(req["ranker"])
    except ValueError as e:
        raise RankerInvalid(str(e))
    if entry.get("edge_status") == registry.EDGE_STATUS_NO_EDGE and req.get("mode") != "paper":
        raise RankerInvalid(
            f"basket evaluator {req['ranker']!r} is edge_status=no_edge (source {entry.get('edge_source')}, "
            f"evidence {entry.get('edge_ref')}); it runs only in explicit paper mode")
    defaults = dict(entry["default_params"])
    unknown = sorted(set(req["params"]) - set(defaults))
    if unknown:
        raise RankerInvalid(f"unknown ranker params {unknown}")
    effective = {**defaults, **req["params"]}
    try:
        registry.validate_params(req["ranker"], req["params"])
    except (TypeError, ValueError) as e:
        raise RankerInvalid(f"invalid ranker params: {e}")
    preflight = entry.get("basket_preflight")
    if preflight is not None:
        try:
            preflight(MappingProxyType(dict(effective)), ctx)
        except (TypeError, ValueError) as e:
            raise RankerInvalid(f"the basket configuration cannot run {req['ranker']}: {e}")
    return entry, effective


def run(raw):
    req, ctx = parse_request(raw)
    out = {
        "v": REQUEST_VERSION,
        "basket_id": req["basket_id"],
        "deadline_ms": req["deadline_ms"],
        "snapshot_id": req["expected"].get("snapshot_id"),
        "generation": req["expected"].get("generation"),
        "cutoff_ms": req["expected"].get("cutoff_ms"),
        "status": "hold",
        "hold_reason": "",
        "eligibility": {},
        "scores": {},
        "weights": {},
        "exclusions": {},
        "input_fingerprint": "",
    }
    entry, effective = admit_ranker(req, ctx)
    evidence = {}
    try:
        cutoff = check_envelope(req)
        selections = {}
        for member in sorted(req["members"], key=lambda m: m["symbol"]):
            record, selection = member_evidence(req, ctx, member, cutoff)
            evidence[member["symbol"]] = record
            if selection is not None:
                selections[member["symbol"]] = selection
        out["eligibility"] = evidence
        out["input_fingerprint"] = input_fingerprint(req, ctx, evidence)
        eligible = sorted(selections)
        if len(eligible) < req["min_eligible"]:
            raise RankerHold(f"the eligible population {len(eligible)} is below the minimum {req['min_eligible']}")
        frames = {sym: _frame(selections[sym].rows) for sym in eligible}
        eligibility = {sym: dict(evidence[sym]) for sym in evidence}
        result = entry["basket_fn"](frames, eligibility, ctx, **effective)
        scores, weights, exclusions = validate_output(result, eligible)
        out.update({"status": "targets", "scores": scores, "weights": weights, "exclusions": exclusions})
        return out, 0
    except RankerHold as e:
        out["eligibility"] = evidence
        if not out["input_fingerprint"] and evidence:
            out["input_fingerprint"] = input_fingerprint(req, ctx, evidence)
        out["status"] = "hold"
        out["hold_reason"] = str(e)
        return out, 0


def main():
    if "--probe-only" in sys.argv[1:]:
        sys.exit(0)
    try:
        out, code = run(sys.stdin.read())
    except RankerInvalid as e:
        print(json.dumps({"status": "invalid", "error": str(e)}))
        sys.exit(1)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({"status": "invalid", "error": f"{type(e).__name__}: {e}"}))
        sys.exit(1)
    print(json.dumps(out, sort_keys=True))
    sys.exit(code)


if __name__ == "__main__":
    main()
