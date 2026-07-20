# Backtesting & Research Harness Registry

Map of the backtesting and offline-research harnesses in `backtest/`. The
reusable tools — core simulators, the M1–M6 validation series, the
regime-promotion pipeline, and the one-shot research scripts — are listed
individually below; the per-study candidate suites under `backtest/candidates/`
(each a self-contained set of executable driver scripts plus specs) are indexed
by study rather than enumerated script-by-script. This file is the source of
truth; the per-subsystem mechanics live in
[`docs/ARCHITECTURE.md` § Backtest harnesses](ARCHITECTURE.md).

**Upkeep rule:** any PR that adds, deprecates, or repurposes a harness updates
its row here in the same PR. A new row is part of the change, not a follow-up.

## Categories

- **Core simulator** — the trade-simulating engine and its entry points / reporting / optimization; produce equity curves and metrics.
- **M-series validator** — the M1–M6 incumbent-relative research methodology (audit slices via `eval_windows.py`); grade a candidate change you hand them.
- **Regime-promotion pipeline** — the offline machinery (#1065–#1097) that decides whether a market-regime *classifier* may replace the incumbent hand-rule.
- **Support library** — shared, non-executable infrastructure imported by the above.
- **Research one-shot** — reproducible evidence for a single closed issue; run to regenerate a documented result, not a maintained tool.
- **Candidate study** — a per-strategy directory under `backtest/candidates/<study>/` bundling that study's own executable driver scripts (screens/gates) and specs; indexed by study below.

## Core simulators

| File | Purpose | Status | Origin |
|------|---------|--------|--------|
| `backtester.py` | Trade-simulating engine; replays a strategy on historical bars and computes metrics. Default `intrabar_resolution="ohlc_walk"` (#1271): SL trigger touches resolve intra-bar (trigger-price fill, open on gap-through, adverse-move-first vs same-bar TP); `"bar_close"` reproduces pre-#1271 baselines. | active | core |
| `run_backtest.py` | Main CLI entry — run strategies across assets/timeframes/modes; threads `--config` with live-matching `user_defaults`, regime gate, HTF, `--intrabar-resolution` (#1271). | active | core |
| `optimizer.py` | Walk-forward rolling in-sample/out-of-sample parameter optimization (anti-overfit). | active | core |
| `reporter.py` | Text performance reports — single, comparison, multi-asset. | active | core |
| `backtest_options.py` | Options-strategy backtester (e.g. `vol_mean_reversion`) against historical spot. | active | core |
| `backtest_theta.py` | Theta-harvesting A/B (conservative vs aggressive vs hold-to-expiry). | active | core |
| `backtest_pairs.py` | Two-leg beta-hedged pairs (z-score) simulator; no live path. | active | core |
| `backtest_carry_pair.py` | Hedged funding-carry pair simulator (SHORT perp + LONG spot): books #988 per-bar funding on the perp leg, per-leg fees, `drift_threshold` spot rebalancing, isolated-margin perp liquidation (gap-through cap). Single-series hedge cancels price PnL, so net = carry − costs; `--perp-symbol` adds a basis series. Runs the six audit datasets × M1 windows; no live path. Gives `delta_neutral_funding` a real edge verdict (#1280 could not model the hedge). | active | #1326 |
| `parity_diff.py` | Backtest-vs-live parity diff — enforces the simulator↔scheduler contract. | active | #906 |

## M-series validators (M1–M6)

| File | Purpose | Status | Origin |
|------|---------|--------|--------|
| `eval_windows.py` | **M1** multi-window incumbent-relative validator — one command per application issue. Threads `--intrabar-resolution` (#1271; default `ohlc_walk`) so `bar_close` legacy baselines are reproducible. | active | #977 |
| `gross_edge_noise.py` | **M1 step-2** sample-noise adjudicator for `graduate_m1` fee-audit verdicts; run before any selectivity work. | active | #1054 |
| `exit_diagnostics.py` | **M3** holding-time / excursion diagnostics — where a strategy's PnL dies. | active | #997 |
| `fee_audit.py` | **M5** registry-wide trade-count × fee-drag selectivity triage. Sweeps the FULL registry incl. discovery-hidden/quarantined names (#1275) — re-screening is the recovery path out of quarantine. | active | #999 #1275 |
| `exit_policy_ab.py` | **M6** regime-conditioned incumbent-relative exit-policy A/B. Threads `--intrabar-resolution` (#1271; default `ohlc_walk`, one module-level mode shared by both arms + the replay) so `bar_close` legacy baselines are reproducible (#1294). | active | #1066 |
| `auto_suggest.py` | Reusable cross-harness driver that sweeps candidates and ranks gate-survivors under one BH correction. Carries advisory Monte Carlo drawdown-risk columns (`mc` harness, #1295) that never influence the gate in any state — including a failed MC run. **Suggest-only — never writes a live default/config/PR.** | active | #1210, #1295 |
| `monte_carlo.py` | Trade-order Monte Carlo resampler — permutation + circular block bootstrap over a run's closed trades; drawdown / final-return percentile bands, P(final < start), P(max DD ≥ kill-switch threshold). Sources: `--trades-json`, `--strategy`, or `--candidate-json` (full candidate shape); `--windows`/`--datasets` fan one run across legs (#1295). Suggest-only diagnostics. | active | #1274, #1295 |
| `tune_live.py` | One-command live-strategy tuner: resolves each config strategy's effective live params (reusing `run_backtest.load_strategy_config`) and its spot/futures registry independently from live strategy type by default (`--registry spot`/`futures` remains a fleet override), searches a bounded neighborhood (registry `constraints` + `DEFAULT_PARAM_RANGES`, operator `--param`/`--overrides`) via stage-1 walk-forward, then gates survivors through `auto_suggest` under selection-aware inference — stage-1 data sliced disjoint from the stage-2 windows, BH family size = searched N (`correction.family_size`). Emits a schema-versioned artifact + patches + progress JSON (#1339–#1341). **Schema v2 (#1386):** each successful per-strategy result carries `promotion_baseline` — raw `open_strategy` / `user_defaults` / `user_close_defaults` blocks with presence bits, captured from the same `load_strategy_config` read (opt-in `include_promotion_baseline`) before any close/stop injection — so #1341 can drift-check without re-resolving. **Suggest-only — never writes a config/default/PR.** | active | #1338 #1386 |

## Regime-promotion pipeline (#1065–#1097)

| File | Purpose | Status | Origin |
|------|---------|--------|--------|
| `regime_calibrate.py` | Fit + walk-forward validate the label-anchored regime HMM; hosts `gate_verdict` (candidate-self-v2 promotion gate). | active | #1065, #1211 |
| `regime_hmm.py` | Label-anchored Gaussian HMM: closed-form fit + causal forward-filter decoder. | active | #1065 |
| `regime_vol_model.py` | Unsupervised vol-state candidates (HMM/GMM/k-means) behind one model-dict schema. Offline only. | active | #1080 |
| `regime_enriched_features.py` | Enriched feature matrix (canonical four + ignored signals) for the #1095 bake-off. | active | #1095 |
| `regime_diagnostics.py` | 7-state regime quality diagnostics (pure scorers + CLI). | active | #1065 |
| `regime_stats.py` | Dependency-free statistics (permutation, BH, Kruskal–Wallis) for regime diagnostics. | active | #1065 |
| `regime_bounded_window_validate.py` | Bounded-window ADX re-validation reflecting the live fetch path. | active | #1082 |
| `directional_certification.py` | Evidence-gated directional-certification backtest consumer — parity mirror of the Go path. | active | #1085 |
| `research/regime_1081_economic_gate.py` | Economic walk-forward gate for regime-conditioned ATR sizing (money-side complement to separation gates). | active | #1081 |
| `research/regime_1083_multi_asset_gate.py` | Multi-asset breadth gate orchestrating the single-cell separation/economic gates. | active | #1083 |
| `research/regime_1076_certify.py` | Producer for the directional-certification artifact (SSoT for the regime→direction gate). | active | #1085 |

## Research one-shots (reproducible evidence, tied to a closed issue)

| File | Purpose | Status | Origin |
|------|---------|--------|--------|
| `research/regime_1073_directional_negative_result.py` | Evidence the 7-state composite has no real forward-*return* separation (but strong forward-vol). | one-shot | #1073 |
| `research/regime_1076_directional_premise.py` | Scope-1: does the regime label predict forward *direction*? | one-shot | #1076 |
| `research/regime_1076_economic_sim.py` | Scope-2: does picking trade side by regime label earn risk-adjusted PnL over base? | one-shot | #1076 |
| `research/regime_1076_aggregate.py` | Aggregates the #1076 battery under one global multiple-comparisons correction. | one-shot | #1076 |
| `research/regime_1080_unsupervised_vol_model.py` | Evidence run for the unsupervised vol-state bake-off. | one-shot | #1080 |
| `research/regime_1095_enriched_vol_model.py` | Evidence run extending the bake-off to the enriched feature matrix. | one-shot | #1095 |
| `research/regime_1120_trail_validation.py` | Incumbent vs proposed regime opening-trail defaults. | one-shot | #1120 |
| `research/regime_1152_exit_retune.py` | M6 entry-locked retune of ranging ratchet ladders + B2 ranging TP group. | one-shot | #1152 |
| `research/regime_1211_incumbent_baseline.py` | Re-measures whether the promotion gate's incumbent baseline is trustworthy, or whether the shipping rule must be redesigned. | one-shot | #1211 |
| `consolidation_research.py` | Consolidation characterization study — segments consolidation regions in history. | one-shot | consolidation |
| `consolidation_strategy_sim.py` | `consolidation_range` entry/exit rules over history, look-ahead safe. | one-shot | consolidation |
| `consolidation_strategy_sweep.py` | Parameter sweep for `consolidation_range` with in-sample/out-of-sample split. | one-shot | consolidation |
| `consolidation_sweep.py` | Fetch-once in-process consolidation parameter-grid runner. | one-shot | consolidation |

## Support libraries

| File | Purpose | Status | Origin |
|------|---------|--------|--------|
| `registry_loader.py` | Loads the spot/futures strategy registry by platform for backtest use. | active | core |

(`regime_stats.py` is listed under the regime-promotion pipeline; it is also imported by `auto_suggest.py`.)

## Candidate studies (`backtest/candidates/`)

Each `backtest/candidates/<study>/` is a self-contained study for one strategy.
Most bundle **executable driver scripts** — per-study screens and gates that
shell out to the M-series harnesses above (M1 shortlist scoring, M2 close-stack
sweeps, M4 regime-gate sweeps, walk-forward fold stability, fee-drag screens,
continuous-audit headlines, live-label fidelity) — alongside JSON specs (fed to
`eval_windows.py` / `auto_suggest.py` via `--candidate-json`) and a `README.md`
recording the study's steps. Two studies share a `driver_common.py` library. A
study may also be spec-only (e.g. `ichimoku_997`: JSON configs + README, no
drivers). The drivers are study-local and largely parallel across studies, so
they are indexed by study here, not enumerated script-by-script.

| Study | What it screens | Drivers | Origin |
|-------|-----------------|---------|--------|
| `breakout_984` | `breakout` close-stack (M2) sweeps, walk-forward, fee-drag, M1 shortlist, audit headline. | 6 | #984 |
| `breakout_1165` | `breakout` regime-gate (M4) sweep, fee-drag, M1 shortlist, audit headline, live-label fidelity (+`driver_common.py`). | 5 + lib | #1165 |
| `squeeze_983` | `squeeze_momentum` close-stack (M2) sweeps, walk-forward, M1 shortlist, audit headline. | 5 | #983 |
| `squeeze_momentum_1198` | `squeeze_momentum` regime-gate (M4) sweep, fee-drag, M1 shortlist, audit headline (+`driver_common.py`). | 4 + lib | #1198 |
| `rahtf_1054` | `regime_adaptive_htf` entry-condition split behind the M1 noise verdict. | 1 | #1054 |
| `limbo_1282` | Spec-only — auto_suggest BH family specs (breakout 984/1165 arms, mean_reversion_pro #981 knobs, chart_pattern #982 gate factors) behind the M5-limbo adjudication (`docs/research/1282-m5-limbo-verdicts.md`). | 0 | #1282 |
| `ichimoku_997` | Spec-only — `atr_stop`/`time_stop`/`zscore` close-config candidates + README, no drivers. | 0 | #997 |

`suggest.template.jsonc` is the annotated spec template for a new study.

## Where results are recorded

- Committed run artifacts: `backtest/research/*.json` (e.g. `regime_1211_baseline_remeasure.json`).
- Narrative writeups: `docs/research/*.md`.
- Cross-cutting mechanics and negative results: `docs/ARCHITECTURE.md` § Backtest harnesses.
