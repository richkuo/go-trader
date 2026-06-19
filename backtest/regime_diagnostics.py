"""7-state regime quality diagnostics (#1065 PR1). Pure scorers; CLI at bottom."""
from __future__ import annotations
import numpy as np
from regime_stats import kruskal_h, benjamini_hochberg  # noqa: F401 (BH used in Task 7)


def forward_returns(close: np.ndarray, horizon: int) -> np.ndarray:
    close = np.asarray(close, dtype=float)
    fwd = np.full(len(close), np.nan)
    if horizon < len(close):
        fwd[:-horizon] = close[horizon:] / close[:-horizon] - 1.0
    return fwd


def coverage(labels: np.ndarray) -> dict:
    labels = np.asarray(labels, dtype=object)
    n = len(labels)
    states, counts = np.unique(labels, return_counts=True)
    return {str(s): {"count": int(c), "pct": float(c / n) if n else 0.0}
            for s, c in zip(states, counts)}


def separation(labels: np.ndarray, fwd: np.ndarray) -> dict:
    labels = np.asarray(labels, dtype=object)
    fwd = np.asarray(fwd, dtype=float)
    valid = ~np.isnan(fwd)
    labels, fwd = labels[valid], fwd[valid]
    per_state, groups = {}, []
    for s in sorted(set(labels.tolist())):
        r = fwd[labels == s]
        if len(r) == 0:
            continue
        groups.append(r)
        per_state[str(s)] = {
            "n": int(len(r)),
            "mean": float(r.mean()),
            "std": float(r.std()),
            "hit_rate": float((r > 0).mean()),
        }
    return {"kruskal_h": kruskal_h(groups), "per_state": per_state}


def stability(labels: np.ndarray) -> dict:
    labels = np.asarray(labels, dtype=object)
    n = len(labels)
    if n < 2:
        return {"transition_rate": 0.0, "flips": 0, "mean_dwell": {}}
    changes = labels[1:] != labels[:-1]
    flips = int(changes.sum())
    runs: dict[str, list[int]] = {}
    cur, length = labels[0], 1
    for x in labels[1:]:
        if x == cur:
            length += 1
        else:
            runs.setdefault(str(cur), []).append(length)
            cur, length = x, 1
    runs.setdefault(str(cur), []).append(length)
    return {
        "transition_rate": float(flips / (n - 1)),
        "flips": flips,
        "mean_dwell": {s: float(np.mean(v)) for s, v in sorted(runs.items())},
    }


def block_shuffle_pvalue(labels, fwd, block_len, n_perm=200, seed=0) -> dict:
    labels = np.asarray(labels, dtype=object)
    fwd = np.asarray(fwd, dtype=float)
    h_obs = separation(labels, fwd)["kruskal_h"]
    n = len(labels)
    block_len = max(1, int(block_len))
    starts = list(range(0, n, block_len))
    rng = np.random.default_rng(seed)
    ge = 0
    for _ in range(n_perm):
        perm = rng.permutation(len(starts))
        shuffled = np.concatenate([labels[s : s + block_len] for s in (starts[i] for i in perm)])
        shuffled = shuffled[:n]
        if separation(shuffled, fwd)["kruskal_h"] >= h_obs:
            ge += 1
    return {"kruskal_h": float(h_obs), "p_value": float((ge + 1) / (n_perm + 1)),
            "block_len": block_len, "n_perm": int(n_perm)}


def _kmeans(z, k, seed, iters=50):
    rng = np.random.default_rng(seed)
    centers = z[rng.choice(len(z), size=k, replace=False)]
    labels = np.zeros(len(z), dtype=int)
    for _ in range(iters):
        d = ((z[:, None, :] - centers[None, :, :]) ** 2).sum(-1)
        new = d.argmin(1)
        if np.array_equal(new, labels):
            break
        labels = new
        for j in range(k):
            if (labels == j).any():
                centers[j] = z[labels == j].mean(0)
    return labels


def kmeans_yardstick(features, fwd, k_range=(2, 3, 4, 5, 6, 7), seed=0) -> dict:
    features = np.asarray(features, dtype=float)
    fwd = np.asarray(fwd, dtype=float)
    mask = ~np.isnan(features).any(1) & ~np.isnan(fwd)
    x, fr = features[mask], fwd[mask]
    mean, std = x.mean(0), x.std(0)
    std[std < 1e-8] = 1.0
    z = (x - mean) / std
    out = {}
    for k in k_range:
        if k > len(z):
            continue
        cl = _kmeans(z, k, seed)
        groups = [fr[cl == j] for j in range(k) if (cl == j).any()]
        out[k] = {"kruskal_h": kruskal_h(groups)}
    return out
