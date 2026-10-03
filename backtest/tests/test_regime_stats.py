import os, sys
import numpy as np
import pytest
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_stats import rank_data, kruskal_h, benjamini_hochberg


def test_rank_data_averages_ties():
    np.testing.assert_allclose(rank_data(np.array([10, 20, 20, 40.0])), [1, 2.5, 2.5, 4])


@pytest.mark.parametrize("groups,expected", [
    pytest.param([[1.0, 2, 3], [4.0, 5, 6]], 27.0 / 7.0, id="no_ties"),
    pytest.param([[1.0, 1, 2], [2.0, 3, 3]], 10.0 / 3.0, id="tie_correction"),
])
def test_kruskal_h_reference_values(groups, expected):
    h = kruskal_h([np.array(g) for g in groups])
    assert abs(h - expected) < 1e-9


def test_benjamini_hochberg_basic():
    flags = benjamini_hochberg([0.001, 0.4, 0.6, 0.8], alpha=0.05)
    assert flags == [True, False, False, False]
