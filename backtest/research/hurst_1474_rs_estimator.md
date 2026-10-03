# Hurst estimator comparison: rescaled range against DFA (#1474)

Report-only estimator comparison over the #1424 pool. It adds a classic rescaled-range (R/S) estimator beside the #1409 DFA single source of truth and asks three questions: how closely the two agree per row, how each is biased and how wide each is on a memoryless series at every window length, and whether the estimator choice moves the #1424 gate separation at all.

NON-GOALS, fixed before the run and enforced by the acceptance criteria. This study adds NO `metrics["hurst_rs"]` key to the live payload; `shared_tools/regime.py`, `scheduler/hurst_gate.go` and `config.example.json` are untouched; no threshold and no estimator swap is recommended. `hurst_exponent` stays the #1409 single source of truth for every live and backtest path and is byte-identical after this work, and `hurst_rescaled_range` is a SECOND estimator that only this research harness reads. A follow-up issue may promote R/S to the live payload only if the agreement section shows a material, signed difference on the confirmatory family.

CONTRACT PATH: this study DEFERS. `hurst_gate_calibration.md` is the live-evidence path cited by `scheduler/hurst_gate.go` and #1412's Stage 0. An estimator comparison decides nothing about a shipping gate: it re-scores the SAME pinned hypothesis under a second measuring instrument, and its whole purpose is to say how much the instrument moves the number. `hurst_1424_gate_resolution.py` keeps the path, and this study's `main` refuses it unconditionally.

## Pre-registered key risk

The gate-separation section is expected to stay INCONCLUSIVE under every estimator, and the reason is inherited rather than new. #1424 measured -0.005098 efficiency units against a row-matched limit of 0.013, and #1426 re-measured -0.004617 against the same 0.013 two-sided. A second estimator changes the measuring instrument and it does not change the pool, the effective N, or the calendar-cluster structure that sets the limit. What the section buys is a bound on ESTIMATOR RISK: if R/S moves the separation by less than the detection limit, the #1424 verdict was not an artefact of the estimator choice, and that is a statement #1424 could not make about itself. A move at or above the limit is the opposite finding, and it licenses a follow-up rather than a threshold. It is a PREDICTION and not a requirement: the machinery below decides.

## Run summary

- Legs scored: 860
- Datasets: 26
- Windows scored: 2013, 2014, 2015, 2016, 2017, 2018, 2019, 2020H1, 2020H2, 2021, 2022, 2023, 2024, 2025H1, is, oos
- Rolling Hurst windows: 128, 256, 512, 1000 (live: 128, 256, 512)
- Estimators: dfa, rs_anis_lloyd, rs_raw
- Pooled `momentum` trades: 10211 (8118 primary cohort)
- Permutation draws: 10000 for p, 2000 for the detection limit; seed 1474
- Elapsed: 16134.95s
- History backfill: SKIPPED, scored on the venue caches as they stood

WARM-UP SHORTFALL at the 1000-bar reference window on 1 dataset(s): SOL/USDT 4h. The reference estimate is UNDEFINED on their earliest scored bars, those bars simply drop out of the agreement rows, and the live windows are unaffected because coverage was audited against them.

## 1. Agreement between the two estimators

The Anis-Lloyd correction is a CONSTANT SHIFT at a fixed window. The corrected estimate fits the same log-log regression against `log(R/S) - log(E[R/S])`, the block grid depends only on the window length, and least squares is linear, so `H_corrected = H_raw - c(W) + 0.5` for one constant `c(W)` per window. Raw and corrected R/S therefore carry IDENTICAL Spearman correlation against DFA and identical row ordering, and they differ only in WHERE the 0.5 edge falls. That edge is exactly what the gate reads, so the two are reported as separate estimators in the separation section rather than folded together.

The 1000-bar reference window is a RESEARCH window and it cannot run live as things stand. `backtest/hurst_gate.py`'s `hurst_live_frame_bars` fetches `max(200, 2*maxPeriod-1+10)` bars, which is 200 bars at every regime period this repo configures, so a 1000-bar rolling estimate is undefined on every live cycle. It is reported here only to show where the two estimators converge once the sample is long enough. Reading it as a live option would need a deeper fetch that no issue has proposed.

Rows are the bars on which BOTH estimators are defined. The side share counts the bars where the two fall on opposite sides of 0.5, the edge the gate's own `anti_signal_side` reads. The signed difference is `candidate - DFA`.

### W128

| Estimator | Rows | Pearson | Spearman | Mean signed diff | Mean abs diff | Opposite side of 0.5 |
|---|---:|---:|---:|---:|---:|---:|
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 1036958 | 0.5059 | 0.4945 | -0.0298 | 0.0882 | 31.64% |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 1036958 | 0.5059 | 0.4945 | +0.0813 | 0.1091 | 39.83% |

Persistent-side share at W128: DFA 42.10%, Anis-Lloyd R/S 33.94% over 1036958 shared bars across 26 datasets.

### W256

| Estimator | Rows | Pearson | Spearman | Mean signed diff | Mean abs diff | Opposite side of 0.5 |
|---|---:|---:|---:|---:|---:|---:|
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 1036958 | 0.6333 | 0.6167 | -0.0136 | 0.0471 | 27.66% |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 1036958 | 0.6333 | 0.6167 | +0.0792 | 0.0843 | 45.54% |

Persistent-side share at W256: DFA 41.49%, Anis-Lloyd R/S 32.74% over 1036958 shared bars across 26 datasets.

### W512

| Estimator | Rows | Pearson | Spearman | Mean signed diff | Mean abs diff | Opposite side of 0.5 |
|---|---:|---:|---:|---:|---:|---:|
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 1036958 | 0.6443 | 0.6245 | -0.0051 | 0.0321 | 27.56% |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 1036958 | 0.6443 | 0.6245 | +0.0731 | 0.0746 | 51.55% |

Persistent-side share at W512: DFA 42.10%, Anis-Lloyd R/S 35.19% over 1036958 shared bars across 26 datasets.

### W1000 (research-only reference window)

| Estimator | Rows | Pearson | Spearman | Mean signed diff | Mean abs diff | Opposite side of 0.5 |
|---|---:|---:|---:|---:|---:|---:|
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 1036582 | 0.6719 | 0.6401 | +0.0010 | 0.0231 | 26.68% |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 1036582 | 0.6719 | 0.6401 | +0.0681 | 0.0685 | 56.80% |

Persistent-side share at W1000: DFA 40.13%, Anis-Lloyd R/S 38.62% over 1036582 shared bars across 26 datasets.

## 2. Bias and spread on a memoryless series

#1409 chose DFA over classic rescaled range with the note "R/S is too noisy at the window lengths this system uses", and that claim was never measured on this repo's data. The bias section is that measurement: it reports the centre and the spread of every estimator on a memoryless series at each window length, so the claim is answered with a number instead of being restated.

500 draws per sample size, independent Gaussian log-return random walks, sigma=0.01, base price 100.0, one numpy default_rng per draw seeded from the issue number, so every cell is reproducible. Seed rule `SEED * 1_000_000 + n * 1_000 + draw`. `n` counts PRICE points, so an estimator sees `n - 1` log returns and needs at least `min_points` = 100 of them.

| n | Estimator | Defined | Mean | Bias vs 0.5 | Q25 | Q75 | IQR | SD |
|---:|---|---:|---:|---:|---:|---:|---:|---:|
| 101 | DFA, the #1409 SSoT `hurst_exponent` | 500/500 | 0.5112 | +0.0112 | 0.4292 | 0.5793 | 0.1501 | 0.1196 |
| 101 | R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 500/500 | 0.4672 | -0.0328 | 0.3438 | 0.5886 | 0.2447 | 0.1755 |
| 101 | R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 500/500 | 0.5856 | +0.0856 | 0.4622 | 0.7069 | 0.2447 | 0.1755 |
| 128 | DFA, the #1409 SSoT `hurst_exponent` | 500/500 | 0.5127 | +0.0127 | 0.4361 | 0.5867 | 0.1507 | 0.1048 |
| 128 | R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 500/500 | 0.4625 | -0.0375 | 0.3773 | 0.5486 | 0.1713 | 0.1303 |
| 128 | R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 500/500 | 0.5736 | +0.0736 | 0.4883 | 0.6596 | 0.1713 | 0.1303 |
| 256 | DFA, the #1409 SSoT `hurst_exponent` | 500/500 | 0.5069 | +0.0069 | 0.4593 | 0.5512 | 0.0919 | 0.0717 |
| 256 | R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 500/500 | 0.4857 | -0.0143 | 0.4314 | 0.5384 | 0.1070 | 0.0789 |
| 256 | R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 500/500 | 0.5786 | +0.0786 | 0.5242 | 0.6313 | 0.1070 | 0.0789 |
| 512 | DFA, the #1409 SSoT `hurst_exponent` | 500/500 | 0.5041 | +0.0041 | 0.4694 | 0.5380 | 0.0686 | 0.0504 |
| 512 | R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 500/500 | 0.4865 | -0.0135 | 0.4522 | 0.5232 | 0.0710 | 0.0495 |
| 512 | R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 500/500 | 0.5647 | +0.0647 | 0.5304 | 0.6014 | 0.0710 | 0.0495 |
| 1000 | DFA, the #1409 SSoT `hurst_exponent` | 500/500 | 0.5030 | +0.0030 | 0.4784 | 0.5297 | 0.0513 | 0.0375 |
| 1000 | R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 500/500 | 0.4872 | -0.0128 | 0.4638 | 0.5115 | 0.0476 | 0.0371 |
| 1000 | R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 500/500 | 0.5543 | +0.0543 | 0.5309 | 0.5786 | 0.0476 | 0.0371 |
| 2000 | DFA, the #1409 SSoT `hurst_exponent` | 500/500 | 0.5013 | +0.0013 | 0.4790 | 0.5252 | 0.0462 | 0.0321 |
| 2000 | R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 500/500 | 0.4902 | -0.0098 | 0.4708 | 0.5108 | 0.0399 | 0.0297 |
| 2000 | R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 500/500 | 0.5477 | +0.0477 | 0.5283 | 0.5683 | 0.0399 | 0.0297 |

## 3. Gate separation under each estimator

The pinned #1424 confirmatory hypothesis is `momentum/gate/W512/arm0.52/dis0.48` on the `momentum` family, sense `arms_on_high_h`. Every row below is ROW-MATCHED: a trade is scored only where EVERY estimator is defined at that window, so the three estimators split the identical row set and only the partition changes. Separations are signed and read against each estimator's OWN row-matched limit, never against a pooled one and never through `abs()`.

Inference is two-sided, inherited from #1426: p2 = min(1, 2 * min(p_ge, p_le)), each tail carrying the add-one convention over the SAME draws; the smallest reachable p is 2/(draws+1). The confirmatory bar is alpha for a family of 1, that is 0.05. The primary target is `signed_fixed_horizon_efficiency` over 96h and the continuity target is `pnl_pct_net`.

### W128

Row matching: 8035 scored rows from 8035 matched (0 dropped because at least one estimator was undefined, 83 for a missing target, 0 for a calendar cluster too short to rotate).

| Estimator | Rows | Kept | Suppressed | Eff kept | Eff suppressed | Separation | Limit | Two-sided p | Gate |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| DFA, the #1409 SSoT `hurst_exponent` | 8035 | 3221 | 4814 | 357.7 | 443.2 | -0.016993 | 0.000 | 0.0002 | ok |
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 8035 | 2759 | 5276 | 353.2 | 458.3 | +0.004412 | 0.018 | 0.1246 | below_limit |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 8035 | 5824 | 2211 | 418.3 | 398.8 | -0.000109 | 0.011 | 0.5611 | below_limit |
| DFA on its OWN rows (not row-matched, #1426 row set) | 8035 | 3221 | 4814 | 357.7 | 443.2 | -0.016993 | 0.000 | 0.0002 | ok |

- Separation move versus DFA, R/S, Anis-Lloyd corrected, `hurst_rescaled_range`: +0.021405 efficiency units.
- Separation move versus DFA, R/S, raw slope, `hurst_rescaled_range(corrected=False)`: +0.016884 efficiency units.

Continuity target, net return in percentage points:

| Estimator | Separation (pp) | Limit (pp) | Two-sided p |
|---|---:|---:|---:|
| DFA, the #1409 SSoT `hurst_exponent` | -0.991420 | 0.000 | 0.0184 |
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | +0.104528 | 1.080 | 0.8003 |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | +0.121421 | 1.160 | 0.8101 |

### W256

Row matching: 8035 scored rows from 8035 matched (0 dropped because at least one estimator was undefined, 83 for a missing target, 0 for a calendar cluster too short to rotate).

| Estimator | Rows | Kept | Suppressed | Eff kept | Eff suppressed | Separation | Limit | Two-sided p | Gate |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| DFA, the #1409 SSoT `hurst_exponent` | 8035 | 3246 | 4789 | 364.9 | 426.8 | -0.002148 | 0.013 | 0.7183 | below_limit |
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 8035 | 2784 | 5251 | 340.5 | 447.5 | -0.002190 | 0.012 | 0.9279 | below_limit |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 8035 | 6979 | 1056 | 439.5 | 293.0 | +0.003597 | 0.020 | 0.2852 | below_limit |
| DFA on its OWN rows (not row-matched, #1426 row set) | 8035 | 3246 | 4789 | 364.9 | 426.8 | -0.002148 | 0.013 | 0.7183 | below_limit |

- Separation move versus DFA, R/S, Anis-Lloyd corrected, `hurst_rescaled_range`: -0.000042 efficiency units.
- Separation move versus DFA, R/S, raw slope, `hurst_rescaled_range(corrected=False)`: +0.005745 efficiency units.

Continuity target, net return in percentage points:

| Estimator | Separation (pp) | Limit (pp) | Two-sided p |
|---|---:|---:|---:|
| DFA, the #1409 SSoT `hurst_exponent` | -0.439165 | 1.640 | 0.2614 |
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | -0.173492 | 1.500 | 0.6407 |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | +0.365552 | 1.980 | 0.7627 |

### W512

Row matching: 8035 scored rows from 8035 matched (0 dropped because at least one estimator was undefined, 83 for a missing target, 0 for a calendar cluster too short to rotate).

| Estimator | Rows | Kept | Suppressed | Eff kept | Eff suppressed | Separation | Limit | Two-sided p | Gate |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| DFA, the #1409 SSoT `hurst_exponent` | 8035 | 3143 | 4892 | 355.9 | 414.9 | -0.003946 | 0.012 | 0.6381 | below_limit |
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | 8035 | 2997 | 5038 | 356.8 | 420.6 | +0.001990 | 0.013 | 0.2298 | below_limit |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | 8035 | 7541 | 494 | 448.3 | 164.8 | +0.001766 | 0.021 | 0.9695 | below_limit |
| DFA on its OWN rows (not row-matched, #1426 row set) | 8035 | 3143 | 4892 | 355.9 | 414.9 | -0.003946 | 0.012 | 0.6381 | below_limit |

- Separation move versus DFA, R/S, Anis-Lloyd corrected, `hurst_rescaled_range`: +0.005936 efficiency units.
- Separation move versus DFA, R/S, raw slope, `hurst_rescaled_range(corrected=False)`: +0.005712 efficiency units.

Continuity target, net return in percentage points:

| Estimator | Separation (pp) | Limit (pp) | Two-sided p |
|---|---:|---:|---:|
| DFA, the #1409 SSoT `hurst_exponent` | -0.074700 | 1.240 | 0.8767 |
| R/S, Anis-Lloyd corrected, `hurst_rescaled_range` | +0.901935 | 1.800 | 0.1680 |
| R/S, raw slope, `hurst_rescaled_range(corrected=False)` | +0.253657 | 4.200 | 0.9825 |

### Verdict on estimator risk

**THE ESTIMATOR CHOICE DOES NOT MOVE THE #1424 NUMBER**

The estimator move is BOUNDED. A swap from DFA to Anis-Lloyd R/S at W512 moves the row-matched separation by 0.005936 efficiency units, which is BELOW the 0.012 detection limit the same rows carry, so on this pool the #1424 verdict is not an artefact of the estimator choice. This bounds the INSTRUMENT and it says nothing about the market: both estimators sit under the same limit, so neither resolves an effect and no threshold ships.

## What this study cannot say

It cannot recommend an estimator for the live path. It scores ONE pinned hypothesis on ONE pool whose sign was already visible before this design was fixed, it ships no threshold, and it leaves `hurst_exponent` as the single source of truth every live and backtest path reads. A bounded estimator move is a statement about the instrument on these rows and it is not evidence that the gate works. An estimator move at or above the limit licenses a follow-up ISSUE and never a configuration change.

