# true_strength_index study (issue 1667)

**Verdict: FAIL.** 1h: fail (pass condition false: candidate_beats_trix_momentum_ddadj, candidate_beats_trix_momentum_sharpe, candidate_beats_vol_momentum_ddadj, candidate_beats_vol_momentum_sharpe, candidate_net_positive_base, candidate_net_positive_stress); 4h: fail (pass condition false: candidate_beats_momentum_pro_ddadj, candidate_beats_momentum_pro_sharpe, candidate_beats_trix_momentum_ddadj, candidate_beats_trix_momentum_sharpe, candidate_net_positive_base, candidate_net_positive_stress).

The Python registry and the Go mirror keep `edge_status="no_edge"` with `edge_source="study_fail"` and this file as `edge_ref`. No study result removes the no-edge label: paper evaluation needs exactly one explicit `--mode=paper`, and live use still needs `allow_no_edge: true`, which is an operator decision and not evidence of profit. Nothing here edits deployed configuration, live defaults or acknowledgements; the study is suggest-only. The run used the registry with `edge_source="unvalidated"` (the registry sha256 below); the only change after scoring is the `edge_source` label in `shared_strategies/open/registry.py` and `scheduler/edge_status.go`.

## Candidate

Entry-only True Strength Index crossover (`shared_strategies/open/true_strength_index.py`). For each endpoint t the evaluator reads exactly W = recursion_length closes t-W+1..t and their W - 1 successive changes. The signed changes and their absolute values are each smoothed by a slow EMA (weight 2 / (slow_period + 1)) and then a fast EMA (weight 2 / (fast_period + 1)); both stages start from the first change of the window. TSI = 100 x double-smoothed change / double-smoothed absolute change. The signal line is an EMA (weight 2 / (signal_period + 1)) of TSI, unset until the first finite TSI of the window and seeded with it. Endpoint t-1 is computed from its own window t-W..t-1, so a decision reads W + 1 bars. Long when TSI - signal > 0 at t, <= 0 at t-1, and TSI at t > 0; short is the exact mirror. Bad candles, timestamp defects (including received-order defects), missing cadence, cadence gaps, a zero endpoint denominator and non-finite derived values inside the support hold the entry with a reason; the close owner and stop keep running.

Close owner: `tiered_tp_pct` (`{"tp_tiers": [{"profit_pct": 0.01, "close_fraction": 0.25}, {"profit_pct": 0.02, "close_fraction": 0.5}, {"profit_pct": 0.03, "close_fraction": 0.75}, {"profit_pct": 0.04, "close_fraction": 1.0}]}`); stop owner: `stop_loss_atr_mult=1.0`. Seed: slow_period=25, fast_period=13, signal_period=7, recursion_length=180. Parameter rules: integers, every period >= 2, fast_period < slow_period, signal_period < W, W >= 6 x slow_period, W <= 198. no regime gate and no higher-timeframe filter on any leg; every arm runs with the same close, stop, cost and funding settings.

## Protocol, provenance and invocations

- Protocol `protocol.json` sha256 `5a8994e7116e5681d346a2211065cf2d2c96fa17c659475f65395431d027f9a5`, frozen before scoring; `study_manifest.json` (sha256 `e2186c1c38ba28cbbeb9567b7b6b1328c4900de4e394f48a6554a38499fed1b7`) records the same hash at freeze time (2026-10-13T01:49:06Z, commit `04754fcdb03b60cce528eb2f1ea0603e4a6180af+uncommitted-changes`, 54 candidate grid combinations validated).
- Run source commit `e6634dbff3384ca694d8f6fa2b5e975c0fe4c4ed`; runtime 2408.4 s. Source file sha256:
  - `backtest/backtester.py`: `e8233491bb19e59ae80115cabeea5644cdd0d6cebbf589c465d6c79f2941b8e0`
  - `backtest/candidates/true_strength_index_1667/run_study.py`: `46ee0641651c29a8cf05724410e5da80adbd2ace45a4c65fd1833971be1ef138`
  - `backtest/eval_windows.py`: `b32d5fbca2b241c25e014890347866f4533c0640318e4ddf34485f0b87fb3f9e`
  - `backtest/parity_diff.py`: `28b9f1bc415838c24f232febe9ff91903c16c74d163e868a63ac721e9439c549`
  - `shared_scripts/check_hyperliquid.py`: `ab4be327eee3741b03c3a4cc3316155fe89a267e038fe0d776a13f421bee6179`
  - `shared_strategies/close/tiered_tp_pct.py`: `f6a480628c37f0da329336aa5efaeacd3dced5fe73622ccfc4ee47a16e07daa6`
  - `shared_strategies/open/registry.py`: `7feb62df0a1d05171932951f2ed1901e5b133bb08f555969847672b8700ee5c4`
  - `shared_strategies/open/trix_momentum.py`: `d9775bbf83c517d5b2d799f054fa79c1e9115fb5af2df953a8d72116a776925f`
  - `shared_strategies/open/true_strength_index.py`: `932f4efb753680e785267555fd84c3f0b8e153bcbbe466065d8957e719e32d8f`
- Inputs: no new acquisition: the inputs are the hash-pinned frozen files of study chande_momentum_1661, read in place; no copies. Source manifest `backtest/candidates/chande_momentum_1661/study_manifest.json` sha256 `f5bd9d2672abf5880b8af57cb01a0ffb70b21ccdf5eab464a9d351e121d580e3`. Every file below is verified against `protocol.json` before each run, and network acquisition is disabled while scoring.
  - `BTC_1h_candles.csv.gz`: `d107c60f72363372d03273ba6439ca42a0d867a9089c5de171e9306af3ba2d17`
  - `BTC_4h_candles.csv.gz`: `dd0758a2862955eb53b060df5fbace3e98f8bc2a3e0254842bca0d36c96f106f`
  - `BTC_funding.csv.gz`: `520fd979d5594b4c2fb950cd52aa327749a124d9971a59ec3419752dd24a27f8`
  - `ETH_1h_candles.csv.gz`: `59d08735aecb22b63bab8189c7c32b11101e80da42b2ec724f39e308548b7913`
  - `ETH_4h_candles.csv.gz`: `03df63c5dc65ba532a252e8c00118986321fcb124934bb7536e842d678fc9dd8`
  - `ETH_funding.csv.gz`: `4b7694d8fc26cb7245ffa882cbdcb2d571d17f3a9f1abd6832fb0deed4985b41`
  - `SOL_1h_candles.csv.gz`: `e4e5c15ffcce696253d78dc3b3c3448725e8cf3a0114a9fbc4b1df5f2a0efeec`
  - `SOL_4h_candles.csv.gz`: `c1e5775973d00b0b07597da3db4213d62378ef566fe2e4a3b619612b59abc8ab`
  - `SOL_funding.csv.gz`: `b29de7625f7484f0971336e31ba52f5b042127e4eef4d8ee1e4554994383cd2b`
- Invocations, from the repository root:
  - `uv run --no-sync python backtest/candidates/true_strength_index_1667/run_study.py freeze`
  - `uv run --no-sync python backtest/candidates/true_strength_index_1667/run_study.py run --cache-db <new file>`
  - `uv run --no-sync python backtest/candidates/true_strength_index_1667/run_study.py render`
- Every leg calls `eval_windows.run_leg` with `registry=futures`, `direction=both`, the frozen close and stop owner, `funding_mode=charge` (the `--funding charge` path), `intrabar_resolution=ohlc_walk` and `warmup_bars=300`.
- Ablations: both ablations are installed only into the research registry instance of the run and removed afterwards; they are never registered for production and are descriptive only. Centerline only: the candidate's own fixed-window TSI on the same decision rows (tsi_valid == 1): long when TSI > 0 and the previous TSI <= 0, short when TSI < 0 and the previous TSI >= 0; the signal line is not used, so signal_period stays at the seed value 7. Signal line only: the candidate's own fixed-window TSI and signal line on the same decision rows: long when TSI - signal > 0 and the previous difference <= 0, short when TSI - signal < 0 and the previous difference >= 0, with no centerline filter.

Windows (finite, half-open, disjoint):

| window | start | end | role |
|---|---|---|---|
| is | 2025-06-10 | 2026-01-01 | training |
| 2023H2 | 2023-07-01 | 2024-01-01 | held-out |
| 2024 | 2024-01-01 | 2025-01-01 | held-out |
| 2025H1a | 2025-01-01 | 2025-06-10 | held-out |
| oos | 2026-01-01 | 2026-10-01 | held-out |

Study-local finite copies of the M1 windows in backtest/eval_windows.py WINDOWS; the shared M1 defaults are unchanged. is is the M1 training window. oos is the open-ended M1 window pinned to end 2026-10-01 (the frozen data end). 2023H2 replaces the M1 2023 window: the frozen candles start 2023-01-01, so 2023-01-01 has no 300-bar warmup, and Hyperliquid funding starts 2023-05-12, so held hours before it are unpriced. 2025H1a is the M1 2025H1 window cut at 2025-06-10 so it does not overlap the is training window. Windows are half-open [start, end) and the five windows are disjoint. The candles were scored before by studies chande_momentum_1661, adaptive_moving_average_1641, dual_thrust_1642 and trix_momentum_1659 for other strategies (trix_momentum_1659 also scored the trix_momentum comparator on them); true_strength_index was never scored on them, but the held-out windows are retrospective data, not live forward evidence. the frozen candles end at 2026-09-30 (last 1h open 1790809200000 ms, last 4h open 1790798400000 ms), so the oos window [2026-01-01, 2026-10-01) ends at the frozen data end.

## Verdict gates

### 1h: fail

| gate or condition | value |
|---|---|
| all_windows_scorable | yes |
| both_sides | yes |
| close_capability | yes |
| integration_parity | yes |
| required_arm_clusters | yes |
| selections_not_fallback | yes |
| candidate_beats_momentum_pro_ddadj | yes |
| candidate_beats_momentum_pro_sharpe | yes |
| candidate_beats_trix_momentum_ddadj | no |
| candidate_beats_trix_momentum_sharpe | no |
| candidate_beats_vol_momentum_ddadj | no |
| candidate_beats_vol_momentum_sharpe | no |
| candidate_net_positive_base | no |
| candidate_net_positive_stress | no |

| arm | clusters | positions | long/short | net PnL USD | net x2 slippage | mean Sharpe | mean ddadj | worst max DD % | fees USD | funding USD | turnover |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | 741 | 1472 | 778/694 | -939.26 | -2247.98 | -0.766 | -0.175 | 34.48 | 1269.61 | -79.50 | 2820.72 |
| centerline_only | 784 | 2008 | 998/1010 | -1822.82 | - | -1.306 | -0.457 | 39.33 | 1680.66 | -6.88 | 3733.97 |
| signal_line_only | 655 | 3007 | 1492/1515 | -2157.17 | - | -0.857 | -0.328 | 62.78 | 2419.48 | -24.28 | 5375.40 |
| momentum_pro | 761 | 1705 | 865/840 | -2404.30 | - | -1.509 | -0.579 | 42.93 | 1408.28 | -137.63 | 3128.81 |
| vol_momentum | 116 | 157 | 80/77 | -67.13 | - | -0.006 | 0.393 | 10.76 | 140.73 | -8.22 | 312.66 |
| trix_momentum | 597 | 992 | 514/478 | -392.58 | - | -0.292 | 0.203 | 29.53 | 869.42 | -55.22 | 1931.60 |

Unscorable windows: none. Largest candidate position share of positive position PnL: 0.0090 (descriptive). M1 incumbent-relative passes for the candidate: 1 of 4 (descriptive). The ablation rows are descriptive and do not enter any gate.

### 4h: fail

| gate or condition | value |
|---|---|
| all_windows_scorable | yes |
| both_sides | yes |
| close_capability | yes |
| integration_parity | yes |
| required_arm_clusters | yes |
| selections_not_fallback | yes |
| candidate_beats_momentum_pro_ddadj | no |
| candidate_beats_momentum_pro_sharpe | no |
| candidate_beats_trix_momentum_ddadj | no |
| candidate_beats_trix_momentum_sharpe | no |
| candidate_beats_vol_momentum_ddadj | yes |
| candidate_beats_vol_momentum_sharpe | yes |
| candidate_net_positive_base | no |
| candidate_net_positive_stress | no |

| arm | clusters | positions | long/short | net PnL USD | net x2 slippage | mean Sharpe | mean ddadj | worst max DD % | fees USD | funding USD | turnover |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | 267 | 501 | 260/241 | -817.29 | -1291.81 | -0.351 | -0.204 | 40.64 | 449.82 | -80.04 | 999.37 |
| centerline_only | 252 | 430 | 214/216 | -583.87 | - | -0.387 | 0.023 | 32.25 | 379.68 | -5.88 | 843.55 |
| signal_line_only | 296 | 1219 | 613/606 | -964.19 | - | -0.343 | -0.175 | 52.77 | 1086.61 | -10.21 | 2414.15 |
| momentum_pro | 154 | 240 | 123/117 | -19.59 | - | -0.210 | 0.349 | 25.17 | 215.41 | -56.29 | 478.59 |
| vol_momentum | 128 | 198 | 99/99 | -542.61 | - | -0.398 | -0.225 | 21.80 | 171.72 | -11.59 | 381.51 |
| trix_momentum | 188 | 293 | 157/136 | -319.75 | - | -0.170 | 0.243 | 28.94 | 259.30 | -52.50 | 576.10 |

Unscorable windows: none. Largest candidate position share of positive position PnL: 0.0190 (descriptive). M1 incumbent-relative passes for the candidate: 0 of 4 (descriptive). The ablation rows are descriptive and do not enter any gate.

## Selections (training window `is`)

| group | arm | how | params | eligible rows | clusters | mean ddadj | mean Sharpe | seed rank |
|---|---|---|---|---|---|---|---|---|
| 1h | candidate | selected | fast_period=18, recursion_length=180, signal_period=9, slow_period=30 | 54 | 149.00 | -0.155 | -0.920 | 45.00 |
| 1h | centerline_only | selected | fast_period=13, recursion_length=180, slow_period=25 | 18 | 140.00 | 0.131 | -0.096 | - |
| 1h | momentum_pro | selected | adx_threshold=18.0, ema_fast=26, ema_long=200, ema_mid=34, pullback_window=6, vol_mult=0.0 | 243 | 137.00 | 1.352 | 0.377 | - |
| 1h | signal_line_only | selected | fast_period=18, recursion_length=180, signal_period=9, slow_period=25 | 54 | 113.00 | -0.554 | -1.245 | - |
| 1h | trix_momentum | selected | period=12, recursion_length=120, signal_period=12 | 18 | 120.00 | -0.445 | -0.825 | - |
| 1h | vol_momentum | selected | eff_entry=0.25, entry_threshold=0.3, exit_threshold=0.0, mom_window=48 | 69 | 19.00 | 0.250 | 0.232 | - |
| 4h | candidate | selected | fast_period=8, recursion_length=180, signal_period=9, slow_period=30 | 54 | 56.00 | 1.580 | 0.821 | 49.00 |
| 4h | centerline_only | selected | fast_period=18, recursion_length=180, slow_period=30 | 18 | 48.00 | -0.286 | -0.467 | - |
| 4h | momentum_pro | selected | adx_threshold=20.0, ema_fast=20, ema_long=200, ema_mid=34, pullback_window=4, vol_mult=1.5 | 243 | 32.00 | 0.696 | 0.516 | - |
| 4h | signal_line_only | selected | fast_period=8, recursion_length=180, signal_period=5, slow_period=30 | 54 | 55.00 | 1.223 | 0.477 | - |
| 4h | trix_momentum | selected | period=12, recursion_length=120, signal_period=6 | 18 | 32.00 | 2.109 | 1.451 | - |
| 4h | vol_momentum | selected | eff_entry=0.25, entry_threshold=0.3, exit_threshold=0.1, mom_window=24 | 54 | 23.00 | 1.284 | 0.386 | - |

## Held-out windows per arm

| group | window | arm | net PnL USD | clusters | positions | long/short | mean Sharpe | mean ddadj | M1 verdict | funding complete |
|---|---|---|---|---|---|---|---|---|---|---|
| 1h | 2023H2 | candidate | -348.64 | 164 | 297 | 156/141 | -1.721 | -0.490 | fail | yes |
| 1h | 2023H2 | centerline_only | -408.71 | 144 | 384 | 193/191 | -1.750 | -0.438 | fail | yes |
| 1h | 2023H2 | signal_line_only | -449.29 | 95 | 520 | 250/270 | -0.819 | -0.519 | fail | yes |
| 1h | 2023H2 | momentum_pro | -220.55 | 124 | 285 | 154/131 | -1.306 | -0.115 | fail | yes |
| 1h | 2023H2 | vol_momentum | 96.33 | 16 | 26 | 14/12 | 0.760 | 2.060 | fail | yes |
| 1h | 2023H2 | trix_momentum | -245.76 | 105 | 179 | 93/86 | -0.855 | -0.195 | fail | yes |
| 1h | 2024 | candidate | 196.08 | 280 | 549 | 297/252 | 0.375 | 0.638 | fail | yes |
| 1h | 2024 | centerline_only | -137.46 | 285 | 721 | 360/361 | -0.134 | -0.080 | fail | yes |
| 1h | 2024 | signal_line_only | -558.84 | 255 | 1190 | 596/594 | -0.469 | -0.220 | fail | yes |
| 1h | 2024 | momentum_pro | -445.65 | 290 | 652 | 345/307 | -0.421 | -0.575 | fail | yes |
| 1h | 2024 | vol_momentum | -18.04 | 39 | 46 | 24/22 | 0.064 | 0.099 | fail | yes |
| 1h | 2024 | trix_momentum | -37.29 | 232 | 380 | 200/180 | -0.040 | 0.616 | fail | yes |
| 1h | 2025H1a | candidate | -40.92 | 113 | 240 | 120/120 | -0.029 | -0.079 | pass | yes |
| 1h | 2025H1a | centerline_only | -728.51 | 159 | 389 | 197/192 | -2.011 | -0.835 | fail | yes |
| 1h | 2025H1a | signal_line_only | 194.56 | 103 | 520 | 257/263 | 0.375 | 0.345 | pass | yes |
| 1h | 2025H1a | momentum_pro | -661.76 | 142 | 285 | 123/162 | -1.835 | -0.741 | fail | yes |
| 1h | 2025H1a | vol_momentum | -35.18 | 20 | 23 | 11/12 | -0.237 | -0.173 | pass | yes |
| 1h | 2025H1a | trix_momentum | 379.91 | 101 | 168 | 86/82 | 0.973 | 1.081 | pass | yes |
| 1h | oos | candidate | -745.78 | 184 | 386 | 205/181 | -1.691 | -0.768 | fail | yes |
| 1h | oos | centerline_only | -548.14 | 196 | 514 | 248/266 | -1.328 | -0.474 | fail | yes |
| 1h | oos | signal_line_only | -1343.60 | 202 | 777 | 389/388 | -2.515 | -0.919 | fail | yes |
| 1h | oos | momentum_pro | -1076.34 | 205 | 483 | 243/240 | -2.476 | -0.887 | fail | yes |
| 1h | oos | vol_momentum | -110.24 | 41 | 62 | 31/31 | -0.611 | -0.413 | fail | yes |
| 1h | oos | trix_momentum | -489.44 | 159 | 265 | 135/130 | -1.246 | -0.693 | fail | yes |
| 4h | 2023H2 | candidate | -3.49 | 44 | 91 | 53/38 | 0.135 | 0.047 | fail | yes |
| 4h | 2023H2 | centerline_only | 103.45 | 35 | 75 | 36/39 | 0.168 | 0.897 | fail | yes |
| 4h | 2023H2 | signal_line_only | -243.50 | 37 | 187 | 97/90 | -0.286 | -0.184 | fail | yes |
| 4h | 2023H2 | momentum_pro | 364.59 | 28 | 44 | 34/10 | 0.380 | 1.807 | fail | yes |
| 4h | 2023H2 | vol_momentum | -141.93 | 22 | 35 | 16/19 | -0.438 | -0.237 | fail | yes |
| 4h | 2023H2 | trix_momentum | 336.84 | 31 | 52 | 37/15 | 0.875 | 1.866 | fail | yes |
| 4h | 2024 | candidate | -402.57 | 108 | 201 | 105/96 | -0.417 | -0.547 | fail | yes |
| 4h | 2024 | centerline_only | -65.04 | 107 | 169 | 85/84 | 0.021 | -0.104 | fail | yes |
| 4h | 2024 | signal_line_only | -321.58 | 130 | 504 | 255/249 | -0.496 | -0.235 | fail | yes |
| 4h | 2024 | momentum_pro | -42.92 | 51 | 85 | 43/42 | -0.105 | 0.333 | fail | yes |
| 4h | 2024 | vol_momentum | -152.39 | 49 | 72 | 36/36 | -0.363 | -0.166 | fail | yes |
| 4h | 2024 | trix_momentum | -504.37 | 73 | 113 | 64/49 | -0.947 | -0.794 | fail | yes |
| 4h | 2025H1a | candidate | -519.69 | 55 | 86 | 42/44 | -1.471 | -0.589 | fail | yes |
| 4h | 2025H1a | centerline_only | -79.44 | 48 | 79 | 39/40 | -0.114 | -0.022 | pass | yes |
| 4h | 2025H1a | signal_line_only | -539.67 | 62 | 220 | 109/111 | -0.885 | -0.563 | fail | yes |
| 4h | 2025H1a | momentum_pro | -72.78 | 33 | 49 | 19/30 | -0.334 | -0.159 | pass | yes |
| 4h | 2025H1a | vol_momentum | -31.02 | 28 | 41 | 21/20 | -0.123 | 0.030 | pass | yes |
| 4h | 2025H1a | trix_momentum | -100.36 | 37 | 49 | 21/28 | -0.462 | -0.032 | fail | yes |
| 4h | oos | candidate | 108.46 | 60 | 123 | 60/63 | 0.350 | 0.273 | fail | yes |
| 4h | oos | centerline_only | -542.84 | 62 | 107 | 54/53 | -1.621 | -0.677 | fail | yes |
| 4h | oos | signal_line_only | 140.56 | 67 | 308 | 152/156 | 0.297 | 0.281 | fail | yes |
| 4h | oos | momentum_pro | -268.48 | 42 | 62 | 27/35 | -0.782 | -0.584 | fail | yes |
| 4h | oos | vol_momentum | -217.27 | 29 | 50 | 26/24 | -0.666 | -0.526 | fail | yes |
| 4h | oos | trix_momentum | -51.86 | 47 | 79 | 35/44 | -0.146 | -0.068 | fail | yes |

## Funding coverage (candidate held-out legs)

| group | window | dataset | expected hours | priced hours | unpriced held hours | funding USD |
|---|---|---|---|---|---|---|
| 1h | 2023H2 | BTC/USDT 1h | 4415 | 4413 | 0 | -6.16 |
| 1h | 2023H2 | ETH/USDT 1h | 4415 | 4413 | 0 | -9.96 |
| 1h | 2023H2 | SOL/USDT 1h | 4415 | 4413 | 0 | -5.14 |
| 1h | 2024 | BTC/USDT 1h | 8783 | 8782 | 0 | -15.11 |
| 1h | 2024 | ETH/USDT 1h | 8783 | 8782 | 0 | -14.27 |
| 1h | 2024 | SOL/USDT 1h | 8783 | 8782 | 0 | -16.63 |
| 1h | 2025H1a | BTC/USDT 1h | 3839 | 3839 | 0 | -3.24 |
| 1h | 2025H1a | ETH/USDT 1h | 3839 | 3839 | 0 | -2.95 |
| 1h | 2025H1a | SOL/USDT 1h | 3839 | 3839 | 0 | -1.03 |
| 1h | oos | BTC/USDT 1h | 6551 | 6551 | 0 | -0.16 |
| 1h | oos | ETH/USDT 1h | 6551 | 6551 | 0 | -1.66 |
| 1h | oos | SOL/USDT 1h | 6551 | 6551 | 0 | -3.18 |
| 4h | 2023H2 | BTC/USDT 4h | 4412 | 4410 | 0 | -9.63 |
| 4h | 2023H2 | ETH/USDT 4h | 4412 | 4410 | 0 | -7.21 |
| 4h | 2023H2 | SOL/USDT 4h | 4412 | 4410 | 0 | -11.95 |
| 4h | 2024 | BTC/USDT 4h | 8780 | 8779 | 0 | -16.09 |
| 4h | 2024 | ETH/USDT 4h | 8780 | 8779 | 0 | -17.85 |
| 4h | 2024 | SOL/USDT 4h | 8780 | 8779 | 0 | -10.66 |
| 4h | 2025H1a | BTC/USDT 4h | 3836 | 3836 | 0 | -0.74 |
| 4h | 2025H1a | ETH/USDT 4h | 3836 | 3836 | 0 | -0.74 |
| 4h | 2025H1a | SOL/USDT 4h | 3836 | 3836 | 0 | -0.62 |
| 4h | oos | BTC/USDT 4h | 6548 | 6548 | 0 | -0.51 |
| 4h | oos | ETH/USDT 4h | 6548 | 6548 | 0 | -1.71 |
| 4h | oos | SOL/USDT 4h | 6548 | 6548 | 0 | -2.33 |

## Fills, exits and entry evidence

- 1h: 1472 candidate held-out positions checked for a bar N signal filled at the bar N+1 open, 0 mismatches; 479 positions closed in more than one part; exit reasons end_of_data, sl, tiered_tp_pct. `evidence/entries_1h.csv.gz` holds 1722 entry rows (900 long, 822 short; sha256 `b4f8b8d33ea6a2abfc8f83148d69713d822955daa3e8911db993fb12de16e502`); 0 rows failed the exact W + 1 row replay (the replay also proves W rows hold with insufficient_history).
- 4h: 501 candidate held-out positions checked for a bar N signal filled at the bar N+1 open, 0 mismatches; 211 positions closed in more than one part; exit reasons end_of_data, sl, tiered_tp_pct. `evidence/entries_4h.csv.gz` holds 573 entry rows (284 long, 289 short; sha256 `093c5cac29d747e8914103aa060c2a1a820f8daad43485eafd524894ed6e0bfd`); 0 rows failed the exact W + 1 row replay (the replay also proves W rows hold with insufficient_history).

## Continuous-history reference (descriptive)

continuous-history TSI (both smoothing stages seeded once from the first close change of the warmed leg frame, the signal line seeded once at the first finite TSI, never restarted); a descriptive reference only, it never makes a decision. The fixed-window values make every decision.

| group | window and dataset | bars | max abs TSI diff | median | max abs signal-line diff | decision differences | fixed / continuous entries |
|---|---|---|---|---|---|---|---|
| 1h | 2023H2 BTC/USDT 1h | 4416 | 6.873e-02 | 1.122e-03 | 9.287e-02 | 0 | 111 / 111 |
| 1h | 2023H2 ETH/USDT 1h | 4416 | 3.801e-02 | 1.154e-03 | 5.317e-02 | 0 | 119 / 119 |
| 1h | 2023H2 SOL/USDT 1h | 4416 | 3.557e-02 | 1.140e-03 | 4.561e-02 | 2 | 112 / 112 |
| 1h | 2024 BTC/USDT 1h | 8784 | 2.331e-02 | 1.154e-03 | 3.125e-02 | 0 | 207 / 207 |
| 1h | 2024 ETH/USDT 1h | 8784 | 3.334e-02 | 1.178e-03 | 4.111e-02 | 0 | 207 / 207 |
| 1h | 2024 SOL/USDT 1h | 8784 | 2.124e-02 | 1.264e-03 | 2.790e-02 | 0 | 217 / 217 |
| 1h | 2025H1a BTC/USDT 1h | 3840 | 2.443e-02 | 1.190e-03 | 4.733e-02 | 0 | 103 / 103 |
| 1h | 2025H1a ETH/USDT 1h | 3840 | 3.445e-02 | 1.236e-03 | 4.905e-02 | 0 | 90 / 90 |
| 1h | 2025H1a SOL/USDT 1h | 3840 | 3.557e-02 | 1.276e-03 | 4.778e-02 | 0 | 89 / 89 |
| 1h | oos BTC/USDT 1h | 6363 | 2.348e-02 | 1.198e-03 | 3.060e-02 | 0 | 154 / 154 |
| 1h | oos ETH/USDT 1h | 6363 | 2.835e-02 | 1.169e-03 | 3.500e-02 | 0 | 164 / 164 |
| 1h | oos SOL/USDT 1h | 6363 | 1.823e-02 | 1.214e-03 | 2.222e-02 | 0 | 149 / 149 |
| 4h | 2023H2 BTC/USDT 4h | 1104 | 1.904e-02 | 5.289e-04 | 2.173e-02 | 0 | 37 / 37 |
| 4h | 2023H2 ETH/USDT 4h | 1104 | 2.412e-02 | 5.613e-04 | 2.898e-02 | 0 | 42 / 42 |
| 4h | 2023H2 SOL/USDT 4h | 1104 | 1.775e-02 | 4.680e-04 | 2.154e-02 | 0 | 37 / 37 |
| 4h | 2024 BTC/USDT 4h | 2196 | 9.462e-03 | 5.791e-04 | 1.278e-02 | 0 | 75 / 75 |
| 4h | 2024 ETH/USDT 4h | 2196 | 1.294e-02 | 5.915e-04 | 1.803e-02 | 0 | 72 / 72 |
| 4h | 2024 SOL/USDT 4h | 2196 | 8.967e-03 | 6.695e-04 | 1.287e-02 | 0 | 77 / 77 |
| 4h | 2025H1a BTC/USDT 4h | 960 | 1.424e-02 | 6.406e-04 | 1.981e-02 | 0 | 30 / 30 |
| 4h | 2025H1a ETH/USDT 4h | 960 | 2.087e-02 | 6.405e-04 | 3.044e-02 | 0 | 37 / 37 |
| 4h | 2025H1a SOL/USDT 4h | 960 | 1.113e-02 | 7.064e-04 | 1.542e-02 | 0 | 26 / 26 |
| 4h | oos BTC/USDT 4h | 1456 | 8.282e-03 | 7.135e-04 | 1.213e-02 | 0 | 42 / 42 |
| 4h | oos ETH/USDT 4h | 1456 | 9.451e-03 | 6.788e-04 | 1.224e-02 | 0 | 49 / 49 |
| 4h | oos SOL/USDT 4h | 1456 | 9.522e-03 | 7.404e-04 | 1.245e-02 | 0 | 49 / 49 |

Synthetic sensitivity fixture: synthetic geometric random walk, generated with numpy.random.default_rng(seed); 1h open-time bars from 2026-01-01; open = previous close, high and low 0.2% outside the body; seed 1667, 6000 bars, log-return standard deviation 0.01, fixture sha256 `0a638449de4400110dc11782c529c6614c38529e0e6a1f5cbc40ae98dba376ff`. It is produced by `run_study.py run` (function `sensitivity_fixture`) through the real registry.

| params | bars | max abs TSI diff | median | max abs signal-line diff | median | decision differences | fixed / continuous entries |
|---|---|---|---|---|---|---|---|
| seed_params (fast_period=13, recursion_length=180, signal_period=7, slow_period=25) | 5820 | 7.627e-04 | 1.061e-04 | 1.011e-03 | 1.417e-04 | 0 | 192 / 192 |

## Parameter stability and variants (candidate)

Training neighbours of the 1h selection:

| params | mean ddadj | mean Sharpe | clusters |
|---|---|---|---|
| fast_period=18, recursion_length=180, signal_period=9, slow_period=30 | -0.155 | -0.920 | 149 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=25 | -0.719 | -1.136 | 138 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=30 | -0.682 | -0.974 | 150 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=30 | -0.768 | -0.960 | 145 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=30 | -0.155 | -0.920 | 149 |

| held-out params | 2023H2 | 2024 | 2025H1a | oos |
|---|---|---|---|---|
| fast_period=13, recursion_length=180, signal_period=7, slow_period=25 | -205.79 USD | -474.91 USD | 150.84 USD | -646.11 USD |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=30 | -352.85 USD | -155.38 USD | 300.30 USD | -650.84 USD |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=30 | -403.92 USD | -169.40 USD | 189.56 USD | -631.74 USD |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=25 | -339.77 USD | -86.96 USD | 271.04 USD | -617.07 USD |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=30 | -303.41 USD | 196.08 USD | -40.92 USD | -735.67 USD |

All 54 training variants of the 1h candidate arm (unsuccessful variants included):

| params | clusters | mean ddadj | mean Sharpe | net PnL USD |
|---|---|---|---|---|
| fast_period=8, recursion_length=180, signal_period=5, slow_period=20 | 156 | -0.485 | -0.733 | -337.46 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=20 | 156 | -0.485 | -0.733 | -337.46 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=20 | 177 | -0.664 | -1.565 | -629.58 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=20 | 177 | -0.664 | -1.565 | -629.58 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=20 | 166 | -0.525 | -1.448 | -455.08 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=20 | 166 | -0.525 | -1.448 | -455.08 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=20 | 158 | -0.729 | -1.202 | -498.08 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=20 | 158 | -0.729 | -1.202 | -498.08 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=20 | 149 | -0.785 | -1.205 | -553.24 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=20 | 149 | -0.785 | -1.205 | -553.24 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=20 | 147 | -0.830 | -1.540 | -646.97 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=20 | 147 | -0.830 | -1.540 | -646.97 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=20 | 153 | -0.678 | -1.093 | -493.47 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=20 | 153 | -0.678 | -1.093 | -493.47 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=20 | 143 | -0.834 | -1.579 | -623.35 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=20 | 143 | -0.834 | -1.579 | -623.35 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=20 | 150 | -0.625 | -1.204 | -455.03 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=20 | 150 | -0.625 | -1.204 | -455.03 |
| fast_period=8, recursion_length=180, signal_period=5, slow_period=25 | 182 | -0.584 | -0.937 | -431.71 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=25 | 182 | -0.584 | -0.937 | -431.71 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=25 | 161 | -0.697 | -1.153 | -485.87 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=25 | 161 | -0.697 | -1.153 | -485.87 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=25 | 153 | -0.843 | -1.324 | -609.22 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=25 | 153 | -0.843 | -1.324 | -609.22 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=25 | 150 | -0.787 | -1.244 | -542.91 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=25 | 150 | -0.787 | -1.244 | -542.91 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=25 | 145 | -0.852 | -1.604 | -666.69 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=25 | 145 | -0.852 | -1.604 | -666.69 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=25 | 152 | -0.854 | -1.654 | -621.49 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=25 | 152 | -0.854 | -1.654 | -621.49 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=25 | 151 | -0.891 | -1.501 | -673.90 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=25 | 151 | -0.891 | -1.501 | -673.90 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=25 | 157 | -0.796 | -1.386 | -546.45 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=25 | 157 | -0.796 | -1.386 | -546.45 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=25 | 138 | -0.719 | -1.136 | -468.12 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=25 | 138 | -0.719 | -1.136 | -468.12 |
| fast_period=8, recursion_length=180, signal_period=5, slow_period=30 | 166 | -0.524 | -0.669 | -345.24 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=30 | 166 | -0.524 | -0.669 | -345.24 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=30 | 155 | -0.804 | -1.410 | -612.36 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=30 | 155 | -0.804 | -1.410 | -612.36 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=30 | 156 | -0.885 | -1.672 | -677.91 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=30 | 156 | -0.885 | -1.672 | -677.91 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=30 | 164 | -0.823 | -1.624 | -694.23 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=30 | 164 | -0.823 | -1.623 | -694.09 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=30 | 157 | -0.826 | -1.686 | -658.44 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=30 | 157 | -0.826 | -1.686 | -658.44 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=30 | 150 | -0.682 | -0.974 | -413.49 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=30 | 150 | -0.682 | -0.974 | -413.49 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=30 | 159 | -0.860 | -1.436 | -597.74 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=30 | 159 | -0.860 | -1.436 | -597.74 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=30 | 145 | -0.768 | -0.960 | -420.73 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=30 | 145 | -0.764 | -0.942 | -415.88 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=30 | 149 | -0.155 | -0.920 | -318.68 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=30 | 149 | -0.155 | -0.920 | -318.68 |

All 18 training variants of the 1h centerline_only arm (unsuccessful variants included):

| params | clusters | mean ddadj | mean Sharpe | net PnL USD |
|---|---|---|---|---|
| fast_period=8, recursion_length=180, slow_period=20 | 135 | -0.422 | -0.875 | -380.98 |
| fast_period=8, recursion_length=198, slow_period=20 | 135 | -0.422 | -0.875 | -380.98 |
| fast_period=13, recursion_length=180, slow_period=20 | 145 | -0.223 | -0.696 | -312.32 |
| fast_period=13, recursion_length=198, slow_period=20 | 145 | -0.223 | -0.696 | -312.32 |
| fast_period=18, recursion_length=180, slow_period=20 | 134 | 0.051 | -0.288 | -76.66 |
| fast_period=18, recursion_length=198, slow_period=20 | 134 | 0.051 | -0.288 | -76.66 |
| fast_period=8, recursion_length=180, slow_period=25 | 129 | -0.424 | -0.981 | -404.97 |
| fast_period=8, recursion_length=198, slow_period=25 | 129 | -0.424 | -0.981 | -404.97 |
| fast_period=13, recursion_length=180, slow_period=25 | 140 | 0.131 | -0.096 | -12.81 |
| fast_period=13, recursion_length=198, slow_period=25 | 140 | 0.131 | -0.096 | -12.81 |
| fast_period=18, recursion_length=180, slow_period=25 | 127 | -0.316 | -0.485 | -188.53 |
| fast_period=18, recursion_length=198, slow_period=25 | 127 | -0.316 | -0.485 | -188.53 |
| fast_period=8, recursion_length=180, slow_period=30 | 123 | -0.222 | -0.205 | -143.38 |
| fast_period=8, recursion_length=198, slow_period=30 | 123 | -0.222 | -0.205 | -143.38 |
| fast_period=13, recursion_length=180, slow_period=30 | 130 | -0.433 | -0.777 | -283.39 |
| fast_period=13, recursion_length=198, slow_period=30 | 130 | -0.433 | -0.777 | -283.39 |
| fast_period=18, recursion_length=180, slow_period=30 | 143 | -0.365 | -0.963 | -314.74 |
| fast_period=18, recursion_length=198, slow_period=30 | 143 | -0.365 | -0.963 | -314.74 |

All 54 training variants of the 1h signal_line_only arm (unsuccessful variants included):

| params | clusters | mean ddadj | mean Sharpe | net PnL USD |
|---|---|---|---|---|
| fast_period=8, recursion_length=180, signal_period=5, slow_period=20 | 109 | -0.660 | -1.546 | -858.98 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=20 | 109 | -0.660 | -1.546 | -858.98 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=20 | 122 | -0.877 | -2.483 | -1282.56 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=20 | 122 | -0.877 | -2.483 | -1282.56 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=20 | 113 | -0.691 | -1.236 | -735.80 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=20 | 113 | -0.691 | -1.236 | -735.80 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=20 | 92 | -0.809 | -1.336 | -843.13 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=20 | 92 | -0.809 | -1.336 | -843.13 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=20 | 105 | -0.579 | -0.852 | -635.82 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=20 | 105 | -0.579 | -0.852 | -635.82 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=20 | 111 | -0.675 | -1.127 | -727.39 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=20 | 111 | -0.675 | -1.127 | -727.39 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=20 | 96 | -0.658 | -0.975 | -710.16 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=20 | 96 | -0.658 | -0.975 | -710.16 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=20 | 118 | -0.737 | -1.333 | -764.96 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=20 | 118 | -0.737 | -1.333 | -764.96 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=20 | 131 | -0.623 | -0.862 | -586.27 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=20 | 131 | -0.623 | -0.862 | -586.27 |
| fast_period=8, recursion_length=180, signal_period=5, slow_period=25 | 106 | -0.783 | -1.951 | -1072.20 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=25 | 106 | -0.783 | -1.951 | -1072.20 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=25 | 92 | -0.766 | -1.291 | -797.31 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=25 | 92 | -0.766 | -1.291 | -797.31 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=25 | 110 | -0.622 | -1.000 | -728.84 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=25 | 110 | -0.622 | -1.000 | -728.84 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=25 | 98 | -0.812 | -1.249 | -834.33 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=25 | 98 | -0.812 | -1.249 | -834.33 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=25 | 114 | -0.683 | -1.207 | -778.29 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=25 | 114 | -0.683 | -1.207 | -778.29 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=25 | 127 | -0.669 | -1.047 | -620.90 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=25 | 127 | -0.669 | -1.047 | -620.90 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=25 | 105 | -0.602 | -0.988 | -705.94 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=25 | 105 | -0.602 | -0.988 | -705.94 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=25 | 116 | -0.695 | -1.287 | -754.82 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=25 | 116 | -0.695 | -1.287 | -754.82 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=25 | 113 | -0.554 | -1.245 | -765.46 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=25 | 113 | -0.554 | -1.245 | -765.46 |
| fast_period=8, recursion_length=180, signal_period=5, slow_period=30 | 86 | -0.800 | -1.462 | -946.00 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=30 | 86 | -0.800 | -1.462 | -946.00 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=30 | 100 | -0.776 | -1.267 | -814.83 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=30 | 100 | -0.776 | -1.267 | -814.83 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=30 | 109 | -0.735 | -1.204 | -788.18 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=30 | 109 | -0.735 | -1.204 | -788.18 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=30 | 105 | -0.620 | -1.129 | -791.76 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=30 | 105 | -0.620 | -1.129 | -791.59 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=30 | 113 | -0.760 | -1.287 | -732.21 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=30 | 113 | -0.760 | -1.287 | -732.21 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=30 | 121 | -0.751 | -1.378 | -822.60 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=30 | 121 | -0.751 | -1.378 | -822.60 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=30 | 117 | -0.739 | -1.091 | -698.42 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=30 | 117 | -0.739 | -1.091 | -698.42 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=30 | 118 | -0.798 | -1.290 | -759.69 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=30 | 118 | -0.798 | -1.290 | -759.71 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=30 | 116 | -0.716 | -1.629 | -850.83 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=30 | 116 | -0.716 | -1.629 | -850.83 |

- 1h momentum_pro: 243 training variants, 243 with at least 10 clusters, mean ddadj range -0.915 to 1.352 (every row is in `results.json`).
- 1h vol_momentum: 81 training variants, 69 with at least 10 clusters, mean ddadj range -0.952 to 0.275 (every row is in `results.json`).
- 1h trix_momentum: 18 training variants, 18 with at least 10 clusters, mean ddadj range -0.975 to -0.445 (every row is in `results.json`).

Training neighbours of the 4h selection:

| params | mean ddadj | mean Sharpe | clusters |
|---|---|---|---|
| fast_period=8, recursion_length=180, signal_period=9, slow_period=30 | 1.580 | 0.821 | 56 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=25 | 1.412 | 0.794 | 56 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=30 | 0.368 | 0.343 | 44 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=30 | 1.031 | 0.514 | 57 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=30 | 1.580 | 0.821 | 56 |

| held-out params | 2023H2 | 2024 | 2025H1a | oos |
|---|---|---|---|---|
| fast_period=13, recursion_length=180, signal_period=7, slow_period=25 | 85.80 USD | -236.31 USD | -571.47 USD | 149.63 USD |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=30 | 239.96 USD | -241.62 USD | -349.52 USD | 274.20 USD |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=30 | 205.51 USD | -141.71 USD | -595.06 USD | 11.87 USD |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=25 | 214.96 USD | -412.60 USD | -613.97 USD | -28.97 USD |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=30 | -3.49 USD | -402.57 USD | -519.69 USD | 116.78 USD |

All 54 training variants of the 4h candidate arm (unsuccessful variants included):

| params | clusters | mean ddadj | mean Sharpe | net PnL USD |
|---|---|---|---|---|
| fast_period=8, recursion_length=180, signal_period=5, slow_period=20 | 68 | 0.355 | 0.316 | 167.88 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=20 | 68 | 0.355 | 0.316 | 167.88 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=20 | 60 | 0.080 | 0.075 | 17.83 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=20 | 60 | 0.080 | 0.075 | 17.83 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=20 | 57 | 0.284 | 0.311 | 115.46 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=20 | 57 | 0.284 | 0.311 | 115.46 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=20 | 60 | 0.117 | -0.009 | 4.13 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=20 | 60 | 0.117 | -0.009 | 4.13 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=20 | 56 | 0.924 | 0.746 | 319.56 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=20 | 56 | 0.924 | 0.746 | 319.56 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=20 | 51 | 1.391 | 0.880 | 335.60 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=20 | 51 | 1.391 | 0.880 | 335.60 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=20 | 58 | 1.268 | 0.668 | 359.15 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=20 | 58 | 1.268 | 0.668 | 359.15 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=20 | 45 | 0.609 | 0.580 | 183.48 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=20 | 45 | 0.609 | 0.580 | 183.48 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=20 | 46 | 0.347 | 0.340 | 96.49 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=20 | 46 | 0.347 | 0.340 | 96.49 |
| fast_period=8, recursion_length=180, signal_period=5, slow_period=25 | 67 | 0.508 | 0.344 | 176.67 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=25 | 67 | 0.508 | 0.344 | 176.67 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=25 | 62 | 0.969 | 0.300 | 163.57 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=25 | 62 | 0.969 | 0.300 | 163.57 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=25 | 56 | 1.412 | 0.794 | 400.89 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=25 | 56 | 1.412 | 0.794 | 400.89 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=25 | 57 | 1.255 | 0.774 | 374.71 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=25 | 57 | 1.255 | 0.774 | 374.71 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=25 | 54 | 0.227 | 0.305 | 105.02 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=25 | 54 | 0.227 | 0.305 | 105.02 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=25 | 42 | 1.333 | 0.904 | 348.95 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=25 | 42 | 1.333 | 0.904 | 348.95 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=25 | 47 | 0.728 | 0.600 | 224.46 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=25 | 47 | 0.728 | 0.600 | 224.46 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=25 | 42 | 0.432 | 0.381 | 120.25 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=25 | 42 | 0.432 | 0.381 | 120.25 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=25 | 42 | 0.332 | 0.445 | 137.64 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=25 | 42 | 0.332 | 0.445 | 137.64 |
| fast_period=8, recursion_length=180, signal_period=5, slow_period=30 | 68 | 0.512 | 0.085 | 29.81 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=30 | 68 | 0.512 | 0.085 | 29.81 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=30 | 57 | 1.031 | 0.514 | 300.33 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=30 | 57 | 1.031 | 0.514 | 300.33 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=30 | 56 | 1.580 | 0.821 | 396.91 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=30 | 56 | 1.580 | 0.821 | 396.91 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=30 | 59 | 0.867 | 0.613 | 285.23 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=30 | 59 | 0.867 | 0.613 | 285.23 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=30 | 45 | 0.809 | 0.671 | 229.40 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=30 | 45 | 0.809 | 0.671 | 229.40 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=30 | 44 | 0.368 | 0.343 | 117.51 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=30 | 44 | 0.368 | 0.343 | 117.51 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=30 | 47 | 1.013 | 0.572 | 264.94 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=30 | 47 | 0.995 | 0.559 | 258.50 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=30 | 43 | 1.364 | 0.759 | 294.68 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=30 | 43 | 1.364 | 0.759 | 294.68 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=30 | 41 | 1.075 | 0.945 | 332.84 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=30 | 41 | 1.075 | 0.945 | 332.84 |

All 18 training variants of the 4h centerline_only arm (unsuccessful variants included):

| params | clusters | mean ddadj | mean Sharpe | net PnL USD |
|---|---|---|---|---|
| fast_period=8, recursion_length=180, slow_period=20 | 77 | -0.806 | -1.748 | -801.40 |
| fast_period=8, recursion_length=198, slow_period=20 | 77 | -0.806 | -1.748 | -801.40 |
| fast_period=13, recursion_length=180, slow_period=20 | 61 | -0.761 | -1.453 | -618.17 |
| fast_period=13, recursion_length=198, slow_period=20 | 61 | -0.761 | -1.453 | -618.17 |
| fast_period=18, recursion_length=180, slow_period=20 | 54 | -0.769 | -1.671 | -559.27 |
| fast_period=18, recursion_length=198, slow_period=20 | 54 | -0.769 | -1.671 | -559.27 |
| fast_period=8, recursion_length=180, slow_period=25 | 67 | -0.858 | -1.846 | -823.72 |
| fast_period=8, recursion_length=198, slow_period=25 | 67 | -0.858 | -1.846 | -823.72 |
| fast_period=13, recursion_length=180, slow_period=25 | 54 | -0.736 | -1.403 | -518.30 |
| fast_period=13, recursion_length=198, slow_period=25 | 54 | -0.736 | -1.403 | -518.30 |
| fast_period=18, recursion_length=180, slow_period=25 | 60 | -0.861 | -1.806 | -573.98 |
| fast_period=18, recursion_length=198, slow_period=25 | 60 | -0.861 | -1.806 | -573.98 |
| fast_period=8, recursion_length=180, slow_period=30 | 59 | -0.838 | -1.365 | -586.65 |
| fast_period=8, recursion_length=198, slow_period=30 | 59 | -0.838 | -1.365 | -586.65 |
| fast_period=13, recursion_length=180, slow_period=30 | 58 | -0.543 | -1.255 | -447.64 |
| fast_period=13, recursion_length=198, slow_period=30 | 58 | -0.543 | -1.255 | -447.64 |
| fast_period=18, recursion_length=180, slow_period=30 | 48 | -0.286 | -0.467 | -156.58 |
| fast_period=18, recursion_length=198, slow_period=30 | 48 | -0.286 | -0.467 | -156.58 |

All 54 training variants of the 4h signal_line_only arm (unsuccessful variants included):

| params | clusters | mean ddadj | mean Sharpe | net PnL USD |
|---|---|---|---|---|
| fast_period=8, recursion_length=180, signal_period=5, slow_period=20 | 47 | 0.582 | 0.417 | 310.92 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=20 | 47 | 0.582 | 0.417 | 310.92 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=20 | 53 | 0.315 | 0.319 | 167.86 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=20 | 53 | 0.315 | 0.319 | 167.86 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=20 | 67 | 0.003 | -0.431 | -160.61 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=20 | 67 | 0.003 | -0.431 | -160.61 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=20 | 61 | 0.031 | -0.224 | -60.18 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=20 | 61 | 0.031 | -0.224 | -60.18 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=20 | 72 | 0.234 | -0.354 | -31.90 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=20 | 72 | 0.234 | -0.354 | -31.90 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=20 | 56 | 0.322 | 0.200 | 168.95 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=20 | 56 | 0.322 | 0.200 | 168.95 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=20 | 68 | 0.518 | 0.013 | 128.86 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=20 | 68 | 0.518 | 0.013 | 128.86 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=20 | 51 | 0.460 | 0.191 | 216.98 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=20 | 51 | 0.460 | 0.191 | 216.98 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=20 | 64 | 0.271 | 0.061 | 102.13 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=20 | 64 | 0.271 | 0.061 | 102.13 |
| fast_period=8, recursion_length=180, signal_period=5, slow_period=25 | 52 | 0.338 | 0.253 | 188.82 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=25 | 52 | 0.338 | 0.253 | 188.82 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=25 | 66 | -0.003 | -0.416 | -146.63 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=25 | 66 | -0.003 | -0.416 | -146.63 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=25 | 71 | 0.561 | -0.333 | 57.10 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=25 | 71 | 0.561 | -0.333 | 57.10 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=25 | 68 | 0.316 | -0.173 | 29.62 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=25 | 68 | 0.316 | -0.173 | 29.62 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=25 | 59 | 0.147 | -0.130 | 10.43 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=25 | 59 | 0.147 | -0.130 | 10.43 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=25 | 55 | 0.459 | -0.111 | 143.80 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=25 | 55 | 0.459 | -0.111 | 143.80 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=25 | 56 | 0.665 | 0.258 | 312.78 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=25 | 56 | 0.665 | 0.258 | 312.78 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=25 | 56 | 0.324 | 0.214 | 194.66 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=25 | 56 | 0.324 | 0.214 | 194.66 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=25 | 59 | 0.379 | 0.239 | 243.37 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=25 | 59 | 0.379 | 0.239 | 243.37 |
| fast_period=8, recursion_length=180, signal_period=5, slow_period=30 | 55 | 1.223 | 0.477 | 396.14 |
| fast_period=8, recursion_length=198, signal_period=5, slow_period=30 | 55 | 1.223 | 0.477 | 396.14 |
| fast_period=8, recursion_length=180, signal_period=7, slow_period=30 | 69 | 0.059 | -0.494 | -118.89 |
| fast_period=8, recursion_length=198, signal_period=7, slow_period=30 | 69 | 0.059 | -0.494 | -118.89 |
| fast_period=8, recursion_length=180, signal_period=9, slow_period=30 | 65 | 0.505 | -0.005 | 139.81 |
| fast_period=8, recursion_length=198, signal_period=9, slow_period=30 | 65 | 0.505 | -0.005 | 139.81 |
| fast_period=13, recursion_length=180, signal_period=5, slow_period=30 | 61 | 0.527 | 0.126 | 163.55 |
| fast_period=13, recursion_length=198, signal_period=5, slow_period=30 | 61 | 0.527 | 0.126 | 163.55 |
| fast_period=13, recursion_length=180, signal_period=7, slow_period=30 | 58 | 0.247 | -0.022 | 78.94 |
| fast_period=13, recursion_length=198, signal_period=7, slow_period=30 | 58 | 0.247 | -0.022 | 78.94 |
| fast_period=13, recursion_length=180, signal_period=9, slow_period=30 | 54 | 0.634 | 0.433 | 385.58 |
| fast_period=13, recursion_length=198, signal_period=9, slow_period=30 | 54 | 0.634 | 0.433 | 385.58 |
| fast_period=18, recursion_length=180, signal_period=5, slow_period=30 | 51 | 0.996 | 0.651 | 463.52 |
| fast_period=18, recursion_length=198, signal_period=5, slow_period=30 | 51 | 0.981 | 0.642 | 455.54 |
| fast_period=18, recursion_length=180, signal_period=7, slow_period=30 | 61 | 0.793 | 0.631 | 413.85 |
| fast_period=18, recursion_length=198, signal_period=7, slow_period=30 | 61 | 0.793 | 0.631 | 413.85 |
| fast_period=18, recursion_length=180, signal_period=9, slow_period=30 | 59 | 1.090 | 0.731 | 434.95 |
| fast_period=18, recursion_length=198, signal_period=9, slow_period=30 | 59 | 1.090 | 0.731 | 434.95 |

- 4h momentum_pro: 243 training variants, 243 with at least 10 clusters, mean ddadj range -0.687 to 0.696 (every row is in `results.json`).
- 4h vol_momentum: 81 training variants, 54 with at least 10 clusters, mean ddadj range -0.855 to 1.284 (every row is in `results.json`).
- 4h trix_momentum: 18 training variants, 18 with at least 10 clusters, mean ddadj range -0.420 to 2.109 (every row is in `results.json`).

## Integration parity

Close capability on Hyperliquid perps: yes. `parity_diff` on the explicit-paper configurations (`paper_parity_1h.json`, `paper_parity_4h.json`; frozen BTC candles): clean = yes. Window `support` is exactly W + 1 bars. The generic paper-batch slot does not enable closed-bar decisions; the timed closed-bar check coverage is in `shared_scripts/test_true_strength_index_checks.py`.

| config | params | batched | window | bars | mismatches | batch mismatches | long / short entries | long / short closes | clean |
|---|---|---|---|---|---|---|---|---|---|
| paper_parity_1h.json | seed | no | 200 | 2000 | 0 | - | 30 / 29 | 17 / 9 | yes |
| paper_parity_1h.json | seed | no | full | 2170 | 0 | - | 30 / 30 | 17 / 9 | yes |
| paper_parity_1h.json | seed | no | 181 | 2019 | 0 | - | 30 / 30 | 17 / 9 | yes |
| paper_parity_1h.json | seed | yes | 200 | 2000 | 0 | 0.00 | 30 / 29 | 17 / 9 | yes |
| paper_parity_1h.json | seed | yes | full | 2170 | 0 | 0.00 | 30 / 30 | 17 / 9 | yes |
| paper_parity_1h.json | seed | yes | 181 | 2019 | 0 | 0.00 | 30 / 30 | 17 / 9 | yes |
| paper_parity_1h.json | selected | no | 200 | 2000 | 0 | - | 24 / 18 | 16 / 9 | yes |
| paper_parity_1h.json | selected | no | full | 2170 | 0 | - | 24 / 19 | 16 / 9 | yes |
| paper_parity_1h.json | selected | no | 181 | 2019 | 0 | - | 24 / 19 | 16 / 9 | yes |
| paper_parity_1h.json | selected | yes | 200 | 2000 | 0 | 0.00 | 24 / 18 | 16 / 9 | yes |
| paper_parity_1h.json | selected | yes | full | 2170 | 0 | 0.00 | 24 / 19 | 16 / 9 | yes |
| paper_parity_1h.json | selected | yes | 181 | 2019 | 0 | 0.00 | 24 / 19 | 16 / 9 | yes |
| paper_parity_4h.json | seed | no | 200 | 1989 | 0 | - | 23 / 25 | 20 / 29 | yes |
| paper_parity_4h.json | seed | no | full | 2159 | 0 | - | 23 / 26 | 20 / 29 | yes |
| paper_parity_4h.json | seed | no | 181 | 2008 | 0 | - | 23 / 26 | 20 / 29 | yes |
| paper_parity_4h.json | seed | yes | 200 | 1989 | 0 | 0.00 | 23 / 25 | 20 / 29 | yes |
| paper_parity_4h.json | seed | yes | full | 2159 | 0 | 0.00 | 23 / 26 | 20 / 29 | yes |
| paper_parity_4h.json | seed | yes | 181 | 2008 | 0 | 0.00 | 23 / 26 | 20 / 29 | yes |
| paper_parity_4h.json | selected | no | 200 | 1989 | 0 | - | 25 / 27 | 20 / 30 | yes |
| paper_parity_4h.json | selected | no | full | 2159 | 0 | - | 25 / 28 | 20 / 30 | yes |
| paper_parity_4h.json | selected | no | 181 | 2008 | 0 | - | 25 / 28 | 20 / 30 | yes |
| paper_parity_4h.json | selected | yes | 200 | 1989 | 0 | 0.00 | 25 / 27 | 20 / 30 | yes |
| paper_parity_4h.json | selected | yes | full | 2159 | 0 | 0.00 | 25 / 28 | 20 / 30 | yes |
| paper_parity_4h.json | selected | yes | 181 | 2008 | 0 | 0.00 | 25 / 28 | 20 / 30 | yes |

## Limitations

- Binance US spot candles stand in for Hyperliquid perpetual prices; proxy results cannot establish Hyperliquid profitability or venue execution parity.
- Costs are the Backtester Hyperliquid taker fee (0.045% per side) and a flat 0.05% slippage per fill; the spread is not modeled separately and there is no lot-size or venue-minimum model on this path.
- Funding is charged from frozen Hyperliquid rates (--funding charge); a leg with unpriced held hours is incomplete and cannot count as economic evidence.
- No venue execution manifest is used; the frozen offline manifest path (fees, spread, slippage, lots and minimums) is available in eval_windows.py but this study does not include venue evidence.
- No regime filter and no higher-timeframe filter are used.
- The held-out windows are retrospective; they are not forward live evidence.
- A pass would not authorize live use: live use still needs `allow_no_edge: true` and a separate reviewed edge decision.
