import importlib.util
from pathlib import Path

import pytest


def _load_close_registry():
    path = Path(__file__).resolve().parent / "registry.py"
    spec = importlib.util.spec_from_file_location("_close_registry_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_close_registry()


def test_tiered_tp_pct_closes_only_unfilled_tier_amount(registry):
    first = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1},
        {"mark_price": 102},
        {},
    )
    already_taken = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 0.5, "initial_quantity": 1},
        {"mark_price": 102},
        {},
    )
    final = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 0.5, "initial_quantity": 1},
        {"mark_price": 104},
        {},
    )

    assert first == {"close_fraction": 0.5, "reason": "tiered_tp_pct:0.02"}
    assert already_taken == {"close_fraction": 0.0, "reason": "noop:already_taken"}
    assert final == {"close_fraction": 1.0, "reason": "tiered_tp_pct:0.04"}


_LIVE_POS = {"side": "long", "avg_cost": 100, "current_quantity": 1,
             "initial_quantity": 1, "entry_atr": 2}


@pytest.mark.parametrize("position,market,params,expected", [
    (_LIVE_POS, {"mark_price": 105, "atr": 3}, {},
     {"close_fraction": 0.4, "reason": "tiered_tp_atr_live:live:1.5"}),
    (_LIVE_POS, {"mark_price": 110}, {},
     {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:entry_fallback:5"}),
    (_LIVE_POS, {"mark_price": 103, "atr": 0}, {},
     {"close_fraction": 0.4, "reason": "tiered_tp_atr_live:entry_fallback:1.5"}),
    ({"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1},
     {"mark_price": 104}, {},
     {"close_fraction": 0.0, "reason": "noop:missing_atr"}),
    (_LIVE_POS, {"mark_price": 110, "atr": 10}, {"atr_source": "entry"},
     {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:entry:5"}),
    ({"side": "short", "avg_cost": 100, "current_quantity": 1,
      "initial_quantity": 1, "entry_atr": 5},
     {"mark_price": 90, "atr": 2}, {},
     {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:live:5"}),
    ({**_LIVE_POS, "risk_anchor_price": 99, "tp_model": "resting_limit"},
     {"mark_price": 105, "atr": 3}, {},
     {"close_fraction": 0.8, "reason": "tiered_tp_atr_live:entry:3", "tier_fill_price": pytest.approx(103.5)}),
])
def test_tiered_tp_atr_live_atr_source_resolution(registry, position, market, params, expected):
    assert registry.evaluate("tiered_tp_atr_live", position, market, params) == expected


@pytest.mark.parametrize("initial,current", [
    (7.6252, 0.3877),
    (4.4594, 1.51),
    (0.1234, 0.0247),
])
def test_final_tier_snaps_to_a_full_close_despite_float_noise(registry, initial, current):
    result = registry.evaluate(
        "tiered_tp_atr",
        {"side": "long", "avg_cost": 100.0, "current_quantity": current,
         "initial_quantity": initial, "entry_atr": 1.0},
        {"mark_price": 200.0},
        {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.4},
                      {"atr_multiple": 2.0, "close_fraction": 0.8},
                      {"atr_multiple": 3.0, "close_fraction": 1.0}]},
    )
    assert result["close_fraction"] == 1.0
