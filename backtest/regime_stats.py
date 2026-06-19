"""Dependency-free statistics for regime diagnostics (#1065). numpy only."""
from __future__ import annotations
import numpy as np


def rank_data(x: np.ndarray) -> np.ndarray:
    x = np.asarray(x, dtype=float)
    n = len(x)
    ranks = np.empty(n, dtype=float)
    order = np.argsort(x, kind="mergesort")
    sx = x[order]
    i = 0
    while i < n:
        j = i
        while j + 1 < n and sx[j + 1] == sx[i]:
            j += 1
        ranks[order[i : j + 1]] = (i + j) / 2.0 + 1.0  # 1-based average rank
        i = j + 1
    return ranks


def kruskal_h(groups: list[np.ndarray]) -> float:
    gs = [np.asarray(g, dtype=float) for g in groups if len(g) > 0]
    if len(gs) < 2:
        return 0.0
    allx = np.concatenate(gs)
    n = len(allx)
    if n < 2:
        return 0.0
    ranks = rank_data(allx)
    h = 0.0
    idx = 0
    for g in gs:
        r = ranks[idx : idx + len(g)]
        idx += len(g)
        h += (r.sum() ** 2) / len(g)
    h = 12.0 / (n * (n + 1)) * h - 3.0 * (n + 1)
    _, counts = np.unique(allx, return_counts=True)
    ties = float((counts ** 3 - counts).sum())
    c = 1.0 - ties / (n ** 3 - n) if n > 1 else 1.0
    return float(h / c) if c > 0 else float(h)


def benjamini_hochberg(pvals: list[float], alpha: float = 0.05) -> list[bool]:
    p = np.asarray(pvals, dtype=float)
    m = len(p)
    if m == 0:
        return []
    order = np.argsort(p)
    ranked = p[order]
    thresh = (np.arange(1, m + 1) / m) * alpha
    passed = ranked <= thresh
    cut = ranked[np.max(np.where(passed)[0])] if passed.any() else -1.0
    return (p <= cut).tolist()
