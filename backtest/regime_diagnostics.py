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
