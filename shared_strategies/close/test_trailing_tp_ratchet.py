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


def test_registry_lists_new_strategies(registry):
    built = registry.build_close_registry("futures")
    assert "trailing_tp_ratchet" in built
    assert "trailing_tp_ratchet_regime" in built
