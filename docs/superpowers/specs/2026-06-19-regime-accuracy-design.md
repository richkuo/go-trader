# 7-State Regime Accuracy: Measurement + Label-Anchored HMM Classifier

**Issue:** #1065
**Status:** Design approved, pending spec review
**Approach:** B — model with named states (keep the 7 semantic labels; replace hand-thresholds + per-bar independence with a fitted transition-prior model)

## Problem

The composite 7-state regime classifier (#795 / #1058) labels each bar one of
`trending_up_clean`, `trending_up_choppy`, `trending_down_clean`,
`trending_down_choppy`, `ranging_quiet`, `ranging_volatile`,
`ranging_directional`. It is a hand-tuned decision tree (`map_composite_label`)
over four ATR-normalized features — `return_eff`, `range_eff`, `efficiency`,
`adx` — with global constant thresholds (`_DEFAULT_COMPOSITE_THRESHOLDS`).

Two gaps:
1. **Accuracy is never measured.** No validation harness, no forward-return
   separation test, no stability/whipsaw metric. "Accuracy" is undefined today.
2. **The classifier is structurally weak.** Thresholds are global constants (not
   calibrated per asset/timeframe), and each bar is classified independently with
   no persistence model, so labels can whipsaw bar-to-bar.

## Goal

Define and measure regime quality, then improve it — without breaking the
semantic-label contract that config-keyed SL/TP/gate ladders depend on.

Two notions of "accuracy", both measured:
- **Separation** (the economic notion): do the 7 states have distinct forward
  return / volatility distributions, and in the right direction? Is that
  separation real signal or an autocorrelation artifact?
- **Stability:** does the label persist sensibly, or whipsaw?

## Approach: label-anchored Gaussian HMM

Keep the 7 semantic labels and the existing four features. Replace the hand
decision tree and per-bar independence with a Hidden Markov Model whose states
are **pinned to the 7 labels** (so state identity stays semantic and config keys
keep working):

- **Emissions** — per-state Gaussian, fit by maximum likelihood on the
  (standardized) feature vectors of bars the *current hand-rule* labels as that
  state. The hand-rule becomes the labeling prior; the model refines hard cutoffs
  into soft, data-fit boundaries. Diagonal covariance (4 features, keep params
  low and the fit robust).
- **Transitions** — the 7×7 matrix counted from the hand-rule label sequence,
  Laplace-smoothed and row-normalized. This *is* the persistence layer:
  data-calibrated, replacing the arbitrary smoothing-window hack.
- **Inference** — causal **forward-filter** only (never Viterbi / forward-
  backward, which peek at the future). At bar *t*:
  `alpha_t(j) ∝ emission(x_t | j) · Σ_i alpha_{t-1}(i) · A[i,j]`, normalized.
  `label_t = argmax_j alpha_t(j)`; `confidence_t = max_j alpha_t(j)` (a new
  output that lets downstream gate on uncertainty).

Fitting is **closed-form** (Gaussian MLE + transition counts) because the states
are supervised by the hand-rule labels — no EM, no `hmmlearn`, no new dependency.

### Parity & look-ahead (the load-bearing invariants)

- **Canonical inference is the *windowed* forward-filter:** run the filter over a
  fixed trailing window of `filter_window` bars, initialized at the model's
  stationary distribution. This produces the label for the window's last bar.
  - **Backtest** (`compute_regime_composite`): for each bar *i*, run the windowed
    filter over bars `[i-filter_window+1, i]`.
  - **Live** (`latest_regime_composite`): run the windowed filter over the last
    `filter_window` bars of the trailing df.
  - Both call **one shared helper** with identical inputs → labels are identical
    by construction. No cross-cycle state is needed (the #879 cycle-reset store
    stays empty-per-cycle); persistence comes from the transition matrix applied
    across the window, not from remembered emitted labels.
  - `filter_window` must be long enough for the init to wash out (HMM geometric
    forgetting); calibration picks and validates it.
- **Look-ahead safety:** the filter at bar *t* uses only bars ≤ *t*. The existing
  N→N+1 gate shift and `_regime_bar_close` snapshot are classifier-agnostic and
  unchanged. Covered by `backtest/tests/test_backtester_lookahead.py`.
- **Features are unchanged:** the HMM consumes the same per-bar feature tuple the
  hand-rule consumes (`return_eff`, `range_eff`, `efficiency`, `adx`, with the
  `COMPOSITE_ADX_PERIOD_CAP` ADX and full-`period` efficiency). Only the
  tuple→label mapping changes.

## Components

### 1. `backtest/regime_diagnostics.py` — measurement harness

Follows the `exit_diagnostics.py` / `eval_windows.py` pattern: imports the
versioned `DATASETS` / `WINDOWS` from `eval_windows.py` for byte-identical
slices; all aggregation is **pure** (operates on label arrays + return arrays) so
it is unit-tested without data access. `--json` output.

Computes labels for a window via `compute_regime_composite` (no strategy needed —
labels are a pure function of OHLCV) under either the hand-rule or a supplied
model, then reports:
- **Coverage** — count and % of bars per state (catches dead/degenerate states).
- **Separation** — per state, the forward-return distribution at horizons
  h ∈ {1, 4, 12} bars: mean, std, t-stat, directional hit-rate; plus one overall
  cross-state statistic (Kruskal–Wallis H). Correctness check: do `trending_up_*`
  precede positive returns, `trending_down_*` negative, `ranging_*` near-zero /
  high-vol?
- **Stability** — transition rate (% bars the label flips), mean dwell length per
  state, total flips.
- **Significance control** — block-shuffle permutation: shuffle labels in blocks
  (preserving marginal frequencies and local autocorrelation), recompute
  separation, report how much collapses. Tells us whether separation is real
  signal or an artifact.
- **Yardstick** — fit an *unsupervised* GMM (and/or HMM) on the same features,
  report its separation. Quantifies the cost of anchoring to 7 named states vs
  letting the data choose — even though we ship the anchored model, we measure
  against this ceiling.

CLI (no strategy): `--symbol` / `--timeframe` or dataset selection, `--windows`,
`--model-json <path>` (optional; default = hand-rule), `--horizons`, `--json`.

### 2. `backtest/regime_calibrate.py` — fit + walk-forward validation

Fits the label-anchored HMM (closed-form) on an **in-sample** window and emits
the model JSON. Validates separation + stability on a **held-out** window against
the hand-rule and the shuffle baseline (walk-forward; the model never sees the
held-out window). Emits the proposed model for review; does **not** mutate live
defaults. Reuses `regime_diagnostics.py`'s pure scorers so fit and score share
identical logic.

CLI: `--symbol` / `--timeframe`, `--in-sample` / `--held-out` windows,
`--filter-window`, `--out <model.json>`, `--json` (validation report).

### 3. `shared_tools/regime.py` — forward-filter inference path

- New inference helper (the windowed forward-filter) shared by
  `latest_regime_composite` (live) and `compute_regime_composite` (backtest).
- **Opt-in** via an optional `model` block on the composite windows spec. When
  the block is **absent, behavior is byte-identical to today's hand-rule** (live
  and backtest) — preserves #1058 parity and de-risks rollout.
- Validation: when present, the `model` block's `states` must equal
  `VALID_LABELS_COMPOSITE`; feature list must match; matrix shapes consistent.

#### `model` block schema (additive, opt-in)

```jsonc
{
  "classifier": "composite",
  "period": 48,
  "thresholds": { /* unchanged; still used to generate anchor labels at fit time */ },
  "model": {
    "type": "label_anchored_hmm",
    "version": 1,
    "features": ["return_eff", "range_eff", "efficiency", "adx"],
    "feature_means": [/* 4, standardization */],
    "feature_stds":  [/* 4 */],
    "states": [/* the 7 labels, fixed order */],
    "emissions": [ { "mean": [/*4*/], "var": [/*4 diag*/] }, /* ...7 */ ],
    "transition": [ [/*7*/], /* ...7 rows */ ],
    "init": [/*7 stationary dist*/],
    "filter_window": 64,
    "fitted_on": { "symbol": "BTC/USDT", "timeframe": "1h", "window": "is" }
  }
}
```

### Go side (sync surface)

The composite spec/threshold surface must carry the `model` block opaquely:
- Add the `model` field to the Go composite-window/threshold struct (the
  `RegimeCompositeThresholds` neighborhood), carried as opaque JSON
  (`json.RawMessage`) — Go does **not** reimplement inference.
- Register `model` so the **unknown-key guard** accepts it (else config load
  rejects it).
- Include `model` in `resolvedForEmit` so `--config` backtests receive the same
  block live uses (the #1058 / #1025 parity path).
- Default-absent → zero behavior change; no `config_version` bump required (the
  field is additive and optional, mirroring #1048's `CircuitBreaker` pattern).

## Decision criterion

Ship the fitted model into a strategy's config **only if** it beats the hand-rule
on held-out separation **and** stability (lower whipsaw, separation not worse).
Otherwise the hand-rule stands. The harness decides — not faith. The model is
opt-in per strategy/window via the `model` block; nothing changes for configs
that don't adopt it.

## Error handling / edge cases

- Empty / too-short window (`len < filter_window` or `< period`) → fall back to
  the hand-rule label (or `ranging_quiet` as today when ATR ≤ 0); never raise.
- Degenerate emission (a hand-rule state with too few bars to fit a Gaussian) →
  Laplace/shrinkage on variance; if still degenerate, that state's emission falls
  back to the hand-rule region indicator. Calibration warns and reports coverage.
- Malformed `model` block (wrong `states`, shape mismatch) → reject at config
  load with a clear message (fail closed; do not silently run the hand-rule).
- Singular covariance avoided by diagonal cov + variance floor.

## Testing

Python:
- `shared_tools/` — windowed forward-filter unit tests; **live/backtest parity**
  (same model + OHLCV → identical label sequence); **default-absent byte-identical**
  to the hand-rule; look-ahead (filter at *t* independent of bars > *t*); fit
  determinism (closed-form, no RNG); degenerate-state fallback.
- `backtest/tests/` — `test_regime_diagnostics.py` (pure scorers on synthetic
  label/return arrays: separation, shuffle control, stability); calibrate
  walk-forward smoke; extend `test_backtester_lookahead.py` coverage for the
  model path.

Go:
- `model` block round-trips spec parse + unknown-key guard accepts it +
  `resolvedForEmit` includes it; malformed block rejected; default-absent config
  unchanged (existing composite tests stay green).

## Out of scope (deferred)

- Full unsupervised re-derivation that lets the data choose the number/shape of
  states (Approach C) — measured as a yardstick only.
- Auto-adopting a fitted model into live defaults — this ships the *tooling* and
  the opt-in path; promoting a specific model per strategy is a follow-up.
- Online/cross-cycle posterior carry — the windowed-from-init filter makes it
  unnecessary for parity.

---
LLM: Opus 4.8 | high | Harness: Claude Code
