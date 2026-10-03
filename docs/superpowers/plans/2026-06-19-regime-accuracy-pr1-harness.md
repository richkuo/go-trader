# Regime Accuracy PR1 — Measurement Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the read-only measurement + calibration harness that quantifies 7-state regime quality (forward-return separation, stability, significance) and fits a label-anchored HMM offline — so we can decide, on data, whether the PR2 classifier change is worth building.

**Architecture:** Pure numpy/pandas scorers and a closed-form label-anchored HMM live in `backtest/`, reusing the versioned `DATASETS`/`WINDOWS` and `load_cached_data` data path from the existing `eval_windows.py`/`exit_diagnostics.py` harness. One additive feature-extraction function is added to `shared_tools/regime.py` (no change to existing behavior) so offline features are byte-consistent with the live labeler. Two CLIs (`regime_diagnostics.py`, `regime_calibrate.py`) tie it together with `--json` output. No live code path, no Go changes, no parity risk — that is all PR2.

**Tech Stack:** Python 3.12 (`uv run --no-sync python`), numpy 2.4 + pandas 3.0 only (no scipy/sklearn), pytest.

## Global Constraints

- **Dependencies:** numpy + pandas ONLY. No scipy, no sklearn, no new dependency. Significance comes from the block-shuffle permutation null (empirical p), not an analytic distribution. The unsupervised yardstick is numpy k-means.
- **Pure aggregation:** every scorer/fit/filter function operates on numpy arrays / dicts and is unit-tested WITHOUT data access (same architecture as `exit_diagnostics.py`). Only the two CLI `main()`s touch `load_cached_data`.
- **Determinism:** any randomness uses an explicit `numpy.random.default_rng(seed)` with a `--seed` flag (default `0`). No bare `np.random.*`.
- **Read-only / no parity risk:** PR1 does not modify any existing function in `shared_tools/regime.py`, any live path, the backtester, or any Go file. The only `regime.py` change is ONE additive new function.
- **Pre-registered gate:** the primary separation metric is **Kruskal–Wallis H at horizon h=4**; per-state stats are exploratory and Benjamini–Hochberg corrected.
- **Commands run from repo root:** `uv run --no-sync python ...`; `uv run --no-sync python -m pytest ...`; `py_compile` after each Python file.
- **Footer:** every commit message ends with `LLM: Opus 4.8 | high | Harness: subagent-driven-development` (or `executing-plans` if inline). No `Co-authored-by` trailer.

---

### Task 1: Additive per-bar feature matrix in `regime.py`

Extract the per-bar composite feature tuple (`return_eff`, `range_eff`, `efficiency`, `adx`) the hand-rule consumes, as a new pure function — so the offline HMM fits on features byte-consistent with `map_composite_label`'s inputs. Additive only; `compute_regime_composite` is untouched.

**Files:**
- Modify: `shared_tools/regime.py` (append one function near `compute_regime_composite`, ~line 289)
- Test: `shared_tools/test_regime_features.py` (create)

**Interfaces:**
- Consumes: existing `_composite_efficiency_metrics`, `compute_regime`, `standard_atr`, `COMPOSITE_ADX_PERIOD_CAP`, `_DEFAULT_COMPOSITE_THRESHOLDS`, `map_composite_label`.
- Produces: `composite_feature_matrix(df: pd.DataFrame, period: int, thresholds: dict | None = None) -> pd.DataFrame` with columns `["return_eff", "range_eff", "efficiency", "adx"]`, same index as `df`, `NaN` rows for warmup (`i < period`) and `atr<=0` bars.

- [ ] **Step 1: Write the failing test** — features fed back through `map_composite_label` must reproduce `compute_regime_composite`'s labels exactly (consistency), and warmup rows are NaN.

```python
# shared_tools/test_regime_features.py
import numpy as np
import pandas as pd
from regime import (
    composite_feature_matrix,
    compute_regime_composite,
    map_composite_label,
    _DEFAULT_COMPOSITE_THRESHOLDS,
)


def _synth(n=300, seed=1):
    rng = np.random.default_rng(seed)
    steps = rng.normal(0, 1, n).cumsum()
    close = 100 + steps
    high = close + np.abs(rng.normal(0, 0.5, n))
    low = close - np.abs(rng.normal(0, 0.5, n))
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    return pd.DataFrame({"open": close, "high": high, "low": low, "close": close}, index=idx)


def test_feature_matrix_reproduces_handrule_labels():
    df = _synth()
    period = 48
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats = composite_feature_matrix(df, period, th)
    labels = compute_regime_composite(df, period=period, thresholds=th)["regime"]
    assert list(feats.columns) == ["return_eff", "range_eff", "efficiency", "adx"]
    assert feats.iloc[:period].isna().all().all()  # warmup is NaN
    for i in range(period, len(df)):
        row = feats.iloc[i]
        if row.isna().any():
            continue  # atr<=0 bar: labeler leaves default, matrix is NaN — consistent
        got = map_composite_label(row["return_eff"], row["adx"], row["range_eff"], row["efficiency"], th)
        assert got == labels.iloc[i], f"bar {i}: {got} != {labels.iloc[i]}"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest shared_tools/test_regime_features.py -v`
Expected: FAIL — `ImportError: cannot import name 'composite_feature_matrix'`.

- [ ] **Step 3: Write minimal implementation** — append to `shared_tools/regime.py` after `compute_regime_composite`.

```python
def composite_feature_matrix(
    df: pd.DataFrame,
    period: int,
    thresholds: dict[str, float] | None = None,
) -> pd.DataFrame:
    """Per-bar composite feature tuple (return_eff, range_eff, efficiency, adx).

    Additive, offline-only (#1065 PR1). Mirrors compute_regime_composite's loop
    but emits the features map_composite_label consumes instead of the label, so
    an offline model fits on byte-consistent inputs. Warmup (i < period) and
    atr<=0 bars are NaN.
    """
    th = {**_DEFAULT_COMPOSITE_THRESHOLDS, **(thresholds or {})}
    cols = ["return_eff", "range_eff", "efficiency", "adx"]
    out = pd.DataFrame(float("nan"), index=df.index, columns=cols)
    n = len(df)
    if n == 0:
        return out
    adx_period = min(period, COMPOSITE_ADX_PERIOD_CAP)
    adx_df = compute_regime(df, period=adx_period, adx_threshold=th["adx"])
    atr_series = standard_atr(df, period=period)
    for i in range(period, n):
        window = df.iloc[i - period + 1 : i + 1]
        atr_val = float(atr_series.iloc[i]) if i < len(atr_series) else 0.0
        if not (atr_val > 0):
            continue
        eff = _composite_efficiency_metrics(window, atr_val, period)
        out.iat[i, 0] = eff["return_eff"]
        out.iat[i, 1] = eff["range_eff"]
        out.iat[i, 2] = eff["efficiency"]
        out.iat[i, 3] = float(adx_df["adx"].iloc[i])
    return out
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest shared_tools/test_regime_features.py -v`
Expected: PASS. Then `uv run --no-sync python -m py_compile shared_tools/regime.py`.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
git add shared_tools/regime.py shared_tools/test_regime_features.py
git commit -m "feat(#1065): composite_feature_matrix — additive per-bar feature extraction

LLM: Opus 4.8 | high | Harness: subagent-driven-development"
```

---

### Task 2: Pure stats primitives `backtest/regime_stats.py`

Dependency-free numpy implementations of the statistics the diagnostic needs: average-rank, Kruskal–Wallis H (with tie correction), and Benjamini–Hochberg. No scipy.

**Files:**
- Create: `backtest/regime_stats.py`
- Test: `backtest/tests/test_regime_stats.py`

**Interfaces:**
- Produces:
  - `rank_data(x: np.ndarray) -> np.ndarray` — 1-based average ranks, ties averaged.
  - `kruskal_h(groups: list[np.ndarray]) -> float` — tie-corrected H statistic (NOT a p-value; significance is permutation-based downstream).
  - `benjamini_hochberg(pvals: list[float], alpha: float = 0.05) -> list[bool]` — per-test reject flags at FDR `alpha`.

- [ ] **Step 1: Write the failing test**

```python
# backtest/tests/test_regime_stats.py
import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_stats import rank_data, kruskal_h, benjamini_hochberg


def test_rank_data_averages_ties():
    # values [10, 20, 20, 40] -> ranks [1, 2.5, 2.5, 4]
    np.testing.assert_allclose(rank_data(np.array([10, 20, 20, 40.0])), [1, 2.5, 2.5, 4])


def test_kruskal_h_zero_when_identical():
    g = np.array([1.0, 2, 3])
    assert abs(kruskal_h([g, g.copy(), g.copy()])) < 1e-9


def test_kruskal_h_large_when_separated():
    a = np.array([1.0, 2, 3, 4]); b = np.array([100.0, 101, 102, 103])
    assert kruskal_h([a, b]) > 5.0  # strongly separated -> large H


def test_benjamini_hochberg_basic():
    # one clearly significant, rest null
    flags = benjamini_hochberg([0.001, 0.4, 0.6, 0.8], alpha=0.05)
    assert flags == [True, False, False, False]
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_stats.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'regime_stats'`.

- [ ] **Step 3: Write minimal implementation**

```python
# backtest/regime_stats.py
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_stats.py -v`
Expected: PASS. Then `uv run --no-sync python -m py_compile backtest/regime_stats.py`.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
git add backtest/regime_stats.py backtest/tests/test_regime_stats.py
git commit -m "feat(#1065): pure-numpy regime stats (rank, kruskal-h, BH-FDR)

LLM: Opus 4.8 | high | Harness: subagent-driven-development"
```

---

### Task 3: Core diagnostic scorers `backtest/regime_diagnostics.py` (coverage / separation / stability)

The pure aggregation core: forward returns, coverage, per-state separation with the primary KW-H metric, and stability.

**Files:**
- Create: `backtest/regime_diagnostics.py` (scorers only this task; CLI in Task 7)
- Test: `backtest/tests/test_regime_diagnostics.py`

**Interfaces:**
- Consumes: `regime_stats.kruskal_h`, `regime_stats.benjamini_hochberg`.
- Produces:
  - `forward_returns(close: np.ndarray, horizon: int) -> np.ndarray` — `close[t+h]/close[t]-1`, NaN at the tail.
  - `coverage(labels: np.ndarray) -> dict` — `{state: {"count": int, "pct": float}}`.
  - `separation(labels: np.ndarray, fwd: np.ndarray) -> dict` — `{"kruskal_h": float, "per_state": {state: {"n","mean","std","hit_rate"}}}`. Bars with NaN `fwd` dropped.
  - `stability(labels: np.ndarray) -> dict` — `{"transition_rate": float, "flips": int, "mean_dwell": {state: float}}`.

- [ ] **Step 1: Write the failing test**

```python
# backtest/tests/test_regime_diagnostics.py
import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_diagnostics import forward_returns, coverage, separation, stability


def test_forward_returns_horizon():
    close = np.array([100.0, 110, 121])
    fr = forward_returns(close, 1)
    np.testing.assert_allclose(fr[:2], [0.10, 0.10])
    assert np.isnan(fr[-1])


def test_coverage_counts():
    labels = np.array(["a", "a", "b", "a"])
    cov = coverage(labels)
    assert cov["a"]["count"] == 3 and abs(cov["a"]["pct"] - 0.75) < 1e-9


def test_separation_directional():
    labels = np.array(["up"] * 50 + ["down"] * 50)
    fwd = np.concatenate([np.full(50, 0.02), np.full(50, -0.02)])
    sep = separation(labels, fwd)
    assert sep["per_state"]["up"]["mean"] > 0 > sep["per_state"]["down"]["mean"]
    assert sep["kruskal_h"] > 5.0


def test_stability_transition_rate():
    labels = np.array(["a", "a", "b", "b", "a"])
    st = stability(labels)
    assert abs(st["transition_rate"] - 0.5) < 1e-9  # 2 flips over 4 transitions
    assert st["flips"] == 2
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_diagnostics.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'regime_diagnostics'`.

- [ ] **Step 3: Write minimal implementation** (scorers only; module header + CLI added in Task 7).

```python
# backtest/regime_diagnostics.py
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_diagnostics.py -v`
Expected: PASS. Then `uv run --no-sync python -m py_compile backtest/regime_diagnostics.py`.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
git add backtest/regime_diagnostics.py backtest/tests/test_regime_diagnostics.py
git commit -m "feat(#1065): regime diagnostic scorers (coverage/separation/stability)

LLM: Opus 4.8 | high | Harness: subagent-driven-development"
```

---

### Task 4: Significance control + yardstick (block-shuffle null, numpy k-means)

Add the permutation null that turns KW-H into an empirical p-value, and the dependency-free k-means yardstick that bounds the separation ceiling the 7-named-state anchoring forgoes.

**Files:**
- Modify: `backtest/regime_diagnostics.py` (append functions)
- Test: `backtest/tests/test_regime_diagnostics.py` (append tests)

**Interfaces:**
- Consumes: `separation` (Task 3).
- Produces:
  - `block_shuffle_pvalue(labels, fwd, block_len, n_perm=200, seed=0) -> dict` — `{"kruskal_h": float, "p_value": float, "block_len": int, "n_perm": int}`. Shuffles label blocks of `block_len`, recomputes KW-H, empirical `p = mean(H_perm >= H_obs)`.
  - `kmeans_yardstick(features, fwd, k_range=(2,3,4,5,6,7), seed=0) -> dict` — `{k: {"kruskal_h": float}}`; numpy Lloyd's k-means on standardized features (NaN rows dropped), KW-H of forward returns by cluster.

- [ ] **Step 1: Write the failing test** (append).

```python
def test_block_shuffle_pvalue_separated_is_significant():
    from regime_diagnostics import block_shuffle_pvalue
    labels = np.array(["up"] * 60 + ["down"] * 60)
    fwd = np.concatenate([np.full(60, 0.02), np.full(60, -0.02)])
    out = block_shuffle_pvalue(labels, fwd, block_len=5, n_perm=100, seed=0)
    assert out["p_value"] < 0.05 and out["kruskal_h"] > 5.0


def test_block_shuffle_pvalue_noise_not_significant():
    from regime_diagnostics import block_shuffle_pvalue
    rng = np.random.default_rng(0)
    labels = np.array(["a", "b"])[rng.integers(0, 2, 200)]
    fwd = rng.normal(0, 0.01, 200)
    out = block_shuffle_pvalue(labels, fwd, block_len=10, n_perm=100, seed=0)
    assert out["p_value"] > 0.05


def test_kmeans_yardstick_runs():
    from regime_diagnostics import kmeans_yardstick
    rng = np.random.default_rng(0)
    feats = np.vstack([rng.normal(0, 1, (100, 4)), rng.normal(5, 1, (100, 4))])
    fwd = np.concatenate([np.full(100, 0.02), np.full(100, -0.02)])
    out = kmeans_yardstick(feats, fwd, k_range=(2, 3), seed=0)
    assert out[2]["kruskal_h"] > 5.0
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_diagnostics.py -k "shuffle or yardstick" -v`
Expected: FAIL — `ImportError: cannot import name 'block_shuffle_pvalue'`.

- [ ] **Step 3: Write minimal implementation** (append to `regime_diagnostics.py`).

```python
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
```

Add `from regime_stats import kruskal_h` to the existing import line if not already present (it is imported in Task 3).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_diagnostics.py -v`
Expected: PASS (all). Then `uv run --no-sync python -m py_compile backtest/regime_diagnostics.py`.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
git add backtest/regime_diagnostics.py backtest/tests/test_regime_diagnostics.py
git commit -m "feat(#1065): block-shuffle significance + numpy k-means yardstick

LLM: Opus 4.8 | high | Harness: subagent-driven-development"
```

---

### Task 5: Label-anchored HMM fit `backtest/regime_hmm.py`

Closed-form fit: standardize features, per-state diagonal-Gaussian MLE on hand-rule labels (variance floor for degenerate states), Laplace-smoothed transition counts, stationary init. No EM.

**Files:**
- Create: `backtest/regime_hmm.py` (fit + stationary this task; filter in Task 6)
- Test: `backtest/tests/test_regime_hmm.py`

**Interfaces:**
- Produces:
  - `stationary_distribution(transition: np.ndarray) -> np.ndarray` — left eigenvector for eigenvalue 1, normalized.
  - `fit_label_anchored_hmm(features, labels, states, *, filter_window, var_floor=1e-3, laplace=1.0, fitted_on=None) -> dict` — returns the `model` dict matching the spec schema (`feature_means`, `feature_stds`, `states`, `emissions` [{"mean","var","n"}], `transition`, `init`, `filter_window`, `type`, `version`, `features`, `fitted_on`). NaN feature rows dropped before fitting.

- [ ] **Step 1: Write the failing test**

```python
# backtest/tests/test_regime_hmm.py
import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_hmm import fit_label_anchored_hmm, stationary_distribution

STATES = ["s0", "s1"]


def test_stationary_distribution_sums_to_one():
    A = np.array([[0.9, 0.1], [0.2, 0.8]])
    pi = stationary_distribution(A)
    assert abs(pi.sum() - 1.0) < 1e-9 and (pi > 0).all()
    np.testing.assert_allclose(pi @ A, pi, atol=1e-9)  # stationary


def test_fit_shapes_and_determinism():
    rng = np.random.default_rng(0)
    feats = np.vstack([rng.normal(0, 1, (80, 4)), rng.normal(4, 1, (80, 4))])
    labels = np.array(["s0"] * 80 + ["s1"] * 80, dtype=object)
    m1 = fit_label_anchored_hmm(feats, labels, STATES, filter_window=16)
    m2 = fit_label_anchored_hmm(feats, labels, STATES, filter_window=16)
    assert m1["states"] == STATES
    assert len(m1["emissions"]) == 2
    assert np.array(m1["transition"]).shape == (2, 2)
    assert m1["emissions"][0]["mean"] == m2["emissions"][0]["mean"]  # deterministic
    # standardized means: s0 cluster ~ negative, s1 ~ positive on each feature
    assert m1["emissions"][0]["mean"][0] < m1["emissions"][1]["mean"][0]


def test_fit_drops_nan_rows():
    feats = np.array([[np.nan, np.nan, np.nan, np.nan], [0.0, 0, 0, 0], [1.0, 1, 1, 1]])
    labels = np.array(["s0", "s0", "s1"], dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=2)
    assert m["emissions"][0]["n"] == 1  # the NaN s0 row was dropped
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_hmm.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'regime_hmm'`.

- [ ] **Step 3: Write minimal implementation**

```python
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_hmm.py -v`
Expected: PASS. Then `uv run --no-sync python -m py_compile backtest/regime_hmm.py`.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
git add backtest/regime_hmm.py backtest/tests/test_regime_hmm.py
git commit -m "feat(#1065): closed-form label-anchored HMM fit + stationary init

LLM: Opus 4.8 | high | Harness: subagent-driven-development"
```

---

### Task 6: Causal forward-filter inference (windowed-from-init, NaN carry, look-ahead safe)

The inference math: a windowed forward-filter that produces a per-bar label + confidence. This is the function PR2 will port into `regime.py`; here it is proven in isolation with look-ahead and NaN-carry tests.

**Files:**
- Modify: `backtest/regime_hmm.py` (append filter)
- Test: `backtest/tests/test_regime_hmm.py` (append)

**Interfaces:**
- Consumes: model dict from `fit_label_anchored_hmm`.
- Produces: `forward_filter_labels(features: np.ndarray, model: dict) -> tuple[np.ndarray, np.ndarray]` — `(labels[object], confidence[float])`. For each bar `i`, runs the filter over `[max(0,i-filter_window+1), i]` from `init`; NaN feature rows apply the transition step with no emission update (carry); the pre-warmup leading bars (where the whole window is NaN) get `model["states"][argmax(init)]`.

- [ ] **Step 1: Write the failing test** (append).

```python
def test_forward_filter_look_ahead_safe():
    from regime_hmm import forward_filter_labels
    rng = np.random.default_rng(0)
    feats = np.vstack([rng.normal(0, 1, (60, 4)), rng.normal(4, 1, (60, 4))])
    labels = np.array(["s0"] * 60 + ["s1"] * 60, dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=8)
    lab_a, _ = forward_filter_labels(feats, m)
    perturbed = feats.copy()
    perturbed[80:] += 100.0  # mutate the FUTURE relative to bar 70
    lab_b, _ = forward_filter_labels(perturbed, m)
    assert list(lab_a[:71]) == list(lab_b[:71])  # labels <=70 unchanged by future


def test_forward_filter_recovers_regime():
    from regime_hmm import forward_filter_labels
    rng = np.random.default_rng(1)
    feats = np.vstack([rng.normal(0, 1, (60, 4)), rng.normal(4, 1, (60, 4))])
    labels = np.array(["s0"] * 60 + ["s1"] * 60, dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=8)
    lab, conf = forward_filter_labels(feats, m)
    assert (lab[20:55] == "s0").mean() > 0.8 and (lab[80:115] == "s1").mean() > 0.8
    assert (conf >= 0).all() and (conf <= 1.0 + 1e-9).all()


def test_forward_filter_nan_carry():
    from regime_hmm import forward_filter_labels
    rng = np.random.default_rng(2)
    feats = np.vstack([rng.normal(0, 1, (40, 4)), rng.normal(4, 1, (40, 4))])
    labels = np.array(["s0"] * 40 + ["s1"] * 40, dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=6)
    feats[50] = np.nan  # a low-ATR bar inside the s1 stretch
    lab, _ = forward_filter_labels(feats, m)
    assert lab[50] in STATES  # no crash; carried, not undefined
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_hmm.py -k forward_filter -v`
Expected: FAIL — `ImportError: cannot import name 'forward_filter_labels'`.

- [ ] **Step 3: Write minimal implementation** (append to `regime_hmm.py`).

```python
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_hmm.py -v`
Expected: PASS (all). Then `uv run --no-sync python -m py_compile backtest/regime_hmm.py`.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
git add backtest/regime_hmm.py backtest/tests/test_regime_hmm.py
git commit -m "feat(#1065): causal windowed forward-filter (look-ahead safe, NaN carry)

LLM: Opus 4.8 | high | Harness: subagent-driven-development"
```

---

### Task 7: `regime_diagnostics.py` CLI — load data, score hand-rule or model, `--json`

Wire the scorers to the data path. Loads a (symbol, timeframe, window) via `load_cached_data`, computes hand-rule labels (or model labels from `--model-json`), reports coverage/separation/stability/significance/yardstick.

**Files:**
- Modify: `backtest/regime_diagnostics.py` (add header bootstrap + `run_window` + `build_parser` + `main`)
- Test: `backtest/tests/test_regime_diagnostics.py` (append a pure `run_window`-on-arrays test; no data access)

**Interfaces:**
- Consumes: `eval_windows.{DATASETS,WINDOWS,dataset_key,parse_dataset_arg}`, `shared_tools.regime.{compute_regime_composite,composite_feature_matrix,_DEFAULT_COMPOSITE_THRESHOLDS}`, `data_fetcher.load_cached_data`, `regime_hmm.forward_filter_labels`.
- Produces: `score_labels(close, labels, features, horizons, block_mult, seed) -> dict` (pure: bundles coverage/separation/stability/significance/yardstick for each horizon); CLI `main(argv) -> int`.

- [ ] **Step 1: Write the failing test** (append — pure, array-level).

```python
def test_score_labels_bundle():
    from regime_diagnostics import score_labels
    rng = np.random.default_rng(0)
    close = 100 * np.cumprod(1 + rng.normal(0, 0.005, 300))
    labels = np.array(["up" if r >= 0 else "down" for r in np.diff(close, prepend=close[0])], dtype=object)
    feats = rng.normal(0, 1, (300, 4))
    out = score_labels(close, labels, feats, horizons=(1, 4), block_mult=3, seed=0)
    assert "h4" in out and "kruskal_h" in out["h4"]["separation"]
    assert "coverage" in out and "stability" in out
    assert "p_value" in out["h4"]["significance"]
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_diagnostics.py -k score_labels -v`
Expected: FAIL — `ImportError: cannot import name 'score_labels'`.

- [ ] **Step 3: Write minimal implementation.** Prepend the sys.path bootstrap to the TOP of `regime_diagnostics.py` (above the existing `from regime_stats import ...`):

```python
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)
_ROOT = os.path.abspath(os.path.join(_THIS_DIR, ".."))
for _p in (_ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)
```

Append `score_labels`, `run_window`, parser and `main`:

```python
def score_labels(close, labels, features, horizons=(1, 4, 12), block_mult=3, seed=0) -> dict:
    labels = np.asarray(labels, dtype=object)
    features = np.asarray(features, dtype=float)
    st = stability(labels)
    mean_dwell = float(np.mean(list(st["mean_dwell"].values()))) if st["mean_dwell"] else 1.0
    out = {"coverage": coverage(labels), "stability": st, "horizons": {}}
    for h in horizons:
        fwd = forward_returns(close, h)
        block_len = max(int(block_mult * mean_dwell), h)
        out["horizons"][f"h{h}"] = {
            "separation": separation(labels, fwd),
            "significance": block_shuffle_pvalue(labels, fwd, block_len, seed=seed),
            "yardstick": kmeans_yardstick(features, fwd, seed=seed),
        }
    # flat aliases for the pre-registered primary (h=4)
    if "h4" in out["horizons"]:
        out["h4"] = out["horizons"]["h4"]
    return out


def run_window(symbol, timeframe, window, *, model=None, horizons=(1, 4, 12), seed=0) -> dict:
    from regime import compute_regime_composite, composite_feature_matrix, _DEFAULT_COMPOSITE_THRESHOLDS
    from data_fetcher import load_cached_data
    from eval_windows import WINDOWS, PLATFORM
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    period = int(model["period"]) if model and "period" in model else 48
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats_df = composite_feature_matrix(df, period, th)
    features = feats_df.to_numpy()
    if model is None:
        labels = compute_regime_composite(df, period=period, thresholds=th)["regime"].to_numpy()
    else:
        from regime_hmm import forward_filter_labels
        labels, _conf = forward_filter_labels(features, model)
    return score_labels(df["close"].to_numpy(), labels, features, horizons=horizons, seed=seed)


def build_parser() -> "argparse.ArgumentParser":
    import argparse
    from eval_windows import WINDOWS, DATASETS
    p = argparse.ArgumentParser(description="7-state regime quality diagnostics (#1065)")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--windows", default="is,oos", help=f"known: {', '.join(WINDOWS)}")
    p.add_argument("--model-json", default=None, help="score a fitted model instead of the hand-rule")
    p.add_argument("--horizons", default="1,4,12")
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--json", default=None, help="write report JSON to this path")
    return p


def main(argv=None) -> int:
    import argparse, json
    args = build_parser().parse_args(argv)
    from eval_windows import WINDOWS
    model = None
    if args.model_json:
        with open(args.model_json) as fh:
            loaded = json.load(fh)
        model = loaded.get("model", loaded) if isinstance(loaded, dict) else loaded
    horizons = tuple(int(x) for x in args.horizons.split(","))
    report = {}
    for w in args.windows.split(","):
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
        report[w] = run_window(args.symbol, args.timeframe, w, model=model,
                               horizons=horizons, seed=args.seed)
    payload = {"symbol": args.symbol, "timeframe": args.timeframe,
               "source": "model" if model else "hand_rule", "windows": report}
    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 4: Run test to verify it passes; smoke the CLI against cached data**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_diagnostics.py -v`
Expected: PASS. Then `uv run --no-sync python -m py_compile backtest/regime_diagnostics.py`.
Smoke (uses cached BTC data — may fetch if cold): `uv run --no-sync python backtest/regime_diagnostics.py --symbol BTC/USDT --timeframe 1h --windows is --horizons 1,4`
Expected: JSON with `windows.is.h4.separation.kruskal_h` and `...significance.p_value`.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
git add backtest/regime_diagnostics.py backtest/tests/test_regime_diagnostics.py
git commit -m "feat(#1065): regime_diagnostics CLI — score hand-rule or model, --json

LLM: Opus 4.8 | high | Harness: subagent-driven-development"
```

---

### Task 8: `regime_calibrate.py` CLI — fit IS, score held-out, gate verdict

Fit the label-anchored HMM on an in-sample window, save the model JSON, score it against the hand-rule on a held-out window, and emit the ship/no-ship gate verdict (separation not worse AND stability improved).

**Files:**
- Create: `backtest/regime_calibrate.py`
- Test: `backtest/tests/test_regime_calibrate.py`

**Interfaces:**
- Consumes: `regime_hmm.{fit_label_anchored_hmm}`, `regime_diagnostics.{run_window,score_labels}`, `shared_tools.regime.{composite_feature_matrix,compute_regime_composite,VALID_LABELS_COMPOSITE,_DEFAULT_COMPOSITE_THRESHOLDS}`, `eval_windows.WINDOWS`, `data_fetcher.load_cached_data`.
- Produces: `gate_verdict(handrule_report: dict, model_report: dict, primary="h4") -> dict` (pure: `{"separation_ok","stability_ok","ship": bool, "detail": {...}}`); CLI `main(argv) -> int`.

- [ ] **Step 1: Write the failing test** (pure gate logic, no data).

```python
# backtest/tests/test_regime_calibrate.py
import os, sys
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_calibrate import gate_verdict


def _report(kw_h, transition_rate):
    return {"stability": {"transition_rate": transition_rate},
            "h4": {"separation": {"kruskal_h": kw_h}}}


def test_gate_ships_when_stability_better_and_separation_kept():
    hr = _report(10.0, 0.40)
    md = _report(9.6, 0.25)  # sep within 5% tolerance, whipsaw down
    v = gate_verdict(hr, md)
    assert v["ship"] is True and v["separation_ok"] and v["stability_ok"]


def test_gate_blocks_when_separation_collapses():
    hr = _report(10.0, 0.40)
    md = _report(4.0, 0.20)  # separation lost
    assert gate_verdict(hr, md)["ship"] is False


def test_gate_blocks_when_no_stability_gain():
    hr = _report(10.0, 0.40)
    md = _report(10.0, 0.42)  # whipsaw not improved
    assert gate_verdict(hr, md)["ship"] is False
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_calibrate.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'regime_calibrate'`.

- [ ] **Step 3: Write minimal implementation**

```python
# backtest/regime_calibrate.py
"""Fit + walk-forward validate the label-anchored regime HMM (#1065 PR1)."""
from __future__ import annotations
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
for _p in (_THIS_DIR, os.path.abspath(os.path.join(_THIS_DIR, "..")),
           os.path.abspath(os.path.join(_THIS_DIR, "..", "shared_tools"))):
    if _p not in sys.path:
        sys.path.insert(0, _p)

SEPARATION_TOLERANCE = 0.05   # model KW-H may dip at most 5% below the hand-rule
STABILITY_MIN_GAIN = 0.02     # transition-rate must drop by >= this (absolute)


def gate_verdict(handrule_report: dict, model_report: dict, primary: str = "h4") -> dict:
    hr_h = handrule_report[primary]["separation"]["kruskal_h"]
    md_h = model_report[primary]["separation"]["kruskal_h"]
    hr_tr = handrule_report["stability"]["transition_rate"]
    md_tr = model_report["stability"]["transition_rate"]
    separation_ok = md_h >= hr_h * (1.0 - SEPARATION_TOLERANCE)
    stability_ok = (hr_tr - md_tr) >= STABILITY_MIN_GAIN
    return {
        "separation_ok": bool(separation_ok),
        "stability_ok": bool(stability_ok),
        "ship": bool(separation_ok and stability_ok),
        "detail": {"handrule_kruskal_h": hr_h, "model_kruskal_h": md_h,
                   "handrule_transition_rate": hr_tr, "model_transition_rate": md_tr},
    }


def fit_on_window(symbol, timeframe, window, *, period=48, filter_window=64):
    from regime import (compute_regime_composite, composite_feature_matrix,
                        VALID_LABELS_COMPOSITE, _DEFAULT_COMPOSITE_THRESHOLDS)
    from regime_hmm import fit_label_anchored_hmm
    from data_fetcher import load_cached_data
    from eval_windows import WINDOWS, PLATFORM
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats = composite_feature_matrix(df, period, th).to_numpy()
    labels = compute_regime_composite(df, period=period, thresholds=th)["regime"].to_numpy()
    states = sorted(VALID_LABELS_COMPOSITE)
    model = fit_label_anchored_hmm(feats, labels, states, filter_window=filter_window,
                                   fitted_on={"symbol": symbol, "timeframe": timeframe, "window": window})
    model["period"] = period
    return model


def build_parser():
    import argparse
    from eval_windows import WINDOWS
    p = argparse.ArgumentParser(description="Fit + validate the regime HMM (#1065)")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--in-sample", default="is", help=f"known: {', '.join(WINDOWS)}")
    p.add_argument("--held-out", default="oos")
    p.add_argument("--period", type=int, default=48)
    p.add_argument("--filter-window", type=int, default=64)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--out", default=None, help="write fitted model JSON here")
    p.add_argument("--json", default=None, help="write validation report JSON here")
    return p


def main(argv=None) -> int:
    import json
    from regime_diagnostics import run_window
    args = build_parser().parse_args(argv)
    model = fit_on_window(args.symbol, args.timeframe, args.in_sample,
                          period=args.period, filter_window=args.filter_window)
    if args.out:
        with open(args.out, "w") as fh:
            json.dump({"model": model}, fh, indent=2, default=float)
    hr = run_window(args.symbol, args.timeframe, args.held_out, model=None, seed=args.seed)
    md = run_window(args.symbol, args.timeframe, args.held_out, model=model, seed=args.seed)
    verdict = gate_verdict(hr, md)
    payload = {"symbol": args.symbol, "timeframe": args.timeframe,
               "in_sample": args.in_sample, "held_out": args.held_out,
               "verdict": verdict, "handrule": hr, "model": md}
    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 4: Run test to verify it passes; smoke the full walk-forward**

Run: `cd /Users/richardkuo/Work/go-trader && uv run --no-sync python -m pytest backtest/tests/test_regime_calibrate.py -v`
Expected: PASS. Then `uv run --no-sync python -m py_compile backtest/regime_calibrate.py`.
Smoke: `uv run --no-sync python backtest/regime_calibrate.py --symbol BTC/USDT --timeframe 1h --in-sample is --held-out oos --out /tmp/regime_model.json`
Expected: JSON with `verdict.ship` (bool) and both `handrule`/`model` held-out reports; `/tmp/regime_model.json` written.

- [ ] **Step 5: Commit + full PR1 suite**

```bash
cd /Users/richardkuo/Work/go-trader
uv run --no-sync python -m pytest shared_tools/test_regime_features.py backtest/tests/test_regime_stats.py backtest/tests/test_regime_diagnostics.py backtest/tests/test_regime_hmm.py backtest/tests/test_regime_calibrate.py -v
git add backtest/regime_calibrate.py backtest/tests/test_regime_calibrate.py
git commit -m "feat(#1065): regime_calibrate CLI — fit IS, walk-forward gate verdict

LLM: Opus 4.8 | high | Harness: subagent-driven-development"
```

---

## Self-Review

**Spec coverage (PR1 portions):**
- Measurement: coverage/separation/stability → Task 3; KW-H primary h=4 → Tasks 3,7; FDR exploratory → Task 2 (`benjamini_hochberg`, surfaced in per-state reporting via Task 7 `separation`); block-shuffle control w/ length ≥ max(h, mean dwell) → Task 4 + `score_labels` block_len; unsupervised yardstick → Task 4 (numpy k-means). ✓
- Calibration: closed-form label-anchored fit → Task 5; walk-forward IS→held-out + gate → Task 8; degenerate-state variance floor → Task 5. ✓
- Inference (needed by calibrate to score the model) → Task 6 forward-filter; look-ahead + NaN carry tests → Task 6. ✓
- Feature consistency with live labeler → Task 1 (`composite_feature_matrix` + reproduction test). ✓
- Pure/unit-tested-without-data, numpy-only, deterministic-seed, read-only → Global Constraints, honored per task. ✓
- **Deferred to PR2 (correctly out of PR1 scope):** `regime.py` live inference wiring, the `model` block on `RegimeWindowSpec`, Go structural validation/fail-closed, `filter_window` in OHLCV fetch sizing, bounded-window ADX parity. PR1's forward-filter is offline-only; the spec's parity invariants attach to PR2's live integration. ✓

**Placeholder scan:** no TBD/TODO; every code step is complete. ✓

**Type consistency:** `score_labels`/`run_window` produce `report["h4"]` consumed by `gate_verdict`; model dict keys (`states`,`emissions`,`transition`,`init`,`filter_window`,`feature_means/stds`,`period`) written by `fit_label_anchored_hmm`/`fit_on_window` and read by `forward_filter_labels`/`run_window` — consistent. `kruskal_h` returns a float (not a tuple) everywhere. ✓

**Note for the implementer:** the `--model-json` loader in Task 7 `main` accepts both calibrate's `{"model": {...}}` wrapper (its `--out` shape) and a bare model dict.
