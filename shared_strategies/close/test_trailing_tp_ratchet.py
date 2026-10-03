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
