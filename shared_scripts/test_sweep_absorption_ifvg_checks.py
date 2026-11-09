import importlib.util
import json
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
_NAME = "_strategy_check_contract_for_sweep_absorption_ifvg"
_spec = importlib.util.spec_from_file_location(_NAME, os.path.join(_HERE, "test_strategy_check_contract.py"))
C = sys.modules.get(_NAME)
if C is None:
    C = importlib.util.module_from_spec(_spec)
    sys.modules[_NAME] = C
    _spec.loader.exec_module(C)

CASE = C.RESEARCH_CASES["sweep_absorption_ifvg"]
POSITION = ["--position-side", "long", "--position-avg-cost=90", "--position-qty=1", "--position-initial-qty=1",
            "--position-entry-atr=0.5"]


def _without(section):
    market = CASE.market()
    if section == "htf":
        market["frames"] = {k: v for k, v in market["frames"].items() if k != "BTC|1h"}
    elif section == "flow":
        market.pop("trade_flows", None)
    elif section == "htf_timing":
        market["frames"]["BTC|1h"] = {k: v for k, v in market["frames"]["BTC|1h"].items() if k != "timing"}
    return {"v": 2, "market": market}


def test_missing_entry_inputs_hold_the_entry_while_the_close_owner_still_executes_solo_and_batch():
    refs = json.dumps(C.research_refs(CASE))
    base = [CASE.name, CASE.symbol, CASE.tf, "--market-stdin", "--mode=paper", "--closed-bar-decisions",
            "--strategy-refs", refs]
    for section, reason in (("htf", "htf_unavailable"), ("htf_timing", "htf_unavailable"),
                            ("flow", "trade_flow_unavailable")):
        stdin = _without(section)
        flat = C.payload(C.run(base, stdin))
        assert not flat.get("error"), (section, flat.get("error"))
        assert flat["signal"] == 0 and flat["indicator_labels"]["sai_reason"] == reason, (section, flat)
        assert flat["closed_bar_decision"]["held"] is False, section
        held = C.payload(C.run(base + POSITION, stdin))
        assert held["signal"] == -1 and held["close_strategy"] == "tiered_tp_pct", (section, held)
        assert held["close_fraction"] == 1.0, section
        slots = [{"id": "flat", "strategy": CASE.name, "mode": "paper", "mode_args": ["--mode=paper"],
                  "closed_bar_decisions": True, "strategy_refs": C.research_refs(CASE)},
                 {"id": "held", "strategy": CASE.name, "mode": "paper", "mode_args": ["--mode=paper"],
                  "closed_bar_decisions": True, "strategy_refs": C.research_refs(CASE), "position_side": "long",
                  "position_ctx": {"side": "long", "avg_cost": 90.0, "current_quantity": 1.0,
                                   "initial_quantity": 1.0, "entry_atr": 0.5}}]
        proc = C.run(["--batch-check", f"--symbol={CASE.symbol}", f"--timeframe={CASE.tf}", "--market-stdin"],
                     dict(stdin, slots=slots))
        results = {r["id"]: r for r in C.payload(proc)["results"]}
        for key in ("signal", "indicators", "indicator_labels"):
            assert results["flat"].get(key) == flat.get(key), (section, key)
        assert results["held"]["signal"] == -1 and results["held"]["close_strategy"] == "tiered_tp_pct", section
    full = C.payload(C.run(base, {"v": 2, "market": CASE.market()}))
    assert full["signal"] == 1 and full["indicators"]["sai_entry_price"] == full["indicators"]["sai_ce"]
    assert full["indicators"]["sai_htf_bias"] == 1.0 and full["indicators"]["sai_absorption_ratio"] > 0.05
