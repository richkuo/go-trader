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


def test_resolves_gate_on_failure_or_rejects_unknown_values(subtests):
    cases = [
        ({}, {}, "open"),
        ({}, {"gate_on_failure": "closed"}, "closed"),
        ({"regime_gate_on_failure": "open"}, {"gate_on_failure": "closed"}, "open"),
        ({"regime_gate_on_failure": "closed"}, {}, "closed"),
    ]
    for strategy_cfg, global_cfg, expected in cases:
        with subtests.test(case="resolves", strategy_cfg=strategy_cfg, global_cfg=global_cfg):
            assert _resolve_gate_on_failure(strategy_cfg, global_cfg) == expected, (strategy_cfg, global_cfg)

    rejected = [
        ({"regime_gate_on_failure": "fail-closed"}, {}),
        ({}, {"gate_on_failure": "garbage"}),
        ({"regime_gate_on_failure": "closed"}, {"gate_on_failure": "garbage"}),
    ]
    for strategy_cfg, global_cfg in rejected:
        with subtests.test(case="unknown_value_rejected", strategy_cfg=strategy_cfg, global_cfg=global_cfg), \
                pytest.raises(ValueError, match="regime_gate_on_failure"):
            _resolve_gate_on_failure(strategy_cfg, global_cfg)
