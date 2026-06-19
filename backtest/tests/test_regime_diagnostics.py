# backtest/tests/test_regime_diagnostics.py
import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_diagnostics import forward_returns, coverage, separation, stability


def test_forward_returns_horizon():
    close = np.array([100.0, 110, 121])
    fr = forward_returns(close, 1)
    np.testing.assert_allclose(fr[:2], [0.10, 0.10])
    assert np.isnan(fr[-1])


def test_coverage_counts():
    labels = np.array(["a", "a", "b", "a"])
    cov = coverage(labels)
    assert cov["a"]["count"] == 3 and abs(cov["a"]["pct"] - 0.75) < 1e-9


def test_separation_directional():
    labels = np.array(["up"] * 50 + ["down"] * 50)
    fwd = np.concatenate([np.full(50, 0.02), np.full(50, -0.02)])
    sep = separation(labels, fwd)
    assert sep["per_state"]["up"]["mean"] > 0 > sep["per_state"]["down"]["mean"]
    assert sep["kruskal_h"] > 5.0


def test_stability_transition_rate():
    labels = np.array(["a", "a", "b", "b", "a"])
    st = stability(labels)
    assert abs(st["transition_rate"] - 0.5) < 1e-9  # 2 flips over 4 transitions
    assert st["flips"] == 2
