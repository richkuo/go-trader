#!/usr/bin/env python3

import copy
import hashlib
import json
import os
import sys
import tempfile
from decimal import Decimal

HERE = os.path.dirname(os.path.abspath(__file__))
FIXTURE = os.path.dirname(HERE)
BACKTEST = os.path.abspath(os.path.join(FIXTURE, "..", ".."))
sys.path.insert(0, BACKTEST)

import ledger_compare as lc
from backtester import _ensure_close_strategies_path

_ensure_close_strategies_path()
from _helpers import hl_round_limit, tier_price

BASE_EXPORT = os.path.join(FIXTURE, "export_paper.json")
BASE_INPUT = os.path.join(FIXTURE, "comparison_input_paper.json")
EXPORT_OUT = os.path.join(FIXTURE, "export_paper_resting.json")
INPUT_OUT = os.path.join(FIXTURE, "comparison_input_paper_resting.json")
TOUCH_EXPORT_OUT = os.path.join(FIXTURE, "export_paper_resting_touch.json")
TOUCH_INPUT_OUT = os.path.join(FIXTURE, "comparison_input_paper_resting_touch.json")
LINKED = ("market", "resting_market")
RESTING_MANIFEST = "resting_market/manifest.json"
BASE_MANIFEST = "market/manifest.json"
INTERVAL_START = "2026-01-05T00:00:00Z"
INTERVAL_END = "2026-01-06T20:00:00Z"
INPUT_VERSION = 4
SZ_DECIMALS = 5
LIMIT_PX = Decimal("62125")
TOUCH_BAR_MS = 1767610800000
CROSS_BAR_MS = 1767625200000
REACH_PX = 62127.0
TOUCH_RECORD_TIME = "2026-01-05T12:00:41Z"
RECORD_LAG_S = 41
FIRST_ROW_ID = 1001
OPEN_TEMPLATE = "paper/trades/149"
PARTIAL_CLOSE_TEMPLATE = "paper/trades/184"
FUNDING_TEMPLATE = "paper/trades/156"
SECOND_TIER_GAP = 5.0


def _load(path):
    with open(path) as fh:
        return json.load(fh)


def _dump(path, obj):
    with open(path, "w") as fh:
        fh.write(json.dumps(obj, indent=2) + "\n")


def _sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def _in_interval(ts):
    return INTERVAL_START <= ts < INTERVAL_END


def _ladder(m1):
    return {"tp_tiers": [{"atr_multiple": m1, "close_fraction": 0.5},
                         {"atr_multiple": m1 + SECOND_TIER_GAP, "close_fraction": 1.0}]}


def _strategy(strategy, m1):
    strategy["close_strategy"] = {"name": "tiered_tp_atr", "params": _ladder(m1)}
    strategy["resting_tp_trade_through"] = True


def _base_export():
    doc = copy.deepcopy(_load(BASE_EXPORT))
    doc["events"] = [ev for ev in doc["events"] if not _in_interval(ev["timestamp"])]
    return doc


def _input(doc, m1, manifest):
    cin = copy.deepcopy(_load(BASE_INPUT))
    cin["schema_version"] = INPUT_VERSION
    cin["description"] = ("Synthetic paper fixture (issue 1727): hl-strict-btc booked in the paper partition with a "
                          "tiered_tp_atr ladder and resting_tp_trade_through true, over resting_market/, whose "
                          "2026-01-05T11:00Z bar touches the first tier limit and whose 2026-01-05T15:00Z bar trades "
                          "two ticks through it.")
    cin["export"]["schema_version"] = doc["schema_version"]
    cin["export"]["booked_sections_sha256"] = lc.booked_sections_sha256(doc)
    cin["market"]["manifest"] = {"path": manifest, "sha256": _sha(os.path.join(FIXTURE, manifest))}
    for seg in cin["historical_configuration"]["timeline"]:
        _strategy(seg["strategy"], m1)
    return cin


def _compare(doc, cin, capture=None):
    with tempfile.TemporaryDirectory() as tmp:
        for name in LINKED:
            os.symlink(os.path.join(FIXTURE, name), os.path.join(tmp, name))
        exp_path, in_path = os.path.join(tmp, "export.json"), os.path.join(tmp, "comparison_input.json")
        _dump(exp_path, doc)
        _dump(in_path, cin)
        orig = lc.run_simulation

        def wrapped(market, signals, plan):
            res = orig(market, signals, plan)
            if capture is not None:
                capture["sim"] = res
            return res

        lc.run_simulation = wrapped
        try:
            return lc.compare(exp_path, in_path, lc.COMPARISON_MODE_STRICT)
        finally:
            lc.run_simulation = orig


def _simulate(doc, m1, manifest):
    capture = {}
    rep = _compare(doc, _input(doc, m1, manifest), capture)
    if capture.get("sim") is None:
        raise SystemExit(f"the comparison did not simulate: {rep['eligibility']['refusals']}")
    if rep["simulation"]["cost_model"]["kind"] != lc.PAPER_COST_MODEL_KIND:
        raise SystemExit("the comparison did not use the paper cost model")
    return rep, capture["sim"]["envelope"]["events"]


def _tier_closes(events):
    return [e for e in events if lc.is_simulated_tier_close(e)]


def _calibrate(doc):
    _, events = _simulate(doc, 1.0, RESTING_MANIFEST)
    first_pos = next(e["position_local_id"] for e in events if e["kind"] == "open")
    closes = [e for e in _tier_closes(events) if e["position_local_id"] == first_pos]
    if not closes or not isinstance(closes[0]["resting_fill"], dict):
        raise SystemExit("the calibration run booked no tier close with resting_fill evidence")
    evidence = closes[0]["resting_fill"]
    anchor, atr = float(evidence["anchor_px"]), float(evidence["atr"])
    m1 = (float(LIMIT_PX) - anchor) / atr
    if hl_round_limit(tier_price("long", anchor, atr, m1), SZ_DECIMALS) != LIMIT_PX:
        raise SystemExit(f"tier multiple {m1!r} does not round to the limit {LIMIT_PX}")
    return m1


def _check_outcomes(events, base_events):
    closes = _tier_closes(events)
    if len(closes) != 1:
        raise SystemExit(f"expected exactly one simulated tier close, got {len(closes)}")
    close = closes[0]
    evidence = close["resting_fill"]
    tier = evidence["tiers"][0] if isinstance(evidence, dict) and evidence.get("tiers") else {}
    if not (isinstance(evidence, dict) and evidence.get("verdict") == "traded_through"
            and tier.get("limit_px") == float(LIMIT_PX) and tier.get("cross_bar_open_ms") == CROSS_BAR_MS
            and (tier.get("ticks_through") or 0) >= 1 and evidence.get("reach_px") == REACH_PX
            and evidence.get("fill_px") == close["raw_price"]):
        raise SystemExit(f"the simulated tier close does not carry the stated trade-through evidence: {evidence}")
    touch_bar = lc._iso(lc.pd.Timestamp(TOUCH_BAR_MS, unit="ms", tz="UTC"))
    if any(e["bar_timestamp"] == touch_bar for e in closes):
        raise SystemExit("a simulated tier close booked on the touch bar")
    opens = [(e["bar_timestamp"], e["quantity"], e["raw_price"]) for e in events if e["kind"] == "open"]
    base_opens = [(e["bar_timestamp"], e["quantity"], e["raw_price"]) for e in base_events if e["kind"] == "open"]
    if opens != base_opens:
        raise SystemExit(f"simulated opens {opens} differ from the market/ run {base_opens}")
    return close


def _set(ev, field, value):
    ev[field]["value"] = value
    ev[field]["raw_value"] = value


def _new_row(template, row_id, ts):
    ev = copy.deepcopy(template)
    old = ev["source_row_id"]
    ev["source_row_id"] = str(row_id)
    ev["event_key"] = f"{ev['source_role']}/{ev['source_table']}/{row_id}"
    ev["timestamp"] = ev["timestamp_raw"] = ts
    for item in ev.values():
        if isinstance(item, dict) and isinstance(item.get("provenance"), list):
            for prov in item["provenance"]:
                if prov.get("source_row_id") == old:
                    prov["source_row_id"] = str(row_id)
    return ev


def _record_time(bar_ts, offset_s):
    return lc._iso(lc._utc(bar_ts, "simulated bar") + lc.pd.Timedelta(seconds=offset_s))


def _book(doc, events, interval_s):
    templates = {ev["event_key"]: ev for ev in _load(BASE_EXPORT)["events"]}
    positions = {}
    rows = []
    for e in events:
        if e["synthetic"] or e["kind"] in ("terminal_liquidation", "seed_inventory"):
            continue
        if e["kind"] == "funding":
            rows.append((e["bar_timestamp"], 2, "funding", e))
        elif e["kind"] == "open":
            rows.append((_record_time(e["bar_timestamp"], RECORD_LAG_S), 0, "open", e))
        elif e["kind"] == "close" and lc.is_simulated_tier_close(e) and e["qty_after"] != 0:
            rows.append((_record_time(e["bar_timestamp"], interval_s + RECORD_LAG_S), 1, "partial_close", e))
        else:
            raise SystemExit(f"{e['event_id']} ({e['kind']}, {e['reason']}) is not an open, partial tier close or "
                             "funding event; the fixture books only those")
    rows.sort(key=lambda r: (r[0], r[1]))
    booked = []
    for i, (ts, _, kind, e) in enumerate(rows):
        if not _in_interval(ts):
            raise SystemExit(f"{e['event_id']} record time {ts} is outside the interval")
        template = templates[{"open": OPEN_TEMPLATE, "partial_close": PARTIAL_CLOSE_TEMPLATE,
                              "funding": FUNDING_TEMPLATE}[kind]]
        ev = _new_row(template, FIRST_ROW_ID + i, ts)
        if kind == "funding":
            realized = float(e["funding_cash"])
            _set(ev, "realized_pnl", realized)
            _set(ev, "row_net_pnl", realized)
            _set(ev, "ledger_delta", realized)
        else:
            pid = positions.setdefault(e["position_local_id"], f"pos-s-{len(positions) + 1}")
            qty, price, fee = float(e["quantity"]), float(e["effective_price"]), float(e["fee_charged"])
            realized = float(e["gross_realized"]) if kind == "partial_close" else 0.0
            _set(ev, "position_id", pid)
            _set(ev, "side", e["action"])
            _set(ev, "quantity", qty)
            _set(ev, "price", price)
            _set(ev, "value", qty * price)
            _set(ev, "exchange_fee", fee)
            _set(ev, "realized_pnl", realized)
            _set(ev, "row_net_pnl", realized - fee)
            _set(ev, "ledger_delta", realized - fee)
        booked.append(ev)
    doc["events"] = doc["events"] + booked
    return doc


def _present_configuration(doc, m1):
    _strategy(doc["current_effective_configuration"]["strategy"], m1)


def _touch_variant(doc, close_key):
    touch = copy.deepcopy(doc)
    moved = [ev for ev in touch["events"] if ev["event_key"] == close_key]
    if len(moved) != 1:
        raise SystemExit(f"booked tier close {close_key} not found")
    moved[0]["timestamp"] = moved[0]["timestamp_raw"] = TOUCH_RECORD_TIME
    return touch


def _verify_resting(rep):
    if not rep["strict_success"] or not rep["strict_checks"]["resting_tier_closes_verified"]:
        raise SystemExit(f"the resting fixture does not reach strict success: {rep['strict_checks']} "
                         f"{rep['eligibility']['refusals']} {rep['resting_fill']['failures']}")


def _verify_touch(rep, close_key):
    false_checks = sorted(k for k, v in rep["strict_checks"].items() if not v)
    if rep["strict_success"] or false_checks != ["matched_within_tolerance", "resting_tier_closes_verified"]:
        raise SystemExit(f"the touch fixture has unexpected strict checks: {false_checks}")
    codes = [(f["code"], f["booked_event_key"]) for f in rep["resting_fill"]["failures"]]
    if codes != [("tier_close_out_of_tolerance", close_key)]:
        raise SystemExit(f"the touch fixture has unexpected resting_fill failures: {codes}")


def main():
    base = _base_export()
    m1 = _calibrate(base)
    _, events = _simulate(base, m1, RESTING_MANIFEST)
    _, base_events = _simulate(base, m1, BASE_MANIFEST)
    _check_outcomes(events, base_events)
    interval_s = _load(os.path.join(FIXTURE, RESTING_MANIFEST))["interval"]
    if interval_s != "1h":
        raise SystemExit(f"resting_market interval {interval_s!r} is not 1h")
    doc = _book(base, events, 3600)
    _present_configuration(doc, m1)
    cin = _input(doc, m1, RESTING_MANIFEST)
    rep = _compare(doc, cin)
    _verify_resting(rep)
    close_key = next(r["event_key"] for r in rep["resting_fill"]["booked_tier_closes"])
    touch = _touch_variant(doc, close_key)
    touch_cin = _input(touch, m1, RESTING_MANIFEST)
    touch_rep = _compare(touch, touch_cin)
    _verify_touch(touch_rep, close_key)
    _dump(EXPORT_OUT, doc)
    _dump(INPUT_OUT, cin)
    _dump(TOUCH_EXPORT_OUT, touch)
    _dump(TOUCH_INPUT_OUT, touch_cin)
    print(f"wrote {', '.join(os.path.relpath(p, BACKTEST) for p in (EXPORT_OUT, INPUT_OUT, TOUCH_EXPORT_OUT, TOUCH_INPUT_OUT))} "
          f"(first tier multiple {m1!r}, limit {LIMIT_PX})")


if __name__ == "__main__":
    main()
