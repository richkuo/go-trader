"""#1300: tuner-path entry-gate failure policy resolution (simulate_strategy).

``simulate_strategy._resolve_gate_on_failure`` mirrors the live/backtest
``regime_gate_on_failure`` resolution: per-strategy field wins over the global
``regime.gate_on_failure``, else the ``"open"`` default. Both surfaces are
validated independently via ``normalize_regime_gate_on_failure`` (the SSoT), so
a garbage global value raises even when a valid per-strategy override would
otherwise short-circuit past it — the class of bug #1300's review flagged.

Not in the pytest testpaths (shared_scripts/test_*.py) — invoke explicitly:
  uv run --no-sync python -m pytest shared_scripts/test_simulate_strategy_gate_on_failure.py
"""
import os
import sys

import pytest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, ROOT)
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

from simulate_strategy import _resolve_gate_on_failure  # noqa: E402


def test_default_open_when_neither_set():
    assert _resolve_gate_on_failure({}, {}) == "open"


def test_global_applies_when_no_per_strategy():
    assert _resolve_gate_on_failure({}, {"gate_on_failure": "closed"}) == "closed"


def test_per_strategy_wins_over_global():
    assert _resolve_gate_on_failure(
        {"regime_gate_on_failure": "open"}, {"gate_on_failure": "closed"}
    ) == "open"


def test_per_strategy_applies_with_no_global():
    assert _resolve_gate_on_failure(
        {"regime_gate_on_failure": "closed"}, {}
    ) == "closed"


def test_unknown_per_strategy_rejected():
    with pytest.raises(ValueError, match="regime_gate_on_failure"):
        _resolve_gate_on_failure({"regime_gate_on_failure": "fail-closed"}, {})


def test_unknown_global_rejected_with_no_override():
    with pytest.raises(ValueError, match="regime_gate_on_failure"):
        _resolve_gate_on_failure({}, {"gate_on_failure": "garbage"})


def test_garbage_global_rejected_even_with_valid_per_strategy_override():
    """The core #1300 regression: a valid per-strategy override must NOT let a
    garbage global value slip through unvalidated. The old `or` chain validated
    only the winning value, so the bad global silently passed."""
    with pytest.raises(ValueError, match="regime_gate_on_failure"):
        _resolve_gate_on_failure(
            {"regime_gate_on_failure": "closed"},
            {"gate_on_failure": "garbage"},
        )
