# regime_adaptive_htf M1 adjudication — deprecate recommendation (#1054)

Part of the M1 program (#977/#978, protocol #995); M5 source #999
(`docs/research/fee-audit-m5.md`). The fee audit graduated
`regime_adaptive_htf` as `graduate_m1` — gross +0.27%/leg, net -0.66%/leg over
37 trades — which presumes a real gross edge exists for selectivity to
salvage. The issue pre-registered the adjudication order: **noise check
first**, mechanism work only if the edge survives, and "if it is statistically
indistinguishable from zero, the verdict flips to deprecate and we close this
with that finding." The edge did not survive. **Verdict: documented deprecate
recommendation** (scoped below — the strategy is also the pending M4 reference
case).

Evidence artifacts + exact reproduce commands: `backtest/candidates/rahtf_1054/`.
New shared tooling: `backtest/gross_edge_noise.py` (M1 step-2 sample-noise
adjudicator, reusable for the two remaining `graduate_m1` rows,
`regime_adaptive` and `tema_cross`).

## Baseline reproduction (M1 step 1)

M5 row (rank 41, generated 2026-06-12) reproduced 2026-07-01 on the identical
cache state, exactly:

| | trades | trades/yr | gross %/leg | net %/leg | fee drag (pp) | net Sharpe | verdict |
|---|---:|---:|---:|---:|---:|---:|---|
| committed M5 row | 37 | 6.3 | +0.27 | -0.66 | 0.94 | -0.05 | `graduate_m1` |
| reproduction | 37 | 6.2 | +0.27 | -0.66 | 0.94 | -0.05 | `graduate_m1` |

(trades/yr 6.2 vs 6.3 is span rounding; every scored figure matches.)

Full M1 net baseline (registry defaults, 8-incumbent median bar recomputed per
window × dataset, 6 audit datasets):

| Window | mean Sharpe / bar | mean DDadj / bar | verdict |
|--------|-------------------|------------------|---------|
| IS (2025-06-10→2026-01-01) | +0.23 / -0.12 | +0.75 / -0.14 | PASS |
| OOS (2026-01-01→latest)    | -0.32 / -0.75 | -0.20 / -0.49 | PASS |
| 2023                        | +0.01 / +1.46 | +1.29 / +3.67 | FAIL |
| 2024                        | +0.09 / +0.90 | +0.08 / +1.07 | FAIL |
| 2025H1                      | -1.09 / -0.42 | -0.46 / -0.37 | FAIL |

Protocol IS+OOS still PASS on the *relative* bar (the incumbent median is
itself deeply negative on those windows — beating it is compatible with the
M5 finding that absolute net is ≤ 0), held-out 0/3. This is the #976 ship
picture unchanged: the strategy clears a bar defined by bleeding incumbents
and loses to it whenever markets trend.

## Noise adjudication (M1 step 2 — the issue's pre-registered gate)

Tool: `backtest/gross_edge_noise.py` — re-runs the fee audit's zero-friction
gross legs (`eval_windows.run_leg`, commission and slippage zeroed: the
identical harness, therefore the identical 37-trade universe) and tests the
pooled per-trade gross returns. Primary test, pre-registered: one-sided
sign-flip permutation on the mean (10000 resamples, seed 1066). Bootstrap CI,
exact sign test, and Wilcoxon are reported as supporting views, never blended
into the verdict.

**On the M5 screen's own slices (is+oos):**

| statistic | value |
|---|---|
| n (per-trade gross returns) | 37 |
| mean / median | +0.082% / +0.290% per trade |
| min / max | -5.46% / +6.24% |
| positive trades | 24/37 |
| **sign-flip permutation (primary)** | **p = 0.3913** |
| bootstrap 95% CI on mean | [-0.510, +0.671], P(mean≤0) = 0.39 |
| sign test (two-sided exact) | p = 0.0989 |
| Wilcoxon signed-rank | p = 0.2944 |
| per-leg view (the M5 statistic) | n=12, mean +0.273%/leg, permutation p = 0.4075, CI [-1.673, +2.423] |

Verdict: **INDISTINGUISHABLE FROM ZERO**. The +0.27%/leg headline is a
mean-zero draw at every level tested.

**Pooled across all five protocol/held-out windows (is, oos, 2023, 2024,
2025H1)** — the wider sample the thin screen pair couldn't provide:

| statistic | value |
|---|---|
| n | 176 |
| mean / median | **-0.026%** / +0.375% per trade |
| positive trades | 111/176 (sign test p = 0.0007) |
| sign-flip permutation (primary) | p = 0.5518 |
| bootstrap 95% CI on mean | [-0.405, +0.331] |
| per-leg view | n=30 legs, mean **-0.180%/leg** |

Verdict: **NO_POSITIVE_EDGE** — with 4.8× the sample, the gross mean is
negative. The significant sign test alongside a negative mean is the
diagnostic signature, not a contradiction: the fade wins small often
(median +0.375%) and loses big rarely (left tail to -11.70%). The mean —
the thing that compounds into equity and that the M5 salvage test measures —
is zero-to-negative; the 37-trade +0.27% was the lucky draw of a
median-positive, mean-zero distribution.

## Mechanism view (issue step 3 — descriptive only, per protocol)

The protocol stops mechanism work at a failed noise gate — sweeping
selectivity knobs *after* the pooled edge reads ≤ 0 would just mine the
sample for a surviving cell (multiple comparisons with no correction
possible at n=37). The descriptive split (`entry_condition_split.py`,
signal-bar `rah_label` join per the fills-at-N+1 contract) confirms there is
nothing for a knob to isolate anyway:

- **One entry condition.** Every trade — 37/37 on the M5 slices, 176/176
  all-window — entered on `ranging_volatile`. `ranging_quiet` never fires (a
  2σ z-excursion inside a quiet range is self-contradictory), so
  `fade_labels` has no sub-vocabulary to tighten.
- **The only visible splits flip sign between pools.** Timeframe: 1h
  +0.612%/trade on is+oos becomes -0.058% all-window; 4h -1.173% becomes
  +0.101%. Window: 2024 is all-legs-positive, 2025H1 is deeply negative.
  Post-hoc slices of a mean-zero sample, exactly as expected.
- **The fee arithmetic was already near-impossible.** Net needs +0.66pp/leg
  against 0.94pp/leg of drag at the frequency floor (6.3 trades/yr,
  fourth-lowest of the 44 audit rows that traded) — selectivity would have to
  more than triple the per-trade edge with fewer than 37 trades to select
  from. With the edge itself indistinguishable from zero, there is no numerator.

## Verdict table (issue task item 5)

| M1 step | outcome |
|---|---|
| 1. Reproduce baseline | Exact — M5 row matches figure-for-figure; full net table: protocol PASS (relative bar), held-out 0/3 |
| 2. Sample-noise check | **Failed the gate**: permutation p = 0.39 (M5 slices); mean gross negative pooled all-window (`NO_POSITIVE_EDGE`) |
| 3. Mechanism isolation | Not run (protocol stop); descriptive split shows a single entry condition, no selectivity axis |
| 4. Plateau + held-out | N/A — no selectivity change ships |
| 5. Verdict | **Deprecate recommendation** with noise evidence (this document) |

## Recommendation and scope

1. **Fade-only default (both registrations, spot and futures — byte-identical
   `default_params`): deprecate.** The `graduate_m1` premise is refuted; no
   further M1/selectivity effort on the shipped fade-only config.
2. **Hold the discovery-hide until the M4 adjudication lands.**
   `regime_adaptive_htf` is the M4 reference case (#998 / PR #1002): the
   trend-entry profile and the switched fade↔trend composite are a *different
   trade universe* whose full protocol run was left as the operator research
   step (`eval_windows.py --profile-allocation`), and this issue's constraint
   was to coordinate, not adjudicate, that axis. Concretely:
   - if the M4 protocol run fails too → add `regime_adaptive_htf` to
     `DISCOVERY_HIDDEN_STRATEGIES` (the #1035 mechanism: hidden from
     `--list-json`/init discovery, still registered for explicit configs and
     backtests) in a follow-up PR — the same two-step (research verdict, then
     dedicated hide PR) used for the four current members;
   - if it passes, the strategy survives as the M4 profile vehicle and only
     the fade-only default's `graduate_m1` claim dies (this finding stands
     either way — the M4 case never rested on the fade edge).
3. **The two remaining `graduate_m1` rows (`regime_adaptive`, `tema_cross`)
   should run this same gate before any selectivity work** — both have far
   larger samples (495 and 444 trades), so the tool will actually have power
   there; their gross means may well survive. That is exactly the reusable
   value of `gross_edge_noise.py`.

No registry, param, or scheduler change ships with this finding —
`--list-json` is untouched.

## Reproduce

See `backtest/candidates/rahtf_1054/README.md` for the six exact commands
(fee-audit row, full M1 table, both noise runs, both entry-condition splits).
Runs executed 2026-07-01; cache state matches the M5 audit (row reproduces
exactly); statistics deterministic at seed 1066, 10000 resamples.

Generated: 2026-07-01

---
Created with LLM: Fable 5 | high | Harness: Claude Code + live M1 runs
