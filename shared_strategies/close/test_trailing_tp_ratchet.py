import importlib.util
import sys
from pathlib import Path

import pytest

_CLOSE_DIR = Path(__file__).resolve().parent
if str(_CLOSE_DIR) not in sys.path:
    sys.path.insert(0, str(_CLOSE_DIR))


def _load(name: str, filename: str):
    path = _CLOSE_DIR / filename
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load("_close_registry_ratchet_test", "registry.py")


@pytest.fixture(scope="module")
def evaluator():
    return _load("_trailing_tp_ratchet_under_test", "trailing_tp_ratchet.py")


def _pos(**over):
    base = {
        "side": "long",
        "avg_cost": 100.0,
        "current_quantity": 1.0,
        "initial_quantity": 1.0,
        "entry_atr": 10.0,
    }
    base.update(over)
    return base


# ---------------------------------------------------------------------------
# Tier parsing helpers
# ---------------------------------------------------------------------------

def test_parse_ratchet_tiers_defaults_close_fraction_zero(evaluator):
    tiers = evaluator.parse_ratchet_tiers(
        [{"atr_multiple": 2.0, "trailing_mult_after": 1.0},
         {"atr_multiple": 1.0, "close_fraction": 0.25, "trailing_mult_after": 1.5}]
    )
    # Sorted ascending by trigger; missing close_fraction -> 0.0.
    assert tiers == [(1.0, 0.25), (2.0, 0.0)]


def test_parse_ratchet_tiers_skips_nonpositive_and_garbage(evaluator):
    tiers = evaluator.parse_ratchet_tiers(
        [{"atr_multiple": 0.0}, {"atr_multiple": -1.0}, "junk",
         {"atr_multiple": 1.5, "close_fraction": 2.0}]  # fraction clamped to 1.0
    )
    assert tiers == [(1.5, 1.0)]


def test_ratchet_tiers_for_regime_list_vs_dict(evaluator):
    plain = [{"atr_multiple": 1.0}]
    assert evaluator.ratchet_tiers_for_regime({"tp_tiers": plain}, "anything") == plain
    table = {"trending_up": plain, "ranging": [{"atr_multiple": 2.0}]}
    assert evaluator.ratchet_tiers_for_regime({"tp_tiers": table}, "trending_up") == plain
    # Dict form with absent / empty regime -> no tiers.
    assert evaluator.ratchet_tiers_for_regime({"tp_tiers": table}, "missing") is None
    assert evaluator.ratchet_tiers_for_regime({"tp_tiers": table}, "") is None


# ---------------------------------------------------------------------------
# Evaluator behaviour (via the registry, end-to-end)
# ---------------------------------------------------------------------------

def test_pure_trailing_rung_takes_no_close(registry):
    """All-zero close_fraction => trail-only; the trailing stop owns the exit."""
    params = {"tp_tiers": [{"atr_multiple": 1.0, "trailing_mult_after": 2.0},
                           {"atr_multiple": 2.0, "trailing_mult_after": 1.0}]}
    res = registry.evaluate("trailing_tp_ratchet", _pos(), {"mark_price": 130.0}, params)  # +3 ATR
    assert res["close_fraction"] == 0.0
    assert res["reason"].startswith("noop:trail_only:default:2")


def test_scale_out_rung_closes_cumulative_fraction(registry):
    params = {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
                           {"atr_multiple": 3.0, "close_fraction": 0.3, "tp_atr_fraction": 0.33}]}
    # +1.5 ATR clears only the trail-only rung -> no close.
    trail = registry.evaluate("trailing_tp_ratchet", _pos(), {"mark_price": 115.0}, params)
    assert trail["close_fraction"] == 0.0
    # +3 ATR clears the scale-out rung -> close 30%.
    hit = registry.evaluate("trailing_tp_ratchet", _pos(), {"mark_price": 130.0}, params)
    assert hit["close_fraction"] == pytest.approx(0.3)
    assert hit["reason"] == "trailing_tp_ratchet:default:3"


def test_double_close_guard(registry):
    params = {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5, "trailing_mult_after": 1.0}]}
    # 50% already closed at this tier -> nothing more to take.
    already = registry.evaluate(
        "trailing_tp_ratchet",
        _pos(current_quantity=0.5, initial_quantity=1.0),
        {"mark_price": 130.0},
        params,
    )
    assert already["close_fraction"] == 0.0


def test_trail_only_rung_above_scale_out_uses_max(registry):
    """A trail-only rung above a scale-out rung must NOT un-close (max-over-cleared)."""
    params = {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.3, "trailing_mult_after": 1.5},
                           {"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 0.75}]}
    res = registry.evaluate("trailing_tp_ratchet", _pos(), {"mark_price": 120.0}, params)  # +2 ATR clears both
    assert res["close_fraction"] == pytest.approx(0.3)
    assert res["reason"] == "trailing_tp_ratchet:default:2"


def test_short_side(registry):
    params = {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5, "trailing_mult_after": 1.0}]}
    res = registry.evaluate(
        "trailing_tp_ratchet",
        _pos(side="short"),
        {"mark_price": 80.0},  # -2 ATR profit for a short
        params,
    )
    assert res["close_fraction"] == pytest.approx(0.5)


def test_regime_form_selects_table(registry):
    params = {"tp_tiers": {
        "trending_up": [{"atr_multiple": 3.0, "close_fraction": 0.3, "trailing_mult_after": 1.0}],
        "ranging": [{"atr_multiple": 1.0, "close_fraction": 0.5, "trailing_mult_after": 1.5}],
    }}
    up = registry.evaluate("trailing_tp_ratchet_regime", _pos(regime="trending_up"), {"mark_price": 130.0}, params)
    assert up["close_fraction"] == pytest.approx(0.3)
    assert up["reason"] == "trailing_tp_ratchet:trending_up:3"
    rng = registry.evaluate("trailing_tp_ratchet_regime", _pos(regime="ranging"), {"mark_price": 130.0}, params)
    assert rng["close_fraction"] == pytest.approx(0.5)


def test_regime_form_missing_regime_noop(registry):
    params = {"tp_tiers": {"trending_up": [{"atr_multiple": 1.0, "close_fraction": 0.5}]}}
    res = registry.evaluate("trailing_tp_ratchet_regime", _pos(), {"mark_price": 130.0}, params)
    assert res == {"close_fraction": 0.0, "reason": "noop:no_tiers_for_regime"}


def test_missing_entry_atr_noop(registry):
    params = {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5}]}
    res = registry.evaluate("trailing_tp_ratchet", _pos(entry_atr=0.0), {"mark_price": 130.0}, params)
    assert res == {"close_fraction": 0.0, "reason": "noop:missing_entry_atr"}
