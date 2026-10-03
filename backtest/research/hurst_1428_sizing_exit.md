# Hurst as a SIZING and EXIT input (#1428)

Every Hurst study on this repository's record asks ADMISSION: should this trade be allowed to happen. #1410, #1422, #1424, #1426 and #1427 all score an entry decision. Two questions that framing never reaches are how BIG the trade should be and when it should END, and this study asks exactly those two, on the same tape, under the same power discipline.

Report-only. This study ships NO threshold and recommends NO configuration. Both arms measure whether a quantity SORTS outcomes; neither searches a threshold, and `decide_recommendation` has no branch that promotes one. A positive finding licenses a follow-up DESIGN issue, never a shipped constant from this run.

## Verdict

Headline: **inconclusive**.

### sizing arm

- Verdict: **inconclusive** over 6 primary hypotheses; 0 reached Benjamini-Hochberg significance at alpha=0.05, 0 were untestable.
- Best cluster p: 0.035396 (`mean_reversion/sizing/W256/shipped`), separation +0.001956.
- Validity gate: FAILED on `momentum`: the detection limit is 0.099000 efficiency units while those SAME rows separate by only +0.047105, BELOW the limit. Nothing at or below that limit is VISIBLE to this design, so this null bounds the effect from above and says nothing either way about a smaller one.
- Kept-side effective N: 52.5 against a floor of 30.

### exit arm

- Verdict: **inconclusive** over 2 primary hypotheses; 0 reached Benjamini-Hochberg significance at alpha=0.05, 0 were untestable.
- Best cluster p: 0.308169 (`mean_reversion/exit/W512/g0.5`), separation -0.021057.
- Validity gate: FAILED on `momentum`: the detection limit is 0.180000 pp of net return, as a paired difference while those SAME rows separate by only -0.021650, BELOW the limit. Nothing at or below that limit is VISIBLE to this design, so this null bounds the effect from above and says nothing either way about a smaller one.
- Kept-side effective N: 1033.4 against a floor of 30.

## Contract path

CONTRACT PATH, resolved MECHANICALLY from this run's verdict rather than asserted up front. `hurst_gate_calibration.md` is the live-evidence path cited by `scheduler/hurst_gate.go` and #1412's Stage 0. #1426 (PR #1452) DEFERS it because it is exploratory-only, and #1427 (PR #1473) DEFERS it because the shipped gate reads the LEVEL of H and never its change, so its committed artefact passes the supersede clause to this study alone. This study is the only remaining one that scores a quantity the shipped code implements, so the clause is available to it. It is claimed ONLY on a confirmatory result, which is the precedent #1426 set and the maintainer restated on the issue: an exploratory or inconclusive run must not move the live evidence, whatever it finds. `claims_contract_path` below reads the decision object, so no human judgement sits between the verdict and the claim.

This run DEFERS `hurst_gate_calibration.md` to `hurst_1424_gate_resolution.py`: no arm returned a confirmatory result whose validity gate passed on a non-degenerate limit, and the maintainer precedent is that only a confirmatory result may move the live evidence. #1426 and #1427 defer for their own reasons.

## Pre-registered design

SHIPPED FORM, the reason this study exists. `scheduler/hurst_gate.go` implements `mode: size` as `clamp(|H - 0.5| / 0.15, size_floor, 1.0)` and applies it where the position size is COMPUTED, and `backtest/hurst_gate.py` mirrors it bar-for-bar as the parity module the Backtester already shares. #1410 swept a DIFFERENT curve, `clamp(1 + gain * e, 0, 1.5)`, which can exceed 1.0 and therefore describes something the shipped code cannot do. No committed artefact has ever scored the form the code actually implements, so the shipped multiplier is uncalibrated BY CONSTRUCTION. This study does not re-implement the form. It CALLS `hurst_gate.HurstGate.size_multiplier`, the same object the Backtester and `parity_diff.py` use, so a drift in the shipped form cannot leave this study scoring a stale curve: the import would move with it, and the import-time assertions below fail loud if the landmarks change.

SIZING VALIDITY CONTRAST, stated per arm because the gate's own kept-versus-suppressed partition does not survive the move to sizing. #1424 already defines a size-arm contrast: weighted permutation p-values, plus a `multiplier < 1.0` versus `multiplier >= 1.0` split that feeds the effective-N coverage floors. That split is well behaved for #1410's curve, which can exceed 1.0, so its `>= 1.0` side is a genuine interval. Under the SHIPPED form the multiplier is CAPPED at 1.0, so `>= 1.0` collapses to the exact-equality set `|H - 0.5| >= 0.15`, which is the TAIL of the H distribution rather than an interval around its middle. The contrast is still row-matched and still signed, and it is the honest one for this form, but its kept side is small by construction. This study therefore reports the kept-side effective N beside every sizing verdict and treats a breach of the 30-effective-trade floor as a POWER statement, never as a null about the market.

EXIT FORM, ONE rule pre-registered before the run and never swept, because power is the binding constraint on this tape and a second form would double the hypothesis family for nothing. A trade's trailing stop distance is scaled by the PERSISTENCE side its entry bar sits on: the persistent side widens the trail by 0.5, the anti-persistent side tightens it by the same amount, the two middle buckets and the NaN bucket keep the base distance exactly. The landmarks are #1410's own committed bucket edges (0.45 and 0.55), not new constants, and 'persistent side' is read through the family's SENSE so a mean-reversion exemplar orients the same way its entry gate does. The base distance is 2 x ATR, the value this repository already uses as its manual-strategy stop default and the same multiple `EXEMPLAR_CLOSE_OVERRIDES` pins for `atr_band_revert`. THE HYPOTHESIS IS THAT PERSISTENCE PREDICTS HOW FAR A MOVE RUNS, so a persistent entry should be given more room before the trail takes it out.

EXIT MECHANISM, named because the Backtester does not expose a per-trade trailing distance and the issue's 'real Backtester re-runs' criterion constrains the mechanism without picking it. `Backtester` takes ONE scalar `trailing_stop_atr_mult` per run, so a persistence-conditional trail cannot be expressed inside a single run. This study uses BUCKET-CONDITIONAL RE-RUNS: for each persistence bucket it masks entry signals down to that bucket exactly as the gate arm masks them, and runs the Backtester twice on the masked series - once at the base distance and once at that bucket's scaled distance. Every leg on both sides is a real `Backtester.run`, and the two sides share their entry masking, so the contrast isolates the EXIT change rather than the masking. A research-local close hook was refused: it would score a code path the live system does not have.

EXIT VALIDITY CONTRAST, stated per arm because an exit change has no kept-versus-suppressed partition at all: it modifies EVERY trade's exit rather than removing some trades. The contrast is therefore PAIRED and row-matched by construction. A bucket's masked baseline run and its masked scaled run admit the SAME entries, so each entry bar carries both outcomes, and the row's value is the signed difference `scaled - baseline` on the arm's primary target. Rows in the neutral and NaN buckets take the base distance on both sides, so their difference is exactly 0 by construction and they form the control side. The published separation is `mean(difference | scaled bucket) - mean(difference | neutral bucket)`, which is signed, row-matched, and reduces to the mean paired effect. The cluster-rotation null rotates the bucket labels over the calendar exactly as #1422 rotates the gate's, so correlated concurrent trades still move together. An entry that appears on only one side of a pair is DROPPED and counted, never imputed.

A paired row has TWO exits, one per side, and the cluster model needs one holding interval per row. The row takes the LATER of the two, so an overlap between two rows is never understated and the effective-N correction can only be conservative. A row whose exit is unreadable on either side carries `exit_ns=None` and is dropped by `effective_n` exactly as #1422 drops such a row, rather than being imputed.

EXIT ARM WINDOW. The exit arm scores exactly ONE rolling-Hurst window, W512, the window #1424's committed primary hypothesis `momentum/gate/W512/arm0.52/dis0.48` already pins. Sweeping all three would triple this arm's hypothesis family and its Backtester cost for a design question the study is not asking, and the pin is inherited rather than chosen here. It is asserted against #1424's committed config id at import time, so a drift there fails loud instead of silently scoring a different window.

TARGETS. The sizing arm keeps #1424's pairing unchanged - signed fixed-horizon Kaufman efficiency over 96h as the primary, net return as continuity - because a size change does not move when a trade ends, so a fixed-horizon outcome stays well defined on its rows. The exit arm SWAPS them: its whole purpose is to change when a trade ends, which makes a FIXED-horizon statistic a poor primary, so net return on the ACTUAL holding period is primary there and efficiency is retained as continuity. The swap is a property of the arm and is pre-registered here, not chosen after the numbers were seen.

DIRECTION, pre-registered as a constant: the test is TWO-SIDED on both arms. Both hypotheses are naturally directional - a bigger size on a more persistent entry should pay, and a wider trail on a persistent entry should pay - and a one-sided test of either would be cheaper in power. It is refused for the reason already on this repository's record: the only effect ever MEASURED on these rows, #1424's confirmatory separation of -0.005 efficiency units, pointed the way its one-sided design could not detect at any size. #1426 exists solely to remove that blind spot and #1427 kept it removed. Re-introducing it here would repeat the same mistake on the same tape. THE COST IS REAL: a two-sided limit can only be at or above its one-sided counterpart at the same alpha. The SIGN is carried and reported everywhere.

p2 = min(1, 2 * min(p_ge, p_le)), each tail carrying the add-one convention over the SAME draws; the smallest reachable p is 2/(draws+1)

PRIOR EXPOSURE, disclosed before the run. The OUTCOME rows are the same tape #1424, #1426 and #1427 scored, and their results are committed. Neither PREDICTOR is: no committed artefact in this repository has ever scored the shipped sizing form, and none has ever tested Hurst on the exit side at all, so neither contrast has been seen and neither sign was known when these constants were fixed. A pre-registered confirmatory claim is therefore available on both arms. What is NOT available is a claim of an independent sample: the outcomes are shared, the effective sample size is set by the same calendar clusters, and the detection limit is of the same order as #1424's. Read a finding here as evidence about NEW quantities on OLD rows.

The pre-registered prediction is INCONCLUSIVE on both arms, and the reason is a power reason rather than a market one. Effective N here is set by independent CALENDAR CLUSTERS rather than rows, and this study scores the same calendar #1424 scored, so its two-sided detection limit can only be at or above the 0.013 efficiency units #1426 measured. The sizing arm carries a second, sharper power risk that is specific to the shipped form: its kept side is the tail `|H - 0.5| >= 0.15`, so the kept-side effective N may fall under the 30-trade floor on its own. The falsifiable half is the limit: if a measured limit comes back BELOW 0.013 on these rows, this prediction's stated mechanism was wrong. The machinery decides the verdict either way.

A detection limit of 0.000 is DEGENERATE, not excellent. The injection search returns it when the zero-injection contrast already clears the significance bar, so the smallest grid step is 'detectable' only because the un-injected data was already significant. A validity gate that passes against a 0.000 limit therefore certifies nothing about the effect SIZE, and this study labels every such pass explicitly rather than reading it as a resolved measurement. #1427 hit exactly this case and its artefact says so; the same disclosure is inherited here unchanged.

### Exit trail ladder

| Bucket | Scale | Trail (x ATR) | In contrast |
|---|---|---|---|
| `persistent` | 1.5 | 3 | scaled |
| `neutral` | 1 | 2 | control |
| `anti_persistent` | 0.5 | 1 | scaled |
| `NaN` | 1 | 2 | control |

## Detection limits

| Arm | Family | Rows | Limit | Separation | Units |
|---|---|---|---|---|---|
| sizing | `momentum` | 8035 | 0.099000 | +0.047105 | efficiency units |
| sizing | `mean_reversion` | 21138 | 0.062000 | +0.021251 | efficiency units |
| exit | `momentum` | 12990 | 0.180000 | -0.021650 | pp of net return, as a paired difference |
| exit | `mean_reversion` | 41563 | 0.060000 | -0.021057 | pp of net return, as a paired difference |

## Configurations - primary cohort

| Config | Arm | Rows (eff.) | kept/suppressed eff. | cluster p | BH | Separation | Verdict |
|---|---|---|---|---|---|---|---|
| `mean_reversion/exit/W512/g0.5` | exit | 41563 (3469.7) | 2452.1/3378.7 | 0.3082 | no | -0.021057 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.308169); 2017: chop loss not reduced (+5.79 pp), return give-up 4.67 pp exceeds tolerance 4.66 pp; 2018: drawdown not reduced (+0.40 pp), chop loss not reduced (+6.06 pp); 2021: drawdown not reduced (+0.77 pp), chop loss not reduced (+1.16 pp); holds on only 1/4 protocol windows with legs (need 3); drawdown holds on only 5/12 held-out windows with legs (need 0.67 of them, min 3 windows) |
| `mean_reversion/sizing/W128/shipped` | sizing | 21138 (1697.4) | 940.5/1640.7 | 0.8075 | no | +0.006299 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.807519); 2017: return give-up 1629176.85 pp exceeds tolerance 165133.50 pp; 2018: return give-up 6872.25 pp exceeds tolerance 737.32 pp; 2021: return give-up 3454338.25 pp exceeds tolerance 346585.44 pp; 2022: return give-up 2196.85 pp exceeds tolerance 246.15 pp; holds on only 0/4 protocol windows with legs (need 3) |
| `mean_reversion/sizing/W256/shipped` | sizing | 21138 (1697.4) | 372.6/1672.4 | 0.0354 | no | +0.001956 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.035396); 2017: return give-up 1642759.37 pp exceeds tolerance 165133.50 pp; 2018: return give-up 6991.04 pp exceeds tolerance 737.32 pp; 2021: return give-up 3463349.02 pp exceeds tolerance 346585.44 pp; 2022: return give-up 2325.19 pp exceeds tolerance 246.15 pp; holds on only 0/4 protocol windows with legs (need 3) |
| `mean_reversion/sizing/W512/shipped` | sizing | 21138 (1697.4) | 170.9/1682.8 | 0.8921 | no | +0.021251 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.892111); 2017: return give-up 1648418.31 pp exceeds tolerance 165133.50 pp; 2018: return give-up 7181.45 pp exceeds tolerance 737.32 pp; 2021: return give-up 3464093.05 pp exceeds tolerance 346585.44 pp; 2022: return give-up 2384.65 pp exceeds tolerance 246.15 pp; holds on only 0/4 protocol windows with legs (need 3) |
| `momentum/exit/W512/g0.5` | exit | 12990 (1382.6) | 1033.4/1256.6 | 0.5473 | no | -0.021650 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.547345) |
| `momentum/sizing/W128/shipped` | sizing | 8035 (456.8) | 301.2/441.1 | 0.5985 | no | +0.007859 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.59854); 2017: return give-up 1245.86 pp exceeds tolerance 172.28 pp; 2021: return give-up 186.98 pp exceeds tolerance 33.69 pp; holds on only 2/4 protocol windows with legs (need 3) |
| `momentum/sizing/W256/shipped` | sizing | 8035 (456.8) | 158.8/447.8 | 0.7099 | no | +0.014617 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.709929); 2017: return give-up 1400.71 pp exceeds tolerance 172.28 pp; 2021: return give-up 242.34 pp exceeds tolerance 33.69 pp; holds on only 2/4 protocol windows with legs (need 3) |
| `momentum/sizing/W512/shipped` | sizing | 8035 (456.8) | 52.5/453.3 | 0.3054 | no | +0.047105 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.305369); 2017: return give-up 1509.66 pp exceeds tolerance 172.28 pp; 2021: return give-up 257.19 pp exceeds tolerance 33.69 pp; holds on only 2/4 protocol windows with legs (need 3) |

## Configurations - exploratory cohort

| Config | Arm | Rows (eff.) | kept/suppressed eff. | cluster p | BH | Separation | Verdict |
|---|---|---|---|---|---|---|---|
| `mean_reversion/exit/W512/g0.5` | exit | 11909 (1120.3) | 726.5/1078.5 | 0.1882 | no | -0.025878 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.188181); is: drawdown not reduced (+0.33 pp); oos: drawdown not reduced (+1.36 pp), chop loss not reduced (+2.52 pp); holds on only 0/2 protocol windows with legs (need 2); drawdown holds on only 1/3 held-out windows with legs (need 0.67 of them, min 3 windows) |
| `mean_reversion/sizing/W128/shipped` | sizing | 6242 (555.3) | 271.4/539.0 | 0.2526 | no | -0.009765 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.252575); is: return give-up 125.31 pp exceeds tolerance 18.49 pp; oos: return give-up 42.32 pp exceeds tolerance 7.99 pp; holds on only 0/2 protocol windows with legs (need 2) |
| `mean_reversion/sizing/W256/shipped` | sizing | 6242 (555.3) | 92.4/551.3 | 0.2500 | no | -0.030501 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.249975); is: return give-up 136.53 pp exceeds tolerance 18.49 pp; oos: return give-up 56.13 pp exceeds tolerance 7.99 pp; holds on only 0/2 protocol windows with legs (need 2) |
| `mean_reversion/sizing/W512/shipped` | sizing | 6242 (555.3) | 15.4/554.0 | 0.1864 | no | -0.102633 | only 15.4 effective kept rows (floor 30); not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.186381); is: return give-up 156.32 pp exceeds tolerance 18.49 pp; oos: return give-up 65.50 pp exceeds tolerance 7.99 pp; holds on only 0/2 protocol windows with legs (need 2) |
| `momentum/exit/W512/g0.5` | exit | 3391 (390.2) | 240.9/378.0 | 0.5018 | no | -0.056547 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.50175); is: drawdown not reduced (+0.29 pp), chop loss not reduced (+0.78 pp); holds on only 1/2 protocol windows with legs (need 2) |
| `momentum/sizing/W128/shipped` | sizing | 2067 (118.5) | 77.5/116.5 | 0.3140 | no | -0.012068 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.313969) |
| `momentum/sizing/W256/shipped` | sizing | 2067 (118.5) | 31.0/117.2 | 0.7679 | no | +0.005093 | not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.767923); is: return give-up 1.73 pp exceeds tolerance 1.00 pp; holds on only 1/2 protocol windows with legs (need 2) |
| `momentum/sizing/W512/shipped` | sizing | 2067 (118.5) | 4.0/118.5 | 0.8451 | no | +0.070633 | only 4.0 effective kept rows (floor 30); not significant after Benjamini-Hochberg on the primary target's cluster p (cluster p=0.845115); is: return give-up 1.13 pp exceeds tolerance 1.00 pp; holds on only 1/2 protocol windows with legs (need 2) |

## Run

- Legs: 860; sizing rows: 37923; exit paired rows: 69853; unpaired exit entries dropped: 6188.
- Datasets: 26; windows: 16; permutations: 10000 (limits 2000); seed 1428.
- Scope complete: True; pre-registered inference: True.
- Warm-up: sufficient on every dataset.
- Wall time: 3985.4 s.
