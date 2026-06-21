"""Unsupervised volatility-state regime model (#1080): HMM/GMM/k-means candidates
behind one model-dict schema decoded by regime_hmm.forward_filter_labels. Offline only."""
from __future__ import annotations
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
for _p in (_THIS_DIR, os.path.abspath(os.path.join(_THIS_DIR, "..")),
           os.path.abspath(os.path.join(_THIS_DIR, "..", "shared_tools"))):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from dataclasses import dataclass

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
    # Re-seed empty clusters before summarizing: an emptied cluster keeps its stale init centroid
    # and the default unit variance, and forward_filter_labels does NOT skip n=0 states -> that
    # ghost Gaussian can still win the decoder argmax and emit a label no training bar supports.
    # Hand each empty cluster the data point farthest from its assigned center (classic
    # empty-cluster repair), stealing only from clusters that can spare a member, so every stored
    # state summarizes >=1 real observation. No-op when every cluster is occupied -> byte-identical.
    empty = [j for j in range(k) if not (assign == j).any()]
    if empty:
        d = ((z[:, None, :] - centers[None, :, :]) ** 2).sum(-1)
        nearest = d[np.arange(n), assign]
        for j in empty:
            sizes = np.bincount(assign, minlength=k)            # recomputed: prior steals shrink donors
            spare = np.where(sizes[assign] >= 2, nearest, -np.inf)
            far = int(spare.argmax())
            if not np.isfinite(spare[far]):
                break  # no donor can spare a point (fewer than k distinct-enough rows) -> degenerate
            assign[far] = j
            em_mean[j] = z[far]
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
    d = em_mean_z.shape[1]
    if mean.shape != (d,) or std.shape != (d,):     # a mis-shaped scalar/wrong-length std would
        raise ValueError(f"feature_means/stds must be shape ({d},); "  # silently broadcast garbage
                         f"got {mean.shape} and {std.shape}")
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


def fit_unsupervised(features, *, family, k, filter_window, period=48,
                     thresholds=None, seed=0, fitted_on=None):
    """Fit one unsupervised family and assemble the forward_filter_labels-decodable model dict.
    Emissions come from the family fit; the transition table + init are always estimated
    empirically from the training-window state sequence; states are named post-fit from centroids."""
    if family not in FITTERS:
        raise ValueError(f"unknown family {family!r}; known: {sorted(FITTERS)}")
    if thresholds is None:
        from regime import _DEFAULT_COMPOSITE_THRESHOLDS
        thresholds = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    features = np.asarray(features, dtype=float)
    mean, std, mask = standardize(features)
    z = (features[mask] - mean) / std
    assign_valid, em_mean, em_var, counts = FITTERS[family](z, k, seed=seed)
    transition = empirical_transition(assign_valid, mask, k)
    init = init_distribution(transition)
    names, mapping = map_latent_to_names(em_mean, mean, std, thresholds)
    emissions = [{"mean": em_mean[i].tolist(), "var": em_var[i].tolist(),
                  "n": int(counts[i])} for i in range(k)]
    return {
        "type": MODEL_TYPE, "version": MODEL_VERSION, "fit_method": family,
        "features": list(FEATURES),
        "feature_means": mean.tolist(), "feature_stds": std.tolist(),
        "states": names, "latent_count": int(k), "emissions": emissions,
        "transition": transition.tolist(), "init": init.tolist(),
        "filter_window": int(filter_window), "period": int(period),
        "fitted_on": dict(fitted_on or {}), "mapping": mapping,
    }


@dataclass
class NonDegeneracyThresholds:
    min_active_labels: int
    max_occupancy: float
    min_transition_rate: float


def _stream_stats(labels):
    from regime_diagnostics import coverage, stability
    cov = coverage(labels)
    active = len(cov)
    max_occ = max((c["pct"] for c in cov.values()), default=0.0)
    tr = stability(labels)["transition_rate"]
    return active, float(max_occ), float(tr)


def non_degeneracy(labels, thresholds):
    labels = np.asarray(labels, dtype=object)
    active, max_occ, tr = _stream_stats(labels)
    reasons = []
    if active < thresholds.min_active_labels:
        reasons.append(f"min_active_labels: {active} < {thresholds.min_active_labels}")
    if max_occ > thresholds.max_occupancy:
        reasons.append(f"max_occupancy: {max_occ:.3f} > {thresholds.max_occupancy:.3f}")
    if tr < thresholds.min_transition_rate:
        reasons.append(f"min_transition_rate: {tr:.4f} < {thresholds.min_transition_rate:.4f}")
    return {"active_labels": active, "max_occupancy": max_occ, "transition_rate": tr,
            "ok": not reasons, "reasons": reasons}


def derive_thresholds(handrule_streams, *, active_margin=1, occupancy_margin=0.05,
                      rate_margin=0.5):
    """Lock non-degeneracy cutoffs from the incumbent's WORST window, loosened by a fixed
    margin (anti-gaming). Must be called before scoring any candidate."""
    if not handrule_streams:
        raise ValueError("derive_thresholds needs at least one hand-rule stream")
    stats = [_stream_stats(np.asarray(s, dtype=object)) for s in handrule_streams]
    worst_active = min(a for a, _, _ in stats)
    worst_occ = max(o for _, o, _ in stats)
    worst_tr = min(t for _, _, t in stats)
    return NonDegeneracyThresholds(
        min_active_labels=max(1, worst_active - active_margin),
        max_occupancy=min(1.0, worst_occ + occupancy_margin),
        min_transition_rate=max(0.0, worst_tr * rate_margin),
    )


def build_parser():
    import argparse
    from eval_windows import WINDOWS
    p = argparse.ArgumentParser(description="Fit one unsupervised vol-regime model (#1080)")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--window", default="is", choices=list(WINDOWS),
                   help=f"known: {', '.join(WINDOWS)}")
    p.add_argument("--family", default="hmm", choices=sorted(FITTERS))
    p.add_argument("--k", type=int, default=4)
    p.add_argument("--period", type=int, default=48)
    p.add_argument("--filter-window", type=int, default=64)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--out", default=None, help="write fitted model JSON here")
    return p


def main(argv=None):
    import json
    from regime import composite_feature_matrix, _DEFAULT_COMPOSITE_THRESHOLDS
    from data_fetcher import load_cached_data
    from eval_windows import WINDOWS, PLATFORM
    args = build_parser().parse_args(argv)
    start, end = WINDOWS[args.window]
    df = load_cached_data(args.symbol, args.timeframe, exchange_id=PLATFORM,
                          start_date=start, end_date=end)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats = composite_feature_matrix(df, args.period, th).to_numpy()
    model = fit_unsupervised(feats, family=args.family, k=args.k,
                             filter_window=args.filter_window, period=args.period,
                             thresholds=th, seed=args.seed,
                             fitted_on={"symbol": args.symbol, "timeframe": args.timeframe,
                                        "window": args.window})
    text = json.dumps({"model": model}, indent=2, default=float)
    if args.out:
        with open(args.out, "w") as fh:
            fh.write(text)
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
