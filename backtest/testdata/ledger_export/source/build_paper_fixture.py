#!/usr/bin/env python3

import copy
import json
import math
import os
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
FIXTURE = os.path.dirname(HERE)
BACKTEST = os.path.abspath(os.path.join(FIXTURE, "..", ".."))
sys.path.insert(0, BACKTEST)

import ledger_compare as lc
from backtester import PAPER_FILL_COST_MODELS

VERSION = 1
MODEL = PAPER_FILL_COST_MODELS[VERSION]
ROLE = "paper"
PARTITION = "paper"
EXPORT_OUT = os.path.join(FIXTURE, "export_paper.json")
INPUT_OUT = os.path.join(FIXTURE, "comparison_input_paper.json")
LINKED = ("market", "regime_market")
INTERVAL_START = "2026-01-05T00:00:00Z"
INTERVAL_END = "2026-01-06T20:00:00Z"


def _load(path):
    with open(path) as fh:
        return json.load(fh)


def _dump(path, obj):
    with open(path, "w") as fh:
        fh.write(json.dumps(obj, indent=2) + "\n")


def _paper_args(args):
    out = [a for a in args if not (isinstance(a, str) and a.startswith("--mode"))]
    return out + ["--mode=paper"]


def _relabel(ev):
    old_role = ev["source_role"]
    ev["source_role"] = ROLE
    ev["partition"] = PARTITION
    ev["event_key"] = ROLE + ev["event_key"][len(old_role):]
    for field, item in ev.items():
        if isinstance(item, dict) and isinstance(item.get("provenance"), list):
            for prov in item["provenance"]:
                if prov.get("source_role") == old_role:
                    prov["source_role"] = ROLE
    if ev["event_kind"]["value"] != "funding":
        _set(ev, "fee_source", "modeled")
        ev["exchange_order_id"]["value"] = None
        ev["exchange_order_id"]["raw_value"] = ""
        ev["exchange_order_id"]["status"] = "unavailable"
        ev["exchange_order_id"]["reason"] = "not_recorded"
    ev["cost_model_version"] = {
        "value": VERSION, "raw_value": VERSION, "status": "available", "reason": None,
        "provenance": [{"kind": "stored", "source_role": ROLE, "source_table": "trades",
                        "source_row_id": ev["source_row_id"], "source_field": "cost_model_version"}],
    }


def _set(ev, field, value):
    ev[field]["value"] = value
    ev[field]["raw_value"] = value


def _rebook_accounting(ev):
    realized = ev["realized_pnl"]["value"]
    fee = ev["exchange_fee"]["value"]
    if not ev["pnl_gross"]["value"]:
        raise SystemExit(f"{ev['event_key']} is a legacy net row; the paper fixture books gross rows only")
    _set(ev, "row_net_pnl", realized - fee)
    _set(ev, "ledger_delta", realized - fee)


def _paper_export():
    doc = copy.deepcopy(_load(os.path.join(FIXTURE, "export.json")))
    doc["selection"]["partition"] = PARTITION
    doc["selection"]["source_role"] = ROLE
    strat = doc["current_effective_configuration"]["strategy"]
    strat["args"] = _paper_args(strat["args"])
    doc["events"] = [ev for ev in doc["events"] if ev["timestamp"] < INTERVAL_END]
    for ev in doc["events"]:
        _relabel(ev)
    return doc


def _paper_input(doc, price_tol, money_tol, qty_tol=1e-9):
    cin = copy.deepcopy(_load(os.path.join(FIXTURE, "comparison_input.json")))
    cin["description"] = ("Synthetic paper fixture: hl-strict-btc booked in the paper partition with the version 1 "
                          "paper cost model (taker fee, adverse SlippagePct, no spread) over the frozen window.")
    cin["export"]["schema_version"] = doc["schema_version"]
    cin["export"]["selection"]["partition"] = PARTITION
    cin["export"]["booked_sections_sha256"] = lc.booked_sections_sha256(doc)
    for seg in cin["historical_configuration"]["timeline"]:
        seg["strategy"]["args"] = _paper_args(seg["strategy"]["args"])
    cin["tolerances"]["price_relative"]["value"] = price_tol
    cin["tolerances"]["money_absolute"]["value"] = money_tol
    cin["tolerances"]["quantity_absolute"]["value"] = qty_tol
    return cin


def _compare(doc, cin, capture):
    with tempfile.TemporaryDirectory() as tmp:
        for name in LINKED:
            os.symlink(os.path.join(FIXTURE, name), os.path.join(tmp, name))
        exp_path, in_path = os.path.join(tmp, "export_paper.json"), os.path.join(tmp, "comparison_input_paper.json")
        _dump(exp_path, doc)
        _dump(in_path, cin)
        orig = lc.run_simulation

        def wrapped(market, signals, plan):
            res = orig(market, signals, plan)
            capture["sim"] = res
            return res

        lc.run_simulation = wrapped
        try:
            return lc.compare(exp_path, in_path, lc.COMPARISON_MODE_STRICT)
        finally:
            lc.run_simulation = orig


def _check_v1_fill(sim_ev):
    raw, eff, qty = float(sim_ev["raw_price"]), float(sim_ev["effective_price"]), float(sim_ev["quantity"])
    if sim_ev["timing"] != "bar_open_fill":
        raise SystemExit(f"{sim_ev['event_id']} timing {sim_ev['timing']!r}: the fixture covers market fills only")
    want = raw * (1 + MODEL["slippage_pct"]) if sim_ev["action"] == "buy" else raw * (1 - MODEL["slippage_pct"])
    if eff != want:
        raise SystemExit(f"{sim_ev['event_id']} effective {eff!r} != paper version {VERSION} price {want!r}")
    fee = float(sim_ev["fee_charged"])
    if not math.isclose(fee, qty * eff * MODEL["taker_fee_pct"], rel_tol=1e-12, abs_tol=0.0):
        raise SystemExit(f"{sim_ev['event_id']} fee {fee!r} is not the paper taker fee")


def main():
    doc = _paper_export()
    capture = {}
    rep = _compare(doc, _paper_input(doc, 0.01, 1.0, 0.001), capture)
    if capture.get("sim") is None:
        raise SystemExit(f"the provisional comparison did not simulate: {rep['eligibility']}")
    if rep["simulation"]["cost_model"]["kind"] != lc.PAPER_COST_MODEL_KIND:
        raise SystemExit("the provisional comparison did not use the paper cost model")
    sim_events = {e["event_id"]: e for e in capture["sim"]["envelope"]["events"]}
    by_key = {ev["event_key"]: ev for ev in doc["events"]}
    rebooked = set()
    for pair in rep["matching"]["matched"]:
        if pair["unmatched_booked_components"] or pair["unmatched_simulated_components"]:
            raise SystemExit(f"{pair['booked_position_id']} has unmatched components")
        for comp in pair["components"]:
            ev, sim_ev = by_key[comp["booked_event_key"]], sim_events[comp["simulated_event_id"]]
            _check_v1_fill(sim_ev)
            qty, price = float(sim_ev["quantity"]), float(sim_ev["effective_price"])
            _set(ev, "quantity", qty)
            _set(ev, "price", price)
            _set(ev, "value", qty * price)
            _set(ev, "exchange_fee", float(sim_ev["fee_charged"]))
            _set(ev, "realized_pnl", float(sim_ev["gross_realized"]) if sim_ev["kind"] == "close" else 0.0)
            _rebook_accounting(ev)
            rebooked.add(ev["event_key"])
    sim_funding = {e["bar_timestamp"]: float(e["funding_cash"]) for e in sim_events.values() if e["kind"] == "funding"}
    for ev in doc["events"]:
        if ev["event_kind"]["value"] != "funding" or not (INTERVAL_START <= ev["timestamp"] < INTERVAL_END):
            continue
        if ev["timestamp"] not in sim_funding:
            raise SystemExit(f"{ev['event_key']} funding at {ev['timestamp']} has no simulated funding accrual")
        _set(ev, "realized_pnl", sim_funding.pop(ev["timestamp"]))
        _rebook_accounting(ev)
        rebooked.add(ev["event_key"])
    if sim_funding:
        raise SystemExit(f"simulated funding with no booked row: {sorted(sim_funding)}")
    for ev in doc["events"]:
        if (INTERVAL_START <= ev["timestamp"] < INTERVAL_END
                and ev["event_kind"]["value"] in ("non_close", "scale_in", "close", "funding")
                and ev["event_key"] not in rebooked):
            raise SystemExit(f"in-interval row {ev['event_key']} was not rebooked with the paper model")
    cin = _paper_input(doc, 1e-9, 1e-9)
    final = _compare(doc, cin, {})
    if not final["strict_success"]:
        raise SystemExit(f"the paper fixture does not reach strict success: {final['eligibility']}")
    _dump(EXPORT_OUT, doc)
    _dump(INPUT_OUT, cin)
    print(f"wrote {os.path.relpath(EXPORT_OUT, BACKTEST)} and {os.path.relpath(INPUT_OUT, BACKTEST)} "
          f"({len(rebooked)} rows rebooked with paper cost model version {VERSION})")


if __name__ == "__main__":
    main()
