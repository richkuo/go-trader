# backtest/tests/test_regime_hmm.py
import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_hmm import fit_label_anchored_hmm, stationary_distribution

STATES = ["s0", "s1"]


def test_stationary_distribution_sums_to_one():
    A = np.array([[0.9, 0.1], [0.2, 0.8]])
    pi = stationary_distribution(A)
    assert abs(pi.sum() - 1.0) < 1e-9 and (pi > 0).all()
    np.testing.assert_allclose(pi @ A, pi, atol=1e-9)  # stationary


def test_fit_shapes_and_determinism():
    rng = np.random.default_rng(0)
    feats = np.vstack([rng.normal(0, 1, (80, 4)), rng.normal(4, 1, (80, 4))])
    labels = np.array(["s0"] * 80 + ["s1"] * 80, dtype=object)
    m1 = fit_label_anchored_hmm(feats, labels, STATES, filter_window=16)
    m2 = fit_label_anchored_hmm(feats, labels, STATES, filter_window=16)
    assert m1["states"] == STATES
    assert len(m1["emissions"]) == 2
    assert np.array(m1["transition"]).shape == (2, 2)
    assert m1["emissions"][0]["mean"] == m2["emissions"][0]["mean"]  # deterministic
    # standardized means: s0 cluster ~ negative, s1 ~ positive on each feature
    assert m1["emissions"][0]["mean"][0] < m1["emissions"][1]["mean"][0]


def test_fit_drops_nan_rows():
    feats = np.array([[np.nan, np.nan, np.nan, np.nan], [0.0, 0, 0, 0], [1.0, 1, 1, 1]])
    labels = np.array(["s0", "s0", "s1"], dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=2)
    assert m["emissions"][0]["n"] == 1  # the NaN s0 row was dropped
