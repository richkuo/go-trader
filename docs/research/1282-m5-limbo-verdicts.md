# M5 limbo adjudication — final verdicts for the 11 pending strategies (#1282)

Generated: 2026-07-10. Data: the shared OHLCV cache (`shared_tools/trading_bot.db`),
last bars 2026-07-10; all runs on post-#1228/#1242/#1271 corrected geometry.

The M5 fee audit (#999, `fee-audit-m5.md`) left 11 strategies in evidence limbo:
three `graduate_m1` verdicts whose #1054 noise gate never ran, and eight
`unscreened_short` legs the long/flat harness never measured. This study runs
the full adjudication pipeline for every one — short-leg screen (#989
`--direction short`), sample-noise permutation gate (#1054
`gross_edge_noise.py`), M1 incumbent-relative protocol (`eval_windows.py`),
and Benjamini–Hochberg family passes (`auto_suggest.py`, specs under
`backtest/candidates/limbo_1282/`) — and records a final VALIDATED or
DEPRECATE verdict for each. Suggest-only throughout; the roster edits shipped
with this PR are the human promotion/deprecation calls made on this evidence.

## Method notes

1. **Wide-pool noise gate is the primary gross-edge test.** The protocol
   `oos` window (2026-01-01 → latest) is the 2026 crash tape. A short leg
   measured only on `is`+`oos` systematically over-reads: every short leg
   below that looked "healthy" or noise-gate-positive on `is`+`oos` turned
   indistinguishable-or-negative once pooled over all five M1 windows
   (`is,oos,2023,2024,2025H1` — `gross_edge_noise.py --windows`, the
   docstring's "wider pooled sample" mode, calendar-coverage deduped). The
   symmetric benefit applies to long legs: three legs whose `is`+`oos` pool
   was noise show a real gross edge over the full sample. Both directions are
   reported below; the wide pool decides.
2. **M1 pass/fail is vs the incumbent bar** (median of 8 incumbents per
   window/dataset), not absolute profitability. Where the name-level call
   hinges on absolute net PnL (roster semantics: "no measured edge"), mean
   per-leg net return is quoted directly.
3. **Harness bug found and fixed in this PR:** `exit_policy_ab._binom_two_sided_p`
   overflowed (`math.comb(n,i)` → float) past n ≈ 1030, crashing every
   wide-pool `gross_edge_noise.py` run on high-churn legs. Rewritten in log
   space (lgamma); regression-tested in `backtest/tests/test_exit_policy_ab.py`
   (the red run is the OverflowError these runs hit).

Reproduce (per strategy; substitute registry/direction):

```
uv run --no-sync python backtest/fee_audit.py --registry futures --strategies <name> --direction short
uv run --no-sync python backtest/gross_edge_noise.py --strategy <name> --registry <reg> [--direction short] --windows is,oos,2023,2024,2025H1
uv run --no-sync python backtest/eval_windows.py --strategy <name> --registry <reg> [--direction short]
uv run --no-sync python backtest/auto_suggest.py --spec backtest/candidates/limbo_1282/suggest_<family>.json
```

## Short-leg screens (fee_audit `--direction short`, is+oos)

| strategy | reg | trades | tr/yr | gross%/leg | net%/leg | drag pp | M5-style verdict |
|---|---|---|---|---|---|---|---|
| tema_cross_bd | futures | 810 | 136.2 | +18.08 | -2.35 | 20.43 | graduate_m1 |
| regime_adaptive | futures | 749 | 125.9 | +14.21 | -4.44 | 18.65 | graduate_m1 |
| triple_ema_bidir | futures | 803 | 135.0 | +13.16 | -7.21 | 20.38 | graduate_m1 |
| consolidation_range | futures | 623 | 104.7 | -6.66 | -20.16 | 13.51 | deprecate |
| funding_skew | futures | 148 | 24.9 | +6.34 | +2.52 | 3.82 | healthy |
| mtf_confluence | futures | 82 | 13.8 | +16.95 | +14.82 | 2.13 | healthy |
| chart_pattern | spot | 185 | 31.1 | +20.58 | +15.33 | 5.25 | healthy |
| momentum_pro | spot | 56 | 9.4 | +18.60 | +17.19 | 1.40 | healthy |
| mean_reversion_pro | spot | 20 | 3.4 | +10.36 | +9.92 | 0.44 | healthy |

Every "healthy" row above is an is+oos artifact of the 2026 crash tape — see
the wide-pool column below.

## Noise gates (gross_edge_noise, permutation p on pooled per-trade gross)

| leg | is+oos pool | wide pool (5 windows) | wide-pool verdict |
|---|---|---|---|
| tema_cross spot long | p=0.459, n=454 | **p=0.0001**, n=1706, +0.41%/tr | DISTINGUISHABLE_POSITIVE |
| regime_adaptive spot long | p=0.214, n=505 | **p=0.0002**, n=1961, +0.30%/tr | DISTINGUISHABLE_POSITIVE |
| breakout futures long | p=0.253, n=265 | **p=0.0003**, n=1000, +0.89%/tr | DISTINGUISHABLE_POSITIVE |
| momentum_pro spot long | — | **p=0.0022**, n=186, +5.77%/tr | DISTINGUISHABLE_POSITIVE |
| mean_reversion_pro spot long | — | **p=0.0006**, n=75, +9.38%/tr | DISTINGUISHABLE_POSITIVE |
| chart_pattern spot long (f0) | — | **p=0.0001**, n=709, +1.41%/tr | DISTINGUISHABLE_POSITIVE |
| chart_pattern spot long (f4) | — | **p=0.0013**, n=192, +6.48%/tr | DISTINGUISHABLE_POSITIVE |
| tema_cross_bd futures short | p=0.038 | p=0.588, n=2805, -0.02%/tr | NO_POSITIVE_EDGE |
| regime_adaptive futures short | p=0.045 | p=0.991, n=2622, -0.18%/tr | NO_POSITIVE_EDGE |
| triple_ema_bidir futures short | p=0.075 | p=0.846, n=2763, -0.07%/tr | NO_POSITIVE_EDGE |
| funding_skew futures short | p=0.206 | p=0.991, n=487, -0.90%/tr | NO_POSITIVE_EDGE |
| consolidation_range futures short | — (gross<0) | p=1.000, n=2034, -0.59%/tr | NO_POSITIVE_EDGE |
| mtf_confluence futures short | p=0.015 | p=0.396, n=274, +0.14%/tr | INDISTINGUISHABLE_FROM_ZERO |
| momentum_pro spot short | p=0.013 | p=0.845, n=188, -0.92%/tr | NO_POSITIVE_EDGE |
| chart_pattern spot short | p=0.011 | p=0.645, n=703, -0.09%/tr | NO_POSITIVE_EDGE |
| mean_reversion_pro spot short | p=0.076, n=20 | p=0.753, n=82, -5.17%/tr | NO_POSITIVE_EDGE |

The pattern is uniform: **no short leg in the set survives the wide pool.**
Every is+oos short "pass" was the 2026 crash window.

## M1 protocol (eval_windows, net vs incumbent bar) — short legs

| leg | is | oos | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| tema_cross_bd futures short | FAIL | PASS | FAIL | FAIL | FAIL | 0/3 |
| regime_adaptive futures short | FAIL | PASS | FAIL | FAIL | FAIL | 0/3 |
| mtf_confluence futures short | PASS | PASS | FAIL | FAIL | PASS | 1/3 |
| momentum_pro spot short | PASS | PASS | FAIL | FAIL | PASS | 1/3 |
| chart_pattern spot short | FAIL | PASS | FAIL | FAIL | PASS | 1/3 |

Same shape as `session_breakout` (#1031), `vol_momentum` (#1021), and the
`momentum_pro` unconditional short (#980): a bear/crash-window edge that does
not survive bull years and, per the wide pool, is not statistically
separable from zero over the full sample.

## Family BH passes (auto_suggest, specs in `backtest/candidates/limbo_1282/`)

**breakout (984/1165 arms, futures):** family {baseline, comp_up_clean_p21,
m4_bear_selective}. All share the entry, so one noise family: wide-pool
p=0.0003, survives BH. M1: baseline is+oos PASS, held-out 0/3 (unchanged
from #984); `comp_up_clean_p21` is+oos PASS **+ 2025H1 PASS** — reproducing
the #1165 result on current data. The #1228 re-measurement follow-up for
these studies was already discharged by #1252 (dated addenda; no verdict
flips); this run independently concurs.

**mean_reversion_pro (#981 frequency knobs, spot long):** family {baseline,
touch_entry, turn_entry, touch_plus_turn}. Noise: baseline (p=0.0006) and touch_entry (p=0.0197)
survive the BH threshold (0.0197); turn_entry (p=0.041) and touch_plus_turn
(p=0.142) do not. M1: **all four incumbent_stands** — no knob
config beats the incumbent bar on the protocol pair. The #981 mechanisms are
adjudicated: no default change, knobs stay default-off.

**chart_pattern (#982/#1167 gate variants, spot long):** family {f0, f4, f5,
f6}. All four noise runs survive BH; M1: **gate_f4 is the sole survivor** —
is, oos, 2023, 2025H1 PASS (4/5; 2024 still fails, the known bull-year
regression). f5/f6 fail oos, reproducing the #982 factor-fragility finding
under BH. The #1167 regime-switched gate was already a documented negative;
no re-run needed.

## Per-strategy verdicts

1. **tema_cross (spot) — DEPRECATE.** Real wide-pool gross edge
   (+0.41%/trade, p=0.0001) fully consumed by churn (75.6 tr/yr, ~10pp
   drag/leg): net is negative on the M5 screen and no selectivity mechanism
   exists or was validated. M1 long: FAIL on all five windows vs the
   incumbent bar (held-out 0/3). Quarantined; the
   recovery path (per the fee_audit re-screen rule) is a validated
   selectivity config that harvests the gross edge net of fees.
2. **regime_adaptive (spot+futures) — DEPRECATE.** Spot long: same
   fee-consumed shape (wide-pool p=0.0002, net -8.46%/leg is+oos; M1 long
   FAIL on all five windows). Futures short: wide-pool NO_POSITIVE_EDGE,
   M1 held-out 0/3.
3. **breakout (futures) — VALIDATED.** Wide-pool gross edge real; #956
   crash-window keep verdict + #984 keep-default-stack (close-stack space
   exhausted) + #1165 `comp_up_clean_p21` documented default-off gate, all
   re-established post-#1228 (#1252) and reproduced here under BH. Not
   quarantined; no default change.
4. **mean_reversion_pro (spot) — VALIDATED (keep, knobs stay default-off).**
   Long leg carries a real low-churn edge (wide-pool +9.38%/trade, p=0.0006,
   0.44pp drag) and is strongly net-positive in the bull held-outs (mean
   +160.8%/leg 2023, +55.7%/leg 2024) — the M5 limbo row was an is+oos
   starvation sample (18 trades). It fails the strict promotion bar (is
   FAIL vs incumbents), so nothing is promoted; but the name demonstrably
   has an edge, so it does not belong in a "no measured edge" quarantine.
   Short leg: no edge (wide pool). #981 knobs adjudicated (above).
5. **triple_ema_bidir (futures) — DEPRECATE.** Long leg gross -8.77%/leg
   (M5); short leg wide-pool NO_POSITIVE_EDGE (p=0.846, n=2763).
6. **tema_cross_bd (futures) — DEPRECATE.** Long leg gross -5.97%/leg; short
   leg is+oos p=0.038 but wide-pool NO_POSITIVE_EDGE and M1 held-out 0/3.
7. **funding_skew (futures) — DEPRECATE.** Long leg gross -9.83%/leg; short
   leg's is+oos "healthy" (+2.52 net) is a crash artifact — wide pool
   -0.90%/trade, p=0.991 (n=487, incl. a -64.6% trade).
8. **consolidation_range (futures) — DEPRECATE.** Both legs gross-negative
   on every pool (short wide-pool p=1.000, incl. a -359% trade).
9. **mtf_confluence (futures) — DEPRECATE stands (stays quarantined).** The
   short leg is the strongest limbo candidate measured here (is+oos net
   +14.82%/leg, noise p=0.015, M1 is+oos+2025H1 PASS) — but the wide pool
   is indistinguishable (p=0.396, n=274, +0.14%/trade) and held-out bulls
   fail. Same crash-artifact shape as the rest; not promotion evidence. The
   name stays in the quarantine roster (spot leg was already a settled
   deprecate). Recovery path: a regime-gated short config validated across
   held-outs.
10. **momentum_pro (spot) — VALIDATED (keep; short stays unshipped).** Long
    leg: real wide-pool gross edge (+5.77%/trade, p=0.0022) at 9.4 tr/yr
    (1.4pp drag); M1 long: is, 2023, 2025H1 PASS (held-out 2/3; oos fails —
    a long leg in a crash window). Short leg: is+oos
    protocol PASS but wide-pool NO_POSITIVE_EDGE — consistent with #980
    (unconditional short fails bulls) and #1166 (regime-gating the short is
    a documented negative across every gate shape; the #980 follow-on was
    already run there, contrary to the issue's premise). Nothing promoted;
    name keeps its registration and incumbent role.
11. **chart_pattern (spot) — VALIDATED (f4 opt-in re-affirmed under BH).**
    Long leg gross edge real at both f0 and f4; `htf_gate_factor=4` is the
    sole M1 survivor of the BH family pass (4/5 windows, 2024 regression
    unchanged) — strengthening #982's default-off opt-in with a
    multiple-comparison-corrected pass. Short leg: crash-artifact shape, no
    wide-pool edge, stays unshipped. Default stays `htf_gate_factor=0`.

## Roster changes shipped with this PR

Added to `M5_DEPRECATED_EDGE_STRATEGIES` (registry.py) and
`m5DeprecatedEdgeStrategies` (edge_status.go), keeping the two rosters
identical (parity test updated 26 → 32):

- `consolidation_range`, `funding_skew`, `regime_adaptive`, `tema_cross`,
  `tema_cross_bd`, `triple_ema_bidir`

`mtf_confluence` stays quarantined (no removal). `mean_reversion_pro`,
`momentum_pro`, `chart_pattern`, `breakout` are NOT quarantined (verdicts
above). The roster's meaning extends slightly with this PR: it now also
covers names whose gross edge is real but demonstrably unharvestable at the
audit fee model with no validated selectivity config (`tema_cross`,
`regime_adaptive`, and the graduate_m1 short legs) — the operational intent
("this config has no validated net edge; don't run it live unknowingly")
is unchanged.

`starterSpotStrategyID` moves `tema_cross` → `chart_pattern` (the init
wizard's default must never be a quarantined name; chart_pattern carries the
strongest adjudicated spot evidence in this study). Quarantined names are
pruned from the init wizard's default strategy lists per the #1275 guard
test.

## Follow-ups (unfiled)

- A selectivity M1 application for `tema_cross` / `regime_adaptive` spot
  longs (real gross edges, ~0.3–0.4%/trade, fully fee-consumed today) is the
  designed recovery path out of quarantine; nobody has attempted it.
- A held-out-validated regime-gated short for `mtf_confluence` futures is
  the analogous recovery path for its crash-window short edge.
