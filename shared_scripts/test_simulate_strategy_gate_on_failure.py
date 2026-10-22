import contextlib
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


@contextlib.contextmanager
def _case(*label):
    try:
        yield
    except (Exception, pytest.fail.Exception) as exc:
        raise AssertionError(f"case {label!r} failed: {exc}") from exc


def test_resolves_gate_on_failure():
    cases = [
        ({}, {}, "open"),
        ({}, {"gate_on_failure": "closed"}, "closed"),
        ({"regime_gate_on_failure": "open"}, {"gate_on_failure": "closed"}, "open"),
        ({"regime_gate_on_failure": "closed"}, {}, "closed"),
    ]
    for strategy_cfg, global_cfg, expected in cases:
        assert _resolve_gate_on_failure(strategy_cfg, global_cfg) == expected, (strategy_cfg, global_cfg)


def test_unknown_value_rejected():
    cases = [
        ({"regime_gate_on_failure": "fail-closed"}, {}),
        ({}, {"gate_on_failure": "garbage"}),
        ({"regime_gate_on_failure": "closed"}, {"gate_on_failure": "garbage"}),
    ]
    for strategy_cfg, global_cfg in cases:
        with _case(strategy_cfg, global_cfg), pytest.raises(ValueError, match="regime_gate_on_failure"):
            _resolve_gate_on_failure(strategy_cfg, global_cfg)
