"""Tests for the regime-aware ATR resolver and close evaluators (#733)."""

from __future__ import annotations

import importlib.util
import os
import sys

import pytest

_THIS_DIR = os.path.dirname(__file__)
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)


def _load(module_name: str, path: str):
    spec = importlib.util.spec_from_file_location(module_name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def regime_atr():
    return _load("_regime_atr_under_test", os.path.join(_THIS_DIR, "regime_atr.py"))


@pytest.fixture(scope="module")
def tiered_regime():
    _load("_regime_atr_under_test", os.path.join(_THIS_DIR, "regime_atr.py"))
    _load("tiered_tp_atr_regime", os.path.join(_THIS_DIR, "tiered_tp_atr_regime.py"))
    return sys.modules["tiered_tp_atr_regime"]


def test_use_defaults_expands(regime_atr):
    block, errs = regime_atr.parse_regime_atr_block(
        {"use_defaults": True}, "stop_loss_atr_regime", regime_atr.SURFACE_STOP_LOSS
    )
    assert errs == []
    assert block.use_defaults
    assert set(block.trend_regime.keys()) == set(regime_atr.CANONICAL_TREND_REGIME_LABELS)
    assert block.trend_regime["ranging"].atr == 1.5
    assert block.trend_regime["trending_up"].atr == 2.0


def test_rejects_bare_label_keys(regime_atr):
    raw = {
        "trending_up": {"atr": 2.0},
        "trending_down": {"atr": 2.0},
        "ranging": {"atr": 1.5},
    }
    _, errs = regime_atr.parse_regime_atr_block(
        raw, "stop_loss_atr_regime", regime_atr.SURFACE_STOP_LOSS
    )
    assert any("trend_regime" in e or "unknown key" in e for e in errs)


def test_requires_exhaustive_labels(regime_atr):
    raw = {
        regime_atr.REGIME_CLASSIFIER_KEY: {
            "trending_up": {"atr": 2.0},
            "ranging": {"atr": 1.5},
        }
    }
    _, errs = regime_atr.parse_regime_atr_block(
        raw, "stop_loss_atr_regime", regime_atr.SURFACE_STOP_LOSS
    )
    assert any("missing required regime labels" in e and "trending_down" in e for e in errs)


def test_close_fraction_rejected_on_stop_loss_surface(regime_atr):
    raw = {
        regime_atr.REGIME_CLASSIFIER_KEY: {
            "trending_up": {"atr": 2.0, "close_fraction": 0.5},
            "trending_down": {"atr": 2.0},
            "ranging": {"atr": 1.5},
        }
    }
    _, errs = regime_atr.parse_regime_atr_block(
        raw, "stop_loss_atr_regime", regime_atr.SURFACE_STOP_LOSS
    )
    assert any("close_fraction" in e for e in errs)


def test_use_defaults_and_explicit_mutex(regime_atr):
    raw = {
        "use_defaults": True,
        regime_atr.REGIME_CLASSIFIER_KEY: {
            "trending_up": {"atr": 2.0},
            "trending_down": {"atr": 2.0},
            "ranging": {"atr": 1.5},
        },
    }
    _, errs = regime_atr.parse_regime_atr_block(
        raw, "stop_loss_atr_regime", regime_atr.SURFACE_STOP_LOSS
    )
    assert any("use_defaults is all-or-nothing" in e for e in errs)


def test_tier_mixed_shape_rejected(regime_atr):
    raw_tiers = [
        {
            regime_atr.REGIME_CLASSIFIER_KEY: {
                "trending_up": {"atr": 3.0, "close_fraction": 0.4},
                "trending_down": {"atr": 3.0, "close_fraction": 0.4},
                "ranging": {"atr": 1.5, "close_fraction": 0.6},
            },
            "close_fraction": 0.5,
        }
    ]
    _, errs = regime_atr.parse_regime_tp_tiers(raw_tiers, "tiered_tp_atr_regime", False)
    assert any("pick one shape per tier" in e for e in errs)


def test_evaluate_use_defaults_frozen(tiered_regime):
    # tier1 ranging: atr=1.5, cf=0.5 — should fire at +1.5×ATR profit
    position = {
        "side": "long",
        "avg_cost": 100.0,
        "current_quantity": 1.0,
        "initial_quantity": 1.0,
        "entry_atr": 2.0,
        "regime": "ranging",
    }
    market = {"mark_price": 103.0}  # +3.0 profit = 1.5× ATR
    result = tiered_regime.evaluate(position, market, {"use_defaults": True})
    assert result["close_fraction"] > 0
    assert "ranging" in result["reason"]


def test_evaluate_missing_regime_noop(tiered_regime):
    position = {
        "side": "long",
        "avg_cost": 100.0,
        "current_quantity": 1.0,
        "initial_quantity": 1.0,
        "entry_atr": 2.0,
        # no regime stamped
    }
    market = {"mark_price": 105.0}
    result = tiered_regime.evaluate(position, market, {"use_defaults": True})
    assert result["close_fraction"] == 0.0
    assert "missing_position_regime" in result["reason"]
