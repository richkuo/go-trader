# Ranging ratchet-tier ladders + B2 ranging TP group — M6 entry-locked retune (#1152)

**Verdict: every incumbent stands.** No candidate geometry cleared the
pre-registered promotion gate robustly across both entry styles, so the
per-substate ratchet ladders (`ratchetTierGroupDefaults` /
`DEFAULT_RATCHET_TIERS_BY_GROUP`) and the collapsed B2 ranging TP group
(`regimeTPTierGroupDefaults["ranging"]` = `(0.5, 50%) → (1.0, 100%)`) ship
unchanged. The `ranging_quiet` ladder is **unevaluable on the audit data**
(the label occupies 0.2–0.9% of bars; zero gated entries under either entry
style) — an evidence gap documented below, not a validation. The strongest
future candidate is a wider B2 volatile ladder (`b2_rv_wider`), which passed
the gate under mean-reversion entries but failed the out-of-sample window
under squeeze entries.

This closes the deferral recorded in
`backtest/research/regime_1120_trail_validation.json:ratchet_tp_note`
(#1120 / PR #1149 retuned only the opening trails; the bar-level harness
cannot isolate per-substate exit geometry).

## Method

Harness: `backtest/exit_policy_ab.py` (M6, #1066) — incumbent-relative,
entry-locked per-entry replay, Wilcoxon signed-rank + sign test on per-entry
ΔPnL, regime-attributed. Driver + full matrix:
`backtest/research/regime_1152_exit_retune.py`; aggregate artifact:
`backtest/research/regime_1152_exit_retune.json`; raw per-run harness JSON:
`backtest/research/regime_1152_runs/`.

- **Gating = substate isolation.** Each run gates entries to one composite
  ranging substate (`--allowed-regimes`, composite p20 via
  `regime.windows.medium`). The entry gate and the position-regime stamp read
  the same shifted (N-1) label series, so every paired entry exercises exactly
  the ladder under test. The bare `ranging_directional` gate covers the
  `_up`/`_down` sub-labels (#1124).
- **Incumbent arms resolve like live** (`--baseline-config`, #951-parity
  v16 configs committed next to the driver): ratchet runs =
  `trailing_tp_ratchet_regime {use_defaults}` +
  `trailing_stop_atr_regime {use_defaults}` opening trails; B2 runs =
  `tiered_tp_atr_regime {use_defaults}` + scalar `stop_loss_atr_mult: 1.5`
  (the ranging SL default). Candidate arms inherit the identical stops
  (`--candidate-stops inherit`), so each A/B isolates the ladder change alone.
- **Ratchet candidates** are explicit regime-keyed `tp_tiers` on the same
  evaluator (exact-match keys, so the directional runs key the bare label AND
  `_up`/`_down`). **B2 candidates** use scalar `tiered_tp_atr` — exact under a
  single-substate gate, since every entry is stamped with that substate.
- **Two entry styles** so a verdict is never an artifact of one open
  strategy's entry timing: `squeeze_momentum` (`sq.`, breakout — the #1120
  lineage) and `mean_reversion` (`mr.`, band-reversion — the style that
  actually trades ranges; 3–15× the paired N). Both `direction: both`,
  futures registry.
- Audit datasets (BTC/ETH/SOL × 1h/4h, binanceus fees) and walk-forward
  windows `is` (2025-06-10 → 2026-01-01) / `oos` (2026-01-01 →) from
  `eval_windows.py`; capital/bootstrap/seed at harness defaults.
- **Pre-registered promotion gate** (`_verdict` in the driver): pooled Δnet/e
  > 0 on BOTH windows, ≥1 individually significant (p<.05) dataset, no
  significant contradiction — required from **both** entry styles before a
  fleet default changes.

Two enabling code changes shipped with this study: `trailing_tp_ratchet`,
`trailing_tp_ratchet_regime`, and `tiered_tp_atr_regime` added to M6's
`_REPLAYABLE_CLOSE_NAMES` (they fire off price/ATR/the frozen open-stamp —
self-contained per entry), and `backtester._load_trailing_ratchet` now ensures
the close-strategies `sys.path` entry before module exec (it crashed with
`ModuleNotFoundError: _helpers` in any process that hadn't already imported
the close registry — exactly the M6 CLI path).

## Results — pooled Δnet%/entry (candidate − incumbent; positive favours candidate)

`n` = paired entries; `+/−ds` = datasets by delta sign; `sig` = datasets with
Wilcoxon p<.05 (signed by direction).

### Ratchet ladders

| run | substate | is | oos | verdict |
|---|---|---|---|---|
| `sq.rv_wider` (1.5/3.0/4.5) | volatile | −0.082 (n=68, 1+/5−) | +0.024 (n=35, 2+/2−) | incumbent stands |
| `mr.rv_wider` | volatile | +0.012 (n=333, 3+/3−) | +0.023 (n=233, 3+/3−) | positive, no sig |
| `sq.rv_quiet_geometry` (0.75/1.5/2.0) | volatile | +0.046 (n=68, 4+/2−) | +0.092 (n=35, 4+/1−) | positive, no sig |
| `mr.rv_quiet_geometry` | volatile | −0.012 (n=333, 2+/4−) | +0.011 (n=233, 3+/3−) | incumbent stands |
| `sq.rd_full_scaleout` (kill the runner) | directional | +0.101 (n=23, 3+/0−) | +0.123 (n=16, 5+/1−) | positive, no sig |
| `mr.rd_full_scaleout` | directional | −0.016 (n=167, **3 sig−**) | −0.004 (n=127, 1 sig+) | incumbent stands |
| `sq.rd_lighter` (15/35/60) | directional | −0.031 (n=23) | −0.063 (n=16, 1+/5−) | incumbent stands |
| `mr.rd_lighter` | directional | +0.003 (n=167, 2+/4−) | +0.004 (n=127, 2+/4−) | positive, no sig |
| `*.rq_*` (both candidates, both opens) | quiet | n=0 | n=0 | **unevaluable** |

**Volatile:** the two candidates point in opposite directions per entry style
(squeeze prefers tighter, mean-reversion mildly prefers wider), all
non-significant — the incumbent middle ladder (1.0/2.0/3.0) is exactly
between the two candidates and stands.

**Directional:** the higher-power mean-reversion run **rejects removing the
let-ride runner** (3 significantly-negative datasets in-sample when the 4th
rung is dropped and scale-out is completed at 75→100%); the squeeze run's
non-significant preference the other way is 7× smaller in N. Lighter early
scale-out adds nothing. The #1059 runner ladder survives its inverse check.

### B2 ranging TP group (collapsed incumbent `(0.5, 50%) → (1.0, 100%)`)

| run | substate | is | oos | verdict |
|---|---|---|---|---|
| `sq.b2_rv_wider` (0.75/1.5) | volatile | +0.060 (n=68, 1 sig+) | −0.050 (n=34, 2+/3−) | incumbent stands |
| `mr.b2_rv_wider` | volatile | +0.005 (n=343, **2 sig+**) | +0.050 (n=241, **2 sig+**) | **passes gate** |
| `sq.b2_rv_patient3` (1.0/2.0/3.0) | volatile | −0.062 (n=68) | −0.115 (n=34) | incumbent stands |
| `mr.b2_rv_patient3` | volatile | +0.065 (n=343, 1 sig+) | −0.002 (n=241) | incumbent stands |
| `sq.b2_rd_patient3` | directional | +0.082 (n=23) | −0.037 (n=16) | incumbent stands |
| `mr.b2_rd_patient3` | directional | −0.136 (n=176, 1+/5−) | −0.015 (n=132) | incumbent stands |
| `sq.b2_rd_wider2` (1.0/2.0) | directional | +0.099 (n=23) | −0.053 (n=16) | incumbent stands |
| `mr.b2_rd_wider2` | directional | −0.142 (n=176, 1+/5−) | −0.020 (n=132) | incumbent stands |
| `*.b2_rq_wider` (both opens) | quiet | n=0 | n=0 | **unevaluable** |

**Directional patience is refuted**, not just unproven: both wider/patient
directional candidates lose ~0.14%/entry in-sample under mean-reversion
entries (5 of 6 datasets negative). The collapsed fast exit is right for the
directional substate.

**`b2_rv_wider` is the one near-miss.** Under mean-reversion entries it passes
the full gate (positive both windows; ETH 1h and SOL 1h individually
significant in BOTH windows, p≤0.003; no significant contradiction), but under
squeeze entries the OOS pooled delta is negative (n=34, non-significant), and
even in the passing mean-reversion run all three 4h datasets are negative
in-sample. A fleet-wide on-chain default does not
change on evidence that one entry style contradicts out-of-sample. **Decision:
keep the collapsed group; re-run `--only sq.b2_rv_wider,mr.b2_rv_wider` once
the OOS window has accumulated more squeeze entries — consistent
cross-entry-style positives would justify a `ranging_volatile` B2 split to
`(0.75, 50%) → (1.5, 100%)`.**

## The `ranging_quiet` evidence gap

Composite p20 label frequencies over 2025-06-10 → 2026-06-01 (cache snapshot
`shared_tools/trading_bot.db`, 2026-07-02):

| dataset | ranging_quiet | ranging_volatile | ranging_directional (family) |
|---|---:|---:|---:|
| BTC/USDT 1h | 0.2% | 19.3% | 11.3% |
| ETH/USDT 1h | 0.2% | 18.7% | 8.7% |
| SOL/USDT 4h | 0.9% | 16.3% | 8.2% |

Quiet ranges essentially do not occur on crypto 1h/4h at these thresholds:
across all six audit datasets, both windows, and both entry styles, the gated
runs produced **zero** control entries. This is structural (the label is
near-absent), not an entry-style power problem. The quiet ladder — which is
also the bare-ADX `ranging` target and predates #1059 unchanged — therefore
keeps its long-lived incumbent geometry by default. Any future validation
needs either a quieter asset class in the audit set or relaxed composite
thresholds, both out of scope here.

## Reproduction

```
uv run --no-sync python backtest/research/regime_1152_exit_retune.py --jobs 6
# single run:
uv run --no-sync python backtest/research/regime_1152_exit_retune.py --only mr.b2_rv_wider
```

Deterministic given the cache snapshot (harness seed 1066, fixed windows).

## Addendum 2026-07-05 — re-run under the corrected simulation geometry (#1243)

Re-ran the full M6 matrix (`regime_1152_exit_retune.py --jobs 6`, 18 A/B runs)
on current `main` (the #1238 audit fixes plus #1250 fee-net-per-trade and #1251
canonical Sortino / `None` profit-factor / half-open windows), identical cache
snapshot. **Verdict holds: every incumbent stands; no ratchet or B2 geometry
ships.** This study exercises exactly the surface the corrections touch —
per-entry ΔPnL on ratchet / tiered-TP ladders with ATR geometry — so several
per-run labels moved, but none crosses the promotion line and the decisive
cross-entry-style gate is unchanged.

**The gate outcome is intact.** The pre-registered gate needs a candidate to
clear BOTH entry styles. The only run that passes under either style,
`mr.b2_rv_wider`, reproduces essentially to the digit (IS +0.005 sig+2 → +0.005
sig+2; OOS +0.050 sig+2 → +0.048 sig+2; still `candidate_beats_incumbent`), and
its squeeze counterpart `sq.b2_rv_wider` still fails out-of-sample (OOS -0.050 →
-0.054, `incumbent_stands`) — if anything the corrected geometry weakened the
squeeze side (its lone IS sig-positive dataset dropped, sig+1 → sig+0). The
cross-style contradiction that blocks the `ranging_volatile` B2 split therefore
persists. **No promotion guidance changes.**

**Three per-run labels shifted, all within the non-promoting band**
(`incumbent_stands` ↔ `positive_but_not_significant`, never reaching
`candidate_beats_incumbent`):

| run | documented | re-run | Δnet/entry shift (is / oos) |
|---|---|---|---|
| `mr.rv_wider` | positive_but_not_significant | incumbent_stands | +0.012 → **-0.004** / +0.023 → +0.016 |
| `mr.rv_quiet_geometry` | incumbent_stands | positive_but_not_significant | -0.012 → +0.007 / +0.011 → +0.005 |
| `mr.b2_rv_patient3` | incumbent_stands | positive_but_not_significant | +0.065 (sig+1) → +0.026 (sig 0) / -0.002 → +0.007 |

All three stay non-significant (0 individually-significant datasets after the
shift), so none is a gate pass; two moved more conservative, one slightly less,
consistent with the audit's "generally more conservative" closed-bar geometry
plus small paired-N changes (n 333 → 334 from the #1251 half-open boundary bar).
Every other run's verdict label is unchanged. The `ranging_quiet` evidence gap
(zero gated entries) is structural and likewise unchanged. **Bottom line: the
#1152 "every incumbent stands" verdict, and the standing recommendation to
re-run `b2_rv_wider` cross-style once the OOS window accumulates more squeeze
entries, both stand under the corrected engine.**

---
Created with LLM: Fable 5 | high | Harness: Claude Code
Updated with LLM: Opus 4.8 | high | Harness: Claude Code
