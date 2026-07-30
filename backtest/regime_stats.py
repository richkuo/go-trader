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


def benjamini_hochberg(pvals: list[float], alpha: float = 0.05,
                       family_size: int | None = None) -> list[bool]:
    """Benjamini-Hochberg FDR mask over ``pvals`` at level ``alpha``.

    ``family_size`` overrides the BH denominator (default ``len(pvals)``). Pass
    a value LARGER than the number of p-values to correct as if the family had
    that many hypotheses, of which only these were tested — the untested
    remainder is treated as p=1 (never rejected). Under BH this collapses to
    ranking the k supplied p-values 1..k but dividing each rank's threshold by
    the full family size N instead of k, i.e. ``thresh_i = (i / N) * alpha``.
    That is exactly the correction a selection-aware two-stage search needs
    (#1338): stage 1 mined N candidates, so the k survivors that reach stage 2
    must be corrected against N, not re-baselined to a fresh family of k.
    Must be ``>= len(pvals)`` — a denominator below the tested count would
    understate, not correct for, multiplicity.
    """
    p = np.asarray(pvals, dtype=float)
    m = len(p)
    if m == 0:
        return []
    denom = m if family_size is None else int(family_size)
    if denom < m:
        raise ValueError(
            f"family_size={denom} is smaller than the number of p-values "
            f"({m}); the BH denominator must cover every tested hypothesis")
    order = np.argsort(p)
    ranked = p[order]
    thresh = (np.arange(1, m + 1) / denom) * alpha
    passed = ranked <= thresh
    cut = ranked[np.max(np.where(passed)[0])] if passed.any() else -1.0
    return (p <= cut).tolist()
