import os
import sys

import pytest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, ROOT)
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

from shared_tools.conftest import load_module

_SIMULATE_STRATEGY = load_module("_simulate_strategy_gate_test", os.path.join(ROOT, "shared_scripts", "simulate_strategy.py"))
_resolve_gate_on_failure = _SIMULATE_STRATEGY._resolve_gate_on_failure


@pytest.mark.parametrize("strategy_cfg,global_cfg,expected", [
    ({}, {}, "open"),
    ({}, {"gate_on_failure": "closed"}, "closed"),
    ({"regime_gate_on_failure": "open"}, {"gate_on_failure": "closed"}, "open"),
    ({"regime_gate_on_failure": "closed"}, {}, "closed"),
])
def test_resolves_gate_on_failure(strategy_cfg, global_cfg, expected):
    assert _resolve_gate_on_failure(strategy_cfg, global_cfg) == expected


@pytest.mark.parametrize("strategy_cfg,global_cfg", [
    ({"regime_gate_on_failure": "fail-closed"}, {}),
    ({}, {"gate_on_failure": "garbage"}),
    ({"regime_gate_on_failure": "closed"}, {"gate_on_failure": "garbage"}),
])
def test_unknown_value_rejected(strategy_cfg, global_cfg):
    with pytest.raises(ValueError, match="regime_gate_on_failure"):
        _resolve_gate_on_failure(strategy_cfg, global_cfg)
