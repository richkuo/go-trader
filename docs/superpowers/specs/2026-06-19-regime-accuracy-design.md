# 7-State Regime Accuracy: Measurement + Label-Anchored HMM Classifier

**Issue:** #1065
**Status:** Design v2 (review findings folded in), pending spec review
**Approach:** B — model with named states (keep the 7 semantic labels; replace
hand-thresholds + per-bar independence with a fitted transition-prior model)

**Delivery:** two phases, **harness-first**.
- **PR1 (measurement, read-only, no parity risk):** `regime_diagnostics.py` +
  `regime_calibrate.py`. Tells us whether a fitted model is even worth building.
- **PR2 (classifier, parity-sensitive):** the forward-filter inference path in
  `regime.py` + the Go spec surface. Built **only if PR1 shows the model beats the
  hand-rule** on the pre-registered gate.

## Problem

The composite 7-state classifier (#795 / #1058) labels each bar one of
`trending_up_clean`, `trending_up_choppy`, `trending_down_clean`,
`trending_down_choppy`, `ranging_quiet`, `ranging_volatile`,
`ranging_directional`. It is a hand-tuned decision tree (`map_composite_label`)
over four ATR-normalized features — `return_eff`, `range_eff`, `efficiency`,
`adx` — with global constant thresholds (`_DEFAULT_COMPOSITE_THRESHOLDS`).

1. **Accuracy is never measured.** No validation harness, no forward-return
   separation test, no stability/whipsaw metric.
2. **The classifier is structurally weak.** Thresholds are global constants (not
   calibrated per asset/timeframe); each bar is classified independently with no
   persistence model, so labels can whipsaw.

## Goal

Define and measure regime quality, then improve it — without breaking the
semantic-label contract that config-keyed SL/TP/gate ladders depend on.

Two measured notions:
- **Separation** (economic): do the 7 states have distinct, correctly-signed
  forward return / volatility distributions, and is that separation real signal
  vs an autocorrelation artifact?
- **Stability:** does the label persist sensibly or whipsaw?

### Honest expectation (sets the gate)

Emissions are MLE-fit on bars the **hand-rule itself labels** as each state, so
the model largely relearns the hand-rule's boundaries. Held-out *separation* lift
is therefore structurally bounded to boundary-softening; the material gain is
**whipsaw reduction** from the data-fit transition matrix. The ship/no-ship gate
is keyed accordingly (below): the model must not *lose* separation and must
*improve* stability. A null separation result is expected, not a failure.

## Approach: label-anchored Gaussian HMM (PR2)

Keep the 7 semantic labels and the four features. Pin HMM states to the 7 labels:
- **Emissions** — per-state diagonal Gaussian, MLE on the standardized feature
  vectors of bars the current hand-rule labels as that state.
- **Transitions** — 7×7 matrix counted from the hand-rule label sequence,
  Laplace-smoothed, row-normalized. This is the persistence layer (replaces any
  smoothing-window hack).
- **Inference** — causal **forward-filter** only:
  `alpha_t(j) ∝ emission(x_t|j) · Σ_i alpha_{t-1}(i)·A[i,j]`, normalized;
  `label_t = argmax_j alpha_t(j)`, `confidence_t = max_j alpha_t(j)`.

Fit is **closed-form** (Gaussian MLE + transition counts), no EM, no new
dependency.

### Parity & look-ahead — the load-bearing invariants

1. **One shared helper, identical bounded-window features on both paths.** The
   forward-filter helper recomputes **all** features — including ADX — over the
   **same bounded `[i-filter_window+1, i]` window from the same start** on both
   live and backtest. This is required because ADX uses Wilder recursive
   smoothing (`_compute_adx_components`), which only converges geometrically:
   backtest's full-series ADX column and live's trailing-df ADX differ by a
   seed-forgetting epsilon that a forward-filter can amplify into a flipped
   near-tie posterior. Computing ADX over an identical bounded window on both
   sides removes the epsilon. (This changes ADX values *only on the model path*;
   the hand-rule default path is untouched and stays byte-identical.) Parity claim
   is "identical **given identical feature inputs**," with the bounded-window rule
   making the inputs identical — not "identical by construction."
2. **Windowed-from-init forward-filter is canonical.** Both paths run the filter
   over a fixed trailing window of `filter_window` bars from the model's
   stationary init; persistence comes from the transition matrix across the
   window, not cross-cycle memory (the #879 cycle-reset store stays empty). The
   backtest cost is **O(n·filter_window)** per window — intentionally, for live
   parity; a future reader must NOT "optimize" it into a single full-sequential
   filter (that breaks parity). `filter_window` must wash out the init; calibration
   picks and validates it.
3. **`filter_window` drives the OHLCV fetch limit.** The warmed-bar requirement
   is `filter_window + period + ADX-warmup`. Both fetch-sizing paths —
   `required_ohlcv_limit` (`shared_tools/regime.py`) and `regimeRequiredOhlcvLimit`
   (`scheduler/regime_multi_window.go`), plus the store's `OhlcvLimit`
   (`regime_store.go`) — must add a `filter_window` term (taking the **max across
   windows**). Without this, live filters over fewer warmed bars than backtest and
   labels diverge.
4. **Low-ATR / undefined-feature bars are handled identically.** Today both paths
   degrade to `ranging_quiet` when `atr<=0` (`regime.py` latest + per-bar loop).
   Inside a filter window such a bar gets a **defined rule: skip its emission
   update and carry alpha forward** (apply the transition step, no emission),
   specified identically in the shared helper so both paths behave the same on
   exactly these bars.
5. **Look-ahead safety.** Filter at bar *t* uses only bars ≤ *t*. Standardization
   `feature_means`/`feature_stds` are **frozen in the model block from the fit
   window and never recomputed** live or in backtest (recompute = fit-apply
   look-ahead). The existing N→N+1 gate shift / `_regime_bar_close` snapshot are
   classifier-agnostic and unchanged; extend `test_backtester_lookahead.py`.

## Components

### PR1 — `backtest/regime_diagnostics.py` (measurement)

Follows the `exit_diagnostics.py` / `eval_windows.py` pattern: imports versioned
`DATASETS` / `WINDOWS` from `eval_windows.py` for byte-identical slices; all
aggregation **pure** (label arrays + return arrays) and unit-tested without data;
`--json`. Computes labels via `compute_regime_composite` under the hand-rule or a
supplied model, then reports:
- **Coverage** — count and % of bars per state.
- **Separation** — forward-return distribution per state at h ∈ {1,4,12}.
  **Pre-registered primary metric: Kruskal–Wallis H at h=4** (one declared test
  for the gate). Per-state means / t-stats / hit-rates are **exploratory**,
  reported with **FDR (Benjamini–Hochberg)** correction — dozens of state×horizon
  comparisons otherwise manufacture false "significant" states.
- **Stability** — transition rate, mean dwell per state, total flips.
- **Significance control** — **block-shuffle** permutation with block length
  **≥ max(h, mean dwell)** (shorter blocks leave autocorrelation intact and the
  control falsely reports "real signal"); recompute separation, report collapse.
- **Yardstick** — unsupervised GMM/HMM on the same features; reports the ceiling
  the 7-named-state anchoring forgoes (measured, not shipped).

CLI: `--symbol`/`--timeframe` or dataset selection, `--windows`,
`--model-json <path>` (optional; default = hand-rule), `--horizons`, `--json`.

### PR1 — `backtest/regime_calibrate.py` (fit + walk-forward validation)

Closed-form fit of the label-anchored HMM on an **in-sample** window; emits the
model JSON. Validates on a **held-out** window (model never sees it) against the
hand-rule and shuffle baselines, reusing `regime_diagnostics.py`'s pure scorers.
Emits the proposed model for review; does **not** mutate live defaults.

**Degenerate states:** a hand-rule state with too few bars to fit a stable
Gaussian gets a **variance floor + shrinkage**; if still degenerate the state is
flagged and its emission likelihood is set to a **calibrated constant on the same
log-density scale** as the Gaussian emissions (an unscaled 0/1 indicator would
make the state either never- or always-selected against unbounded Gaussian
densities). Calibration warns and reports per-state bar counts.

CLI: `--symbol`/`--timeframe`, `--in-sample`/`--held-out`, `--filter-window`,
`--out <model.json>`, `--json`.

**Ship/no-ship gate (decision criterion):** adopt a fitted model for a
strategy/window **only if**, on the held-out window, it (a) does **not** reduce
the primary separation metric (KW-H at h=4) beyond noise, **and** (b) **improves
stability** (lower transition rate / longer mean dwell) materially. Otherwise the
hand-rule stands.

### PR2 — `shared_tools/regime.py` (forward-filter inference path)

- New windowed-forward-filter helper, shared by `latest_regime_composite` (live)
  and `compute_regime_composite` (backtest), computing bounded-window features per
  invariant 1.
- **Opt-in** via an optional `model` block on the composite windows spec.
  **Absent → byte-identical to today's hand-rule**, live and backtest (#1058
  parity preserved).
- `fitted_on` guard: when a strategy's `(symbol, timeframe)` ≠ the model's
  `fitted_on`, **warn loudly** (mis-applied model = silent systematic mislabel
  into the SL/TP/gate money path).

#### `model` block schema (additive, opt-in)

```jsonc
{
  "classifier": "composite",
  "period": 48,
  "thresholds": { /* unchanged; generates anchor labels at fit time */ },
  "model": {
    "type": "label_anchored_hmm", "version": 1,
    "features": ["return_eff","range_eff","efficiency","adx"],
    "feature_means": [/*4, frozen from fit*/], "feature_stds": [/*4*/],
    "states": [/* the 7 labels, fixed order */],
    "emissions": [ { "mean": [/*4*/], "var": [/*4 diag*/] }, /* ...7 */ ],
    "transition": [ [/*7*/], /* ...7 */ ], "init": [/*7 stationary*/],
    "filter_window": 64,
    "fitted_on": { "symbol": "BTC/USDT", "timeframe": "1h", "window": "is" }
  }
}
```

### PR2 — Go spec surface (corrected hook points)

- Add `Model json.RawMessage` to **`RegimeWindowSpec`** (`scheduler/regime_window_spec.go`,
  sibling of `Thresholds`) — **not** the `RegimeCompositeThresholds` struct.
  Rationale: `RegimeWindowsMap.UnmarshalJSON` does **not** set
  `DisallowUnknownFields`, so an unregistered `model` key is **silently dropped**,
  and the unknown-key guard (`validateStrategyJSONKeys`) only covers `strategies[]`,
  never `regime.windows`. A dropped `model` → live runs the hand-rule while
  backtest runs the model = silent parity break.
- **Fail-closed structural validation at config load** (resolves the #879
  fail-open trap): Go validates the block's *structure* — `states` ==
  `VALID_LABELS_COMPOSITE`, `transition` is 7×7, `emissions` length 7, vector
  lengths match `features` — and **rejects** a malformed block at load. This is
  shape-checking, not reimplementing inference; without it a mis-shaped block
  becomes a Python parse failure that routes through the #879 store → empty
  payload → **entries fail open (ungated)**, the inverse of safe for a money-path
  classifier.
- Thread `model` into the emitted windows spec (`resolvedForEmit` /
  `regimeWindowsSpecJSON`) so `--config` backtests receive the same block live
  uses, and into the fetch-limit sizing (invariant 3).
- Default-absent → zero behavior change; additive/optional field, **no
  `config_version` bump** (mirrors #1048 `CircuitBreaker *bool`).

## Edge cases / fail-closed

- Empty/short window (`len < filter_window+period`) → hand-rule label (or
  `ranging_quiet` when ATR ≤ 0); never raise.
- Singular covariance avoided by diagonal cov + variance floor.
- Malformed `model` block → **rejected at Go config load** (fail closed), not
  silently degraded.

## Testing

Python:
- `shared_tools/` — windowed forward-filter units; **live/backtest parity** (same
  model + OHLCV → identical label sequence, incl. bounded-window ADX);
  **default-absent byte-identical** to the hand-rule; look-ahead (filter at *t*
  independent of bars > *t*; standardization frozen); low-ATR carry-alpha on both
  paths; fit determinism; degenerate-state fallback scaling.
- `backtest/tests/` — `test_regime_diagnostics.py` (pure scorers: KW-H, FDR,
  block-shuffle with the length rule, stability) on synthetic arrays; calibrate
  walk-forward smoke; extend `test_backtester_lookahead.py` for the model path.

Go:
- `Model` round-trips `RegimeWindowSpec` parse and survives `resolvedForEmit`;
  malformed block (bad `states`, wrong shapes) **rejected at load**; fetch-limit
  includes `filter_window` (max across windows); default-absent config unchanged
  (existing composite tests stay green).

## Out of scope (deferred)

- Full unsupervised re-derivation (Approach C) — yardstick only.
- Auto-promoting a fitted model into live defaults — PR2 ships the opt-in path;
  per-strategy adoption is a follow-up gated on the harness result.
- Online/cross-cycle posterior carry — windowed-from-init makes it unnecessary.

---
LLM: Opus 4.8 | high | Harness: Claude Code
