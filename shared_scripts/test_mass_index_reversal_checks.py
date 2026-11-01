import importlib.util
import json
import os
import sys

_NAME = "_strategy_check_contract_for_mass_index_reversal"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["mass_index_reversal"]
NAME = CASE.name
H = C.H
T0 = C.DAY_T0
UP_AT, DOWN_AT = C.MIR_UP_AT, C.MIR_DOWN_AT


def _market(bars):
    return C.envelope(bars, H, "BTC|1h", snapshot_id=CASE.snapshot_id)["market"]


def _check(bars):
    return C.check(CASE, bars, closed=True)


def _assert_closed_completion(out, bars, signal, reason, case):
    meta = out["closed_bar_decision"]
    assert meta["held"] is False and meta["forming_rows_dropped"] == 1, case
    assert out["signal"] == signal and out["indicator_labels"]["mir_reason"] == reason, case
    ind = out["indicators"]
    assert ind["mir_valid"] == 1.0 and ind["mir_bulge_completed"] == 1.0, case
    assert ind["mir_eval_ts_ms"] == float(bars[-2]["t"]), case
    assert ind["mir_bulge_armed_ts_ms"] == float(T0 + UP_AT * H), case
    assert ind["mir_bulge_age"] == float(DOWN_AT - UP_AT) and ind["mir_required_history"] == 148.0, case
    assert ind["mir_mass_index"] < 26.5 <= ind["mir_prev_mass_index"], case


def test_closed_short_completion_bar_authorizes_the_entry_a_forming_one_cannot_and_the_mixed_batch_matches():
    bars = C.mir_bars(DOWN_AT)
    solo = _check(bars)
    _assert_closed_completion(solo, bars, -1, "entry_short",
                              "closed_bar_paper_check_decides_on_the_closed_completion_bar[entry_short]")
    case = "a_forming_completion_candle_cannot_authorize_an_entry"
    forming = C.mir_bars(DOWN_AT - 1)
    out = _check(forming)
    assert out["signal"] == 0 and out["indicator_labels"]["mir_reason"] == "armed", case
    assert out["indicators"]["mir_eval_ts_ms"] == float(forming[-2]["t"]), case
    extreme = [dict(b) for b in forming]
    extreme[-1].update({"c": 1.0, "l": 0.5, "o": 1.0})
    out = _check(extreme)
    assert out["signal"] == 0 and out["indicators"]["mir_eval_ts_ms"] == float(forming[-2]["t"]), case
    assert solo["signal"] == -1, case
    case = "a_mixed_batch_refuses_the_invalid_slot_and_matches_the_solo_check"
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
            "closed_bar_decisions": True, "strategy_refs": json.loads(C.refs(CASE))}
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "closed_bar_decisions": True, "params": {"recursion_length": 40}}
    fallback = {"id": "fallback", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
                "open_strategy": "breakout", "close_strategies": [NAME],
                "close_params_by_name": {NAME: {"ema_period": 1}}}
    proc = C.run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--market-stdin"],
                 {"v": 2, "slots": [good, bad, fallback], "market": _market(bars)})
    assert proc.returncode == 1, case
    results = {r["id"]: r for r in C.payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "recursion_length" in results["bad"]["error"], case
    assert f"close strategy '{NAME}' has invalid parameters" in results["fallback"]["error"], case
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels", "closed_bar_decision"):
        assert results["good"].get(key) == solo.get(key), (case, key)
    assert solo["signal"] == -1, case


def test_closed_bar_paper_check_decides_on_the_closed_long_completion_bar():
    bars = C.mir_bars(DOWN_AT, -0.05)
    _assert_closed_completion(_check(bars), bars, 1, "entry_long",
                              "closed_bar_paper_check_decides_on_the_closed_completion_bar[entry_long]")
