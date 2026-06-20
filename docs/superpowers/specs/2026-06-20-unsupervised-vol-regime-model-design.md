# Unsupervised volatility-state regime model (#1080)

**Status:** design approved — pending spec review
**Issue:** #1080 ([C65] Unsupervised volatility-state regime model mapped to the 7 named labels)
**Scope:** offline / research only. No Go, no live classifier, no `check_regime.py` changes.

## Problem

The regime calibration gate (`backtest/regime_calibrate.py:gate_verdict`) was re-founded on
**forward-volatility** separation (#1078 / PR #1079): the 7-state composite hand-rule has no
real forward-*return* separation to beat (#1073), but strongly separates forward realized
volatility (#1077: significant on 4/5 eval windows). The gate can now honestly ship a model
that keeps that volatility separation while improving label stability.

The only model wired today is the **label-anchored** Gaussian HMM
(`backtest/regime_hmm.py:fit_label_anchored_hmm`). `regime_calibrate.fit_on_window:57` computes
the hand-rule labels with `compute_regime_composite(...)` and feeds them as the HMM's fit target
(`regime_calibrate.py:67-69`). Emissions/transitions are estimated from that label series, so the
model is supervised by the incumbent — it can resemble the hand-rule in-sample by construction and
cannot independently beat it.

We need a genuinely **unsupervised** volatility-state model whose states are learned from the
feature matrix alone, then mapped into the existing 7-label vocabulary for compatibility.

## Goal

Build an unsupervised volatility-state regime model that:

1. Discovers states from `shared_tools/regime.py:composite_feature_matrix`
   (`return_eff`, `range_eff`, `efficiency`, `adx`) **without** using hand-rule labels as the fit target.
2. Maps each learned latent state deterministically to one of the seven
   `VALID_LABELS_COMPOSITE` names.
3. Clears the #1078 forward-volatility gate out-of-sample (separation within tolerance,
   block-shuffle p ≤ alpha, stability gain ≥ the floor) without degenerating to a
   constant / single-dominant label stream.
4. Selects the winning family + state-count by **held-out gate behavior**, not in-sample fit.

## The unifying architectural insight

Every downstream consumer — the scorer (`regime_diagnostics.run_window` →
`forward_filter_labels`), the gate (`regime_calibrate.gate_verdict`), the bounded-window
promotion harness (`regime_bounded_window_validate.py`, verified schema-driven, not type-gated),
and the eventual #1074 live path — reads **one model-dict schema**:

```
{
  "type": "unsupervised_vol_regime",     # informational; downstream is schema-driven, never type-gated
  "version": 1,
  "fit_method": "hmm" | "gmm" | "kmeans",# which family estimated the emissions
  "features": ["return_eff","range_eff","efficiency","adx"],
  "feature_means": [...4],               # standardization stats from the TRAINING window
  "feature_stds":  [...4],
  "states":  ["ranging_quiet", "trending_up_clean", ...],  # latent index -> composite name (post-fit map)
  "latent_count": K,
  "emissions": [{"mean":[...4 z-space], "var":[...4 z-space], "n": int}, ...],  # per latent state
  "transition": [[...K], ...K],          # empirical, from the training-window state sequence
  "init": [...K],                        # stationary distribution of `transition`
  "filter_window": int,
  "period": 48,
  "fitted_on": {"symbol","timeframe","window"},
  "mapping": { "0": {"name": "...", "centroid_raw":[...4], "volatility_rank": int}, ... }
}
```

`forward_filter_labels` reads `states`, `feature_means/stds`, `emissions[].mean/var`, `init`,
`transition`, `filter_window` — **all present** for any family. Therefore:

- **All three families produce the SAME schema.** They differ only in *how the per-state Gaussian
  emissions are estimated*. The transition table and init are always estimated empirically from the
  resulting training-window state sequence; the causal decoder is always the existing
  `forward_filter_labels`.
- **Zero edits** to the scorer, gate, bounded-window harness, or the live contract.
- GMM and k-means inherit temporal smoothing "for free" via the shared transition table + causal decoder.

### How each family fills the schema

Fit on **standardized** features (z-space), mirroring `regime_hmm` (emissions live in z-space; the
decoder re-standardizes inputs with `feature_means/stds`):

- **HMM** — Baum-Welch / EM jointly estimates per-state Gaussian emissions **and** the transition
  table. (Init from the stationary distribution.)
- **GMM** — EM fits the mixture; per-component mean/var become the emissions; the transition table is
  **counted** from the MAP hard-assignment sequence on the training window.
- **k-means** — cluster centers are the means; per-cluster within-cluster variance (floored) are the
  vars; the transition table is **counted** from the hard-assignment sequence.

**Empirical transition estimation** (shared helper) mirrors `regime_hmm`'s NaN discipline
(`regime_hmm.py:46-49`): count `i→j` only between bars adjacent in the original series **and** both
feature-valid (non-NaN); Laplace-smooth; row-normalize. `init = stationary_distribution(transition)`
(reuse the existing helper).

## The five pieces

### 1. State → name mapping (isolated, post-fit, leakage-free)

Each latent state's centroid (z-space emission mean) is un-standardized to raw feature space
(`raw = z_mean * feature_stds + feature_means`), then named by running the **existing**
`shared_tools/regime.py:map_composite_label(return_eff=raw[0], adx_val=raw[3], range_eff=raw[1],
efficiency=raw[2], thresholds)`. This:

- reuses the canonical decision boundaries (semantically consistent with the rest of the system),
- is deterministic,
- consumes only **training-window state summaries** (centroids) — never per-bar hand-rule labels as
  a fit target, and never OOS data to rename states.

`volatility_rank` is assigned by centroid `range_eff` (the volatility proxy). If K < 7, only K names
are used; unused labels stay unused — **no fabricated duplicate labels** to pad coverage. Two distinct
learned states legitimately mapping to the same name is allowed (genuine, not cosmetic).

### 2. Anti-collapse (non-degeneracy) guards

Named thresholds, reported per eval window; a candidate **fails** if any window collapses:

- `MIN_ACTIVE_LABELS` — minimum distinct labels emitted on a window.
- `MAX_LABEL_OCCUPANCY` — maximum fraction any single label may occupy.
- `MIN_TRANSITION_RATE` — minimum flips/bar (guards a frozen constant stream).

These directly counter PR #1079's documented failure mode: a near-constant stream can win the
stability arm while losing real separation.

**Threshold derivation (anti-gaming):** the cutoff values are **derived from the hand-rule
incumbent's own worst-window behavior** (its minimum distinct-label count, maximum single-label
occupancy, and minimum transition rate across the eval windows), each loosened by a fixed margin,
and **locked before any candidate is scored**. They are never hand-picked numbers and never tuned to
let a chosen model pass — the incumbent never collapses, so a guard set a fixed margin looser than
its worst window is defensible by construction and cannot be reverse-engineered to a favored
candidate.

### 3. Look-ahead safety

- Fit consumes only the training window's feature matrix (no `compute_regime_composite` as target).
- Decoding is the already-causal `forward_filter_labels` (label at bar N uses only bars ≤ N within
  `filter_window`).
- Naming uses only training-window centroids.
- **Regression test:** mutate / truncate future rows and prove labels through bar N are unchanged.

### 4. The bake-off (the arbiter)

A walk-forward driver fits each **family × candidate state-count K∈{2..7}** on the in-sample window,
decodes + scores each on the held-out windows through `score_labels(target="volatility")` +
`gate_verdict`, then applies the non-degeneracy guards. Selection:

- **Eligible** = `gate_verdict(...).ship is True` on the held-out window **and** non-degenerate on
  every eval window.
- **Winner** = among eligible candidates, the highest out-of-sample volatility KW-H separation;
  stability gain breaks ties.
- All candidates (eligible or not) are reported, with the reason any failed.

Economic payoff (regime-conditioned ATR sizing vs flat-ATR) is **#1081**, not selected here.

### 5. Reproducible evidence script

`backtest/research/regime_1080_unsupervised_vol_model.py`, same style as
`regime_1073_directional_negative_result.py` (parameterized by `--symbol/--timeframe`, read-only,
prints and optionally writes JSON). Per candidate it reports: OOS separation, block-shuffle p-value,
stability gain, coverage / non-degeneracy, selected K, and the full state→name map.

## Files

| File | Role |
|------|------|
| `backtest/regime_vol_model.py` | **New.** Pure library: `fit_unsupervised(family, features, k, ...)` for the three families, the shared empirical-transition + init helper, the centroid→name mapping, the non-degeneracy scorer. A CLI fits one chosen family on a window and writes a model JSON (mirrors `regime_calibrate`'s `--out`). |
| `backtest/research/regime_1080_unsupervised_vol_model.py` | **New.** Walk-forward bake-off driver + reproducible evidence. |
| `backtest/tests/test_regime_vol_model.py` | **New.** Tests (below). |

No edits to existing modules — verified the schema is decoded, not type-checked, everywhere downstream.

## Testing

- **No-leakage:** the fit path never calls `compute_regime_composite` and never receives hand-rule
  labels (assert via call interception / signature, and by construction).
- **Causality:** look-ahead regression — mutating/truncating future rows leaves labels ≤ N unchanged.
- **Mapping:** deterministic state→name; monotonicity (holding direction/efficiency fixed, growing
  range/move magnitude never assigns a lower-volatility ranging label); tie handling; K<7 unused-label
  behavior; every emitted label ∈ `VALID_LABELS_COMPOSITE`.
- **Schema / decoder reuse:** a fitted model from each family decodes cleanly through
  `forward_filter_labels` and scores through `score_labels`.
- **Non-degeneracy:** synthetic constant / single-dominant streams trip the guards; a healthy stream
  passes.
- **Report shape:** bake-off report carries the documented per-candidate fields.
- **Bounded-window compatibility:** a fitted model passes through `regime_bounded_window_validate`'s
  `validate_frames` without a type/adapter error.

## Known caveat (surfaced, not solved)

`gate_verdict` only ships on a window where the **hand-rule incumbent itself** is statistically
significant (`incumbent_trustworthy = hr_p ≤ SIGNIFICANCE_ALPHA`, `regime_calibrate.py:39`); it
**abstains** otherwise (significant on 4/5 windows per #1077). If a chosen held-out window is the
non-significant one, the gate abstains regardless of model quality. The harness reports this
(`verdict.abstained`) across all held-out windows rather than crashing or silently failing.

## Note for #1074 (live wiring)

The HMM **fit** receives the NaN-compacted feature matrix (`z = features[mask]`), so Baum-Welch
treats gap-separated bars as temporally adjacent when estimating *emissions* — a minor in-sample
approximation. It does **not** affect correctness downstream: the stored `transition` is recomputed
gap-correctly by `empirical_transition` (mirroring `regime_hmm`'s NaN discipline), and the causal
decoder `forward_filter_labels` honors NaN bars via its carry branch. k-means/GMM are
order-independent and unaffected. No action needed here; flagged so #1074 doesn't re-derive it.

## Out of scope

- Live / Go classifier wiring and parity — **#1074**.
- Economic payoff of regime-conditioned ATR sizing vs flat ATR — **#1081**.
- Multi-asset validation beyond BTC/USDT 1h — **#1083**.

---
Created with LLM: Opus 4.8 | xhigh | Harness: Claude Code
