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


def test_kruskal_h_reference_value_no_ties():
    # Two perfectly-ordered tie-free groups: [1,2,3] vs [4,5,6]. Ranks are 1..6, so
    # R1=6, R2=15; H = 12/(N(N+1)) * (R1^2/n1 + R2^2/n2) - 3(N+1) with N=6
    #   = (12/42) * (36/3 + 225/3) - 21 = (2/7)*87 - 21 = 27/7. No ties -> correction c=1.
    h = kruskal_h([np.array([1.0, 2, 3]), np.array([4.0, 5, 6])])
    assert abs(h - 27.0 / 7.0) < 1e-9


def test_kruskal_h_reference_value_with_tie_correction():
    # Tied groups [1,1,2] vs [2,3,3]. Sorted values 1,1,2,2,3,3 -> avg ranks
    # 1.5,1.5,3.5,3.5,5.5,5.5; R1=6.5, R2=14.5 -> H_uncorrected = 64/21.
    # Tie correction c = 1 - sum(t^3 - t)/(N^3 - N) = 1 - 18/210 = 32/35.
    # Corrected H = (64/21)/(32/35) = 10/3 exactly -> pins the tie-correction branch.
    h = kruskal_h([np.array([1.0, 1, 2]), np.array([2.0, 3, 3])])
    assert abs(h - 10.0 / 3.0) < 1e-9


def test_benjamini_hochberg_basic():
    # one clearly significant, rest null
    flags = benjamini_hochberg([0.001, 0.4, 0.6, 0.8], alpha=0.05)
    assert flags == [True, False, False, False]
