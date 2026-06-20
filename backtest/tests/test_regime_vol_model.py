import os, sys
import numpy as np
import pytest

_THIS = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import regime_vol_model as rvm


def test_standardize_masks_nan_rows_and_floors_zero_std():
    feats = np.array([[1.0, 10.0, 0.5, 20.0],
                      [3.0, 10.0, 0.5, 30.0],
                      [np.nan, 10.0, 0.5, 40.0]], dtype=float)
    mean, std, mask = rvm.standardize(feats)
    assert mask.tolist() == [True, True, False]          # NaN row dropped
    assert mean[0] == pytest.approx(2.0)                  # mean over valid rows only
    assert std[1] == 1.0                                  # zero-variance col floored to 1.0


def test_empirical_transition_skips_pairs_spanning_a_dropped_bar():
    # bars: 0->valid(0), 1->valid(1), 2->NaN(dropped), 3->valid(0)
    valid_mask = np.array([True, True, False, True])
    assignments_valid = np.array([0, 1, 0])              # only for the 3 valid bars
    A = rvm.empirical_transition(assignments_valid, valid_mask, k=2, laplace=1.0)
    # only adjacency 0->1 counts (bars 0,1). Pair (1,2)&(2,3) span the dropped bar.
    assert A.shape == (2, 2)
    assert np.allclose(A.sum(1), 1.0)                     # row-stochastic
    # 0->1 got the lone real count; with laplace=1 row0 = [1,2]/3
    assert A[0, 1] > A[0, 0]


def test_init_distribution_is_stationary_and_sums_to_one():
    A = np.array([[0.9, 0.1], [0.2, 0.8]])
    pi = rvm.init_distribution(A)
    assert pi.sum() == pytest.approx(1.0)
    assert np.allclose(pi @ A, pi, atol=1e-8)            # stationary


def test_logsumexp_matches_naive():
    v = np.array([-1.0, -2.0, -3.0])
    assert rvm._logsumexp(v) == pytest.approx(np.log(np.exp(v).sum()))
    m = np.array([[-1.0, -2.0], [0.0, -5.0]])
    assert np.allclose(rvm._logsumexp_rows(m),
                       np.log(np.exp(m).sum(1)))


def _three_blobs(seed=0, per=200):
    rng = np.random.default_rng(seed)
    centers = np.array([[-3, -3, -3, -3], [0, 0, 0, 0], [3, 3, 3, 3]], dtype=float)
    pts, truth = [], []
    for c_idx, c in enumerate(centers):
        pts.append(rng.normal(c, 0.25, size=(per, 4)))
        truth += [c_idx] * per
    return np.vstack(pts), np.array(truth)


def _purity(assign, truth, k):
    correct = 0
    for j in range(k):
        members = truth[assign == j]
        if len(members):
            maj = np.bincount(members).argmax()
            correct += int((members == maj).sum())
    return correct / len(truth)


def test_fit_kmeans_recovers_three_blobs():
    z, truth = _three_blobs()
    assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 3, seed=0)
    assert em_mean.shape == (3, 4) and em_var.shape == (3, 4)
    assert counts.sum() == len(z)
    assert (em_var > 0).all()
    assert _purity(assign, truth, 3) > 0.95
