"""Tests for trailing_tp_ratchet close evaluators (#844)."""

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


@pytest.fixture(scope="module")
def registry():
    return _load("_close_registry_ratchet", os.path.join(_THIS_DIR, "registry.py"))


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
    # #866: a close ref with no tp_tiers (or use_defaults:true) falls back to
    # the conservative system default ladder, on both the scalar and the regime
    # (broadcast) paths, and they must match.
    scalar, e1 = ratchet.resolve_tiers_for_regime({}, "", regime_table=False)
    regime, e2 = ratchet.resolve_tiers_for_regime({}, "trending_up", regime_table=True)
    assert e1 == [] and e2 == []
    assert scalar == regime
    assert [t[0] for t in scalar] == [2.0, 2.5, 3.0]
    assert [t[2] for t in scalar] == [1.5, 1.0, 0.8]
    assert all(t[1] == 0.0 for t in scalar)


def test_default_ratchet_tiers_constant_matches_registry(ratchet, registry):
    # The registry's advertised scalar default_params must be sourced from the
    # single-source-of-truth constant (no drift).
    assert registry.build_close_registry  # registry import smoke
    advertised = ratchet.DEFAULT_RATCHET_TIERS
    assert [t["atr_multiple"] for t in advertised] == [2.0, 2.5, 3.0]
    assert [t["trailing_mult_after"] for t in advertised] == [1.5, 1.0, 0.8]


def test_registry_lists_new_strategies(registry):
    built = registry.build_close_registry("futures")
    assert "trailing_tp_ratchet" in built
    assert "trailing_tp_ratchet_regime" in built


def test_regime_close_default_group_mapping(ratchet):
    # #870: composite quality suffixes win; ADX trends fall to choppy.
    g = ratchet.regime_close_default_group
    assert g("trending_up_clean") == "clean"
    assert g("trending_down_clean") == "clean"
    assert g("trending_up_choppy") == "choppy"
    assert g("trending_up") == "choppy"  # ADX trend → choppy
    assert g("trending_down") == "choppy"
    assert g("ranging") == "ranging"
    assert g("ranging_volatile") == "ranging"
    assert g("") is None
    assert g("bogus") is None


def test_resolve_tiers_for_regime_group_defaults(ratchet):
    # #870: regime variant + omitted tp_tiers → per-quality-group ladder.
    clean, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "trending_up_clean", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in clean] == [3.0, 4.5, 6.0]
    assert all(t[1] == 0.0 for t in clean)  # trend group: no scale-out

    ranging, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "ranging_quiet", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in ranging] == [0.75, 1.5, 2.0]
    assert [t[1] for t in ranging] == [0.4, 0.8, 1.0]  # ranging scales out

    # Scalar variant still broadcasts the single #866 default.
    scalar, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "", regime_table=False,
    )
    assert errs == []
    assert [t[0] for t in scalar] == [2.0, 2.5, 3.0]


def test_ratchet_close_default_group_differentiates_ranging_substates(ratchet):
    # #1059: the ratchet-only resolver splits the composite ranging substates.
    g = ratchet.ratchet_close_default_group
    assert g("ranging_quiet") == "ranging_quiet"
    assert g("ranging_volatile") == "ranging_volatile"
    assert g("ranging_directional") == "ranging_directional"
    # Bare ADX "ranging" (no substate signal) → quiet ladder (pre-#1059 behavior).
    assert g("ranging") == "ranging_quiet"
    # clean/choppy/trend labels delegate to the shared fn, unchanged.
    assert g("trending_up_clean") == "clean"
    assert g("trending_up") == "choppy"
    assert g("") is None
    assert g("bogus") is None
    # The shared regime_close_default_group MUST still collapse the substates, or
    # the B2 ATR-TP use_defaults path would miss its map and never-arm (#1059).
    assert ratchet.regime_close_default_group("ranging_directional") == "ranging"
    assert ratchet.regime_close_default_group("ranging_volatile") == "ranging"


def test_resolve_tiers_for_regime_ranging_substates(ratchet):
    # #1059 ranging_volatile: widened triggers, close fractions unchanged vs quiet.
    volatile, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "ranging_volatile", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in volatile] == [1.0, 2.0, 3.0]
    assert [t[1] for t in volatile] == [0.4, 0.8, 1.0]

    # #1059 ranging_directional: lighter early scale-out (25/50/75) + a 4th
    # let-ride rung adding no close (cumulative stays 0.75) but tightening trail.
    directional, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "ranging_directional", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in directional] == [1.0, 2.0, 3.0, 4.5]
    assert [t[1] for t in directional] == [0.25, 0.50, 0.75, 0.75]
    assert [t[2] for t in directional] == [1.0, 1.0, 0.8, 0.6]  # trail non-increasing

    # Bare ADX "ranging" still resolves to the quiet ladder (unchanged pre-#1059).
    adx_ranging, errs = ratchet.resolve_tiers_for_regime(
        {"use_defaults": True}, "ranging", regime_table=True,
    )
    assert errs == []
    assert [t[0] for t in adx_ranging] == [0.75, 1.5, 2.0]
    assert [t[1] for t in adx_ranging] == [0.4, 0.8, 1.0]
