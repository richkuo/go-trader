
from __future__ import annotations

import importlib.util
import os
import sys

import pytest

_THIS_DIR = os.path.dirname(__file__)
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)


def _load(name: str, path: str):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def ratchet():
    return _load("_ratchet_under_test", os.path.join(_THIS_DIR, "trailing_tp_ratchet.py"))


def test_trail_only_tier_returns_zero_close_fraction(ratchet):
    params = {
        "tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
            {"atr_multiple": 2.0, "close_fraction": 0.3, "trailing_mult_after": 1.0},
        ]
    }
    pos = {
        "side": "long",
        "avg_cost": 100,
        "current_quantity": 1,
        "initial_quantity": 1,
        "entry_atr": 10,
    }
    hit0 = ratchet.evaluate_scalar(pos, {"mark_price": 110}, params)
    assert hit0["close_fraction"] == 0.0
    hit1 = ratchet.evaluate_scalar(pos, {"mark_price": 125}, params)
    assert hit1["close_fraction"] == pytest.approx(0.3)


def test_double_close_guard(ratchet):
    params = {
        "tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.5, "trailing_mult_after": 1.5},
        ]
    }
    pos = {
        "side": "long",
        "avg_cost": 100,
        "current_quantity": 0.5,
        "initial_quantity": 1,
        "entry_atr": 10,
    }
    out = ratchet.evaluate_scalar(pos, {"mark_price": 115}, params)
    assert out["close_fraction"] == 0.0
    assert "already_taken" in out["reason"]


def test_tp_atr_fraction_trail_spec(ratchet):
    tier = {"atr_multiple": 2.0, "close_fraction": 0.0, "tp_atr_fraction": 0.5}
    assert ratchet.resolve_trailing_mult_after(tier, 2.0) == pytest.approx(1.0)


def test_rejects_decreasing_cumulative_close_fraction(ratchet):
    params = {
        "tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.4, "trailing_mult_after": 2.0},
            {"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
        ]
    }
    tiers, errs = ratchet.resolve_tiers_for_regime(params, "", regime_table=False)
    assert tiers == []
    assert any("close_fraction" in e for e in errs)


def test_regime_table_resolution(ratchet):
    params = {
        "tp_tiers": {
            "trending_up": [
                {"atr_multiple": 1.0, "close_fraction": 0.25, "trailing_mult_after": 1.5},
            ],
            "ranging": [
                {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
            ],
        }
    }
    tiers, errs = ratchet.resolve_tiers_for_regime(
        params, "ranging", regime_table=True,
    )
    assert errs == []
    assert len(tiers) == 1
    assert tiers[0][2] == 2.0


def test_omitted_tp_tiers_resolves_system_default(ratchet):
    scalar, e1 = ratchet.resolve_tiers_for_regime({}, "", regime_table=False)
    regime, e2 = ratchet.resolve_tiers_for_regime({}, "trending_up", regime_table=True)
    assert e1 == [] and e2 == []
    assert scalar == regime
    assert [t[0] for t in scalar] == [2.0, 2.5, 3.0]
    assert [t[2] for t in scalar] == [1.5, 1.0, 0.8]
    assert all(t[1] == 0.0 for t in scalar)


def test_regime_close_default_group_mapping(ratchet):
    g = ratchet.regime_close_default_group
    assert g("trending_up_clean") == "clean"
    assert g("trending_down_clean") == "clean"
    assert g("trending_up_choppy") == "choppy"
    assert g("trending_up") == "choppy"
    assert g("trending_down") == "choppy"
    assert g("ranging") == "ranging"
    assert g("ranging_volatile") == "ranging"
    assert g("") is None
    assert g("bogus") is None


def test_resolve_tiers_for_regime_group_defaults(ratchet):
    clean, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "trending_up_clean", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in clean] == [3.0, 4.5, 6.0]
    assert all(t[1] == 0.0 for t in clean)

    ranging, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "ranging_quiet", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in ranging] == [0.75, 1.5, 2.0]
    assert [t[1] for t in ranging] == [0.4, 0.8, 1.0]

    scalar, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "", regime_table=False,
    )
    assert errs == []
    assert [t[0] for t in scalar] == [2.0, 2.5, 3.0]


def test_ratchet_close_default_group_differentiates_ranging_substates(ratchet):
    g = ratchet.ratchet_close_default_group
    assert g("ranging_quiet") == "ranging_quiet"
    assert g("ranging_volatile") == "ranging_volatile"
    assert g("ranging_directional") == "ranging_directional"
    assert g("ranging_directional_up") == "ranging_directional"
    assert g("ranging_directional_down") == "ranging_directional"
    assert g("ranging") == "ranging_quiet"
    assert g("trending_up_clean") == "clean"
    assert g("trending_up") == "choppy"
    assert g("") is None
    assert g("bogus") is None
    assert ratchet.regime_close_default_group("ranging_directional") == "ranging"
    assert ratchet.regime_close_default_group("ranging_volatile") == "ranging"


def test_resolve_tiers_for_regime_ranging_substates(ratchet):
    volatile, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "ranging_volatile", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in volatile] == [1.0, 2.0, 3.0]
    assert [t[1] for t in volatile] == [0.4, 0.8, 1.0]

    directional, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "ranging_directional", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in directional] == [1.0, 2.0, 3.0, 4.5]
    assert [t[1] for t in directional] == [0.25, 0.50, 0.75, 0.75]
    assert [t[2] for t in directional] == [1.0, 1.0, 0.8, 0.6]

    for label in ("ranging_directional_up", "ranging_directional_down"):
        tiers, errs = ratchet.resolve_tiers_for_regime(
            {"use_defaults": True}, label, regime_table=True,
        )
        assert errs == []
        assert tiers == directional, f"{label} must mirror the ranging_directional ladder"

    adx_ranging, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "ranging", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in adx_ranging] == [0.75, 1.5, 2.0]
    assert [t[1] for t in adx_ranging] == [0.4, 0.8, 1.0]
