# regime_adaptive entry filters

Issue 2017. This study asks whether an existing entry filter keeps a positive return after fees and funding.
The protocol was locked before the judgment windows were scored. The shortlist was written before those windows were scored.

Protocol `a28e98acc455405dbbd33c4592e6f7c7c0a0a97d205e74a2c50b14172abbfc9e`. Lock file `protocol_lock.json`. Selection `85574ffcf3c79b1dc3200e6d94c2e1370da3d2a15d1ba9221748919d370f353d`.

## Verdicts

| Frame | Direction | Verdict | Suggestion arm | Reason |
| --- | --- | --- | --- | --- |
| audit | long | inconclusive | combo | suggestion arm is below the held-out sample floor |
| perps | long | inconclusive | combo | native Hyperliquid candles do not cover every frozen judgment window, and the scored book has no venue lot or minimum spec |
| perps | short | inconclusive | combo | native Hyperliquid candles do not cover every frozen judgment window, and the scored book has no venue lot or minimum spec |
| perps | both | inconclusive | combo | native Hyperliquid candles do not cover every frozen judgment window, and the scored book has no venue lot or minimum spec |

## What the frames are

Spot-registry long entries on BTC, ETH and SOL at 1h and 4h. Candles are the Binance US cache files. Fees are the corrected Hyperliquid schedule. Funding is charged because run_leg prices a Hyperliquid frame when no stop context overrides strategy_type, which is the fee_audit convention. This is not a spot-exchange execution model and it is not a native Hyperliquid candle study. The audit verdict uses this path because it is the historical measurement path.

Futures-registry entries at 30m and 1h. Each direction is its own run, so long, short and both do not share cash or position order. The both run sequences both sides on one cash book. The 2026-11-10 note of a Hyperliquid perps deployment at 30m and 1h with direction both is unverified and names no symbol, stop, size or leverage. This frame is a research context, not that deployment. A perps verdict needs native Hyperliquid candles covering every judgment window, including warm-up, and a lot and minimum execution spec. The scored book is the proxy cache path, which has neither. The proxy measurements are reported and cannot change the perps verdict.

## Windows

train, judge_2023, judge_2025a, judge_is and judge_oos share no dates. judge_2025a ends where judge_is starts, so the pair does not copy the overlapping M1 is and 2025H1 windows. continuous overlaps train and every judgment window. It is the drawdown path only and is not a selection sample or a significance sample.

Issues 1282 and 1315 already measured regime_adaptive on the M1 windows is, oos, 2023, 2024 and 2025H1. Those calendars overlap this study. The lock does not make them unseen. The 2023 window here is the later part of that year, not the M1 year that starts on 2023-01-01. The original screen net of -8.46% used the binanceus fee model. The corrected screen on is+oos reported +3.92% gross, -4.33% net and 8.24 percentage points of fee drag. This study does not reuse those samples or that fee model as its own measurements.

Proxy funding prints start on 2023-05-12. A 500-bar warm-up on 4h reaches back about 84 days, so judge_2023 and continuous start on 2023-08-07 and the warm-up stays inside the funding file.

## Training and the shortlist

### audit long

| Arm | Family | Train score | Positions | Clusters | Eligible | Status |
| --- | --- | --- | --- | --- | --- | --- |
| bar | bar | -0.4447 | 141 | 101 | True | unjudged |
| control | baseline | -0.4030 | 603 | 153 | True | shortlist |
| gate_adx_range | gate | -0.0425 | 144 | 95 | True | unjudged |
| gate_adx_up | gate | 0.1205 | 199 | 110 | True | unjudged |
| gate_comp_mr | gate | 0.2657 | 48 | 44 | True | pick |
| gate_comp_up | gate | 0.1288 | 195 | 109 | True | unjudged |
| gate_comp_up_clean | gate | 0.1212 | 193 | 108 | True | unjudged |
| htf | htf | 0.1807 | 103 | 79 | True | pick |
| z_1_5 | threshold | -0.5515 | 768 | 158 | True | unjudged |
| z_2_5 | threshold | -0.3163 | 412 | 148 | True | unjudged |
| z_3_0 | threshold | -0.1125 | 274 | 132 | True | pick |
| combo | combo | 0.0000 | 0 | 0 | False | shortlist |

Suggestion arm: `combo`.

### perps both

| Arm | Family | Train score | Positions | Clusters | Eligible | Status |
| --- | --- | --- | --- | --- | --- | --- |
| bar | bar | -0.6667 | 913 | 158 | True | unjudged |
| control | baseline | -0.8223 | 2783 | 159 | True | shortlist |
| gate_adx_range | gate | -0.7400 | 882 | 157 | True | unjudged |
| gate_adx_up | gate | -0.6470 | 1137 | 159 | True | unjudged |
| gate_comp_mr | gate | -0.4353 | 479 | 142 | True | unjudged |
| gate_comp_up | gate | -0.7058 | 1131 | 159 | True | unjudged |
| gate_comp_up_clean | gate | -0.3252 | 441 | 146 | True | pick |
| htf | htf | -0.7773 | 1961 | 159 | True | pick |
| z_1_5 | threshold | -0.7905 | 3244 | 159 | True | unjudged |
| z_2_5 | threshold | -0.6222 | 2118 | 159 | True | unjudged |
| z_3_0 | threshold | -0.5767 | 1510 | 159 | True | pick |
| combo | combo | -0.1972 | 295 | 111 | True | shortlist |

Suggestion arm: `combo`.

### perps long

| Arm | Family | Train score | Positions | Clusters | Eligible | Status |
| --- | --- | --- | --- | --- | --- | --- |
| bar | bar | -0.4147 | 637 | 158 | True | unjudged |
| control | baseline | -0.6172 | 1948 | 159 | True | shortlist |
| gate_adx_range | gate | -0.7925 | 645 | 153 | True | unjudged |
| gate_adx_up | gate | -0.5128 | 652 | 158 | True | unjudged |
| gate_comp_mr | gate | -0.3940 | 287 | 126 | True | unjudged |
| gate_comp_up | gate | -0.6490 | 654 | 157 | True | unjudged |
| gate_comp_up_clean | gate | -0.3252 | 441 | 146 | True | pick |
| htf | htf | -0.5973 | 1081 | 144 | True | pick |
| z_1_5 | threshold | -0.5323 | 2334 | 159 | True | unjudged |
| z_2_5 | threshold | -0.2685 | 1377 | 159 | True | unjudged |
| z_3_0 | threshold | -0.2668 | 918 | 159 | True | pick |
| combo | combo | -0.1972 | 295 | 111 | True | shortlist |

Suggestion arm: `combo`.

### perps short

| Arm | Family | Train score | Positions | Clusters | Eligible | Status |
| --- | --- | --- | --- | --- | --- | --- |
| bar | bar | -0.8097 | 666 | 158 | True | unjudged |
| control | baseline | -0.8845 | 2006 | 159 | True | shortlist |
| gate_adx_range | gate | -0.5257 | 596 | 149 | True | unjudged |
| gate_adx_up | gate | -0.5502 | 717 | 158 | True | unjudged |
| gate_comp_mr | gate | -0.5140 | 273 | 118 | True | pick |
| gate_comp_up | gate | -0.5797 | 675 | 158 | True | unjudged |
| gate_comp_up_clean | gate | 0.0000 | 0 | 0 | False | unjudged |
| htf | htf | -0.7938 | 958 | 132 | True | pick |
| z_1_5 | threshold | -0.8915 | 2449 | 159 | True | unjudged |
| z_2_5 | threshold | -0.8765 | 1418 | 159 | True | pick |
| z_3_0 | threshold | -0.9255 | 938 | 159 | True | unjudged |
| combo | combo | -0.5208 | 45 | 36 | True | shortlist |

Suggestion arm: `combo`.

## Judged shortlist

Position means are not annualized. Positions per year use the sum of the judgment-window lengths, 2.149 years. Fee drag is the position fee divided by entry notional. Gross position return is the net return plus that drag, so it is after funding.

### audit long

| Arm | Positions | Closes | Clusters | Pos/year | Net % | Gross % | Fee drag pp | Leg return % | Stress net % | Stress leg % | Worst continuous DD % |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| control | 1194 | 1194 | 332 | 555.5522 | 0.2080 | 0.3985 | 0.0902 | 11.1412 | 0.1077 | 6.1096 | -52.5700 |
| gate_comp_mr | 64 | 64 | 55 | 29.7783 | 0.1506 | 0.3410 | 0.0901 | 0.4037 | 0.0504 | 0.1362 | -10.6800 |
| htf | 212 | 212 | 167 | 98.6408 | 2.7448 | 2.9395 | 0.0916 | 22.0558 | 2.6418 | 21.1321 | -82.1800 |
| z_3_0 | 521 | 521 | 277 | 242.4143 | 0.5058 | 0.6968 | 0.0903 | 11.6354 | 0.4052 | 9.2808 | -54.5800 |
| combo | 0 | 0 | 0 | 0.0000 | - | - | - | 0.0000 | - | 0.0000 | 0.0000 |

### perps both

| Arm | Positions | Closes | Clusters | Pos/year | Net % | Gross % | Fee drag pp | Leg return % | Stress net % | Stress leg % | Worst continuous DD % |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| control | 5841 | 6932 | 339 | 2717.7392 | -0.1172 | 0.0612 | 0.0900 | -24.7954 | -0.2144 | -39.1337 | -97.0900 |
| gate_comp_up_clean | 853 | 853 | 305 | 396.8895 | -0.0371 | 0.1562 | 0.0900 | -1.3371 | -0.1319 | -4.5263 | -50.0900 |
| htf | 4155 | 4782 | 339 | 1933.2659 | -0.1257 | 0.0617 | 0.0900 | -18.4754 | -0.2268 | -30.6396 | -95.3300 |
| z_3_0 | 3027 | 3728 | 339 | 1408.4226 | -0.1211 | 0.0598 | 0.0900 | -14.2896 | -0.2193 | -23.5679 | -94.4500 |
| combo | 532 | 532 | 227 | 247.5325 | 0.0964 | 0.2950 | 0.0901 | 2.0483 | -0.0133 | -0.3521 | -36.5000 |

Proxy measurement: negative (held-out net return is not positive at base and stress).
That label is not the frame verdict.

### perps long

| Arm | Positions | Closes | Clusters | Pos/year | Net % | Gross % | Fee drag pp | Leg return % | Stress net % | Stress leg % | Worst continuous DD % |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| control | 4181 | 5109 | 339 | 1945.3634 | -0.1092 | 0.0815 | 0.0900 | -17.8313 | -0.2119 | -30.6375 | -91.2000 |
| gate_comp_up_clean | 853 | 853 | 305 | 396.8895 | -0.0371 | 0.1562 | 0.0900 | -1.3371 | -0.1319 | -4.5263 | -50.0900 |
| htf | 2222 | 2584 | 307 | 1033.8669 | -0.1071 | 0.0831 | 0.0900 | -9.6867 | -0.2112 | -17.7000 | -80.4300 |
| z_3_0 | 1900 | 2386 | 338 | 884.0446 | -0.1546 | 0.0390 | 0.0900 | -11.3333 | -0.2554 | -17.7879 | -77.9500 |
| combo | 532 | 532 | 227 | 247.5325 | 0.0964 | 0.2950 | 0.0901 | 2.0483 | -0.0133 | -0.3521 | -36.5000 |

Proxy measurement: negative (held-out net return is not positive at base and stress).
That label is not the frame verdict.

### perps short

| Arm | Positions | Closes | Clusters | Pos/year | Net % | Gross % | Fee drag pp | Leg return % | Stress net % | Stress leg % | Worst continuous DD % |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| control | 4207 | 5204 | 339 | 1957.4608 | -0.1518 | 0.0294 | 0.0900 | -21.9279 | -0.2504 | -33.4975 | -96.6400 |
| gate_comp_mr | 575 | 879 | 273 | 267.5398 | -0.0877 | 0.0926 | 0.0900 | -2.0783 | -0.2078 | -4.8271 | -28.4900 |
| htf | 2120 | 2431 | 289 | 986.4076 | -0.1375 | 0.0476 | 0.0900 | -10.4850 | -0.2354 | -17.6033 | -83.5800 |
| z_2_5 | 2929 | 3634 | 339 | 1362.8245 | -0.1456 | 0.0347 | 0.0900 | -14.9358 | -0.2415 | -23.9108 | -91.3600 |
| combo | 98 | 146 | 82 | 45.5981 | -0.0209 | 0.1685 | 0.0900 | -0.0821 | -0.1521 | -0.6192 | -10.2400 |

Proxy measurement: negative (held-out net return is not positive at base and stress).
That label is not the frame verdict.

## Statistics

Family size 44. Method benjamini_hochberg. Alpha 0.05. Survivors 0.
Tests use base-cost net position returns on the judgment windows. Unjudged searched arms enter with p=1. Stress cost is a robustness check and is not an extra hypothesis.

| Hypothesis | p | BH pass |
| --- | --- | --- |
| audit|long|gate_adx_up | 1.000000 | False |
| audit|long|gate_adx_range | 1.000000 | False |
| audit|long|gate_comp_up_clean | 1.000000 | False |
| audit|long|gate_comp_up | 1.000000 | False |
| audit|long|gate_comp_mr | 0.191881 | False |
| audit|long|htf | 0.082992 | False |
| audit|long|z_1_5 | 1.000000 | False |
| audit|long|z_2_5 | 1.000000 | False |
| audit|long|z_3_0 | 0.017698 | False |
| audit|long|bar | 1.000000 | False |
| audit|long|combo | 1.000000 | False |
| perps|long|gate_adx_up | 1.000000 | False |
| perps|long|gate_adx_range | 1.000000 | False |
| perps|long|gate_comp_up_clean | 0.654335 | False |
| perps|long|gate_comp_up | 1.000000 | False |
| perps|long|gate_comp_mr | 1.000000 | False |
| perps|long|htf | 0.989201 | False |
| perps|long|z_1_5 | 1.000000 | False |
| perps|long|z_2_5 | 1.000000 | False |
| perps|long|z_3_0 | 0.998700 | False |
| perps|long|bar | 1.000000 | False |
| perps|long|combo | 0.209079 | False |
| perps|short|gate_adx_up | 1.000000 | False |
| perps|short|gate_adx_range | 1.000000 | False |
| perps|short|gate_comp_up_clean | 1.000000 | False |
| perps|short|gate_comp_up | 1.000000 | False |
| perps|short|gate_comp_mr | 0.987101 | False |
| perps|short|htf | 0.998900 | False |
| perps|short|z_1_5 | 1.000000 | False |
| perps|short|z_2_5 | 1.000000 | False |
| perps|short|z_3_0 | 1.000000 | False |
| perps|short|bar | 1.000000 | False |
| perps|short|combo | 0.580142 | False |
| perps|both|gate_adx_up | 1.000000 | False |
| perps|both|gate_adx_range | 1.000000 | False |
| perps|both|gate_comp_up_clean | 0.654335 | False |
| perps|both|gate_comp_up | 1.000000 | False |
| perps|both|gate_comp_mr | 1.000000 | False |
| perps|both|htf | 1.000000 | False |
| perps|both|z_1_5 | 1.000000 | False |
| perps|both|z_2_5 | 1.000000 | False |
| perps|both|z_3_0 | 0.999700 | False |
| perps|both|bar | 1.000000 | False |
| perps|both|combo | 0.209079 | False |

## M1 incumbent comparison

Incumbent-relative scores use eval_windows.compute_incumbent_legs and score_candidate at base cost on the judgment windows for long and short. Direction both has no M1 claim because the plain incumbent path does not model both sides. An M1 pass is not the profitability verdict.

A window passes when the arm's mean Sharpe and mean drawdown-adjusted return are both above the incumbent bar, at least half the datasets trade, and funding is complete. This pass is not the profitability verdict.

- audit long control judge_2023: fail
- audit long control judge_2025a: pass
- audit long control judge_is: pass
- audit long control judge_oos: fail
- audit long gate_comp_mr judge_2023: fail
- audit long gate_comp_mr judge_2025a: fail
- audit long gate_comp_mr judge_is: pass
- audit long gate_comp_mr judge_oos: fail
- audit long htf judge_2023: pass
- audit long htf judge_2025a: fail
- audit long htf judge_is: pass
- audit long htf judge_oos: fail
- audit long z_3_0 judge_2023: fail
- audit long z_3_0 judge_2025a: pass
- audit long z_3_0 judge_is: pass
- audit long z_3_0 judge_oos: pass
- audit long combo judge_2023: degenerate
- audit long combo judge_2025a: degenerate
- audit long combo judge_is: degenerate
- audit long combo judge_oos: degenerate
- perps both: no incumbent comparison.
- perps long control judge_2023: fail
- perps long control judge_2025a: fail
- perps long control judge_is: fail
- perps long control judge_oos: fail
- perps long gate_comp_up_clean judge_2023: fail
- perps long gate_comp_up_clean judge_2025a: pass
- perps long gate_comp_up_clean judge_is: fail
- perps long gate_comp_up_clean judge_oos: fail
- perps long htf judge_2023: fail
- perps long htf judge_2025a: fail
- perps long htf judge_is: fail
- perps long htf judge_oos: fail
- perps long z_3_0 judge_2023: fail
- perps long z_3_0 judge_2025a: pass
- perps long z_3_0 judge_is: fail
- perps long z_3_0 judge_oos: fail
- perps long combo judge_2023: fail
- perps long combo judge_2025a: pass
- perps long combo judge_is: pass
- perps long combo judge_oos: fail
- perps short control judge_2023: fail
- perps short control judge_2025a: fail
- perps short control judge_is: fail
- perps short control judge_oos: fail
- perps short gate_comp_mr judge_2023: pass
- perps short gate_comp_mr judge_2025a: fail
- perps short gate_comp_mr judge_is: fail
- perps short gate_comp_mr judge_oos: fail
- perps short htf judge_2023: fail
- perps short htf judge_2025a: pass
- perps short htf judge_is: fail
- perps short htf judge_oos: fail
- perps short z_2_5 judge_2023: fail
- perps short z_2_5 judge_2025a: pass
- perps short z_2_5 judge_is: fail
- perps short z_2_5 judge_oos: fail
- perps short combo judge_2023: pass
- perps short combo judge_2025a: pass
- perps short combo judge_is: fail
- perps short combo judge_oos: fail

## Costs and close

Base cost is the Hyperliquid taker fee 0.045% and the default slippage 0.05%. Stress cost doubles slippage and keeps the taker fee. Gross cost sets commission and slippage to zero. All three still request funding_mode charge. Stress is a robustness check of the same hypothesis, so it does not add a hypothesis to the family.

Proxy funding uses the pinned tema_cross_2016 manifest. Hours that manifest marks venue-absent are charged at zero. They are not missing data.

The ladders are the trailing_tp_ratchet default groups and the trail multiples are the regime trailing defaults, copied from commit ab5214ca so a later default edit cannot move this study. The perps close is trailing_tp_ratchet_regime. Every close label has an explicit ladder and an explicit trail.

The close reads the atr_composite labels. A gate arm changes only regime_gate_window. Composite thresholds are the strategy defaults, including adx 20 rather than the classifier default 25. The strategy's own ADX period is capped at 14; this window period is 20. They are different classifiers.

## Reproduce

```
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py verify-inputs
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py lock
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py coverage
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py select --cache /tmp/ra2017.db --work /tmp/ra2017
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py judge --cache /tmp/ra2017.db --work /tmp/ra2017
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py render
```

No command sends an order. This study does not change a live config, a paper config, a registry default, the quarantine roster, or edge_status.

No frame and direction is positive, so this study writes no suggested config.

