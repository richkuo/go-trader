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


def _directional_position(side: str, regime: str) -> dict:
    return {
        "side": side,
        "avg_cost": 100,
        "current_quantity": 1,
        "initial_quantity": 1,
        "entry_atr": 10,
        "regime": regime,
    }


def _tier(multiple: float, fraction: float) -> list:
    return [{"atr_multiple": multiple, "close_fraction": fraction, "trailing_mult_after": 1.5}]


def test_regime_directional_sub_label_falls_back_to_bare_block(ratchet):
    params = {"tp_tiers": {"ranging_directional": _tier(1.0, 0.5)}}
    out = ratchet.evaluate_regime(
        _directional_position("long", "ranging_directional_up"), {"mark_price": 115}, params
    )
    assert out["close_fraction"] == 0.5
    assert out["reason"] == "trailing_tp_ratchet_regime:ranging_directional_up:1"


def test_regime_directional_exact_sub_label_wins_over_bare_block(ratchet):
    params = {
        "tp_tiers": {
            "ranging_directional": _tier(1.0, 0.5),
            "ranging_directional_down": _tier(1.2, 0.25),
        }
    }
    out = ratchet.evaluate_regime(
        _directional_position("short", "ranging_directional_down"), {"mark_price": 85}, params
    )
    assert out["close_fraction"] == 0.25
    assert out["reason"] == "trailing_tp_ratchet_regime:ranging_directional_down:1.2"


def test_regime_directional_sub_label_without_either_key_is_noop(ratchet):
    params = {"tp_tiers": {"trending_up": _tier(1.0, 0.5)}}
    out = ratchet.evaluate_regime(
        _directional_position("long", "ranging_directional_up"), {"mark_price": 115}, params
    )
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:tier_resolution_failed"
