import importlib
import importlib.util
import json
from pathlib import Path

import pytest

_FIXTURE = json.loads(
    (Path(__file__).resolve().parents[2] / "backtest" / "testdata" / "tp_tier_parity.json").read_text()
)


def _load_close_registry():
    path = Path(__file__).resolve().parent / "registry.py"
    spec = importlib.util.spec_from_file_location("_close_registry_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_close_registry()


@pytest.mark.parametrize("case", _FIXTURE["ladder_rule"], ids=lambda c: c["id"])
def test_resting_ladder_rule_matches_fixture(registry, case):
    got = importlib.import_module("tiered_tp_atr").resting_tiers(case["tiers"])

    assert (None if got is None else [list(pair) for pair in got]) == case["want"]


_FILL_RULE_EVALUATORS = (
    "tiered_tp_atr",
    "tiered_tp_atr_live",
    "tiered_tp_atr_regime",
    "tiered_tp_atr_live_regime",
)


def _fill_rule_params():
    for case in _FIXTURE["fill_rule"]:
        for name in case.get("evaluators", _FILL_RULE_EVALUATORS):
            yield pytest.param(case, name, id=f"{case['id']}-{name}")


def _fill_rule_market(case):
    helpers = importlib.import_module("_helpers")
    side = case["position"]["side"]
    walk = case["mode"] == "ohlc_walk"
    bars = [
        helpers.resting_rule_bar(
            c[0], c[2], c[3], c[4], side,
            entry_bar=i == 0 and case["entry_bar_in_frame"], walk_mode=walk,
        )
        for i, c in enumerate(case["candles"])
    ]
    rule = helpers.build_resting_rule(
        sz_decimals=case["sz_decimals"], bars=bars, stop_trigger_px=case["stop_trigger_px"],
        coverage=case["coverage"], held=bool(case["held"]), hold_reason=case["held"],
    )
    return {"mark_price": case["candles"][-1][4], **case["market"], "resting_tp_rule": rule}


@pytest.mark.parametrize("case,name", list(_fill_rule_params()))
def test_resting_fill_rule_matches_fixture(registry, case, name):
    p = case["position"]
    anchor = p["risk_anchor_price"] or p["avg_cost"]
    result = registry.evaluate(name, {
        "side": p["side"], "avg_cost": anchor, "risk_anchor_price": anchor,
        "current_quantity": p["quantity"], "initial_quantity": p["initial_quantity"],
        "entry_atr": p["entry_atr"], "regime": p["regime"], "tp_model": "resting_limit",
    }, _fill_rule_market(case), _FIXTURE["fill_rule_ladders"][name])
    want = case["want"]

    assert result["close_fraction"] == pytest.approx(want["close_fraction"])
    assert result.get("tier_fill_price", 0.0) == pytest.approx(want["fill_price"], abs=0, rel=1e-12)
    if "reason" in want:
        assert result["reason"] == want["reason"]
    evidence = result.get("resting_fill")
    if "verdict" in want:
        assert evidence["verdict"] == want["verdict"]
    else:
        assert evidence is None
    if "stop_reached_bar_open_ms" in want:
        assert evidence["stop_reached_bar_open_ms"] == want["stop_reached_bar_open_ms"]
    if "coverage" in want:
        assert evidence["coverage"] == want["coverage"]

