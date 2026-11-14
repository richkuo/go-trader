# chart_pattern_2015 report

Offline, suggest-only study for issue 2015. Every number below regenerates from `run_study.py` on the hash-checked inputs in `study_manifest.json`; this file is written by `run_study.py render` and checked by `run_study.py render --check`.

Question: Does htf_gate_factor=4 improve net results against htf_gate_factor=0 for chart_pattern, on the historical audit reference and on the requested Hyperliquid perps shape, per direction?

## Verdict

| contract | group | factor | outcome | mean daily paired diff (pct) | p | BH q | positions | blocks | usable members | venue supports | reasons |
|---|---|---|---|---|---|---|---|---|---|---|---|
| ratchet_both | proxy_1h | f4 (f4) | negative | +0.0551 | 0.0077 | 0.0440 | 1318 | 121 | 5/5 | no | gated net not positive (net PnL -1418.37 USD, mean daily -0.000452); venue evidence does not support the gate |
| ratchet_both | proxy_30m | f4 (f4) | negative | +0.0807 | 0.0002 | 0.0020 | 2691 | 121 | 5/5 | no | gated net not positive (net PnL -3802.48 USD, mean daily -0.001283); venue evidence does not support the gate |
| ratchet_long | proxy_1h | f4 (f4) | negative | +0.0166 | 0.1406 | 0.4017 | 700 | 121 | 5/5 | no | paired improvement not significant after correction (p 0.1406, q 0.4017, mean daily +0.000166); gated net not positive (net PnL -741.71 USD, mean daily -0.000245); venue evidence does not support the gate |
| ratchet_long | proxy_1h | f2 (selected) | negative | +0.0194 | 0.0842 | 0.2806 | 792 | 121 | 5/5 | no | paired improvement not significant after correction (p 0.0842, q 0.2806, mean daily +0.000194); gated net not positive (net PnL -615.78 USD, mean daily -0.000217); venue evidence does not support the gate |
| ratchet_long | proxy_30m | f4 (f4) | negative | +0.0664 | 0.0001 | 0.0020 | 1408 | 121 | 5/5 | no | gated net not positive (net PnL -2493.81 USD, mean daily -0.000785); venue evidence does not support the gate |
| ratchet_short | proxy_1h | f4 (f4) | negative | +0.0365 | 0.0088 | 0.0440 | 618 | 121 | 5/5 | no | gated net not positive (net PnL -752.46 USD, mean daily -0.000208); venue evidence does not support the gate |
| ratchet_short | proxy_30m | f4 (f4) | negative | +0.0151 | 0.1786 | 0.4465 | 1285 | 121 | 5/5 | no | paired improvement not significant after correction (p 0.1786, q 0.4465, mean daily +0.000151); gated net not positive (net PnL -1760.47 USD, mean daily -0.000494); venue evidence does not support the gate |
| ratchet_short | proxy_30m | f2 (selected) | negative | +0.0237 | 0.0487 | 0.1948 | 1419 | 121 | 5/5 | no | paired improvement not significant after correction (p 0.0487, q 0.1948, mean daily +0.000237); gated net not positive (net PnL -1444.83 USD, mean daily -0.000409); venue evidence does not support the gate |
| ref_long | proxy_1h | f4 (f4) | negative | -0.0427 | 0.8449 | 1.0000 | 167 | 121 | 5/5 | yes | paired improvement not significant after correction (p 0.8449, q 1.0000, mean daily -0.000427); gated net not positive (net PnL -1077.36 USD, mean daily -0.000119) |
| ref_long | proxy_30m | f4 (f4) | negative | +0.0201 | 0.3229 | 0.6457 | 316 | 121 | 5/5 | no | paired improvement not significant after correction (p 0.3229, q 0.6457, mean daily +0.000201); gated net not positive (net PnL -147.23 USD, mean daily +0.000080); venue evidence does not support the gate |
| ref_short | proxy_1h | f4 (f4) | negative | -0.0438 | 0.8378 | 1.0000 | 165 | 121 | 5/5 | no | paired improvement not significant after correction (p 0.8378, q 1.0000, mean daily -0.000438); gated net not positive (net PnL -3136.67 USD, mean daily -0.000970); venue evidence does not support the gate |
| ref_short | proxy_30m | f4 (f4) | negative | +0.0244 | 0.2892 | 0.6426 | 316 | 121 | 5/5 | no | paired improvement not significant after correction (p 0.2892, q 0.6426, mean daily +0.000244); gated net not positive (net PnL -2731.84 USD, mean daily -0.000713); venue evidence does not support the gate |

Family size 20 (Benjamini-Hochberg, alpha 0.05). Finite-frame replay passed: yes. Full-frame causality passed: no.

Deployment verdict: inconclusive. No sanitized chart_pattern deployment snapshot is available, so every deployment verdict is inconclusive; research results describe the research configuration only.

Audit reference: inconclusive. historical reference only: the full-frame signal path fails the causality check, so its numbers cannot support an opt-in

Suggested parameter lines: none (no opt-in result).

## Audit reference (spot registry, long, open-as-close, full frame)

| factor | window | 982 | 1282 | current | current mean Sharpe | current bar Sharpe | diag: funding off | diag: 982 costs |
|---|---|---|---|---|---|---|---|---|
| f0 | is | FAIL | not documented | fail | -0.52 | -0.20 | fail | fail |
| f0 | oos | PASS | not documented | fail | -0.30 | -0.14 | fail | fail |
| f0 | 2023 | FAIL | not documented | funding_incomplete | 1.26 | 1.38 | fail | fail |
| f0 | 2024 | PASS | not documented | pass | 0.96 | 0.64 | pass | pass |
| f0 | 2025H1 | FAIL | not documented | fail | -0.55 | -0.40 | fail | fail |
| f4 | is | PASS | PASS | pass | -0.02 | -0.20 | pass | pass |
| f4 | oos | PASS | PASS | pass | 0.39 | -0.14 | pass | pass |
| f4 | 2023 | PASS | PASS | funding_incomplete | 1.47 | 1.38 | pass | pass |
| f4 | 2024 | FAIL | FAIL | fail | 0.28 | 0.64 | fail | fail |
| f4 | 2025H1 | PASS | PASS | pass | 0.50 | -0.40 | pass | pass |
| f5 | is | PASS | not documented | pass | 0.07 | -0.20 | pass | pass |
| f5 | oos | FAIL | FAIL | pass | 0.13 | -0.14 | pass | pass |
| f5 | 2023 | PASS | not documented | funding_incomplete | 1.49 | 1.38 | pass | pass |
| f5 | 2024 | FAIL | not documented | fail | 0.48 | 0.64 | fail | fail |
| f5 | 2025H1 | PASS | not documented | pass | 0.20 | -0.40 | pass | pass |
| f6 | is | PASS | not documented | pass | 0.31 | -0.20 | pass | pass |
| f6 | oos | FAIL | FAIL | pass | -0.09 | -0.14 | pass | pass |
| f6 | 2023 | PASS | not documented | funding_incomplete | 1.60 | 1.38 | pass | pass |
| f6 | 2024 | FAIL | not documented | fail | 0.36 | 0.64 | fail | fail |
| f6 | 2025H1 | PASS | not documented | pass | -0.10 | -0.40 | pass | pass |

Columns: `982` and `1282` are the documented verdicts. `current` is the current-default rerun (Hyperliquid taker fee 0.045 percent, 0.05 percent slippage, perps accounting, charged funding, oos to 2026-11-10). `funding_incomplete` means the window has hours before the first Hyperliquid funding rate (2023-05-12), so the current harness refuses a verdict. The two diagnostic memos are labeled references: `funding off` keeps current fees, `982 costs` uses the issue 982 cost model (0.1 percent commission, 0.05 percent slippage, no funding).

### What changed against issue 982

- f0 oos: 982 PASS (Sharpe -0.61 vs bar -0.75); 982 costs on the 982 dates (oos_982) pass (Sharpe -0.67 vs bar -0.76): the engine change on the same dates; 982 costs on the full oos fail (Sharpe -0.34 vs bar -0.16): adds the data-range change; current fees without funding fail (Sharpe -0.23 vs bar -0.08): adds the fee change; current fail (Sharpe -0.30 vs bar -0.14): adds charged funding.
- f0 2023: 982 FAIL (Sharpe 1.33 vs bar 1.46); 982 costs fail (Sharpe 1.33 vs bar 1.46): the engine and data change at the 982 cost model; current fees without funding fail (Sharpe 1.44 vs bar 1.56): adds the fee change; current funding_incomplete (Sharpe 1.26 vs bar 1.38): adds charged funding.
- f4 2023: 982 PASS (Sharpe 1.67 vs bar 1.46); 982 costs pass (Sharpe 1.67 vs bar 1.46): the engine and data change at the 982 cost model; current fees without funding pass (Sharpe 1.69 vs bar 1.56): adds the fee change; current funding_incomplete (Sharpe 1.47 vs bar 1.38): adds charged funding.
- f5 oos: 982 FAIL (Sharpe -0.87 vs bar -0.75); 982 costs on the 982 dates (oos_982) fail (Sharpe -0.98 vs bar -0.76): the engine change on the same dates; 982 costs on the full oos pass (Sharpe 0.20 vs bar -0.16): adds the data-range change; current fees without funding pass (Sharpe 0.22 vs bar -0.08): adds the fee change; current pass (Sharpe 0.13 vs bar -0.14): adds charged funding.
- f5 2023: 982 PASS (Sharpe 1.70 vs bar 1.46); 982 costs pass (Sharpe 1.69 vs bar 1.46): the engine and data change at the 982 cost model; current fees without funding pass (Sharpe 1.72 vs bar 1.56): adds the fee change; current funding_incomplete (Sharpe 1.49 vs bar 1.38): adds charged funding.
- f6 oos: 982 FAIL (Sharpe -1.02 vs bar -0.75); 982 costs on the 982 dates (oos_982) fail (Sharpe -1.14 vs bar -0.76): the engine change on the same dates; 982 costs on the full oos pass (Sharpe -0.02 vs bar -0.16): adds the data-range change; current fees without funding pass (Sharpe 0.00 vs bar -0.08): adds the fee change; current pass (Sharpe -0.09 vs bar -0.14): adds charged funding.
- f6 2023: 982 PASS (Sharpe 1.81 vs bar 1.46); 982 costs pass (Sharpe 1.80 vs bar 1.46): the engine and data change at the 982 cost model; current fees without funding pass (Sharpe 1.82 vs bar 1.56): adds the fee change; current funding_incomplete (Sharpe 1.60 vs bar 1.38): adds charged funding.

A step that keeps the verdict and moves the Sharpe means attributes that movement to the named change; a step that flips the verdict names the change that flips it. The engine change is the residual between the issue 982 numbers and the 982-cost rerun on the same dates, so it also holds any cache difference between the 2026-07-01 shared cache and the frozen candles.

### Current-default rerun per window

| window | factor | verdict | mean Sharpe | bar Sharpe | mean DDadj | bar DDadj | mean return % | worst DD % | positions | close records |
|---|---|---|---|---|---|---|---|---|---|---|
| is | f0 | fail | -0.52 | -0.20 | -0.15 | -0.19 | -7.34 | -42.52 | 109 | 109 |
| is | f4 | pass | -0.02 | -0.20 | 0.09 | -0.19 | 1.54 | -43.11 | 29 | 29 |
| is | f5 | pass | 0.07 | -0.20 | 0.14 | -0.19 | 4.12 | -44.71 | 25 | 25 |
| is | f6 | pass | 0.31 | -0.20 | 0.34 | -0.19 | 9.92 | -36.52 | 21 | 21 |
| oos | f0 | fail | -0.30 | -0.14 | -0.37 | -0.20 | -13.56 | -48.22 | 177 | 177 |
| oos | f4 | pass | 0.39 | -0.14 | 0.34 | -0.20 | 5.78 | -29.99 | 43 | 43 |
| oos | f5 | pass | 0.13 | -0.14 | 0.11 | -0.20 | 0.49 | -29.92 | 37 | 37 |
| oos | f6 | pass | -0.09 | -0.14 | -0.17 | -0.20 | -5.65 | -32.47 | 37 | 37 |
| 2023 | f0 | funding_incomplete | 1.26 | 1.38 | 4.62 | 3.56 | 169.58 | -40.05 | 250 | 250 |
| 2023 | f4 | funding_incomplete | 1.47 | 1.38 | 4.40 | 3.56 | 170.56 | -45.76 | 58 | 58 |
| 2023 | f5 | funding_incomplete | 1.49 | 1.38 | 6.02 | 3.56 | 202.35 | -44.65 | 52 | 52 |
| 2023 | f6 | funding_incomplete | 1.60 | 1.38 | 5.79 | 3.56 | 196.21 | -53.83 | 46 | 46 |
| 2024 | f0 | pass | 0.96 | 0.64 | 1.33 | 0.59 | 36.94 | -53.82 | 191 | 191 |
| 2024 | f4 | fail | 0.28 | 0.64 | 0.20 | 0.59 | 2.35 | -68.69 | 58 | 58 |
| 2024 | f5 | fail | 0.48 | 0.64 | 0.36 | 0.59 | 10.78 | -69.53 | 50 | 50 |
| 2024 | f6 | fail | 0.36 | 0.64 | 0.19 | 0.59 | 4.77 | -73.03 | 46 | 46 |
| 2025H1 | f0 | fail | -0.55 | -0.40 | -0.32 | -0.35 | -18.05 | -46.97 | 96 | 96 |
| 2025H1 | f4 | pass | 0.50 | -0.40 | 0.33 | -0.35 | 5.53 | -39.58 | 27 | 27 |
| 2025H1 | f5 | pass | 0.20 | -0.40 | 0.14 | -0.35 | -1.18 | -35.76 | 24 | 24 |
| 2025H1 | f6 | pass | -0.10 | -0.40 | -0.10 | -0.35 | -4.73 | -35.83 | 23 | 23 |
| 2025H1t | f0 | pass | -0.12 | -0.39 | -0.05 | -0.30 | -9.35 | -46.97 | 81 | 81 |
| 2025H1t | f4 | pass | 0.94 | -0.39 | 0.97 | -0.30 | 11.97 | -39.58 | 23 | 23 |
| 2025H1t | f5 | pass | 0.61 | -0.39 | 0.53 | -0.30 | 5.31 | -35.76 | 21 | 21 |
| 2025H1t | f6 | pass | 0.25 | -0.39 | 0.15 | -0.30 | 1.50 | -28.92 | 21 | 21 |

Paired factor 4 minus factor 0, daily equity, oos + 2024 + 2025H1t: mean daily difference +0.0154 percent over 121 blocks, one-sided bootstrap p 0.3287. This is a historical reference and enters no correction family.

Incumbent memo instances: `current` (1baf98a25c220335), `current_funding_off` (605d57b67d5dd398), `legacy_cost` (282f5765de6b8a20).

## Replay and causality checks

| check | compared | mismatches | passed |
|---|---|---|---|
| decomposition | 1344 | 0 | yes |
| signal_check_path | 1344 | 0 | yes |
| future_invariance | 448 | 0 | yes |
| htf_visibility | 896 | 0 | yes |
| next_bar_fill | 174 | 0 | yes |
| full_frame_causality | 540 | 1 | no |

Signal-check path: shared_scripts/check_hyperliquid.py build_shared_signal_state(df=frame, ohlcv_limit=frame length, regime_enabled) + evaluate_signal_slot (prebuilt frame, no adapter, no network).

### Full-frame path causality (historical audit path)

| dataset | window | full-frame signals | sampled | reproduced on cut frame | mismatches |
|---|---|---|---|---|---|
| BTC 1h | is | 178 | 30 | 30 | 0 |
| BTC 1h | oos | 296 | 30 | 30 | 0 |
| BTC 1h | 2024 | 383 | 30 | 30 | 0 |
| BTC 4h | is | 37 | 30 | 30 | 0 |
| BTC 4h | oos | 60 | 30 | 30 | 0 |
| BTC 4h | 2024 | 77 | 30 | 29 | 1 |
| ETH 1h | is | 191 | 30 | 30 | 0 |
| ETH 1h | oos | 284 | 30 | 30 | 0 |
| ETH 1h | 2024 | 366 | 30 | 30 | 0 |
| ETH 4h | is | 32 | 30 | 30 | 0 |
| ETH 4h | oos | 56 | 30 | 30 | 0 |
| ETH 4h | 2024 | 88 | 30 | 30 | 0 |
| SOL 1h | is | 169 | 30 | 30 | 0 |
| SOL 1h | oos | 264 | 30 | 30 | 0 |
| SOL 1h | 2024 | 321 | 30 | 30 | 0 |
| SOL 4h | is | 32 | 30 | 30 | 0 |
| SOL 4h | oos | 56 | 30 | 30 | 0 |
| SOL 4h | 2024 | 68 | 30 | 30 | 0 |

a full-frame signal at bar N must equal chart_pattern_core on the same window frame cut at bar N; a mismatch means the historical path read candles after the decision bar or depended on where the window frame ends

### Full frame against the live-shaped finite frame (diagnostic)

| dataset | window | factor | full-frame signals | finite-frame signals | same bar and side | full-frame longs | finite-frame longs |
|---|---|---|---|---|---|---|---|
| BTC 1h | is | f0 | 178 | 192 | 155 | 84 | 99 |
| BTC 1h | is | f4 | 104 | 108 | 79 | 46 | 48 |
| BTC 1h | oos | f0 | 296 | 300 | 262 | 152 | 157 |
| BTC 1h | oos | f4 | 183 | 164 | 142 | 85 | 77 |
| BTC 1h | 2024 | f0 | 383 | 394 | 322 | 203 | 211 |
| BTC 1h | 2024 | f4 | 247 | 219 | 166 | 142 | 126 |
| ETH 1h | is | f0 | 191 | 183 | 156 | 94 | 89 |
| ETH 1h | is | f4 | 115 | 90 | 75 | 54 | 45 |
| ETH 1h | oos | f0 | 284 | 304 | 257 | 145 | 159 |
| ETH 1h | oos | f4 | 168 | 166 | 124 | 84 | 85 |
| ETH 1h | 2024 | f0 | 366 | 366 | 307 | 195 | 190 |
| ETH 1h | 2024 | f4 | 208 | 183 | 135 | 116 | 93 |
| SOL 1h | is | f0 | 169 | 170 | 138 | 97 | 99 |
| SOL 1h | is | f4 | 98 | 98 | 65 | 51 | 48 |
| SOL 1h | oos | f0 | 264 | 280 | 233 | 131 | 145 |
| SOL 1h | oos | f4 | 166 | 155 | 123 | 74 | 70 |
| SOL 1h | 2024 | f0 | 321 | 320 | 270 | 171 | 164 |
| SOL 1h | 2024 | f4 | 193 | 173 | 141 | 103 | 89 |

Network during checks: {"name_resolution": "refused: frozen study run: network access is blocked", "socket_connect": "refused: frozen study run: network access is blocked", "unfrozen_symbol_load": "refused: frozen study run: network access is blocked"}; refused attempts 3 (the probe).

## Coverage and exclusions

| section | group | dataset | window | status | reason |
|---|---|---|---|---|---|
| perps | bridge_1h | BNB 1h | tail | included | 9 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_1h | BTC 1h | tail | included | 9 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_1h | ETH 1h | tail | included | 9 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_1h | HYPE 1h | tail | included | 9 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_1h | SOL 1h | tail | included | 9 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_30m | BNB 30m | tail | included | 18 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_30m | BTC 30m | tail | included | 18 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_30m | ETH 30m | tail | included | 18 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_30m | HYPE 30m | tail | included | 18 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | bridge_30m | SOL 30m | tail | included | 18 missing proxy candles in the window are scored as they are (bridge rule) |
| perps | proxy_1h | BNB 1h | is | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BNB 1h | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BNB 1h | 2023 | excluded | 3144 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2023-05-12 00:00:00.048000) |
| perps | proxy_1h | BNB 1h | 2024 | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BNB 1h | 2025H1t | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BNB 1h | stitched | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BTC 1h | is | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BTC 1h | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BTC 1h | 2023 | excluded | 3144 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2023-05-12 00:00:00.048000) |
| perps | proxy_1h | BTC 1h | 2024 | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BTC 1h | 2025H1t | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | BTC 1h | stitched | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | ETH 1h | is | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | ETH 1h | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | ETH 1h | 2023 | excluded | 3144 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2023-05-12 00:00:00.048000) |
| perps | proxy_1h | ETH 1h | 2024 | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | ETH 1h | 2025H1t | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | ETH 1h | stitched | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | HYPE 1h | is | excluded | warm-up lacks 400 consecutive bars (315 missing, first 2025-05-24 08:00:00) |
| perps | proxy_1h | HYPE 1h | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | HYPE 1h | 2023 | excluded | warm-up lacks 400 consecutive bars (400 missing, first 2022-12-15 08:00:00); no candles in the window; 8760 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2024-12-05 10:00:00.004000) |
| perps | proxy_1h | HYPE 1h | 2024 | excluded | warm-up lacks 400 consecutive bars (400 missing, first 2023-12-15 08:00:00); no candles in the window; 8146 unpriced funding hours (first 2024-01-01 00:00:00; venue funding starts 2024-12-05 10:00:00.004000) |
| perps | proxy_1h | HYPE 1h | 2025H1t | excluded | warm-up lacks 400 consecutive bars (400 missing, first 2024-12-15 08:00:00) |
| perps | proxy_1h | HYPE 1h | stitched | excluded | warm-up lacks 400 consecutive bars (315 missing, first 2025-05-24 08:00:00) |
| perps | proxy_1h | SOL 1h | is | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | SOL 1h | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | SOL 1h | 2023 | excluded | 3144 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2023-05-12 00:00:00.048000) |
| perps | proxy_1h | SOL 1h | 2024 | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | SOL 1h | 2025H1t | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_1h | SOL 1h | stitched | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BNB 30m | is | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BNB 30m | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BNB 30m | 2023 | excluded | 3144 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2023-05-12 00:00:00.048000) |
| perps | proxy_30m | BNB 30m | 2024 | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BNB 30m | 2025H1t | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BNB 30m | stitched | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BTC 30m | is | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BTC 30m | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BTC 30m | 2023 | excluded | 3144 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2023-05-12 00:00:00.048000) |
| perps | proxy_30m | BTC 30m | 2024 | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BTC 30m | 2025H1t | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | BTC 30m | stitched | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | ETH 30m | is | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | ETH 30m | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | ETH 30m | 2023 | excluded | 3144 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2023-05-12 00:00:00.048000) |
| perps | proxy_30m | ETH 30m | 2024 | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | ETH 30m | 2025H1t | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | ETH 30m | stitched | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | HYPE 30m | is | excluded | warm-up lacks 400 consecutive bars (230 missing, first 2025-06-01 16:00:00) |
| perps | proxy_30m | HYPE 30m | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | HYPE 30m | 2023 | excluded | warm-up lacks 400 consecutive bars (400 missing, first 2022-12-23 16:00:00); no candles in the window; 8760 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2024-12-05 10:00:00.004000) |
| perps | proxy_30m | HYPE 30m | 2024 | excluded | warm-up lacks 400 consecutive bars (400 missing, first 2023-12-23 16:00:00); no candles in the window; 8146 unpriced funding hours (first 2024-01-01 00:00:00; venue funding starts 2024-12-05 10:00:00.004000) |
| perps | proxy_30m | HYPE 30m | 2025H1t | excluded | warm-up lacks 400 consecutive bars (400 missing, first 2024-12-23 16:00:00) |
| perps | proxy_30m | HYPE 30m | stitched | excluded | warm-up lacks 400 consecutive bars (230 missing, first 2025-06-01 16:00:00) |
| perps | proxy_30m | SOL 30m | is | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | SOL 30m | oos | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | SOL 30m | 2023 | excluded | 3144 unpriced funding hours (first 2023-01-01 00:00:00; venue funding starts 2023-05-12 00:00:00.048000) |
| perps | proxy_30m | SOL 30m | 2024 | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | SOL 30m | 2025H1t | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | proxy_30m | SOL 30m | stitched | included | complete warm-up prefix, candles present, every scored hour priced or proven venue-absent |
| perps | venue_1h | BNB 1h | tail | included | complete candles, warm-up and funding |
| perps | venue_1h | BTC 1h | tail | included | complete candles, warm-up and funding |
| perps | venue_1h | ETH 1h | tail | included | complete candles, warm-up and funding |
| perps | venue_1h | HYPE 1h | tail | included | complete candles, warm-up and funding |
| perps | venue_1h | SOL 1h | tail | included | complete candles, warm-up and funding |
| perps | venue_30m | BNB 30m | tail | included | complete candles, warm-up and funding |
| perps | venue_30m | BTC 30m | tail | included | complete candles, warm-up and funding |
| perps | venue_30m | ETH 30m | tail | included | complete candles, warm-up and funding |
| perps | venue_30m | HYPE 30m | tail | included | complete candles, warm-up and funding |
| perps | venue_30m | SOL 30m | tail | included | complete candles, warm-up and funding |

Audit rows are all included (the audit path loads no warm-up); their funding-incomplete windows are reported in the audit table.

## In-sample selection (is only)

| contract | group | factor | mean return % | mean DDadj | mean Sharpe | positions | beats f0 on both | pick |
|---|---|---|---|---|---|---|---|---|
| ratchet_both | proxy_1h | f0 | -16.29 | -0.51 | -1.07 | 488 | - |  |
| ratchet_both | proxy_1h | f2 | -9.77 | -0.37 | -0.75 | 326 | yes |  |
| ratchet_both | proxy_1h | f3 | -12.11 | -0.61 | -0.83 | 297 | no |  |
| ratchet_both | proxy_1h | f4 | -13.74 | -0.55 | -1.02 | 289 | no |  |
| ratchet_both | proxy_1h | f5 | -16.29 | -0.51 | -1.07 | 488 | no |  |
| ratchet_both | proxy_1h | f6 | -16.29 | -0.51 | -1.07 | 488 | no |  |
| ratchet_both | proxy_30m | f0 | -22.88 | -0.69 | -1.28 | 1018 | - |  |
| ratchet_both | proxy_30m | f2 | -21.33 | -0.66 | -1.39 | 651 | yes |  |
| ratchet_both | proxy_30m | f3 | -20.84 | -0.72 | -1.41 | 595 | no |  |
| ratchet_both | proxy_30m | f4 | -20.73 | -0.72 | -1.42 | 585 | no |  |
| ratchet_both | proxy_30m | f5 | -22.88 | -0.69 | -1.28 | 1018 | no |  |
| ratchet_both | proxy_30m | f6 | -22.88 | -0.69 | -1.28 | 1018 | no |  |
| ratchet_long | proxy_1h | f0 | -10.24 | -0.41 | -0.75 | 265 | - |  |
| ratchet_long | proxy_1h | f2 | -2.40 | -0.01 | -0.30 | 166 | yes | selected |
| ratchet_long | proxy_1h | f3 | -4.04 | -0.35 | -0.47 | 148 | yes | shortlist |
| ratchet_long | proxy_1h | f4 | -4.82 | -0.15 | -0.74 | 145 | yes |  |
| ratchet_long | proxy_1h | f5 | -10.24 | -0.41 | -0.75 | 265 | no |  |
| ratchet_long | proxy_1h | f6 | -10.24 | -0.41 | -0.75 | 265 | no |  |
| ratchet_long | proxy_30m | f0 | -18.07 | -0.76 | -1.45 | 564 | - |  |
| ratchet_long | proxy_30m | f2 | -18.89 | -0.79 | -1.96 | 368 | no |  |
| ratchet_long | proxy_30m | f3 | -17.95 | -0.83 | -1.90 | 339 | no |  |
| ratchet_long | proxy_30m | f4 | -18.17 | -0.83 | -1.93 | 335 | no |  |
| ratchet_long | proxy_30m | f5 | -18.07 | -0.76 | -1.45 | 564 | no |  |
| ratchet_long | proxy_30m | f6 | -18.07 | -0.76 | -1.45 | 564 | no |  |
| ratchet_short | proxy_1h | f0 | -9.62 | -0.39 | -0.92 | 235 | - |  |
| ratchet_short | proxy_1h | f2 | -8.47 | -0.50 | -0.74 | 160 | no |  |
| ratchet_short | proxy_1h | f3 | -8.55 | -0.62 | -0.72 | 149 | no |  |
| ratchet_short | proxy_1h | f4 | -9.61 | -0.65 | -0.80 | 144 | no |  |
| ratchet_short | proxy_1h | f5 | -9.62 | -0.39 | -0.92 | 235 | no |  |
| ratchet_short | proxy_1h | f6 | -9.62 | -0.39 | -0.92 | 235 | no |  |
| ratchet_short | proxy_30m | f0 | -9.89 | -0.42 | -0.66 | 484 | - |  |
| ratchet_short | proxy_30m | f2 | -3.17 | -0.19 | -0.20 | 283 | yes | selected |
| ratchet_short | proxy_30m | f3 | -3.31 | -0.20 | -0.29 | 256 | yes | shortlist |
| ratchet_short | proxy_30m | f4 | -2.90 | -0.12 | -0.26 | 251 | yes |  |
| ratchet_short | proxy_30m | f5 | -9.89 | -0.42 | -0.66 | 484 | no |  |
| ratchet_short | proxy_30m | f6 | -9.89 | -0.42 | -0.66 | 484 | no |  |
| ref_long | proxy_1h | f0 | 2.53 | 0.24 | 0.06 | 123 | - |  |
| ref_long | proxy_1h | f2 | -2.46 | -0.16 | -0.12 | 52 | no |  |
| ref_long | proxy_1h | f3 | -4.12 | -0.16 | -0.23 | 43 | no |  |
| ref_long | proxy_1h | f4 | 8.31 | 0.27 | 0.17 | 36 | yes |  |
| ref_long | proxy_1h | f5 | 2.53 | 0.24 | 0.06 | 123 | no |  |
| ref_long | proxy_1h | f6 | 2.53 | 0.24 | 0.06 | 123 | no |  |
| ref_long | proxy_30m | f0 | 8.54 | 0.35 | 0.20 | 264 | - |  |
| ref_long | proxy_30m | f2 | -3.76 | 0.13 | -0.29 | 104 | no |  |
| ref_long | proxy_30m | f3 | -1.39 | 0.06 | -0.01 | 81 | no |  |
| ref_long | proxy_30m | f4 | -3.31 | -0.12 | -0.03 | 73 | no |  |
| ref_long | proxy_30m | f5 | 8.54 | 0.35 | 0.20 | 264 | no |  |
| ref_long | proxy_30m | f6 | 8.54 | 0.35 | 0.20 | 264 | no |  |
| ref_short | proxy_1h | f0 | 2.63 | 0.12 | 0.16 | 122 | - |  |
| ref_short | proxy_1h | f2 | -1.79 | 0.03 | -0.01 | 49 | no |  |
| ref_short | proxy_1h | f3 | -4.74 | -0.13 | 0.01 | 40 | no |  |
| ref_short | proxy_1h | f4 | 7.08 | 0.32 | 0.51 | 33 | yes |  |
| ref_short | proxy_1h | f5 | 2.63 | 0.12 | 0.16 | 122 | no |  |
| ref_short | proxy_1h | f6 | 2.63 | 0.12 | 0.16 | 122 | no |  |
| ref_short | proxy_30m | f0 | 8.61 | 0.45 | 0.34 | 262 | - |  |
| ref_short | proxy_30m | f2 | -4.29 | 0.07 | -0.03 | 100 | no |  |
| ref_short | proxy_30m | f3 | -1.44 | 0.10 | 0.19 | 77 | no |  |
| ref_short | proxy_30m | f4 | -1.81 | 0.23 | 0.18 | 69 | no |  |
| ref_short | proxy_30m | f5 | 8.61 | 0.45 | 0.34 | 262 | no |  |
| ref_short | proxy_30m | f6 | 8.61 | 0.45 | 0.34 | 262 | no |  |

Rule: Per perps contract and proxy group, on is only: factor 4 is mandatory and is never replaced. An additional factor F in {2, 3, 5, 6} is shortlisted only if its mean net return and mean drawdown-adjusted return across the included datasets both exceed factor 0, it opens at least 30 positions, and an adjacent tested factor (the neighbour of F in 2, 3, 4, 5, 6) also exceeds factor 0 on both measures. At most one additional factor is selected per contract and group: the shortlisted factor with the highest mean drawdown-adjusted return, ties to the lower factor. select writes selection.json once, before any judged window runs, and refuses to replace it.

## Paired results per contract and group

### ratchet_both / proxy_1h / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | FAIL | 4 | -13.74 | -16.29 | -1.02 | -1.07 | -26.44 | -33.90 | 289 | 488 | 361 | 605 | -549.46 | -652.14 |
| oos | PASS | 5 | 1.99 | -21.64 | -0.02 | -0.89 | -32.87 | -44.92 | 545 | 1006 | 676 | 1279 | 99.42 | -1082.27 |
| 2024 | FAIL | 4 | -24.42 | -32.04 | -1.09 | -0.94 | -44.04 | -45.37 | 559 | 980 | 680 | 1236 | -976.77 | -1281.61 |
| 2025H1t | PASS | 4 | -13.51 | -26.64 | -1.43 | -2.05 | -26.91 | -35.00 | 214 | 389 | 265 | 494 | -541.02 | -1067.45 |
| stitched | PASS | 4 | -20.02 | -38.67 | -0.61 | -1.10 | -33.83 | -56.58 | 737 | 1295 | 909 | 1620 | -800.55 | -1546.76 |
| bridge_1h | PASS | 5 | 6.21 | -15.33 | 0.17 | -1.29 | -22.55 | -40.46 | 346 | 635 | 431 | 807 | 310.66 | -766.69 |
| venue_1h | PASS | 5 | -10.53 | -24.59 | -1.03 | -1.68 | -29.80 | -45.59 | 367 | 643 | 469 | 829 | -526.45 | -1229.45 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0551 percent, 121 blocks, p 0.0077, BH q 0.0440; gated net PnL -1418.37 USD over 1318 positions. Outcome: **negative**.

### ratchet_both / proxy_30m / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | FAIL | 4 | -20.73 | -22.88 | -1.42 | -1.28 | -38.50 | -39.33 | 585 | 1018 | 718 | 1278 | -829.90 | -916.82 |
| oos | PASS | 5 | -32.24 | -46.52 | -1.83 | -2.17 | -56.01 | -82.67 | 1126 | 2078 | 1366 | 2582 | -1612.13 | -2326.60 |
| 2024 | PASS | 4 | -36.55 | -54.06 | -1.53 | -1.90 | -67.07 | -71.16 | 1133 | 1942 | 1402 | 2466 | -1462.06 | -2162.38 |
| 2025H1t | PASS | 4 | -18.21 | -29.15 | -1.59 | -2.00 | -29.44 | -40.76 | 432 | 768 | 516 | 941 | -728.29 | -1166.83 |
| stitched | FAIL | 4 | -44.57 | -51.69 | -1.59 | -1.54 | -56.62 | -63.91 | 1481 | 2655 | 1799 | 3309 | -1782.77 | -2068.01 |
| bridge_30m | PASS | 5 | -6.44 | -17.17 | -1.29 | -2.02 | -19.78 | -38.50 | 338 | 641 | 403 | 793 | -322.07 | -859.55 |
| venue_30m | PASS | 5 | -7.53 | -14.56 | -1.21 | -1.57 | -24.89 | -40.52 | 368 | 656 | 455 | 820 | -376.58 | -728.83 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0807 percent, 121 blocks, p 0.0002, BH q 0.0020; gated net PnL -3802.48 USD over 2691 positions. Outcome: **negative**.

### ratchet_long / proxy_1h / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | PASS | 4 | -4.82 | -10.24 | -0.74 | -0.75 | -19.72 | -30.93 | 145 | 265 | 188 | 340 | -192.71 | -409.82 |
| oos | PASS | 5 | 5.24 | -0.65 | 0.08 | -0.26 | -17.24 | -29.86 | 271 | 530 | 344 | 677 | 262.25 | -32.28 |
| 2024 | FAIL | 4 | -16.23 | -16.37 | -0.90 | -0.61 | -35.05 | -36.16 | 316 | 529 | 393 | 683 | -649.09 | -654.78 |
| 2025H1t | PASS | 4 | -8.85 | -17.52 | -1.29 | -1.81 | -18.17 | -27.93 | 113 | 206 | 137 | 263 | -354.87 | -703.51 |
| stitched | PASS | 4 | -8.87 | -18.40 | -0.45 | -0.68 | -28.93 | -47.16 | 365 | 692 | 461 | 879 | -354.88 | -736.10 |
| bridge_1h | PASS | 5 | 11.07 | -2.04 | 0.64 | -0.43 | -9.05 | -21.69 | 166 | 328 | 211 | 410 | 553.57 | -101.76 |
| venue_1h | PASS | 5 | -2.67 | -10.47 | -0.54 | -0.97 | -18.14 | -33.68 | 179 | 347 | 224 | 435 | -133.67 | -523.69 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0166 percent, 121 blocks, p 0.1406, BH q 0.4017; gated net PnL -741.71 USD over 700 positions. Outcome: **negative**.

### ratchet_long / proxy_1h / f2 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | PASS | 4 | -2.40 | -10.24 | -0.30 | -0.75 | -17.52 | -30.93 | 166 | 265 | 217 | 340 | -95.92 | -409.82 |
| oos | PASS | 5 | 5.90 | -0.65 | 0.15 | -0.26 | -14.89 | -29.86 | 295 | 530 | 370 | 677 | 295.01 | -32.28 |
| 2024 | FAIL | 4 | -18.45 | -16.37 | -0.93 | -0.61 | -35.03 | -36.16 | 368 | 529 | 460 | 683 | -738.07 | -654.78 |
| 2025H1t | PASS | 4 | -4.40 | -17.52 | -0.95 | -1.81 | -25.41 | -27.93 | 129 | 206 | 156 | 263 | -172.72 | -703.51 |
| stitched | PASS | 4 | -5.33 | -18.40 | -0.24 | -0.68 | -25.76 | -47.16 | 406 | 692 | 515 | 879 | -213.29 | -736.10 |
| bridge_1h | PASS | 5 | 10.30 | -2.04 | 0.70 | -0.43 | -11.01 | -21.69 | 173 | 328 | 213 | 410 | 515.33 | -101.76 |
| venue_1h | PASS | 5 | -3.80 | -10.47 | -0.41 | -0.97 | -20.74 | -33.68 | 193 | 347 | 231 | 435 | -190.20 | -523.69 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0194 percent, 121 blocks, p 0.0842, BH q 0.2806; gated net PnL -615.78 USD over 792 positions. Outcome: **negative**.

### ratchet_long / proxy_30m / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | FAIL | 4 | -18.17 | -18.07 | -1.93 | -1.45 | -37.58 | -28.59 | 335 | 564 | 417 | 724 | -727.47 | -723.60 |
| oos | PASS | 5 | -21.63 | -42.80 | -1.64 | -2.66 | -43.14 | -63.53 | 578 | 1138 | 707 | 1413 | -1081.61 | -2140.81 |
| 2024 | PASS | 4 | -28.21 | -40.32 | -1.67 | -1.95 | -49.55 | -63.60 | 629 | 1038 | 788 | 1341 | -1128.34 | -1612.91 |
| 2025H1t | PASS | 4 | -7.09 | -12.57 | -0.88 | -1.27 | -18.42 | -29.77 | 201 | 384 | 240 | 469 | -283.86 | -503.68 |
| stitched | PASS | 4 | -35.00 | -49.12 | -1.82 | -2.08 | -49.23 | -61.83 | 797 | 1478 | 982 | 1861 | -1400.05 | -1965.45 |
| bridge_30m | PASS | 5 | -2.90 | -12.89 | -0.81 | -2.23 | -15.17 | -21.72 | 183 | 370 | 208 | 454 | -144.84 | -645.11 |
| venue_30m | PASS | 5 | -0.25 | -4.93 | -0.19 | -0.78 | -20.01 | -16.39 | 190 | 352 | 225 | 435 | -12.63 | -247.21 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0664 percent, 121 blocks, p 0.0001, BH q 0.0020; gated net PnL -2493.81 USD over 1408 positions. Outcome: **negative**.

### ratchet_short / proxy_1h / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | FAIL | 4 | -9.61 | -9.62 | -0.80 | -0.92 | -18.92 | -25.73 | 144 | 235 | 173 | 277 | -384.27 | -385.66 |
| oos | PASS | 5 | -2.65 | -18.62 | -0.20 | -0.99 | -35.25 | -45.96 | 274 | 497 | 332 | 626 | -132.55 | -931.04 |
| 2024 | PASS | 4 | -10.57 | -18.29 | -0.63 | -0.73 | -24.25 | -33.71 | 243 | 464 | 287 | 567 | -422.93 | -731.57 |
| 2025H1t | PASS | 4 | -4.92 | -9.82 | -0.67 | -0.91 | -13.50 | -22.06 | 101 | 190 | 128 | 243 | -196.98 | -393.04 |
| stitched | PASS | 4 | -11.65 | -24.57 | -0.46 | -0.92 | -35.25 | -46.11 | 372 | 636 | 448 | 777 | -465.95 | -982.72 |
| bridge_1h | PASS | 5 | -4.58 | -13.86 | -0.49 | -1.38 | -29.08 | -38.43 | 180 | 320 | 220 | 411 | -229.20 | -692.85 |
| venue_1h | PASS | 5 | -8.16 | -16.18 | -0.95 | -1.48 | -24.14 | -35.40 | 188 | 320 | 245 | 427 | -408.25 | -808.71 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0365 percent, 121 blocks, p 0.0088, BH q 0.0440; gated net PnL -752.46 USD over 618 positions. Outcome: **negative**.

### ratchet_short / proxy_30m / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | PASS | 4 | -2.90 | -9.89 | -0.26 | -0.66 | -18.14 | -23.98 | 251 | 484 | 302 | 593 | -115.86 | -396.13 |
| oos | FAIL | 5 | -14.63 | -10.28 | -1.03 | -0.48 | -33.56 | -54.92 | 549 | 1005 | 661 | 1248 | -731.52 | -513.82 |
| 2024 | PASS | 4 | -13.88 | -23.15 | -0.56 | -0.86 | -35.79 | -47.89 | 504 | 952 | 614 | 1188 | -555.47 | -926.10 |
| 2025H1t | FAIL | 4 | -11.84 | -16.53 | -1.26 | -1.37 | -24.33 | -30.83 | 232 | 397 | 277 | 487 | -473.48 | -661.14 |
| stitched | FAIL | 4 | -13.78 | -9.39 | -0.57 | -0.29 | -36.54 | -38.37 | 685 | 1265 | 818 | 1557 | -551.34 | -375.66 |
| bridge_30m | FAIL | 5 | -3.77 | -5.50 | -1.25 | -0.68 | -10.54 | -23.98 | 155 | 289 | 195 | 362 | -188.53 | -275.14 |
| venue_30m | FAIL | 5 | -7.04 | -9.57 | -1.77 | -1.32 | -20.97 | -39.14 | 178 | 328 | 230 | 419 | -351.81 | -478.19 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0151 percent, 121 blocks, p 0.1786, BH q 0.4465; gated net PnL -1760.47 USD over 1285 positions. Outcome: **negative**.

### ratchet_short / proxy_30m / f2 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | PASS | 4 | -3.17 | -9.89 | -0.20 | -0.66 | -16.45 | -23.98 | 283 | 484 | 340 | 593 | -126.98 | -396.13 |
| oos | FAIL | 5 | -7.58 | -10.28 | -0.47 | -0.48 | -38.84 | -54.92 | 607 | 1005 | 726 | 1248 | -378.76 | -513.82 |
| 2024 | PASS | 4 | -14.39 | -23.15 | -0.56 | -0.86 | -39.68 | -47.89 | 551 | 952 | 653 | 1188 | -575.84 | -926.10 |
| 2025H1t | PASS | 4 | -12.26 | -16.53 | -1.19 | -1.37 | -25.07 | -30.83 | 261 | 397 | 306 | 487 | -490.23 | -661.14 |
| stitched | PASS | 4 | -4.58 | -9.39 | -0.16 | -0.29 | -30.81 | -38.37 | 761 | 1265 | 902 | 1557 | -183.33 | -375.66 |
| bridge_30m | FAIL | 5 | -3.86 | -5.50 | -0.96 | -0.68 | -16.36 | -23.98 | 170 | 289 | 207 | 362 | -192.79 | -275.14 |
| venue_30m | FAIL | 5 | -7.86 | -9.57 | -1.85 | -1.32 | -20.44 | -39.14 | 202 | 328 | 247 | 419 | -392.92 | -478.19 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0237 percent, 121 blocks, p 0.0487, BH q 0.1948; gated net PnL -1444.83 USD over 1419 positions. Outcome: **negative**.

### ref_long / proxy_1h / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | PASS | 4 | 8.31 | 2.53 | 0.17 | 0.06 | -30.01 | -34.51 | 36 | 123 | 36 | 123 | 329.35 | 99.06 |
| oos | FAIL | 5 | -4.38 | -3.24 | -0.11 | -0.28 | -37.72 | -47.36 | 65 | 270 | 65 | 270 | -218.89 | -163.97 |
| 2024 | FAIL | 4 | 0.84 | 34.17 | 0.27 | 0.92 | -47.51 | -35.89 | 69 | 234 | 69 | 234 | 30.56 | 1361.69 |
| 2025H1t | FAIL | 4 | -22.20 | -14.78 | -0.99 | -0.57 | -51.52 | -49.61 | 33 | 110 | 33 | 110 | -889.03 | -593.53 |
| stitched | PASS | 4 | -4.67 | -18.82 | -0.00 | -0.30 | -47.94 | -59.87 | 86 | 339 | 86 | 339 | -186.60 | -752.56 |
| bridge_1h | PASS | 5 | 3.91 | -4.88 | 0.15 | -0.44 | -29.56 | -40.75 | 43 | 173 | 43 | 173 | 195.55 | -245.00 |
| venue_1h | PASS | 5 | 6.62 | 1.95 | 0.26 | 0.02 | -29.01 | -37.11 | 41 | 178 | 41 | 178 | 331.20 | 97.37 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference -0.0427 percent, 121 blocks, p 0.8449, BH q 1.0000; gated net PnL -1077.36 USD over 167 positions. Outcome: **negative**.

### ref_long / proxy_30m / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | FAIL | 4 | -3.31 | 8.54 | -0.03 | 0.20 | -40.39 | -36.03 | 73 | 264 | 73 | 264 | -136.26 | 339.19 |
| oos | FAIL | 5 | -7.95 | -9.73 | -0.55 | -0.23 | -54.19 | -43.45 | 136 | 541 | 136 | 541 | -397.71 | -490.13 |
| 2024 | PASS | 4 | 25.86 | 11.75 | 0.77 | 0.47 | -44.75 | -40.46 | 119 | 503 | 119 | 503 | 1032.12 | 465.73 |
| 2025H1t | PASS | 4 | -19.50 | -28.09 | -0.96 | -1.36 | -46.67 | -62.00 | 61 | 207 | 61 | 207 | -781.64 | -1126.40 |
| stitched | FAIL | 4 | -27.99 | -8.36 | -0.52 | -0.14 | -63.01 | -55.90 | 181 | 692 | 181 | 692 | -1119.75 | -336.88 |
| bridge_30m | FAIL | 5 | 8.84 | 8.41 | 0.74 | 0.99 | -27.55 | -33.95 | 43 | 167 | 43 | 167 | 442.13 | 416.01 |
| venue_30m | FAIL | 5 | 11.59 | 12.71 | 0.83 | 1.38 | -31.23 | -23.58 | 44 | 170 | 44 | 170 | 579.36 | 632.05 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0201 percent, 121 blocks, p 0.3229, BH q 0.6457; gated net PnL -147.23 USD over 316 positions. Outcome: **negative**.

### ref_short / proxy_1h / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | PASS | 4 | 7.08 | 2.63 | 0.51 | 0.16 | -33.30 | -37.81 | 33 | 122 | 33 | 122 | 282.20 | 103.56 |
| oos | PASS | 5 | -9.25 | -14.99 | -0.03 | -0.26 | -70.83 | -64.18 | 65 | 270 | 65 | 270 | -466.15 | -753.09 |
| 2024 | FAIL | 4 | -44.22 | -24.91 | -1.25 | -0.45 | -64.04 | -60.65 | 68 | 231 | 68 | 231 | -1769.17 | -996.61 |
| 2025H1t | FAIL | 4 | -22.48 | -10.29 | -0.71 | -0.43 | -57.66 | -37.48 | 32 | 108 | 32 | 108 | -901.35 | -412.41 |
| stitched | PASS | 4 | 11.18 | -3.02 | 0.35 | 0.03 | -44.32 | -42.78 | 86 | 340 | 86 | 340 | 443.51 | -124.34 |
| bridge_1h | PASS | 5 | -11.21 | -18.57 | -0.40 | -0.91 | -42.35 | -49.73 | 43 | 173 | 43 | 173 | -564.53 | -931.84 |
| venue_1h | PASS | 5 | -8.01 | -11.44 | -0.21 | -0.41 | -40.68 | -49.11 | 41 | 179 | 41 | 179 | -404.39 | -575.96 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference -0.0438 percent, 121 blocks, p 0.8378, BH q 1.0000; gated net PnL -3136.67 USD over 165 positions. Outcome: **negative**.

### ref_short / proxy_30m / f4 against f0

| window | gate vs f0 | datasets | return % gated | return % f0 | Sharpe gated | Sharpe f0 | worst DD % gated | worst DD % f0 | positions gated | positions f0 | close records gated | close records f0 | net USD gated | net USD f0 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| is | FAIL | 4 | -1.81 | 8.61 | 0.18 | 0.34 | -42.79 | -29.60 | 69 | 262 | 69 | 262 | -72.56 | 342.07 |
| oos | FAIL | 5 | -17.84 | -11.68 | -0.42 | -0.09 | -59.24 | -70.85 | 137 | 538 | 137 | 538 | -895.38 | -585.01 |
| 2024 | PASS | 4 | -25.98 | -36.69 | -0.48 | -0.85 | -53.95 | -65.29 | 117 | 499 | 117 | 499 | -1041.11 | -1467.37 |
| 2025H1t | PASS | 4 | -19.84 | -26.53 | -0.92 | -1.25 | -54.93 | -54.43 | 62 | 205 | 62 | 205 | -795.35 | -1061.33 |
| stitched | FAIL | 4 | -15.05 | 10.27 | -0.12 | 0.25 | -54.68 | -39.37 | 181 | 689 | 181 | 689 | -605.01 | 409.85 |
| bridge_30m | FAIL | 5 | -7.19 | -8.34 | -1.02 | -0.77 | -24.00 | -26.47 | 43 | 164 | 43 | 164 | -363.60 | -418.15 |
| venue_30m | FAIL | 5 | -5.43 | -5.10 | -0.97 | -0.42 | -22.92 | -28.82 | 44 | 168 | 44 | 168 | -275.50 | -256.67 |

Statistic over oos, 2024 and 2025H1t: mean daily paired difference +0.0244 percent, 121 blocks, p 0.2892, BH q 0.6426; gated net PnL -2731.84 USD over 316 positions. Outcome: **negative**.

`gate vs f0` is PASS when the gated arm's mean Sharpe and mean drawdown-adjusted return both exceed factor 0 over the datasets both arms scored. `is` is the selection window; `stitched` is one continuous run over is and oos. venue rows use Hyperliquid candles; bridge rows use proxy candles on the venue dates.

## Resolved research configuration

| contract | kind | direction | close | ATR method | htf_filter | trailing stop | regime classifier |
|---|---|---|---|---|---|---|---|
| ratchet_both | ratchet | both | trailing_tp_ratchet_regime | simple | no | {"use_defaults": true} | adx |
| ratchet_long | ratchet | long | trailing_tp_ratchet_regime | simple | no | {"use_defaults": true} | adx |
| ratchet_short | ratchet | short | trailing_tp_ratchet_regime | simple | no | {"use_defaults": true} | adx |
| ref_long | open_as_close | long | open-as-close | simple | no | none | - |
| ref_short | open_as_close | short | open-as-close | simple | no | none | - |

Ratchet tiers and trailing multiple per regime label (use_defaults):

| label | tiers | trailing stop ATR multiple |
|---|---|---|
| ranging | 0.75 ATR close 0.4 trail 1.0; 1.5 ATR close 0.8 trail 0.75; 2.0 ATR close 1.0 trail 0.75 | 2.0 |
| trending_down | 2.0 ATR close 0.0 trail 1.5; 2.5 ATR close 0.0 trail 1.0; 3.0 ATR close 0.0 trail 0.8 | 2.5 |
| trending_up | 2.0 ATR close 0.0 trail 1.5; 2.5 ATR close 0.0 trail 1.0; 3.0 ATR close 0.0 trail 0.8 | 2.5 |

Carried by the study: atr_method: copied from the resolution (build_candidate does not copy it); closed_bar_decisions: True in the configuration; the candidate has no field for it, so the study's finite-frame signal path models it (closed-bar frame of the last 200 candles, next-bar fill); htf_filter: false in the configuration; copied when true; regime_period: build_candidate writes regime_period 14 and regime_adx_threshold 20; validate_candidate refuses them without a gate consumer, so the study drops them; run_candidate_leg then uses the same defaults (14, 20).

Not modeled: shared-wallet pool sizing, forming-candle decisions, venue lot and minimum-notional gates on the proxy path.

Refused contract `ref_both`: eval_windows.validate_candidate refuses direction both without close_strategies (the plain signal path runs one leg at a time); a sum of ref_long and ref_short is a diagnostic, never one bidirectional book (harness: candidate has direction='both' but no close_strategies. The plain signal path runs one leg at a time (long/flat, or short/flat under direction='short'), so the ...).

## Disclosures

- Binance US spot candles are a proxy for Hyperliquid perps prices. Proxy results are labeled proxy; Hyperliquid venue evidence is reported separately and covers only the venue tail windows.
- No chart_pattern deployment snapshot was available. research_config.json is an explicit research assumption, not a deployment.
- The frame length of 200 bars follows from the research regime block. A configuration with longer regime windows fetches more candles and changes the gate's EMA seeding and the pattern detectors' view.
- Live forming-candle behavior without closed_bar_decisions is not modeled; the research configuration sets closed_bar_decisions true.
- Shared-wallet sizing, leverage tiers other than 1 and margin pooling are not modeled.
- The is, oos, 2023, 2024 and 2025H1 windows were examined before for this strategy (see prior_exposure).
- Prior exposure: The windows is, oos, 2023, 2024 and 2025H1 were examined by issue 982 (2026-07-01) and issue 1282 (2026-07-10) for this strategy and these factors on the spot long audit datasets. The oos window has since accrued four months. The perps datasets (30m, 1h, BNB, HYPE, short and bidirectional books, the ratchet close) were not examined for chart_pattern before this study, but their calendar windows overlap the examined ones.

Protocol lock: `06bb9dc650fb35f2647cd54a40dcc20c24e66f8f65235b268e5b183353b291d5` at 2026-11-14T19:37:04Z (source commit `ab5214ca216e5923a3f8f5c104e161fca4d41033+uncommitted-changes`).

Results: 2058 legs, source commit `ab5214ca216e5923a3f8f5c104e161fca4d41033+uncommitted-changes`. Refused network attempts in the main process: 3 (the deliberate probe of a socket, a name lookup and an unfrozen symbol). Every scoring worker runs with the same block on its own copy of the throwaway cache.
