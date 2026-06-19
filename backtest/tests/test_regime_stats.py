# backtest/tests/test_regime_stats.py
import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_stats import rank_data, kruskal_h, benjamini_hochberg


def test_rank_data_averages_ties():
    # values [10, 20, 20, 40] -> ranks [1, 2.5, 2.5, 4]
    np.testing.assert_allclose(rank_data(np.array([10, 20, 20, 40.0])), [1, 2.5, 2.5, 4])


def test_kruskal_h_zero_when_identical():
    g = np.array([1.0, 2, 3])
    assert abs(kruskal_h([g, g.copy(), g.copy()])) < 1e-9


def test_kruskal_h_large_when_separated():
    a = np.array([1.0, 2, 3, 4]); b = np.array([100.0, 101, 102, 103])
    assert kruskal_h([a, b]) > 5.0  # strongly separated -> large H


def test_benjamini_hochberg_basic():
    # one clearly significant, rest null
    flags = benjamini_hochberg([0.001, 0.4, 0.6, 0.8], alpha=0.05)
    assert flags == [True, False, False, False]
