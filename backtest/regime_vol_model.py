"""Unsupervised volatility-state regime model (#1080): HMM/GMM/k-means candidates
behind one model-dict schema decoded by regime_hmm.forward_filter_labels. Offline only."""
from __future__ import annotations
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
for _p in (_THIS_DIR, os.path.abspath(os.path.join(_THIS_DIR, "..")),
           os.path.abspath(os.path.join(_THIS_DIR, "..", "shared_tools"))):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
from regime_hmm import stationary_distribution

FEATURES = ["return_eff", "range_eff", "efficiency", "adx"]
MODEL_TYPE = "unsupervised_vol_regime"
MODEL_VERSION = 1


def standardize(features):
    features = np.asarray(features, dtype=float)
    mask = ~np.isnan(features).any(axis=1)
    x = features[mask]
    mean = x.mean(0) if len(x) else np.zeros(features.shape[1])
    std = x.std(0) if len(x) else np.ones(features.shape[1])
    std = np.where(std < 1e-8, 1.0, std)
    return mean, std, mask


def empirical_transition(assignments_valid, valid_mask, k, *, laplace=1.0):
    """Count i->j only between bars adjacent in the ORIGINAL series AND both feature-valid,
    mirroring regime_hmm's NaN discipline. Laplace-smoothed, row-normalized."""
    valid_mask = np.asarray(valid_mask, dtype=bool)
    full = np.full(len(valid_mask), -1, dtype=int)
    full[valid_mask] = np.asarray(assignments_valid, dtype=int)
    A = np.full((k, k), float(laplace))
    for i in range(len(valid_mask) - 1):
        if valid_mask[i] and valid_mask[i + 1]:
            A[full[i], full[i + 1]] += 1.0
    return A / A.sum(1, keepdims=True)


def init_distribution(transition):
    return stationary_distribution(np.asarray(transition, dtype=float))


def _logsumexp(v: np.ndarray) -> float:
    v = np.asarray(v, dtype=float)
    m = float(np.max(v))
    return m + float(np.log(np.exp(v - m).sum()))


def _logsumexp_rows(m: np.ndarray) -> np.ndarray:
    m = np.asarray(m, dtype=float)
    mx = m.max(axis=1, keepdims=True)
    return (mx[:, 0] + np.log(np.exp(m - mx).sum(axis=1)))
