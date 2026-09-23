import importlib
import importlib.util
import json
from pathlib import Path

import pytest

_FIXTURE = json.loads(
    (Path(__file__).resolve().parents[2] / "backtest" / "testdata" / "tp_tier_parity.json").read_text()
)

_GEOMETRY_MODE = {
    "tiered_tp_atr": (False, ""),
    "tiered_tp_atr_live": (True, ""),
    "tiered_tp_atr_regime": (False, "position"),
    "tiered_tp_atr_live_regime": (True, "live"),
    "tiered_tp_atr_live_regime_dynamic": (True, "live"),
}


def _load_close_registry():
    path = Path(__file__).resolve().parent / "registry.py"
    spec = importlib.util.spec_from_file_location("_close_registry_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_close_registry()


def _resting_position(case):
    p = case["position"]
    ctx = {
        "side": p["side"],
        "avg_cost": p["avg_cost"],
        "current_quantity": p["quantity"],
        "initial_quantity": p["initial_quantity"],
        "entry_atr": p["entry_atr"],
        "tp_model": "resting_limit",
    }
    if p["risk_anchor_price"]:
        ctx["risk_anchor_price"] = p["risk_anchor_price"]
    regime = p["regime_applied_label"] or p["regime"]
    if regime:
        ctx["regime"] = regime
    return ctx


def _resting_ladder(name, params, regime):
    if name in ("tiered_tp_atr", "tiered_tp_atr_live"):
        return importlib.import_module("tiered_tp_atr").ladder_for(params, True)
    return importlib.import_module("tiered_tp_atr_regime").regime_ladder_for(params, regime, True).get("tiers")


@pytest.mark.parametrize("case", _FIXTURE["ladder_rule"], ids=lambda c: c["id"])
def test_resting_ladder_rule_matches_fixture(registry, case):
    got = importlib.import_module("tiered_tp_atr").resting_tiers(case["tiers"])

    assert (None if got is None else [list(pair) for pair in got]) == case["want"]


@pytest.mark.parametrize("case", _FIXTURE["geometry"], ids=lambda c: c["id"])
def test_resting_tier_geometry_matches_fixture(registry, case):
    ref = _FIXTURE["ladders"][case["ladder"]]
    params = {**ref["params"], **case.get("params_override", {})}
    position = _resting_position(case)
    market = {k: v for k, v in case["market"].items() if v}
    want = case["want"]
    live_atr, regime_source = _GEOMETRY_MODE[ref["name"]]

    geometry = importlib.import_module("tiered_tp_atr").resolve_tp_tier_geometry(
        position, market, params, live_atr=live_atr, regime_source=regime_source,
    )
    ladder = _resting_ladder(ref["name"], params, geometry.regime)
    result = registry.evaluate(ref["name"], position, market, params)

    assert geometry.resting_limit
    assert geometry.anchor == pytest.approx(want["anchor"])
    assert geometry.atr == pytest.approx(want["atr"])
    assert geometry.regime == want["regime"]
    assert [fraction for _, fraction in ladder] == pytest.approx(want["fractions"])
    if want["tier_prices"] is not None:
        sign = -1 if position["side"] == "short" else 1
        prices = [geometry.anchor + sign * multiple * geometry.atr for multiple, _ in ladder]
        assert prices == pytest.approx(want["tier_prices"])
    assert result["close_fraction"] == pytest.approx(want["close_fraction"])
    assert result.get("tier_fill_price", 0.0) == pytest.approx(want["fill_price"])
