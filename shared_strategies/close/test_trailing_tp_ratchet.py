"""Tests for the trailing_tp_ratchet close evaluator (#844)."""

import importlib.util
import sys
from pathlib import Path

import pytest


def _load_evaluator():
    here = Path(__file__).resolve().parent
    if str(here) not in sys.path:
        sys.path.insert(0, str(here))  # so the module's `from _helpers import ...` resolves
    path = here / "trailing_tp_ratchet.py"
    spec = importlib.util.spec_from_file_location("_trailing_tp_ratchet_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def ev():
    return _load_evaluator()


def _pos(qty=1.0, initial=1.0, regime="trending_up", side="long", avg=100.0, atr=10.0):
    return {
        "side": side,
        "avg_cost": avg,
        "current_quantity": qty,
        "initial_quantity": initial,
        "entry_atr": atr,
        "regime": regime,
    }


REGIME_PARAMS = {
    "tp_tiers": {
        "trending_up": [
            {"atr_multiple": 1.5, "close_fraction": 0.0, "trailing_mult_after": 2.0},
            {"atr_multiple": 3.0, "close_fraction": 0.3, "tp_atr_fraction": 0.33},
        ],
        "ranging": [
            {"atr_multiple": 1.0, "close_fraction": 0.25, "trailing_mult_after": 1.5},
        ],
    }
}


def test_trail_only_rung_no_close(ev):
    # +1.6 ATR clears tier1 (close_fraction 0) → trail tightens, no scale-out.
    out = ev.evaluate(_pos(), {"mark_price": 116.0}, REGIME_PARAMS)
    assert out["close_fraction"] == 0.0
    assert out["post_tp_trailing_atr_mult"] == 2.0


def test_scale_out_and_tighten(ev):
    # +3.1 ATR clears tier2 → close 0.3 and trail = 0.33 * 3.0 ≈ 0.99 (tightest cleared).
    out = ev.evaluate(_pos(), {"mark_price": 131.0}, REGIME_PARAMS)
    assert out["close_fraction"] == pytest.approx(0.3)
    assert out["post_tp_trailing_atr_mult"] == pytest.approx(0.99)


def test_double_close_guard(ev):
    # Already scaled out 0.3 of initial (qty 0.7 of initial 1.0); re-hitting tier2
    # closes nothing more, but the trail still reports tightest cleared.
    out = ev.evaluate(_pos(qty=0.7, initial=1.0), {"mark_price": 131.0}, REGIME_PARAMS)
    assert out["close_fraction"] == 0.0
    assert out["post_tp_trailing_atr_mult"] == pytest.approx(0.99)


def test_not_hit_noop(ev):
    out = ev.evaluate(_pos(), {"mark_price": 105.0}, REGIME_PARAMS)
    assert out == {"close_fraction": 0.0, "reason": "noop:not_hit"}


def test_regime_selection(ev):
    # ranging table: +1.1 ATR clears its single tier (close 0.25, trail 1.5).
    out = ev.evaluate(_pos(regime="ranging"), {"mark_price": 111.0}, REGIME_PARAMS)
    assert out["close_fraction"] == pytest.approx(0.25)
    assert out["post_tp_trailing_atr_mult"] == 1.5


def test_unknown_regime_noop(ev):
    out = ev.evaluate(_pos(regime="trending_down"), {"mark_price": 131.0}, REGIME_PARAMS)
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:no_tiers"


def test_plain_list_form(ev):
    params = {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.25, "trailing_mult_after": 1.5}]}
    out = ev.evaluate(_pos(regime=""), {"mark_price": 111.0}, params)
    assert out["close_fraction"] == pytest.approx(0.25)
    assert out["post_tp_trailing_atr_mult"] == 1.5


def test_pure_trailing_all_zero(ev):
    # Every tier close_fraction 0 → never scales out; only the trail tightens.
    params = {"tp_tiers": [
        {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
        {"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
    ]}
    out = ev.evaluate(_pos(regime=""), {"mark_price": 125.0}, params)
    assert out["close_fraction"] == 0.0
    assert out["post_tp_trailing_atr_mult"] == 1.0  # tightest of the two cleared


def test_monotonic_tightest_among_cleared(ev):
    # Mis-ordered trails: higher tier looser. Evaluator returns the tightest.
    params = {"tp_tiers": [
        {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
        {"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 3.0},
    ]}
    out = ev.evaluate(_pos(regime=""), {"mark_price": 125.0}, params)
    assert out["post_tp_trailing_atr_mult"] == 1.0


def test_short_side(ev):
    params = {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5, "trailing_mult_after": 1.5}]}
    # short: profit when mark < avg. mark 88 = -1.2 ATR profit → tier clears.
    out = ev.evaluate(_pos(regime="", side="short"), {"mark_price": 88.0}, params)
    assert out["close_fraction"] == pytest.approx(0.5)
    assert out["post_tp_trailing_atr_mult"] == 1.5


def test_missing_entry_atr_noop(ev):
    out = ev.evaluate(_pos(atr=0.0, regime=""), {"mark_price": 131.0},
                      {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5}]})
    assert out == {"close_fraction": 0.0, "reason": "noop:missing_entry_atr"}


def test_tier_without_trail_spec(ev):
    # A tier with neither trailing_mult_after nor tp_atr_fraction: closes, no trail key.
    params = {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5}]}
    out = ev.evaluate(_pos(regime=""), {"mark_price": 115.0}, params)
    assert out["close_fraction"] == pytest.approx(0.5)
    assert "post_tp_trailing_atr_mult" not in out
