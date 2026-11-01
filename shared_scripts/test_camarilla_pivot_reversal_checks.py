import importlib.util
import json
import os
import sys

import pytest

_NAME = "_strategy_check_contract_for_camarilla_pivot_reversal"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["camarilla_pivot_reversal"]
NAME = CASE.name
H = C.H
DAY = C.DAY
TP = CASE.tp


def _market(bars):
    return C.envelope(bars, H, "BTC|1h", snapshot_id=CASE.snapshot_id)["market"]


def _check(bars, *, closed=True, cutoff=None):
    return C.check(CASE, bars, closed=closed, cutoff=cutoff)


def _assert_closed_crossing(out, bars, signal, reason, case):
    meta = out["closed_bar_decision"]
    assert meta["held"] is False and meta["forming_rows_dropped"] == 1, case
    assert out["signal"] == signal and out["indicator_labels"]["cpr_reason"] == reason, case
    ind = out["indicators"]
    assert ind["cpr_valid"] == 1.0 and ind["cpr_eval_ts_ms"] == float(bars[-2]["t"]), case
    assert ind["cpr_decision_boundary_ms"] == float(bars[-2]["t"] + H), case
    assert ind["cpr_decision_cutoff_ms"] == float(bars[-1]["t"] + H // 2), case
    assert (ind["cpr_prior_high"], ind["cpr_prior_low"], ind["cpr_prior_close"]) == (110.0, 90.0, 100.0), case
    rng = ind["cpr_prior_high"] - ind["cpr_prior_low"]
    assert ind["cpr_s3"] == ind["cpr_prior_close"] - 1.1 * rng / 4, case
    assert ind["cpr_r4"] == ind["cpr_prior_close"] + 1.1 * rng / 2, case
    assert (ind["cpr_prev_close"], ind["cpr_close"]) == (bars[-3]["c"], bars[-2]["c"]), case
    assert ind["cpr_prior_day_open_ms"] == ind["cpr_day_open_ms"] - DAY, case


def test_closed_bar_long_crossing_decides_on_the_closed_candle_and_anchors_the_frame_hold_and_mixed_batch():
    bars = C.cpr_bars([100.0, 94.0, 95.0, 96.0])
    solo = _check(bars)
    _assert_closed_crossing(solo, bars, 1, "entry_long",
                            "closed_bar_paper_check_decides_on_the_closed_crossing_candle[entry_long]")
    case = "without_closed_bar_decisions_the_check_frame_holds_every_entry"
    out = _check(bars, closed=False)
    assert "closed_bar_decision" not in out, case
    assert out["signal"] == 0 and out["open_action"] == "none", case
    assert out["indicator_labels"]["cpr_reason"] == "undeclared_timing", case
    assert out["indicators"]["cpr_valid"] == 0.0, case
    assert solo["signal"] == 1, case
    case = "a_mixed_batch_evaluates_valid_peers_and_matches_the_solo_check"
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
            "closed_bar_decisions": True, "strategy_refs": json.loads(C.refs(CASE))}
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "closed_bar_decisions": True, "params": {"expected_interval_ms": 4 * H}}
    proc = C.run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--market-stdin"],
                 {"v": 2, "slots": [good, bad], "market": _market(bars)})
    assert proc.returncode == 1, case
    results = {r["id"]: r for r in C.payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "does not match timeframe '1h'" in results["bad"]["error"], case
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels", "closed_bar_decision"):
        assert results["good"].get(key) == solo.get(key), (case, key)
    assert solo["signal"] == 1, case


def test_closed_bar_paper_check_decides_on_the_closed_short_crossing_candle():
    bars = C.cpr_bars([100.0, 106.0, 105.0, 104.0])
    _assert_closed_crossing(_check(bars), bars, -1, "entry_short",
                            "closed_bar_paper_check_decides_on_the_closed_crossing_candle[entry_short]")


def test_a_stale_closed_bar_holds_and_a_forming_crossing_waits_for_its_close():
    bars = C.cpr_bars([100.0, 94.0, 95.0])
    stale = _check(bars, cutoff=bars[-1]["t"] + 2 * H + 1)
    assert stale["signal"] == 0 and stale["indicator_labels"]["cpr_reason"] == "decision_bar_stale"
    forming = _check(bars)
    assert forming["signal"] == 0 and forming["indicators"]["cpr_eval_ts_ms"] == float(bars[-2]["t"])
    closed = _check(bars + [C.cpr_bar(bars[-1]["t"] + H, 95.0, 95.5)])
    assert closed["signal"] == 1 and closed["indicators"]["cpr_eval_ts_ms"] == float(bars[-1]["t"])


@pytest.mark.parametrize("timeframe,params,needle", [
    ("4h", {"expected_interval_ms": H}, "does not match timeframe '4h'"),
    ("4h", None, "registry default expected_interval_ms=3600000"),
    ("5m", {"expected_interval_ms": 300_000}, "expected_interval_ms >= 900000"),
    ("6h", {"expected_interval_ms": 21_600_000}, "not a supported interval"),
    ("1h", {"expected_interval_ms": True}, "expected_interval_ms"),
])
def test_cadence_errors_are_refused_before_any_candle_fetch(timeframe, params, needle):
    strategy_refs = json.dumps({"open": {"name": NAME, "params": params or {}}, "closes": [TP]})
    proc = C.run([NAME, "BTC", timeframe, "--mode=paper", "--strategy-refs", strategy_refs])
    assert proc.returncode == 1
    out = C.payload(proc)
    assert out["signal"] == 0 and needle in out["error"]
    assert f"open strategy '{NAME}' has invalid parameters" in out["error"]
    assert "Fetching BTC" not in proc.stderr


def test_a_batch_with_only_invalid_slots_refuses_every_slot_without_building_shared_state():
    slots = [
        {"id": "cadence", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
         "params": {"expected_interval_ms": H}},
        {"id": "fallback", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "open_strategy": "breakout", "close_strategies": [NAME]},
    ]
    proc = C.run(["--batch-check", "--symbol=BTC", "--timeframe=4h"], {"v": 1, "slots": slots})
    assert proc.returncode == 1
    out = C.payload(proc)
    assert out["error"] == "" and out["error_scope"] == ""
    results = {r["id"]: r for r in out["results"]}
    assert sorted(results) == ["cadence", "fallback"]
    assert "does not match timeframe '4h'" in results["cadence"]["error"]
    assert f"close strategy '{NAME}' has invalid parameters" in results["fallback"]["error"]
    assert all(r["signal"] == 0 for r in results.values())
    assert "Fetching BTC" not in proc.stderr
