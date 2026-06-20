# Unsupervised volatility-state regime model (#1080) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build an offline, genuinely unsupervised volatility-state regime model (three candidate families) whose learned states map to the seven composite labels and clear the #1078 forward-volatility gate out-of-sample, selected by a walk-forward bake-off.

**Architecture:** All three families (Baum-Welch HMM, GMM, k-means) emit ONE model-dict schema decoded by the existing causal `forward_filter_labels` — they differ only in how the per-state Gaussian emissions are estimated; the transition table and init are always estimated empirically from the training-window state sequence, and the latent→name step runs the existing `map_composite_label` on each un-standardized state centroid. Zero edits to existing modules.

**Tech Stack:** Python 3, numpy only (no sklearn/scipy/hmmlearn — hand-rolled EM/Viterbi, matching the codebase). Tests via pytest under `backtest/tests/`.

## Global Constraints

- **Offline / research only.** No Go, no live classifier, no `shared_scripts/check_regime.py` changes. No edits to any existing module — new files only.
- **Features (exact order):** `["return_eff", "range_eff", "efficiency", "adx"]` — `composite_feature_matrix` column order. Index 0=return_eff, 1=range_eff, 2=efficiency, 3=adx.
- **Emissions live in z-space** (standardized with the model's `feature_means`/`feature_stds`), matching `regime_hmm` and what `forward_filter_labels` expects.
- **No leakage:** the fit path must never call `compute_regime_composite` and never receive hand-rule labels as a fit target. The latent→name mapping uses only training-window centroids.
- **Causal contract:** decoding is the existing `forward_filter_labels` (label at bar N uses only bars ≤ N within `filter_window`). Nothing in this plan reads forward bars.
- **Model-dict schema (every family produces this):**
  ```
  type="unsupervised_vol_regime", version=1, fit_method∈{hmm,gmm,kmeans},
  features=[...4], feature_means=[...4], feature_stds=[...4],
  states=[name per latent index], latent_count=K,
  emissions=[{mean:[...4 z], var:[...4 z], n:int} per state],
  transition=[K×K], init=[...K], filter_window=int, period=48,
  fitted_on={symbol,timeframe,window}, mapping={"i":{name,centroid_raw,volatility_rank}}
  ```
- **Run commands** from repo root with `uv run --no-sync python ...`; `python -m py_compile` and `pytest` likewise. `gofmt`/Go build N/A (no Go).
- **Determinism:** every fitter takes `seed` and uses `np.random.default_rng(seed)`. No `Math.random`/unbounded nondeterminism.
- **Spec:** `docs/superpowers/specs/2026-06-20-unsupervised-vol-regime-model-design.md`.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `backtest/regime_vol_model.py` | Pure library: standardization + empirical-transition/init helpers, the three fitters, latent→name mapping, model-dict assembly (`fit_unsupervised`), non-degeneracy scorer + incumbent-derived thresholds, and a single-family `--out` CLI. |
| `backtest/research/regime_1080_unsupervised_vol_model.py` | Walk-forward bake-off driver + reproducible evidence (`--symbol/--timeframe`, read-only, prints/writes JSON). |
| `backtest/tests/test_regime_vol_model.py` | All tests. |

---

### Task 1: Module scaffold + numeric helpers (standardize, empirical transition, init, logsumexp)

**Files:**
- Create: `backtest/regime_vol_model.py`
- Test: `backtest/tests/test_regime_vol_model.py`

**Interfaces:**
- Consumes: `regime_hmm.stationary_distribution` (existing).
- Produces:
  - `standardize(features) -> (mean: np.ndarray[D], std: np.ndarray[D], mask: np.ndarray[bool,N])`
  - `empirical_transition(assignments_valid: np.ndarray[int], valid_mask: np.ndarray[bool], k: int, *, laplace=1.0) -> np.ndarray[k,k]`
  - `init_distribution(transition) -> np.ndarray[k]`
  - `_logsumexp(v: np.ndarray) -> float`, `_logsumexp_rows(m: np.ndarray) -> np.ndarray` (axis=1)
  - Module constants `FEATURES`, `MODEL_TYPE="unsupervised_vol_regime"`, `MODEL_VERSION=1`.

- [ ] **Step 1: Write the failing tests**

```python
# backtest/tests/test_regime_vol_model.py
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'regime_vol_model'`.

- [ ] **Step 3: Write the module scaffold + helpers**

```python
# backtest/regime_vol_model.py
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -q`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add backtest/regime_vol_model.py backtest/tests/test_regime_vol_model.py
git commit -m "feat(#1080): regime_vol_model scaffold + standardize/transition/init helpers"
```

---

### Task 2: k-means fitter

**Files:**
- Modify: `backtest/regime_vol_model.py`
- Test: `backtest/tests/test_regime_vol_model.py`

**Interfaces:**
- Produces: `fit_kmeans(z, k, *, seed=0, iters=100, var_floor=1e-3) -> (assign: int[n], em_mean: float[k,D], em_var: float[k,D], counts: int[k])`. `z` is the standardized, valid-only feature matrix. Emissions in z-space. This is the canonical fitter signature reused by Tasks 3, 4, 6.

- [ ] **Step 1: Write the failing test**

```python
def _three_blobs(seed=0, per=200):
    rng = np.random.default_rng(seed)
    centers = np.array([[-3, -3, -3, -3], [0, 0, 0, 0], [3, 3, 3, 3]], dtype=float)
    pts, truth = [], []
    for c_idx, c in enumerate(centers):
        pts.append(rng.normal(c, 0.25, size=(per, 4)))
        truth += [c_idx] * per
    return np.vstack(pts), np.array(truth)


def _purity(assign, truth, k):
    # fraction of points whose learned cluster's majority-true-label matches their truth
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
    assert _purity(assign, truth, 3) > 0.95          # clean separation recovered
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py::test_fit_kmeans_recovers_three_blobs -q`
Expected: FAIL with `AttributeError: module 'regime_vol_model' has no attribute 'fit_kmeans'`.

- [ ] **Step 3: Implement `fit_kmeans`**

```python
def fit_kmeans(z, k, *, seed=0, iters=100, var_floor=1e-3):
    z = np.asarray(z, dtype=float)
    n = len(z)
    rng = np.random.default_rng(seed)
    centers = z[rng.choice(n, size=k, replace=False)].copy()
    assign = np.zeros(n, dtype=int)
    for _ in range(iters):
        d = ((z[:, None, :] - centers[None, :, :]) ** 2).sum(-1)
        new = d.argmin(1)
        if np.array_equal(new, assign):
            break
        assign = new
        for j in range(k):
            if (assign == j).any():
                centers[j] = z[assign == j].mean(0)
    em_mean = centers
    em_var = np.ones((k, z.shape[1]))
    counts = np.zeros(k, dtype=int)
    for j in range(k):
        members = z[assign == j]
        counts[j] = len(members)
        if len(members) >= 2:
            em_var[j] = members.var(0)
    em_var = np.maximum(em_var, var_floor)
    return assign, em_mean, em_var, counts
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py::test_fit_kmeans_recovers_three_blobs -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backtest/regime_vol_model.py backtest/tests/test_regime_vol_model.py
git commit -m "feat(#1080): hand-rolled k-means fitter (canonical fitter signature)"
```

---

### Task 3: GMM fitter (diagonal-covariance EM)

**Files:**
- Modify: `backtest/regime_vol_model.py`
- Test: `backtest/tests/test_regime_vol_model.py`

**Interfaces:**
- Consumes: `fit_kmeans` (Task 2) for initialization.
- Produces: `fit_gmm(z, k, *, seed=0, iters=100, var_floor=1e-3, tol=1e-4) -> (assign, em_mean, em_var, counts)` — same signature as `fit_kmeans`.

- [ ] **Step 1: Write the failing test**

```python
def test_fit_gmm_recovers_three_blobs():
    z, truth = _three_blobs(seed=1)
    assign, em_mean, em_var, counts = rvm.fit_gmm(z, 3, seed=0)
    assert em_mean.shape == (3, 4) and (em_var > 0).all()
    assert counts.sum() == len(z)
    assert _purity(assign, truth, 3) > 0.95
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py::test_fit_gmm_recovers_three_blobs -q`
Expected: FAIL with `AttributeError: ... 'fit_gmm'`.

- [ ] **Step 3: Implement `fit_gmm`**

```python
def _diag_logprob(z, mu, var):
    # log N(z; mu, diag(var)) per row -> [n]
    diff = z - mu
    return -0.5 * (np.log(2 * np.pi * var) + diff ** 2 / var).sum(1)


def fit_gmm(z, k, *, seed=0, iters=100, var_floor=1e-3, tol=1e-4):
    z = np.asarray(z, dtype=float)
    n, d = z.shape
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
        Nk = resp.sum(0) + 1e-10
        weights = Nk / n
        mu = (resp.T @ z) / Nk[:, None]
        for j in range(k):
            diff = z - mu[j]
            var[j] = (resp[:, j][:, None] * diff ** 2).sum(0) / Nk[j]
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py::test_fit_gmm_recovers_three_blobs -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backtest/regime_vol_model.py backtest/tests/test_regime_vol_model.py
git commit -m "feat(#1080): GMM (diagonal-covariance EM) fitter"
```

---

### Task 4: HMM fitter (Baum-Welch EM + Viterbi decode)

**Files:**
- Modify: `backtest/regime_vol_model.py`
- Test: `backtest/tests/test_regime_vol_model.py`

**Interfaces:**
- Consumes: `fit_kmeans` (init), `_diag_logprob`, `_logsumexp`, `_logsumexp_rows`.
- Produces: `fit_hmm(z, k, *, seed=0, iters=50, var_floor=1e-3, tol=1e-4) -> (assign, em_mean, em_var, counts)` — same signature. Note: this `assign` is the Viterbi MAP path; the model-level transition is still re-derived empirically in Task 6 for cross-family consistency.

- [ ] **Step 1: Write the failing test**

```python
def _markov_sequence(seed=0, n=1500):
    # sticky 3-state chain emitting separated Gaussians -> temporal structure present
    rng = np.random.default_rng(seed)
    A = np.array([[0.95, 0.04, 0.01], [0.03, 0.94, 0.03], [0.01, 0.04, 0.95]])
    centers = np.array([[-3, -3, -3, -3], [0, 0, 0, 0], [3, 3, 3, 3]], dtype=float)
    s = 0; states = []
    for _ in range(n):
        states.append(s)
        s = rng.choice(3, p=A[s])
    states = np.array(states)
    z = np.array([rng.normal(centers[s], 0.4) for s in states])
    return z, states


def test_fit_hmm_recovers_markov_states():
    z, truth = _markov_sequence()
    assign, em_mean, em_var, counts = rvm.fit_hmm(z, 3, seed=0)
    assert em_mean.shape == (3, 4) and (em_var > 0).all()
    assert counts.sum() == len(z)
    assert _purity(assign, truth, 3) > 0.9
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py::test_fit_hmm_recovers_markov_states -q`
Expected: FAIL with `AttributeError: ... 'fit_hmm'`.

- [ ] **Step 3: Implement `fit_hmm` (+ `_viterbi`)**

```python
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
    n, d = z.shape
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
            log_alpha[t] = _logsumexp_rows(log_alpha[t - 1][:, None] + logA) + logB[t]
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py::test_fit_hmm_recovers_markov_states -q`
Expected: PASS.

- [ ] **Step 5: Register the fitter registry + commit**

Append to `regime_vol_model.py`:

```python
FITTERS = {"kmeans": fit_kmeans, "gmm": fit_gmm, "hmm": fit_hmm}
```

```bash
git add backtest/regime_vol_model.py backtest/tests/test_regime_vol_model.py
git commit -m "feat(#1080): Baum-Welch HMM fitter + Viterbi + fitter registry"
```

---

### Task 5: Latent-state → composite-name mapping

**Files:**
- Modify: `backtest/regime_vol_model.py`
- Test: `backtest/tests/test_regime_vol_model.py`

**Interfaces:**
- Consumes: `regime.map_composite_label`, `regime._DEFAULT_COMPOSITE_THRESHOLDS`, `regime.VALID_LABELS_COMPOSITE`.
- Produces: `map_latent_to_names(em_mean_z, feature_means, feature_stds, thresholds) -> (names: list[str], mapping: dict)` where `mapping[str(i)] = {"name", "centroid_raw":[...4], "volatility_rank":int}`. `volatility_rank` ranks states by centroid `range_eff` ascending (stable by index on ties).

- [ ] **Step 1: Write the failing tests**

```python
def test_map_latent_to_names_uses_canonical_boundaries_and_is_deterministic():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(4); std = np.ones(4)
    # state 0 raw centroid: tiny move, low adx, narrow range -> ranging_quiet
    # state 1 raw centroid: big positive move, high adx, clean -> trending_up_clean
    em = np.array([[0.0, 0.0, 0.1, 0.0],
                   [0.5, 0.5, 0.9, 40.0]], dtype=float)
    names, mapping = rvm.map_latent_to_names(em, mean, std, dict(TH))
    assert names == ["ranging_quiet", "trending_up_clean"]
    from regime import VALID_LABELS_COMPOSITE
    assert all(nm in VALID_LABELS_COMPOSITE for nm in names)
    # deterministic
    names2, _ = rvm.map_latent_to_names(em, mean, std, dict(TH))
    assert names2 == names
    # centroid_raw round-trips through (mean,std) == identity here
    assert mapping["1"]["centroid_raw"] == [0.5, 0.5, 0.9, 40.0]


def test_volatility_rank_orders_by_range_eff():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(4); std = np.ones(4)
    em = np.array([[0.0, 0.8, 0.1, 10.0],    # widest range
                   [0.0, 0.01, 0.1, 10.0],   # narrowest
                   [0.0, 0.3, 0.1, 10.0]], dtype=float)
    _, mapping = rvm.map_latent_to_names(em, mean, std, dict(TH))
    ranks = {int(i): m["volatility_rank"] for i, m in mapping.items()}
    assert ranks[1] < ranks[2] < ranks[0]   # narrow < mid < wide


def test_map_composite_label_monotone_in_range_within_ranging():
    # holding move below threshold + low adx (ranging family), growing range_eff
    # past range_pct must move quiet -> volatile, never the reverse.
    from regime import map_composite_label, _DEFAULT_COMPOSITE_THRESHOLDS as TH
    quiet = map_composite_label(0.0, 5.0, 0.0, 0.1, dict(TH))
    volatile = map_composite_label(0.0, 5.0, 0.9, 0.1, dict(TH))
    assert quiet == "ranging_quiet"
    assert volatile == "ranging_volatile"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -k "map_latent or volatility_rank or monotone" -q`
Expected: FAIL with `AttributeError: ... 'map_latent_to_names'` (the third test, which only uses existing `map_composite_label`, passes — that is fine; it pins the property the mapping relies on).

- [ ] **Step 3: Implement `map_latent_to_names`**

```python
def map_latent_to_names(em_mean_z, feature_means, feature_stds, thresholds):
    from regime import map_composite_label
    em_mean_z = np.asarray(em_mean_z, dtype=float)
    mean = np.asarray(feature_means, dtype=float)
    std = np.asarray(feature_stds, dtype=float)
    raw = em_mean_z * std + mean                       # un-standardize centroids -> raw features
    # volatility_rank by range_eff (col 1), ascending, stable on ties
    order = sorted(range(len(raw)), key=lambda i: (raw[i, 1], i))
    rank = {i: r for r, i in enumerate(order)}
    names, mapping = [], {}
    for i in range(len(raw)):
        # map_composite_label(return_eff, adx_val, range_eff, efficiency, thresholds)
        name = map_composite_label(raw[i, 0], raw[i, 3], raw[i, 1], raw[i, 2], thresholds)
        names.append(name)
        mapping[str(i)] = {"name": name, "centroid_raw": raw[i].tolist(),
                           "volatility_rank": int(rank[i])}
    return names, mapping
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -k "map_latent or volatility_rank or monotone" -q`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add backtest/regime_vol_model.py backtest/tests/test_regime_vol_model.py
git commit -m "feat(#1080): latent-state -> composite-name mapping via canonical map_composite_label"
```

---

### Task 6: `fit_unsupervised` — assemble the model dict + contract tests (schema, decode, no-leakage, causality, bounded-window)

**Files:**
- Modify: `backtest/regime_vol_model.py`
- Test: `backtest/tests/test_regime_vol_model.py`

**Interfaces:**
- Consumes: `standardize`, `FITTERS`, `empirical_transition`, `init_distribution`, `map_latent_to_names`; `regime_hmm.forward_filter_labels`, `regime_diagnostics.score_labels`.
- Produces: `fit_unsupervised(features, *, family, k, filter_window, period=48, thresholds=None, seed=0, fitted_on=None) -> dict` (the schema in Global Constraints). `features` is the raw (NaN-bearing) `composite_feature_matrix` numpy array.

- [ ] **Step 1: Write the failing tests**

```python
REQUIRED_KEYS = {"type", "version", "fit_method", "features", "feature_means",
                 "feature_stds", "states", "latent_count", "emissions",
                 "transition", "init", "filter_window", "period", "fitted_on", "mapping"}


def _feature_blob_matrix(seed=0):
    # 3 separated blobs in raw composite-feature space, plus 5 leading NaN warmup rows
    rng = np.random.default_rng(seed)
    centers = np.array([[0.0, 0.02, 0.1, 8.0],     # ranging_quiet-ish
                        [0.4, 0.5, 0.9, 40.0],     # trending_up_clean-ish
                        [-0.4, 0.5, 0.9, 40.0]], dtype=float)
    rows = []
    for c in centers:
        rows.append(rng.normal(c, [0.02, 0.02, 0.02, 1.0], size=(150, 4)))
    feats = np.vstack(rows)
    feats = np.vstack([np.full((5, 4), np.nan), feats])   # warmup NaN rows
    return feats


@pytest.mark.parametrize("family", ["kmeans", "gmm", "hmm"])
def test_fit_unsupervised_schema_and_decodes(family):
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH, VALID_LABELS_COMPOSITE
    from regime_hmm import forward_filter_labels
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family=family, k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0,
                                 fitted_on={"symbol": "BTC/USDT", "timeframe": "1h", "window": "is"})
    assert REQUIRED_KEYS <= set(model)
    assert model["fit_method"] == family and model["latent_count"] == 3
    assert len(model["states"]) == 3 and len(model["emissions"]) == 3
    assert np.allclose(np.sum(model["transition"], axis=1), 1.0)
    assert all(s in VALID_LABELS_COMPOSITE for s in model["states"])
    labels, conf = forward_filter_labels(feats, model)        # decodes unchanged
    assert len(labels) == len(feats)
    assert set(labels[~np.isnan(feats).any(1)]).issubset(VALID_LABELS_COMPOSITE)


def test_fit_unsupervised_no_leakage_does_not_call_compute_regime_composite(monkeypatch):
    import regime
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    def _boom(*a, **k):
        raise AssertionError("fit path must not call compute_regime_composite")
    monkeypatch.setattr(regime, "compute_regime_composite", _boom)
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    assert model["latent_count"] == 3                 # fit succeeded without the hand-rule


def test_decode_is_causal_future_bars_do_not_change_past_labels():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    from regime_hmm import forward_filter_labels
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family="hmm", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    base, _ = forward_filter_labels(feats, model)
    t = 200
    mutated = feats.copy()
    mutated[t + 1:] = mutated[t + 1:] * 5.0 + 1.0      # corrupt all future bars
    after, _ = forward_filter_labels(mutated, model)
    assert list(base[: t + 1]) == list(after[: t + 1])  # labels <= t unchanged


def test_fitted_model_scores_through_score_labels():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    from regime_hmm import forward_filter_labels
    from regime_diagnostics import score_labels
    feats = _feature_blob_matrix()
    close = np.cumprod(1 + np.zeros(len(feats))) * 100 + np.arange(len(feats))  # monotone close
    model = rvm.fit_unsupervised(feats, family="gmm", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    labels, _ = forward_filter_labels(feats, model)
    rep = score_labels(close, labels, feats, target="volatility")
    assert "stability" in rep and "coverage" in rep
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -k "fit_unsupervised or causal or score_labels" -q`
Expected: FAIL with `AttributeError: ... 'fit_unsupervised'`.

- [ ] **Step 3: Implement `fit_unsupervised`**

```python
def fit_unsupervised(features, *, family, k, filter_window, period=48,
                     thresholds=None, seed=0, fitted_on=None):
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -k "fit_unsupervised or causal or score_labels" -q`
Expected: PASS (kmeans/gmm/hmm parametrized schema test + no-leakage + causality + score_labels).

- [ ] **Step 5: Add the bounded-window compatibility contract test**

The #1082 harness reads exactly these keys via `forward_filter_labels` + provenance. Pin the contract so a schema drift fails loudly here, not in promotion:

```python
def test_model_satisfies_bounded_window_and_forward_filter_contract():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    import regime_bounded_window_validate as bw   # import must not raise
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family="hmm", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0,
                                 fitted_on={"symbol": "BTC/USDT", "timeframe": "1h", "window": "is"})
    # forward_filter_labels reads exactly these; provenance reads fitted_on/period.
    for key in ("states", "feature_means", "feature_stds", "emissions",
                "init", "transition", "filter_window", "period", "fitted_on"):
        assert key in model
    prov = bw._provenance_status(model, "BTC/USDT", "1h", "oos",
                                 {"symbol": "BTC/USDT", "timeframe": "1h", "window": "oos"})
    assert prov["verified"] is True            # fitted_on stamp is read, no type gate
```

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py::test_model_satisfies_bounded_window_and_forward_filter_contract -q`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backtest/regime_vol_model.py backtest/tests/test_regime_vol_model.py
git commit -m "feat(#1080): fit_unsupervised model-dict assembly + schema/decode/no-leakage/causality/bounded-window contracts"
```

---

### Task 7: Non-degeneracy scorer + incumbent-derived thresholds + single-family CLI

**Files:**
- Modify: `backtest/regime_vol_model.py`
- Test: `backtest/tests/test_regime_vol_model.py`

**Interfaces:**
- Consumes: `regime_diagnostics.coverage`, `regime_diagnostics.stability`.
- Produces:
  - `NonDegeneracyThresholds` dataclass: `min_active_labels:int`, `max_occupancy:float`, `min_transition_rate:float`.
  - `non_degeneracy(labels, thresholds) -> {"active_labels":int,"max_occupancy":float,"transition_rate":float,"ok":bool,"reasons":[str]}`.
  - `derive_thresholds(handrule_streams: list[np.ndarray], *, active_margin=1, occupancy_margin=0.05, rate_margin=0.5) -> NonDegeneracyThresholds` — locked from the incumbent's WORST window, loosened by margins (anti-gaming).
  - CLI `main(argv)` writing one fitted model JSON via `--out`.

- [ ] **Step 1: Write the failing tests**

```python
def test_non_degeneracy_flags_constant_stream():
    from regime_vol_model import NonDegeneracyThresholds, non_degeneracy
    thr = NonDegeneracyThresholds(min_active_labels=2, max_occupancy=0.8,
                                  min_transition_rate=0.05)
    constant = np.array(["ranging_quiet"] * 500, dtype=object)
    rep = non_degeneracy(constant, thr)
    assert rep["ok"] is False
    assert rep["active_labels"] == 1
    assert "min_active_labels" in " ".join(rep["reasons"])


def test_non_degeneracy_passes_healthy_stream():
    from regime_vol_model import NonDegeneracyThresholds, non_degeneracy
    thr = NonDegeneracyThresholds(min_active_labels=2, max_occupancy=0.9,
                                  min_transition_rate=0.01)
    rng = np.random.default_rng(0)
    stream = rng.choice(["ranging_quiet", "trending_up_clean", "ranging_volatile"], size=600)
    rep = non_degeneracy(stream.astype(object), thr)
    assert rep["ok"] is True and rep["reasons"] == []


def test_derive_thresholds_is_looser_than_incumbent_worst_window():
    from regime_vol_model import derive_thresholds, non_degeneracy
    # incumbent stream A: 3 active, max occ ~0.5, healthy switching
    a = np.array((["x", "y", "z"] * 200), dtype=object)
    # incumbent stream B: 2 active, max occ ~0.75 (the WORST window on occupancy)
    b = np.array((["x"] * 150 + ["y"] * 50) * 2, dtype=object)
    thr = derive_thresholds([a, b])
    # derived guard must ADMIT both incumbent windows (looser than worst)
    assert non_degeneracy(a, thr)["ok"] is True
    assert non_degeneracy(b, thr)["ok"] is True
    # and be strictly looser than B's measured occupancy
    occ_b = max(c["pct"] for c in __import__("regime_diagnostics").coverage(b).values())
    assert thr.max_occupancy >= occ_b
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -k "non_degeneracy or derive_thresholds" -q`
Expected: FAIL with `ImportError: cannot import name 'NonDegeneracyThresholds'`.

- [ ] **Step 3: Implement the scorer, threshold derivation, and CLI**

```python
from dataclasses import dataclass


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
    p.add_argument("--window", default="is", help=f"known: {', '.join(WINDOWS)}")
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -k "non_degeneracy or derive_thresholds" -q`
Expected: PASS (3 tests).

- [ ] **Step 5: Verify the module compiles + CLI parses**

Run: `uv run --no-sync python -m py_compile backtest/regime_vol_model.py`
Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
git add backtest/regime_vol_model.py backtest/tests/test_regime_vol_model.py
git commit -m "feat(#1080): non-degeneracy scorer + incumbent-locked thresholds + single-family CLI"
```

---

### Task 8: Walk-forward bake-off driver + reproducible evidence script

**Files:**
- Create: `backtest/research/regime_1080_unsupervised_vol_model.py`
- Test: `backtest/tests/test_regime_vol_model.py`

**Interfaces:**
- Consumes: `regime_vol_model.{fit_unsupervised, non_degeneracy, derive_thresholds, FITTERS}`, `regime_diagnostics.run_window`, `regime_calibrate.gate_verdict`, `regime.compute_regime_composite`, `regime_hmm.forward_filter_labels`.
- Produces (in the research module):
  - `select_winner(candidates: list[dict]) -> dict|None` — pure ranking; eligible = `verdict.ship and non_degenerate_all_windows`; winner = max held-out `model_kruskal_h`, stability gain tiebreak.
  - `run_bakeoff(symbol, timeframe, *, in_sample="is", held_out="oos", eval_windows=DEFAULT_WINDOWS, families=("hmm","gmm","kmeans"), k_range=range(2,8), period=48, filter_window=64, seed=0) -> dict`.

- [ ] **Step 1: Write the failing test (pure ranking — no data fetch)**

```python
def test_select_winner_prefers_eligible_high_separation():
    import importlib.util, os
    here = os.path.dirname(os.path.abspath(__file__))
    path = os.path.join(here, "..", "research", "regime_1080_unsupervised_vol_model.py")
    spec = importlib.util.spec_from_file_location("regime_1080_research", path)
    mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
    cands = [
        {"family": "hmm", "k": 3, "verdict": {"ship": True},
         "non_degenerate_all": True, "model_kruskal_h": 90.0, "stability_gain": 0.05},
        {"family": "gmm", "k": 4, "verdict": {"ship": True},
         "non_degenerate_all": True, "model_kruskal_h": 120.0, "stability_gain": 0.03},
        {"family": "kmeans", "k": 5, "verdict": {"ship": False},   # ineligible: didn't ship
         "non_degenerate_all": True, "model_kruskal_h": 999.0, "stability_gain": 0.9},
        {"family": "hmm", "k": 7, "verdict": {"ship": True},
         "non_degenerate_all": False, "model_kruskal_h": 999.0, "stability_gain": 0.9},  # degenerate
    ]
    win = mod.select_winner(cands)
    assert win["family"] == "gmm" and win["k"] == 4      # highest sep among eligible


def test_select_winner_returns_none_when_no_eligible():
    import importlib.util, os
    here = os.path.dirname(os.path.abspath(__file__))
    path = os.path.join(here, "..", "research", "regime_1080_unsupervised_vol_model.py")
    spec = importlib.util.spec_from_file_location("regime_1080_research2", path)
    mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
    assert mod.select_winner([{"family": "hmm", "k": 3, "verdict": {"ship": False},
                               "non_degenerate_all": True, "model_kruskal_h": 1.0,
                               "stability_gain": 0.0}]) is None
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -k select_winner -q`
Expected: FAIL with `FileNotFoundError` / `No module named` for the research file.

- [ ] **Step 3: Implement the research driver**

```python
# backtest/research/regime_1080_unsupervised_vol_model.py
"""Reproducible evidence for #1080: a genuinely UNSUPERVISED volatility-state regime model
(HMM/GMM/k-means candidates) learned from the composite feature matrix, mapped to the 7
composite labels, selected by a walk-forward bake-off against the #1078 forward-volatility gate.

Each candidate (family x latent-count K) is fit on the in-sample window, decoded causally on
the held-out window, scored on forward VOLATILITY separation + stability, run through
regime_calibrate.gate_verdict, and checked for non-degeneracy on every eval window. The winner
is the highest held-out separation among gate-passing, non-degenerate candidates.

Run (needs the OHLCV cache reachable from shared_tools/):

    uv run --no-sync python backtest/research/regime_1080_unsupervised_vol_model.py
    uv run --no-sync python backtest/research/regime_1080_unsupervised_vol_model.py \
        --symbol ETH/USDT --timeframe 1h --json /tmp/regime_1080_eth.json

Parameterized by --symbol/--timeframe so #1083 (multi-asset) can reuse it. Read-only;
no live or Go path touched. Economic payoff (vs flat-ATR) is #1081, not decided here.
"""
from __future__ import annotations
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
from regime import (compute_regime_composite, composite_feature_matrix,
                    _DEFAULT_COMPOSITE_THRESHOLDS)
from data_fetcher import load_cached_data
from eval_windows import WINDOWS, PLATFORM
from regime_diagnostics import run_window
from regime_calibrate import gate_verdict
from regime_hmm import forward_filter_labels
import regime_vol_model as rvm

DEFAULT_WINDOWS = ("is", "oos", "2023", "2024", "2025H1")


def select_winner(candidates):
    """Eligible = gate ship AND non-degenerate on every eval window. Winner = max held-out
    model_kruskal_h, stability_gain as tiebreak. Returns None when none are eligible."""
    eligible = [c for c in candidates
                if c.get("verdict", {}).get("ship") and c.get("non_degenerate_all")]
    if not eligible:
        return None
    return max(eligible, key=lambda c: (c["model_kruskal_h"], c["stability_gain"]))


def _handrule_streams(symbol, timeframe, eval_windows, period, th):
    streams = {}
    for w in eval_windows:
        start, end = WINDOWS[w]
        df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                              start_date=start, end_date=end)
        feats = composite_feature_matrix(df, period, th).to_numpy()
        valid = ~np.isnan(feats).any(1)
        labels = compute_regime_composite(df, period=period, thresholds=th)["regime"].to_numpy()
        streams[w] = labels[valid]
    return streams


def _model_label_stream(symbol, timeframe, window, model, period, th):
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    feats = composite_feature_matrix(df, period, th).to_numpy()
    valid = ~np.isnan(feats).any(1)
    labels, _ = forward_filter_labels(feats, model)
    return np.asarray(labels, dtype=object)[valid]


def run_bakeoff(symbol="BTC/USDT", timeframe="1h", *, in_sample="is", held_out="oos",
                eval_windows=DEFAULT_WINDOWS, families=("hmm", "gmm", "kmeans"),
                k_range=range(2, 8), period=48, filter_window=64, seed=0):
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    hr_streams = _handrule_streams(symbol, timeframe, eval_windows, period, th)
    thresholds = rvm.derive_thresholds(list(hr_streams.values()))
    hr_held = run_window(symbol, timeframe, held_out, model=None, seed=seed, target="volatility")
    hr_tr = hr_held["stability"]["transition_rate"]
    # fit window feature matrix once
    start, end = WINDOWS[in_sample]
    fit_df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    fit_feats = composite_feature_matrix(fit_df, period, th).to_numpy()

    candidates = []
    for family in families:
        for k in k_range:
            model = rvm.fit_unsupervised(fit_feats, family=family, k=k,
                                         filter_window=filter_window, period=period,
                                         thresholds=th, seed=seed,
                                         fitted_on={"symbol": symbol, "timeframe": timeframe,
                                                    "window": in_sample})
            md = run_window(symbol, timeframe, held_out, model=model, seed=seed, target="volatility")
            verdict = gate_verdict(hr_held, md)
            nd = {w: rvm.non_degeneracy(_model_label_stream(symbol, timeframe, w, model, period, th),
                                        thresholds) for w in eval_windows}
            candidates.append({
                "family": family, "k": k, "verdict": verdict,
                "model_kruskal_h": md["h4"]["separation"]["kruskal_h"],
                "model_p_value": md["h4"]["significance"]["p_value"],
                "stability_gain": float(hr_tr - md["stability"]["transition_rate"]),
                "coverage": md["coverage"],
                "non_degeneracy": {w: nd[w] for w in eval_windows},
                "non_degenerate_all": all(nd[w]["ok"] for w in eval_windows),
                "states": model["states"], "mapping": model["mapping"],
            })
    winner = select_winner(candidates)
    return {
        "symbol": symbol, "timeframe": timeframe, "in_sample": in_sample,
        "held_out": held_out, "target": "volatility",
        "non_degeneracy_thresholds": vars(thresholds),
        "handrule_held_out": {"kruskal_h": hr_held["h4"]["separation"]["kruskal_h"],
                              "p_value": hr_held["h4"]["significance"]["p_value"],
                              "transition_rate": hr_tr,
                              "abstained": bool(hr_held["h4"]["significance"]["p_value"]
                                                > __import__("regime_calibrate").SIGNIFICANCE_ALPHA)},
        "candidates": candidates,
        "winner": ({"family": winner["family"], "k": winner["k"]} if winner else None),
    }


def build_parser():
    import argparse
    p = argparse.ArgumentParser(description="#1080 unsupervised vol-regime bake-off")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--in-sample", default="is")
    p.add_argument("--held-out", default="oos")
    p.add_argument("--period", type=int, default=48)
    p.add_argument("--filter-window", type=int, default=64)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--json", default=None, help="write the bake-off report JSON here")
    return p


def main(argv=None):
    import json
    args = build_parser().parse_args(argv)
    report = run_bakeoff(args.symbol, args.timeframe, in_sample=args.in_sample,
                         held_out=args.held_out, period=args.period,
                         filter_window=args.filter_window, seed=args.seed)
    text = json.dumps(report, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    w = report["winner"]
    print(f"\nWINNER: {w}" if w else "\nWINNER: none eligible (no gate-passing, "
          "non-degenerate candidate on this window)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 4: Run the ranking tests to verify they pass**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -k select_winner -q`
Expected: PASS (2 tests).

- [ ] **Step 5: Compile-check both files + run the full new test file**

Run:
```bash
uv run --no-sync python -m py_compile backtest/regime_vol_model.py backtest/research/regime_1080_unsupervised_vol_model.py
uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py -q
```
Expected: compile clean; all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add backtest/research/regime_1080_unsupervised_vol_model.py backtest/tests/test_regime_vol_model.py
git commit -m "feat(#1080): walk-forward bake-off driver + reproducible evidence script"
```

---

### Task 9: Live data smoke run + regression guard in the broader suite

**Files:**
- Modify: `backtest/tests/test_regime_vol_model.py` (add a guarded live-data smoke that skips when the OHLCV cache is absent).

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Add a cache-guarded smoke test**

```python
def test_bakeoff_smoke_on_cached_data_if_available():
    import importlib.util
    here = os.path.dirname(os.path.abspath(__file__))
    path = os.path.join(here, "..", "research", "regime_1080_unsupervised_vol_model.py")
    spec = importlib.util.spec_from_file_location("regime_1080_smoke", path)
    mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
    try:
        report = mod.run_bakeoff("BTC/USDT", "1h", families=("kmeans",),
                                 k_range=range(3, 4), eval_windows=("is", "oos"))
    except Exception as e:  # noqa: BLE001 — no cached OHLCV in CI -> skip, not fail
        pytest.skip(f"no cached OHLCV / data path unavailable: {e}")
    assert "candidates" in report and len(report["candidates"]) == 1
    assert "non_degeneracy_thresholds" in report
```

- [ ] **Step 2: Run it (skips cleanly without data, passes with it)**

Run: `uv run --no-sync python -m pytest backtest/tests/test_regime_vol_model.py::test_bakeoff_smoke_on_cached_data_if_available -q`
Expected: PASS or SKIP (never FAIL).

- [ ] **Step 3: Run the full backtest suite to confirm no collateral breakage**

Run: `uv run --no-sync python -m pytest backtest/ -q`
Expected: all pass (the new file adds tests; no existing test changes).

- [ ] **Step 4: If cached data exists locally, run the real bake-off once and eyeball the winner**

Run: `uv run --no-sync python backtest/research/regime_1080_unsupervised_vol_model.py --json /tmp/regime_1080_btc.json`
Expected: JSON report; stderr prints WINNER (a family+K) or "none eligible" with the abstain reason surfaced. This is the empirical result the issue's evidence criterion asks for — record it in the PR body.

- [ ] **Step 5: Commit**

```bash
git add backtest/tests/test_regime_vol_model.py
git commit -m "test(#1080): cache-guarded bake-off smoke + full-suite regression guard"
```

---

## Self-Review

**Spec coverage:**
- Discover states without hand-rule target → Tasks 2-4 + Task 6 (`fit_unsupervised` standardizes raw features, never calls `compute_regime_composite`; no-leakage test in Task 6). ✓
- Deterministic latent→name mapping into `VALID_LABELS_COMPOSITE` → Task 5. ✓
- Clears #1078 volatility gate OOS, non-degenerate → Tasks 7 (guards) + 8 (gate wiring, selection). ✓
- Latent-count is a choice, swept 2-7, selected by held-out behavior → Task 8 `k_range`, `select_winner`. ✓
- No OOS leakage in mapping/scoring → Task 5 (centroids only) + Task 6 causality test. ✓
- Causal decoder discipline → reuses `forward_filter_labels`; Task 6 causality regression. ✓
- Gate-ready report through `score_labels(target="volatility")` + `gate_verdict` → Tasks 6, 8. ✓
- #1082 bounded-window compatibility (no adapter needed) → Task 6 contract test. ✓
- Named non-degeneracy thresholds, derived from incumbent + locked pre-scoring → Task 7 `derive_thresholds`. ✓
- Reproducible evidence script in #1073 style → Task 8. ✓
- Tests: fit, causal decode, mapping, no-leakage, report shape, non-degeneracy, bounded-window → Tasks 1-9. ✓
- Known caveat (gate abstains when incumbent not significant) surfaced → Task 8 report `handrule_held_out.abstained` + stderr message. ✓
- Out of scope (live/Go, economics #1081, multi-asset #1083) — untouched. ✓

**Placeholder scan:** none — every code step shows complete code; no TBD/TODO/"handle edge cases".

**Type consistency:** the fitter signature `(z,k,*,seed,...) -> (assign,em_mean,em_var,counts)` is identical across Tasks 2/3/4 and consumed by `fit_unsupervised` (Task 6). `non_degeneracy`/`derive_thresholds`/`NonDegeneracyThresholds` names match between Task 7 definition and Task 8 use. `select_winner` candidate dict keys (`verdict.ship`, `non_degenerate_all`, `model_kruskal_h`, `stability_gain`) match between Task 8's test and `run_bakeoff` output.

---
Created with LLM: Opus 4.8 | xhigh | Harness: Claude Code
