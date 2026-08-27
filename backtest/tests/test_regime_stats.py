import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_stats import rank_data, kruskal_h, benjamini_hochberg


def test_rank_data_averages_ties():
    np.testing.assert_allclose(rank_data(np.array([10, 20, 20, 40.0])), [1, 2.5, 2.5, 4])


def test_kruskal_h_zero_when_identical():
    g = np.array([1.0, 2, 3])
    assert abs(kruskal_h([g, g.copy(), g.copy()])) < 1e-9


def test_kruskal_h_large_when_separated():
    a = np.array([1.0, 2, 3, 4]); b = np.array([100.0, 101, 102, 103])
    assert kruskal_h([a, b]) > 5.0


def test_kruskal_h_reference_value_no_ties():
    h = kruskal_h([np.array([1.0, 2, 3]), np.array([4.0, 5, 6])])
    assert abs(h - 27.0 / 7.0) < 1e-9


def test_kruskal_h_reference_value_with_tie_correction():
    h = kruskal_h([np.array([1.0, 1, 2]), np.array([2.0, 3, 3])])
    assert abs(h - 10.0 / 3.0) < 1e-9


def test_benjamini_hochberg_basic():
    flags = benjamini_hochberg([0.001, 0.4, 0.6, 0.8], alpha=0.05)
    assert flags == [True, False, False, False]
