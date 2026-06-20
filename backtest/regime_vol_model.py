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


def fit_kmeans(z, k, *, seed=0, iters=100, var_floor=1e-3):
    z = np.asarray(z, dtype=float)
    n = len(z)
    rng = np.random.default_rng(seed)
    centers = z[rng.choice(n, size=k, replace=False)].copy()
    assign = None  # None != any real assignment -> first iteration never early-breaks (k=1 safe)
    for _ in range(iters):
        d = ((z[:, None, :] - centers[None, :, :]) ** 2).sum(-1)
        new = d.argmin(1)
        if assign is not None and np.array_equal(new, assign):
            break
        assign = new
        for j in range(k):
            if (assign == j).any():
                centers[j] = z[assign == j].mean(0)
    em_mean = centers.copy()  # decouple from `centers` so callers can't mutate the alias
    em_var = np.ones((k, z.shape[1]))
    counts = np.zeros(k, dtype=int)
    for j in range(k):
        members = z[assign == j]
        counts[j] = len(members)
        if len(members) >= 2:
            em_var[j] = members.var(0)
    em_var = np.maximum(em_var, var_floor)
    return assign, em_mean, em_var, counts


def _diag_logprob(z, mu, var):
    # log N(z; mu, diag(var)) per row -> [n]
    diff = z - mu
    return -0.5 * (np.log(2 * np.pi * var) + diff ** 2 / var).sum(1)


def fit_gmm(z, k, *, seed=0, iters=100, var_floor=1e-3, tol=1e-4):
    z = np.asarray(z, dtype=float)
    n = z.shape[0]
    assign0, mu, var, _ = fit_kmeans(z, k, seed=seed)
    mu = mu.copy(); var = var.copy()
    weights = np.array([max(int((assign0 == j).sum()), 1) for j in range(k)], dtype=float)
    weights /= weights.sum()
    prev_ll = -np.inf
    for _ in range(iters):
        log_resp = np.empty((n, k))
        for j in range(k):
            log_resp[:, j] = np.log(weights[j] + 1e-300) + _diag_logprob(z, mu[j], var[j])
        lse = _logsumexp_rows(log_resp)
        ll = float(lse.sum())
        resp = np.exp(log_resp - lse[:, None])
        Nk = resp.sum(0)                       # exact responsibility mass; sum(Nk) == n
        safe_Nk = np.maximum(Nk, 1e-10)        # guard division only — never inflates the mass
        weights = Nk / n
        mu = (resp.T @ z) / safe_Nk[:, None]
        for j in range(k):
            diff = z - mu[j]
            var[j] = (resp[:, j][:, None] * diff ** 2).sum(0) / safe_Nk[j]
        var = np.maximum(var, var_floor)
        if prev_ll != -np.inf and abs(ll - prev_ll) < tol * abs(prev_ll):
            break
        prev_ll = ll
    log_resp = np.empty((n, k))
    for j in range(k):
        log_resp[:, j] = np.log(weights[j] + 1e-300) + _diag_logprob(z, mu[j], var[j])
    assign = log_resp.argmax(1)
    counts = np.array([int((assign == j).sum()) for j in range(k)], dtype=int)
    return assign, mu, var, counts


def _viterbi(z, mu, var, A, pi):
    n, k = len(z), len(pi)
    logA = np.log(A + 1e-300)
    logB = np.column_stack([_diag_logprob(z, mu[j], var[j]) for j in range(k)])
    delta = np.log(pi + 1e-300) + logB[0]
    back = np.zeros((n, k), dtype=int)
    for t in range(1, n):
        m = delta[:, None] + logA            # [k_prev, k_next]
        back[t] = m.argmax(0)
        delta = m.max(0) + logB[t]
    path = np.zeros(n, dtype=int)
    path[-1] = int(delta.argmax())
    for t in range(n - 2, -1, -1):
        path[t] = back[t + 1, path[t + 1]]
    return path


def fit_hmm(z, k, *, seed=0, iters=50, var_floor=1e-3, tol=1e-4):
    z = np.asarray(z, dtype=float)
    n = len(z)
    _, mu, var, _ = fit_kmeans(z, k, seed=seed)
    mu = mu.copy(); var = var.copy()
    A = np.full((k, k), 1.0 / k)
    pi = np.full(k, 1.0 / k)
    prev_ll = -np.inf
    for _ in range(iters):
        logB = np.column_stack([_diag_logprob(z, mu[j], var[j]) for j in range(k)])
        logA = np.log(A + 1e-300)
        log_alpha = np.empty((n, k)); log_alpha[0] = np.log(pi + 1e-300) + logB[0]
        for t in range(1, n):
            # alpha[t,j] = logsumexp_i(alpha[t-1,i] + logA[i,j]) + logB[t,j]; reduce over the
            # SOURCE state i (axis 0). Transpose [i,j]->[j,i] so the per-row reducer sums over i.
            log_alpha[t] = _logsumexp_rows((log_alpha[t - 1][:, None] + logA).T) + logB[t]
        ll = _logsumexp(log_alpha[-1])
        log_beta = np.zeros((n, k))
        for t in range(n - 2, -1, -1):
            log_beta[t] = _logsumexp_rows(logA + (logB[t + 1] + log_beta[t + 1])[None, :])
        log_gamma = log_alpha + log_beta
        log_gamma -= _logsumexp_rows(log_gamma)[:, None]
        gamma = np.exp(log_gamma)
        xi_sum = np.zeros((k, k))
        for t in range(n - 1):
            log_xi = (log_alpha[t][:, None] + logA
                      + (logB[t + 1] + log_beta[t + 1])[None, :])
            log_xi -= _logsumexp(log_xi.ravel())
            xi_sum += np.exp(log_xi)
        pi = gamma[0] + 1e-10; pi /= pi.sum()
        A = xi_sum + 1e-10; A /= A.sum(1, keepdims=True)
        Nk = gamma.sum(0) + 1e-10
        mu = (gamma.T @ z) / Nk[:, None]
        for j in range(k):
            diff = z - mu[j]
            var[j] = (gamma[:, j][:, None] * diff ** 2).sum(0) / Nk[j]
        var = np.maximum(var, var_floor)
        if prev_ll != -np.inf and abs(ll - prev_ll) < tol * abs(prev_ll):
            break
        prev_ll = ll
    assign = _viterbi(z, mu, var, A, pi)
    counts = np.array([int((assign == j).sum()) for j in range(k)], dtype=int)
    return assign, mu, var, counts


FITTERS = {"kmeans": fit_kmeans, "gmm": fit_gmm, "hmm": fit_hmm}


def map_latent_to_names(em_mean_z, feature_means, feature_stds, thresholds):
    """Name each latent state by un-standardizing its centroid to raw feature space and running
    the canonical map_composite_label on it. Uses only training-window centroids (no per-bar
    hand-rule labels). volatility_rank orders states by centroid range_eff (ascending)."""
    from regime import map_composite_label
    em_mean_z = np.asarray(em_mean_z, dtype=float)
    mean = np.asarray(feature_means, dtype=float)
    std = np.asarray(feature_stds, dtype=float)
    raw = em_mean_z * std + mean                       # un-standardize centroids -> raw features
    order = sorted(range(len(raw)), key=lambda i: (raw[i, 1], i))   # by range_eff, stable on ties
    rank = {i: r for r, i in enumerate(order)}
    names, mapping = [], {}
    for i in range(len(raw)):
        # map_composite_label(return_eff, adx_val, range_eff, efficiency, thresholds)
        name = map_composite_label(raw[i, 0], raw[i, 3], raw[i, 1], raw[i, 2], thresholds)
        names.append(name)
        mapping[str(i)] = {"name": name, "centroid_raw": raw[i].tolist(),
                           "volatility_rank": int(rank[i])}
    return names, mapping
