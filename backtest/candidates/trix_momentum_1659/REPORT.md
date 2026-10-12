# trix_momentum study (issue 1659)

**Verdict: FAIL.** 1h: fail (pass condition false: candidate_both_net_positive_base, candidate_both_net_positive_stress, candidate_long_net_positive_base, candidate_long_net_positive_stress); 4h: fail (pass condition false: candidate_both_net_positive_base, candidate_both_net_positive_stress, candidate_long_beats_tema_cross_ddadj, candidate_long_beats_tema_cross_sharpe, candidate_long_net_positive_base, candidate_long_net_positive_stress).

The Python registry and the Go mirror keep `edge_status="no_edge"` with `edge_source="study_fail"` and this file as `edge_ref`. No study result removes the no-edge label: paper evaluation needs exactly one explicit `--mode=paper`, and live use still needs `allow_no_edge: true`, which is an operator decision and not evidence of profit. Nothing here edits deployed configuration, live defaults or acknowledgements; the study is suggest-only.

## Candidate

Entry-only triple-smoothed momentum crossover (`shared_strategies/open/trix_momentum.py`). For each endpoint t the evaluator takes the L + 1 closes t-L..t, seeds three chained EMAs (weight 2 / (period + 1)) with close t-L and updates them in order for each later close. The local oscillator is 100 x (third EMA j - third EMA j-1) / third EMA j-1. The signal line is an EMA (weight 2 / (signal_period + 1)) of the local oscillator, seeded with its first value. Endpoint t-1 is computed from its own run t-1-L..t-1, so a decision reads L + 2 bars. Long when oscillator - signal line > 0 at t, <= 0 at t-1, and the oscillator at t > 0; short is the exact mirror. Bad candles, timestamp defects, missing cadence, cadence gaps, an invalid denominator and non-finite derived values inside the window hold the entry with a reason; the close owner and stop keep running.

Close owner: `tiered_tp_pct` (`{"tp_tiers": [{"profit_pct": 0.01, "close_fraction": 0.25}, {"profit_pct": 0.02, "close_fraction": 0.5}, {"profit_pct": 0.03, "close_fraction": 0.75}, {"profit_pct": 0.04, "close_fraction": 1.0}]}`); stop owner: `stop_loss_atr_mult=1.0`. Seed: period=18, signal_period=9, recursion_length=120. Parameter rules: integers, period >= 1, signal_period >= 1, signal_period < L, L >= 6 x period, L <= 197.

## Protocol, provenance and invocations

- Protocol `protocol.json` sha256 `5fcbba6e07839a590dde0a181a02968033c906405f5eefa6646dfb92359b77c3`, frozen before scoring; `study_manifest.json` (sha256 `e7c98dbd9a6589f640fd39eb7f80a2aee357439cb20608b5c0d5c45de442d9d3`) records the same hash at freeze time (2026-10-12T13:15:30Z, commit `7275d441eb3e0b2aa677acca776a949fabc9dc0f+uncommitted-changes`).
- Run source commit `217ce148e464d01343bae8c2b05210fa1b07e992`; runtime 651.0 s. Source file sha256:
  - `backtest/backtester.py`: `e8233491bb19e59ae80115cabeea5644cdd0d6cebbf589c465d6c79f2941b8e0`
  - `backtest/candidates/trix_momentum_1659/run_study.py`: `01a66f65af09408c96dab46cbc6a7da694b7a843f1c63ec657f2396e5f0a351e`
  - `backtest/eval_windows.py`: `f6c1e6e8b854f115e43acdf95c14078dd5c4a91ae1419e9b4a5da185613aea23`
  - `backtest/parity_diff.py`: `28b9f1bc415838c24f232febe9ff91903c16c74d163e868a63ac721e9439c549`
  - `shared_strategies/close/tiered_tp_pct.py`: `f6a480628c37f0da329336aa5efaeacd3dced5fe73622ccfc4ee47a16e07daa6`
  - `shared_strategies/open/registry.py`: `4ad1a272266e511174cfdc6cde70bbef1e9636f21a437f906511482cacc9028e`
  - `shared_strategies/open/trix_momentum.py`: `d9775bbf83c517d5b2d799f054fa79c1e9115fb5af2df953a8d72116a776925f`
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
  - `uv run --no-sync python backtest/candidates/trix_momentum_1659/run_study.py freeze`
  - `uv run --no-sync python backtest/candidates/trix_momentum_1659/run_study.py run --cache-db <new file>`
  - `uv run --no-sync python backtest/candidates/trix_momentum_1659/run_study.py render`
- Every leg calls `eval_windows.run_leg` with `registry=futures`, the arm's declared direction, the frozen close and stop owner, `funding_mode=charge` (the `--funding charge` path), `intrabar_resolution=ohlc_walk` and `warmup_bars=300`.

Windows (finite, disjoint):

| window | start | end | role |
|---|---|---|---|
| is | 2025-06-10 | 2026-01-01 | training |
| 2023H2 | 2023-07-01 | 2024-01-01 | held-out |
| 2024 | 2024-01-01 | 2025-01-01 | held-out |
| 2025H1a | 2025-01-01 | 2025-06-10 | held-out |
| oos | 2026-01-01 | 2026-10-01 | held-out |

Study-local finite copies of the M1 windows in backtest/eval_windows.py WINDOWS; the shared M1 defaults are unchanged. is is the M1 training window. oos is the open-ended M1 window pinned to end 2026-10-01 (the frozen data end). 2023H2 replaces the M1 2023 window: the frozen candles start 2023-01-01, so 2023-01-01 has no 300-bar warmup, and Hyperliquid funding starts 2023-05-12, so held hours before it are unpriced. 2025H1a is the M1 2025H1 window cut at 2025-06-10 so it does not overlap the is training window. The five windows are disjoint. The candles were scored before by studies chande_momentum_1661, adaptive_moving_average_1641 and dual_thrust_1642 for other strategies; this candidate was never scored on them, but the held-out windows are retrospective data, not live forward evidence.

## Verdict gates

### 1h: fail

| gate | value |
|---|---|
| all_windows_scorable | yes |
| both_sides | yes |
| candidate_clusters | yes |
| close_capability | yes |
| integration_parity | yes |
| selections_not_fallback | yes |
| candidate_both_net_positive_base | no |
| candidate_both_net_positive_stress | no |
| candidate_long_beats_macd_ddadj | yes |
| candidate_long_beats_macd_sharpe | yes |
| candidate_long_beats_tema_cross_ddadj | yes |
| candidate_long_beats_tema_cross_sharpe | yes |
| candidate_long_net_positive_base | no |
| candidate_long_net_positive_stress | no |

| arm | clusters | positions | long/short | net PnL USD | net x2 slippage | mean Sharpe | mean ddadj | worst max DD % | fees USD | funding USD | turnover |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate_both | 597 | 992 | 514/478 | -392.58 | -1474.19 | -0.292 | 0.203 | 29.53 | 869.42 | -55.22 | 1931.60 |
| candidate_long | 332 | 523 | 523/0 | -246.03 | -861.14 | -0.219 | 0.175 | 19.82 | 463.30 | -91.14 | 1029.33 |
| macd | 768 | 1934 | 1934/0 | -2652.24 | - | -1.383 | -0.616 | 53.40 | 1501.70 | -250.34 | 3336.36 |
| tema_cross | 495 | 813 | 813/0 | -792.34 | - | -0.733 | -0.366 | 27.31 | 706.05 | -125.74 | 1568.64 |

Unscorable windows: none. Largest candidate_both position share of positive position PnL: 0.0160 (descriptive). M1 incumbent-relative passes for candidate_both: 1 of 4 (descriptive).

### 4h: fail

| gate | value |
|---|---|
| all_windows_scorable | yes |
| both_sides | yes |
| candidate_clusters | yes |
| close_capability | yes |
| integration_parity | yes |
| selections_not_fallback | yes |
| candidate_both_net_positive_base | no |
| candidate_both_net_positive_stress | no |
| candidate_long_beats_macd_ddadj | yes |
| candidate_long_beats_macd_sharpe | yes |
| candidate_long_beats_tema_cross_ddadj | no |
| candidate_long_beats_tema_cross_sharpe | no |
| candidate_long_net_positive_base | no |
| candidate_long_net_positive_stress | no |

| arm | clusters | positions | long/short | net PnL USD | net x2 slippage | mean Sharpe | mean ddadj | worst max DD % | fees USD | funding USD | turnover |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate_both | 188 | 293 | 157/136 | -319.75 | -742.39 | -0.170 | 0.243 | 28.94 | 259.30 | -52.50 | 576.10 |
| candidate_long | 104 | 144 | 144/0 | -209.79 | -365.02 | -0.370 | -0.106 | 16.99 | 130.62 | -72.47 | 290.21 |
| macd | 282 | 639 | 639/0 | -1739.89 | - | -0.867 | -0.557 | 36.06 | 530.48 | -187.92 | 1178.59 |
| tema_cross | 157 | 253 | 253/0 | 197.44 | - | 0.162 | 0.320 | 22.68 | 232.69 | -125.64 | 516.97 |

Unscorable windows: none. Largest candidate_both position share of positive position PnL: 0.0306 (descriptive). M1 incumbent-relative passes for candidate_both: 0 of 4 (descriptive).

## Selections (training window `is`)

| group | arm | how | params | eligible rows | clusters | mean ddadj | mean Sharpe | seed rank |
|---|---|---|---|---|---|---|---|---|
| 1h | candidate_both | selected | period=12, recursion_length=120, signal_period=12 | 18 | 120.00 | -0.445 | -0.825 | 13.00 |
| 1h | candidate_long | selected | period=12, recursion_length=120, signal_period=12 | 18 | 68.00 | -0.647 | -1.208 | 10.00 |
| 1h | macd | selected | fast_period=12, signal_period=12, slow_period=34 | 27 | 128.00 | -0.619 | -1.189 | - |
| 1h | tema_cross | selected | long_period=25, mid_period=10, short_period=8 | 27 | 91.00 | 0.883 | 0.162 | - |
| 4h | candidate_both | selected | period=12, recursion_length=120, signal_period=6 | 18 | 32.00 | 2.109 | 1.451 | 16.00 |
| 4h | candidate_long | selected | period=12, recursion_length=120, signal_period=9 | 18 | 16.00 | 1.595 | 1.033 | 13.00 |
| 4h | macd | selected | fast_period=12, signal_period=9, slow_period=26 | 27 | 51.00 | 0.388 | 0.015 | - |
| 4h | tema_cross | selected | long_period=34, mid_period=10, short_period=5 | 27 | 37.00 | 0.468 | -0.496 | - |

## Held-out windows per arm

| group | window | arm | net PnL USD | clusters | positions | long/short | mean Sharpe | mean ddadj | M1 verdict | funding complete |
|---|---|---|---|---|---|---|---|---|---|---|
| 1h | 2023H2 | candidate_both | -245.76 | 105 | 179 | 93/86 | -0.855 | -0.195 | fail | yes |
| 1h | 2023H2 | candidate_long | -90.54 | 57 | 96 | 96/0 | -0.249 | 0.358 | fail | yes |
| 1h | 2023H2 | macd | -384.84 | 126 | 336 | 336/0 | -1.288 | -0.690 | fail | yes |
| 1h | 2023H2 | tema_cross | -123.00 | 98 | 156 | 156/0 | -1.275 | -0.306 | fail | yes |
| 1h | 2024 | candidate_both | -37.29 | 232 | 380 | 200/180 | -0.040 | 0.616 | fail | yes |
| 1h | 2024 | candidate_long | 23.55 | 134 | 202 | 202/0 | 0.157 | 0.081 | fail | yes |
| 1h | 2024 | macd | -796.85 | 297 | 734 | 734/0 | -0.986 | -0.756 | fail | yes |
| 1h | 2024 | tema_cross | -263.59 | 203 | 328 | 328/0 | -0.389 | -0.413 | fail | yes |
| 1h | 2025H1a | candidate_both | 379.91 | 101 | 168 | 86/82 | 0.973 | 1.081 | pass | yes |
| 1h | 2025H1a | candidate_long | 117.43 | 56 | 87 | 87/0 | 0.261 | 0.769 | pass | yes |
| 1h | 2025H1a | macd | -78.72 | 127 | 324 | 324/0 | 0.033 | -0.066 | pass | yes |
| 1h | 2025H1a | tema_cross | -71.00 | 72 | 116 | 116/0 | -0.205 | -0.241 | pass | yes |
| 1h | oos | candidate_both | -489.44 | 159 | 265 | 135/130 | -1.246 | -0.693 | fail | yes |
| 1h | oos | candidate_long | -296.47 | 85 | 138 | 138/0 | -1.044 | -0.507 | fail | yes |
| 1h | oos | macd | -1391.83 | 218 | 540 | 540/0 | -3.293 | -0.953 | fail | yes |
| 1h | oos | tema_cross | -334.75 | 122 | 213 | 213/0 | -1.065 | -0.504 | fail | yes |
| 4h | 2023H2 | candidate_both | 336.84 | 31 | 52 | 37/15 | 0.875 | 1.866 | fail | yes |
| 4h | 2023H2 | candidate_long | 1.30 | 21 | 31 | 31/0 | -0.307 | 0.258 | fail | yes |
| 4h | 2023H2 | macd | -219.53 | 41 | 110 | 110/0 | -0.482 | -0.477 | fail | yes |
| 4h | 2023H2 | tema_cross | 144.05 | 26 | 43 | 43/0 | 0.524 | 0.564 | fail | yes |
| 4h | 2024 | candidate_both | -504.37 | 73 | 113 | 64/49 | -0.947 | -0.794 | fail | yes |
| 4h | 2024 | candidate_long | -130.45 | 45 | 57 | 57/0 | -0.331 | -0.370 | fail | yes |
| 4h | 2024 | macd | -632.41 | 122 | 256 | 256/0 | -0.798 | -0.623 | fail | yes |
| 4h | 2024 | tema_cross | 111.51 | 65 | 96 | 96/0 | 0.220 | 0.696 | fail | yes |
| 4h | 2025H1a | candidate_both | -100.36 | 37 | 49 | 21/28 | -0.462 | -0.032 | fail | yes |
| 4h | 2025H1a | candidate_long | -80.84 | 16 | 21 | 21/0 | -0.767 | -0.250 | fail | yes |
| 4h | 2025H1a | macd | -629.57 | 49 | 99 | 99/0 | -1.782 | -0.670 | fail | yes |
| 4h | 2025H1a | tema_cross | -64.29 | 24 | 43 | 43/0 | -0.326 | -0.170 | pass | yes |
| 4h | oos | candidate_both | -51.86 | 47 | 79 | 35/44 | -0.146 | -0.068 | fail | yes |
| 4h | oos | candidate_long | 0.20 | 22 | 35 | 35/0 | -0.077 | -0.062 | fail | yes |
| 4h | oos | macd | -258.38 | 70 | 174 | 174/0 | -0.405 | -0.459 | fail | yes |
| 4h | oos | tema_cross | 6.17 | 42 | 71 | 71/0 | 0.230 | 0.189 | fail | yes |

## Funding coverage (candidate_both held-out legs)

| group | window | dataset | expected hours | priced hours | unpriced held hours | funding USD |
|---|---|---|---|---|---|---|
| 1h | 2023H2 | BTC/USDT 1h | 4415 | 4413 | 0 | -9.48 |
| 1h | 2023H2 | ETH/USDT 1h | 4415 | 4413 | 0 | -2.56 |
| 1h | 2023H2 | SOL/USDT 1h | 4415 | 4413 | 0 | -3.89 |
| 1h | 2024 | BTC/USDT 1h | 8783 | 8782 | 0 | -12.09 |
| 1h | 2024 | ETH/USDT 1h | 8783 | 8782 | 0 | -7.34 |
| 1h | 2024 | SOL/USDT 1h | 8783 | 8782 | 0 | -11.41 |
| 1h | 2025H1a | BTC/USDT 1h | 3839 | 3839 | 0 | 0.04 |
| 1h | 2025H1a | ETH/USDT 1h | 3839 | 3839 | 0 | -3.00 |
| 1h | 2025H1a | SOL/USDT 1h | 3839 | 3839 | 0 | -1.15 |
| 1h | oos | BTC/USDT 1h | 6551 | 6551 | 0 | 1.07 |
| 1h | oos | ETH/USDT 1h | 6551 | 6551 | 0 | -1.64 |
| 1h | oos | SOL/USDT 1h | 6551 | 6551 | 0 | -3.75 |
| 4h | 2023H2 | BTC/USDT 4h | 4412 | 4410 | 0 | -9.97 |
| 4h | 2023H2 | ETH/USDT 4h | 4412 | 4410 | 0 | -4.23 |
| 4h | 2023H2 | SOL/USDT 4h | 4412 | 4410 | 0 | -8.64 |
| 4h | 2024 | BTC/USDT 4h | 8780 | 8779 | 0 | -10.14 |
| 4h | 2024 | ETH/USDT 4h | 8780 | 8779 | 0 | -10.82 |
| 4h | 2024 | SOL/USDT 4h | 8780 | 8779 | 0 | -6.33 |
| 4h | 2025H1a | BTC/USDT 4h | 3836 | 3836 | 0 | -0.90 |
| 4h | 2025H1a | ETH/USDT 4h | 3836 | 3836 | 0 | -0.70 |
| 4h | 2025H1a | SOL/USDT 4h | 3836 | 3836 | 0 | 0.28 |
| 4h | oos | BTC/USDT 4h | 6548 | 6548 | 0 | 1.24 |
| 4h | oos | ETH/USDT 4h | 6548 | 6548 | 0 | -0.31 |
| 4h | oos | SOL/USDT 4h | 6548 | 6548 | 0 | -1.97 |

## Fills, exits and entry evidence

- 1h: 992 candidate_both held-out positions checked for a bar N signal filled at the bar N+1 open, 0 mismatches; 339 positions closed in more than one part; exit reasons end_of_data, sl, tiered_tp_pct. `evidence/entries_1h.csv.gz` holds 1070 entry rows (sha256 `1c544ea87fcedf9b0811f3efcdbfa53bb5af5677e5c13762d60c4d5459e77097`); 0 rows failed the exact L + 2 row replay (the replay also proves L + 1 rows hold with insufficient_history).
- 4h: 293 candidate_both held-out positions checked for a bar N signal filled at the bar N+1 open, 0 mismatches; 116 positions closed in more than one part; exit reasons end_of_data, sl, tiered_tp_pct. `evidence/entries_4h.csv.gz` holds 304 entry rows (sha256 `13990db176a95f1fe2ab23222650ab11989097c6801be8a532d03274223f77b7`); 0 rows failed the exact L + 2 row replay (the replay also proves L + 1 rows hold with insufficient_history).

## Continuous-history reference (descriptive)

continuous-history TRIX (all three EMAs and the signal line seeded once at the first bar of the warmed leg frame and never restarted); a descriptive reference only, it never makes a decision. The fixed-window values make every decision.

| group | window and dataset | bars | max abs oscillator diff | median | max abs signal-line diff | decision differences | fixed / continuous entries |
|---|---|---|---|---|---|---|---|
| 1h | 2023H2 BTC/USDT 1h | 4416 | 0.00000038 | 0.00000001 | 0.00000244 | 0 | 68 / 68 |
| 1h | 2023H2 ETH/USDT 1h | 4416 | 0.00000033 | 0.00000002 | 0.00000208 | 0 | 66 / 66 |
| 1h | 2023H2 SOL/USDT 1h | 4416 | 0.00000095 | 0.00000004 | 0.00000586 | 0 | 64 / 64 |
| 1h | 2024 BTC/USDT 1h | 8784 | 0.00000049 | 0.00000002 | 0.00000316 | 0 | 134 / 134 |
| 1h | 2024 ETH/USDT 1h | 8784 | 0.00000081 | 0.00000003 | 0.00000546 | 0 | 134 / 134 |
| 1h | 2024 SOL/USDT 1h | 8784 | 0.00000086 | 0.00000005 | 0.00000530 | 0 | 140 / 140 |
| 1h | 2025H1a BTC/USDT 1h | 3840 | 0.00000042 | 0.00000002 | 0.00000268 | 0 | 58 / 58 |
| 1h | 2025H1a ETH/USDT 1h | 3840 | 0.00000079 | 0.00000004 | 0.00000502 | 0 | 54 / 54 |
| 1h | 2025H1a SOL/USDT 1h | 3840 | 0.00000100 | 0.00000006 | 0.00000626 | 0 | 67 / 67 |
| 1h | oos BTC/USDT 1h | 6422 | 0.00000036 | 0.00000002 | 0.00000235 | 0 | 101 / 101 |
| 1h | oos ETH/USDT 1h | 6422 | 0.00000052 | 0.00000003 | 0.00000341 | 0 | 90 / 90 |
| 1h | oos SOL/USDT 1h | 6422 | 0.00000056 | 0.00000003 | 0.00000341 | 0 | 94 / 94 |
| 4h | 2023H2 BTC/USDT 4h | 1104 | 0.00000048 | 0.00000003 | 0.00000082 | 0 | 20 / 20 |
| 4h | 2023H2 ETH/USDT 4h | 1104 | 0.00000049 | 0.00000003 | 0.00000082 | 0 | 19 / 19 |
| 4h | 2023H2 SOL/USDT 4h | 1104 | 0.00000111 | 0.00000008 | 0.00000185 | 0 | 19 / 19 |
| 4h | 2024 BTC/USDT 4h | 2196 | 0.00000065 | 0.00000006 | 0.00000110 | 0 | 41 / 41 |
| 4h | 2024 ETH/USDT 4h | 2196 | 0.00000117 | 0.00000007 | 0.00000197 | 0 | 34 / 34 |
| 4h | 2024 SOL/USDT 4h | 2196 | 0.00000108 | 0.00000010 | 0.00000184 | 0 | 42 / 42 |
| 4h | 2025H1a BTC/USDT 4h | 960 | 0.00000050 | 0.00000005 | 0.00000084 | 0 | 17 / 17 |
| 4h | 2025H1a ETH/USDT 4h | 960 | 0.00000113 | 0.00000008 | 0.00000189 | 0 | 17 / 17 |
| 4h | 2025H1a SOL/USDT 4h | 960 | 0.00000125 | 0.00000011 | 0.00000210 | 0 | 16 / 16 |
| 4h | oos BTC/USDT 4h | 1515 | 0.00000074 | 0.00000005 | 0.00000122 | 0 | 22 / 22 |
| 4h | oos ETH/USDT 4h | 1515 | 0.00000090 | 0.00000006 | 0.00000150 | 0 | 29 / 29 |
| 4h | oos SOL/USDT 4h | 1515 | 0.00000093 | 0.00000007 | 0.00000155 | 0 | 28 / 28 |

## Parameter stability and variants (candidate_both)

Training neighbours of the 1h selection:

| params | mean ddadj | mean Sharpe | clusters |
|---|---|---|---|
| period=12, recursion_length=120, signal_period=12 | -0.445 | -0.825 | 120 |
| period=18, recursion_length=120, signal_period=12 | -0.975 | -2.603 | 100 |
| period=12, recursion_length=120, signal_period=9 | -0.726 | -1.468 | 129 |
| period=12, recursion_length=144, signal_period=12 | -0.445 | -0.825 | 120 |

| held-out params | 2023H2 | 2024 | 2025H1a | oos |
|---|---|---|---|---|
| period=12, recursion_length=120, signal_period=9 | -222.73 USD | -11.93 USD | 432.85 USD | -315.60 USD |
| period=12, recursion_length=144, signal_period=12 | -245.76 USD | -37.29 USD | 379.91 USD | -489.44 USD |
| period=18, recursion_length=120, signal_period=12 | -251.51 USD | 235.08 USD | -172.80 USD | -189.23 USD |
| period=18, recursion_length=120, signal_period=9 | -182.15 USD | -7.19 USD | -198.57 USD | -185.95 USD |

All 18 training variants of the 1h candidate_both arm (unsuccessful variants included):

| params | clusters | mean ddadj | mean Sharpe | net PnL USD |
|---|---|---|---|---|
| period=12, recursion_length=120, signal_period=6 | 128 | -0.709 | -1.099 | -389.95 |
| period=12, recursion_length=144, signal_period=6 | 128 | -0.709 | -1.099 | -389.95 |
| period=12, recursion_length=192, signal_period=6 | 128 | -0.709 | -1.099 | -389.95 |
| period=12, recursion_length=120, signal_period=9 | 129 | -0.726 | -1.468 | -485.26 |
| period=12, recursion_length=144, signal_period=9 | 129 | -0.726 | -1.468 | -485.26 |
| period=12, recursion_length=192, signal_period=9 | 129 | -0.726 | -1.468 | -485.26 |
| period=12, recursion_length=120, signal_period=12 | 120 | -0.445 | -0.825 | -259.54 |
| period=12, recursion_length=144, signal_period=12 | 120 | -0.445 | -0.825 | -259.54 |
| period=12, recursion_length=192, signal_period=12 | 120 | -0.445 | -0.825 | -259.54 |
| period=18, recursion_length=120, signal_period=6 | 115 | -0.761 | -1.294 | -360.02 |
| period=18, recursion_length=144, signal_period=6 | 116 | -0.778 | -1.326 | -367.55 |
| period=18, recursion_length=192, signal_period=6 | 116 | -0.778 | -1.326 | -367.55 |
| period=18, recursion_length=120, signal_period=9 | 106 | -0.937 | -2.362 | -586.72 |
| period=18, recursion_length=144, signal_period=9 | 106 | -0.937 | -2.362 | -586.72 |
| period=18, recursion_length=192, signal_period=9 | 106 | -0.937 | -2.362 | -586.72 |
| period=18, recursion_length=120, signal_period=12 | 100 | -0.975 | -2.603 | -579.81 |
| period=18, recursion_length=144, signal_period=12 | 100 | -0.975 | -2.584 | -578.87 |
| period=18, recursion_length=192, signal_period=12 | 100 | -0.975 | -2.584 | -578.87 |

Training neighbours of the 4h selection:

| params | mean ddadj | mean Sharpe | clusters |
|---|---|---|---|
| period=12, recursion_length=120, signal_period=6 | 2.109 | 1.451 | 32 |
| period=18, recursion_length=120, signal_period=6 | 0.417 | 0.176 | 29 |
| period=12, recursion_length=120, signal_period=9 | 1.386 | 1.028 | 31 |
| period=12, recursion_length=144, signal_period=6 | 2.109 | 1.451 | 32 |

| held-out params | 2023H2 | 2024 | 2025H1a | oos |
|---|---|---|---|---|
| period=12, recursion_length=120, signal_period=9 | 16.41 USD | -124.55 USD | -175.75 USD | 12.70 USD |
| period=12, recursion_length=144, signal_period=6 | 336.84 USD | -504.37 USD | -100.36 USD | -3.01 USD |
| period=18, recursion_length=120, signal_period=6 | 96.57 USD | -140.86 USD | -236.47 USD | 73.85 USD |
| period=18, recursion_length=120, signal_period=9 | 25.48 USD | -64.62 USD | -69.57 USD | -59.41 USD |

All 18 training variants of the 4h candidate_both arm (unsuccessful variants included):

| params | clusters | mean ddadj | mean Sharpe | net PnL USD |
|---|---|---|---|---|
| period=12, recursion_length=120, signal_period=6 | 32 | 2.109 | 1.451 | 382.97 |
| period=12, recursion_length=144, signal_period=6 | 32 | 2.109 | 1.451 | 382.97 |
| period=12, recursion_length=192, signal_period=6 | 32 | 2.109 | 1.451 | 382.97 |
| period=12, recursion_length=120, signal_period=9 | 31 | 1.386 | 1.028 | 314.93 |
| period=12, recursion_length=144, signal_period=9 | 31 | 1.386 | 1.028 | 314.93 |
| period=12, recursion_length=192, signal_period=9 | 31 | 1.386 | 1.028 | 314.93 |
| period=12, recursion_length=120, signal_period=12 | 30 | 1.503 | 1.027 | 316.68 |
| period=12, recursion_length=144, signal_period=12 | 30 | 1.503 | 1.027 | 316.68 |
| period=12, recursion_length=192, signal_period=12 | 30 | 1.503 | 1.027 | 316.68 |
| period=18, recursion_length=120, signal_period=6 | 29 | 0.417 | 0.176 | 93.61 |
| period=18, recursion_length=144, signal_period=6 | 29 | 0.417 | 0.176 | 93.61 |
| period=18, recursion_length=192, signal_period=6 | 29 | 0.417 | 0.176 | 93.61 |
| period=18, recursion_length=120, signal_period=9 | 33 | -0.420 | -0.826 | -197.91 |
| period=18, recursion_length=144, signal_period=9 | 33 | -0.420 | -0.826 | -197.91 |
| period=18, recursion_length=192, signal_period=9 | 33 | -0.420 | -0.826 | -197.91 |
| period=18, recursion_length=120, signal_period=12 | 27 | -0.110 | -0.475 | -125.79 |
| period=18, recursion_length=144, signal_period=12 | 27 | -0.110 | -0.475 | -125.79 |
| period=18, recursion_length=192, signal_period=12 | 27 | -0.110 | -0.475 | -125.79 |

## Integration parity

Close capability on Hyperliquid perps: yes. `parity_diff` on the explicit-paper configurations (`paper_parity_1h.json`, `paper_parity_4h.json`; frozen BTC candles): clean = yes. The generic paper-batch slot does not enable closed-bar decisions; the timestamped closed-bar check coverage is in `shared_scripts/test_trix_momentum_checks.py`.

| config | params | batched | window | bars | mismatches | batch mismatches | long / short entries | long / short closes | clean |
|---|---|---|---|---|---|---|---|---|---|
| paper_parity_1h.json | seed | no | 200 | 2000 | 0 | - | 12 / 9 | 13 / 4 | yes |
| paper_parity_1h.json | seed | no | full | 2170 | 0 | - | 14 / 9 | 13 / 4 | yes |
| paper_parity_1h.json | seed | yes | 200 | 2000 | 0 | 0.00 | 12 / 9 | 13 / 4 | yes |
| paper_parity_1h.json | seed | yes | full | 2170 | 0 | 0.00 | 14 / 9 | 13 / 4 | yes |
| paper_parity_1h.json | selected | no | 200 | 2000 | 0 | - | 12 / 10 | 12 / 5 | yes |
| paper_parity_1h.json | selected | no | full | 2170 | 0 | - | 13 / 10 | 12 / 5 | yes |
| paper_parity_1h.json | selected | yes | 200 | 2000 | 0 | 0.00 | 12 / 10 | 12 / 5 | yes |
| paper_parity_1h.json | selected | yes | full | 2170 | 0 | 0.00 | 13 / 10 | 12 / 5 | yes |
| paper_parity_4h.json | seed | no | 200 | 1989 | 0 | - | 9 / 18 | 10 / 28 | yes |
| paper_parity_4h.json | seed | no | full | 2159 | 0 | - | 9 / 18 | 10 / 28 | yes |
| paper_parity_4h.json | seed | yes | 200 | 1989 | 0 | 0.00 | 9 / 18 | 10 / 28 | yes |
| paper_parity_4h.json | seed | yes | full | 2159 | 0 | 0.00 | 9 / 18 | 10 / 28 | yes |
| paper_parity_4h.json | selected | no | 200 | 1989 | 0 | - | 9 / 18 | 12 / 32 | yes |
| paper_parity_4h.json | selected | no | full | 2159 | 0 | - | 10 / 18 | 15 / 32 | yes |
| paper_parity_4h.json | selected | yes | 200 | 1989 | 0 | 0.00 | 9 / 18 | 12 / 32 | yes |
| paper_parity_4h.json | selected | yes | full | 2159 | 0 | 0.00 | 10 / 18 | 15 / 32 | yes |

## Limitations

- Binance US spot candles stand in for Hyperliquid perpetual prices; proxy results cannot establish Hyperliquid profitability or venue execution parity.
- Costs are the Backtester Hyperliquid taker fee (0.045% per side) and a flat 0.05% slippage per fill; the spread is not modeled separately and there is no lot-size or venue-minimum model on this path.
- Funding is charged from frozen Hyperliquid rates (--funding charge); a leg with unpriced held hours is incomplete and cannot count as economic evidence.
- No venue execution manifest is used; the frozen offline manifest path (fees, spread, slippage, lots and minimums) is available in eval_windows.py but this study does not include venue evidence.
- No higher-timeframe filter is used.
- A pass would not authorize live use: live use still needs `allow_no_edge: true` and a separate reviewed edge decision.
