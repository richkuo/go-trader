import importlib.util
import json
import os
import sys

import pytest

_NAME = "_strategy_check_contract_for_calendar_conditioned_entry"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["calendar_conditioned_entry"]
NAME = CASE.name
SMALL = CASE.params
H = C.H


def _market(bars):
    return C.envelope(bars, H, "BTC|1h", snapshot_id=CASE.snapshot_id)["market"]


def _check(bars, *, params=None, closed=True, closes=C.DEFAULT, limit=800):
    return C.check(CASE, bars, closed=closed, limit=limit, strategy_refs=C.refs(CASE, closes=closes, params=params))


def test_explicit_paper_check_decides_on_the_closed_bar_and_anchors_the_frame_hold_and_mixed_batch():
    bars = C.cce_bars()
    out = _check(bars)
    solo = out
    case = "explicit_paper_check_decides_on_the_closed_bar_for_the_next_calendar_cell"
    meta = out["closed_bar_decision"]
    assert meta["held"] is False and meta["forming_rows_dropped"] == 1, case
    assert out["signal"] == 1 and out["open_action"] == "long", case
    assert out["indicator_labels"]["cce_reason"] == "entry_long", case
    ind = out["indicators"]
    assert ind["cce_valid"] == 1.0 and ind["cce_eval_ts_ms"] == float(bars[-2]["t"]), case
    assert ind["cce_next_open_ms"] == float(bars[-1]["t"]) and ind["cce_next_cell"] == 14.0, case
    assert ind["cce_decision_boundary_ms"] == float(bars[-2]["t"] + H), case
    assert ind["cce_decision_cutoff_ms"] == float(bars[-1]["t"] + H // 2), case
    assert ind["cce_family_size"] == 336.0 and ind["cce_long_lower_bound"] > 0.0, case
    assert ind["cce_long_net_edge"] == ind["cce_gross_mean"] - SMALL["cost_hurdle"], case
    assert ind["cce_refit_boundary_ms"] <= ind["cce_decision_boundary_ms"], case
    assert ind["cce_gross_mean"] != round(ind["cce_gross_mean"], 6), case
    case = "without_closed_bar_decisions_the_check_frame_holds_every_entry"
    out = _check(bars, closed=False)
    assert "closed_bar_decision" not in out, case
    assert out["signal"] == 0 and out["open_action"] == "none", case
    assert out["indicator_labels"]["cce_reason"] == "undeclared_timing", case
    assert solo["signal"] == 1, case
    case = "a_mixed_batch_evaluates_valid_peers_and_matches_the_solo_check"
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
            "closed_bar_decisions": True, "strategy_refs": json.loads(C.refs(CASE))}
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "closed_bar_decisions": True, "params": {**SMALL, "confidence": 1.5}}
    proc = C.run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "800", "--market-stdin"],
                 {"v": 2, "slots": [good, bad], "market": _market(bars)})
    assert proc.returncode == 1, case
    results = {r["id"]: r for r in C.payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "has invalid parameters" in results["bad"]["error"], case
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels", "closed_bar_decision"):
        assert results["good"].get(key) == solo.get(key), (case, key)
    assert solo["signal"] == 1, case


def test_the_production_frame_and_default_parameters_hold_for_missing_training_history():
    bars = C.cce_bars(n=201)
    out = _check(bars, params={}, limit=200, closes=({"name": "time_stop", "params": {"max_bars": 4}},))
    assert out["closed_bar_decision"]["held"] is False
    assert out["signal"] == 0 and out["open_action"] == "none"
    assert out["indicator_labels"]["cce_reason"] == "insufficient_history"
    assert out["indicators"]["cce_required_bars"] >= 4368.0 and out["indicators"]["cce_history_bars"] == 200.0


def test_a_cadence_mismatch_is_refused_before_any_candle_fetch():
    strategy_refs = json.dumps({"open": {"name": NAME}, "closes": [C.TIME_STOP]})
    proc = C.run([NAME, "BTC", "4h", "--mode=paper", "--strategy-refs", strategy_refs])
    assert proc.returncode == 1
    out = C.payload(proc)
    assert out["signal"] == 0 and "registry default expected_interval_ms=3600000" in out["error"]
    assert "Fetching BTC" not in proc.stderr


@pytest.mark.parametrize("effect", [0.0])
def test_a_frame_without_a_calendar_effect_holds_below_the_corrected_threshold(effect):
    out = _check(C.cce_bars(effect=effect))
    assert out["signal"] == 0 and out["indicator_labels"]["cce_reason"] in ("below_threshold",
                                                                           "insufficient_effective_obs")
    assert out["indicators"]["cce_valid"] == 1.0
