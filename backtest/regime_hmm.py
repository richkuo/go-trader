# backtest/regime_hmm.py
"""Label-anchored Gaussian HMM: closed-form fit + causal forward-filter (#1065)."""
from __future__ import annotations
import numpy as np

MODEL_TYPE = "label_anchored_hmm"
MODEL_VERSION = 1
FEATURES = ["return_eff", "range_eff", "efficiency", "adx"]


def stationary_distribution(transition: np.ndarray) -> np.ndarray:
    A = np.asarray(transition, dtype=float)
    vals, vecs = np.linalg.eig(A.T)
    i = int(np.argmin(np.abs(vals - 1.0)))
    v = np.abs(np.real(vecs[:, i]))
    s = v.sum()
    return v / s if s > 0 else np.full(len(A), 1.0 / len(A))


def fit_label_anchored_hmm(features, labels, states, *, filter_window,
                           var_floor=1e-3, laplace=1.0, fitted_on=None) -> dict:
    features = np.asarray(features, dtype=float)
    labels = np.asarray(labels, dtype=object)
    mask = ~np.isnan(features).any(1)
    x, y = features[mask], labels[mask]
    mean = x.mean(0) if len(x) else np.zeros(features.shape[1])
    std = x.std(0) if len(x) else np.ones(features.shape[1])
    std = np.where(std < 1e-8, 1.0, std)
    z = (x - mean) / std
    emissions = []
    for s in states:
        zs = z[y == s]
        if len(zs) >= 2:
            em_mean, em_var = zs.mean(0), zs.var(0)
        else:  # degenerate: anchor at standardized origin, unit variance (flagged by n)
            em_mean, em_var = np.zeros(z.shape[1]), np.ones(z.shape[1])
        em_var = np.maximum(em_var, var_floor)
        emissions.append({"mean": em_mean.tolist(), "var": em_var.tolist(), "n": int(len(zs))})
    si = {s: i for i, s in enumerate(states)}
    k = len(states)
    A = np.full((k, k), float(laplace))
    for a, b in zip(y[:-1], y[1:]):
        A[si[a], si[b]] += 1.0
    A = A / A.sum(1, keepdims=True)
    return {
        "type": MODEL_TYPE, "version": MODEL_VERSION, "features": list(FEATURES),
        "feature_means": mean.tolist(), "feature_stds": std.tolist(),
        "states": list(states), "emissions": emissions,
        "transition": A.tolist(), "init": stationary_distribution(A).tolist(),
        "filter_window": int(filter_window), "fitted_on": fitted_on or {},
    }


def _logsumexp(v: np.ndarray) -> float:
    m = float(np.max(v))
    return m + float(np.log(np.exp(v - m).sum()))


def forward_filter_labels(features: np.ndarray, model: dict):
    features = np.asarray(features, dtype=float)
    n = len(features)
    states = list(model["states"])
    k = len(states)
    mean = np.asarray(model["feature_means"], dtype=float)
    std = np.asarray(model["feature_stds"], dtype=float)
    em_mean = np.array([e["mean"] for e in model["emissions"]], dtype=float)
    em_var = np.array([e["var"] for e in model["emissions"]], dtype=float)
    log_init = np.log(np.asarray(model["init"], dtype=float) + 1e-300)
    log_A = np.log(np.asarray(model["transition"], dtype=float) + 1e-300)
    w = int(model["filter_window"])
    default_label = states[int(np.argmax(model["init"]))]

    labels = np.array([default_label] * n, dtype=object)
    conf = np.zeros(n, dtype=float)
    for i in range(n):
        lo = max(0, i - w + 1)
        alpha = log_init.copy()
        seen = False
        for t in range(lo, i + 1):
            x = features[t]
            # predict: alpha'_j = logsumexp_i(alpha_i + log_A[i,j])
            pred = np.array([_logsumexp(alpha + log_A[:, j]) for j in range(k)])
            if np.isnan(x).any():
                alpha = pred  # carry: transition only, no emission
                continue
            z = (x - mean) / std
            log_emit = -0.5 * (np.log(2 * np.pi * em_var) + (z - em_mean) ** 2 / em_var).sum(1)
            alpha = pred + log_emit
            alpha -= _logsumexp(alpha)  # normalize
            seen = True
        if seen:
            j = int(np.argmax(alpha))
            labels[i] = states[j]
            conf[i] = float(np.exp(alpha[j] - _logsumexp(alpha)))
    return labels, conf
