import importlib.util
import json
import os
import sys

import numpy as np

_NAME = "_strategy_check_contract_for_elder_force_pullback"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["elder_force_pullback"]
NAME = CASE.name
CROSS_AT = C.EFP_CROSS_AT


def _market(bars):
    return C.envelope(bars, C.H, "BTC|1h", snapshot_id=CASE.snapshot_id)["market"]


def _check(bars, *, closed=True):
    return C.check(CASE, bars, closed=closed)


def _assert_closed_crossing(out, bars, signal, reason, case):
    meta = out["closed_bar_decision"]
    assert meta["held"] is False and meta["forming_rows_dropped"] == 1, case
    assert out["signal"] == signal and out["indicator_labels"]["efp_reason"] == reason, case
    ind = out["indicators"]
    assert ind["efp_valid"] == 1.0 and ind["efp_cross"] == float(signal), case
    assert ind["efp_eval_ts_ms"] == float(bars[-2]["t"]) and ind["efp_required_history"] == 152.0, case
    assert ind["efp_raw_force"] == (bars[-2]["c"] - bars[-3]["c"]) * 100.0, case
    assert np.sign(ind["efp_prev_force"]) == -signal and np.sign(ind["efp_force"]) == signal, case


def test_closed_long_crossing_bar_authorizes_the_entry_a_forming_one_cannot_and_the_mixed_batch_matches():
    bars = C.efp_bars(CROSS_AT)
    solo = _check(bars)
    _assert_closed_crossing(solo, bars, 1, "entry_long",
                            "closed_bar_paper_check_decides_on_the_closed_crossing_bar[entry_long]")
    case = "a_forming_crossing_candle_or_its_volume_cannot_authorize_an_entry"
    forming = C.efp_bars(CROSS_AT - 1)
    out = _check(forming)
    assert out["signal"] == 0 and out["indicator_labels"]["efp_reason"] == "no_cross", case
    assert out["indicators"]["efp_eval_ts_ms"] == float(forming[-2]["t"]), case
    extreme = [dict(b) for b in forming]
    extreme[-1].update({"v": 1e9, "c": forming[-1]["c"] + 50.0, "h": forming[-1]["c"] + 51.0})
    got = _check(extreme)
    assert got["signal"] == 0 and got["indicators"] == out["indicators"], case
    assert solo["signal"] == 1, case
    case = "a_mixed_batch_refuses_the_invalid_slot_and_matches_the_solo_check"
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
            "closed_bar_decisions": True, "strategy_refs": json.loads(C.refs(CASE))}
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "closed_bar_decisions": True, "params": {"recursion_length": 40}}
    fallback = {"id": "fallback", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
                "open_strategy": "breakout", "close_strategies": [NAME],
                "close_params_by_name": {NAME: {"force_period": 1}}}
    proc = C.run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--market-stdin"],
                 {"v": 2, "slots": [good, bad, fallback], "market": _market(bars)})
    assert proc.returncode == 1, case
    results = {r["id"]: r for r in C.payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "recursion_length" in results["bad"]["error"], case
    assert f"close strategy '{NAME}' has invalid parameters" in results["fallback"]["error"], case
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels", "closed_bar_decision"):
        assert results["good"].get(key) == solo.get(key), (case, key)
    assert solo["signal"] == 1, case
    assert results["good"]["indicators"]["efp_force"] == solo["indicators"]["efp_force"], case


def test_closed_bar_paper_check_decides_on_the_closed_short_crossing_bar():
    bars = C.efp_bars(CROSS_AT, -1)
    _assert_closed_crossing(_check(bars), bars, -1, "entry_short",
                            "closed_bar_paper_check_decides_on_the_closed_crossing_bar[entry_short]")


def test_legacy_checks_without_closed_bar_decisions_decide_on_the_forming_row():
    bars = C.efp_bars(CROSS_AT - 1)
    legacy = _check(bars, closed=False)
    assert "closed_bar_decision" not in legacy or legacy["closed_bar_decision"] is None
    assert legacy["signal"] == 1 and legacy["indicator_labels"]["efp_reason"] == "entry_long"
    assert legacy["indicators"]["efp_close"] == bars[-1]["c"] and legacy["indicators"]["efp_volume"] == bars[-1]["v"]
