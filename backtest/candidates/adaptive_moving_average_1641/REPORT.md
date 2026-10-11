# adaptive_moving_average study (issue 1641)

**Verdict: FAIL.** 1h: fail (false: beats_comparators, m1_passes, net_positive_base, net_positive_stress); 4h: fail (false: beats_comparators, m1_passes, net_positive_base, net_positive_stress).

The registry and the Go mirror keep `edge_status="no_edge"` with `edge_source="study_fail"` and this file as `edge_ref`. A study result never removes the no-edge label: paper evaluation needs one explicit `--mode=paper`, and live use still needs `allow_no_edge: true` plus a separate reviewed edge decision. Nothing here edits deployed configuration, live defaults or acknowledgements; the study is suggest-only.

## Candidate

Entry-only Kaufman adaptive moving average (`shared_strategies/open/adaptive_moving_average.py`). For efficiency period E, efficiency at bar k is |close k - close k-E| divided by the sum of the E absolute one-bar close changes ending at k (0 when that sum is 0). The smoothing constant is (efficiency x (2/(fast+1) - 2/(slow+1)) + 2/(slow+1)) squared. Every average at bar t restarts from close t-L and applies exactly L updates (t-L+1 through t), so one decision reads L + E + max(S, 1) rows and full-series, prefix and bounded-frame decisions are identical. Long when close t > average t, close t-1 <= average t-1 and average t - average t-S > 0; short is the exact mirror. Equality at the current bar and a zero slope hold. Bad prices, bad volume, timestamp defects, missing cadence and cadence gaps inside the window hold the entry with a reason; the close owner and the stop keep running.

Close owner: `tiered_tp_pct` (`{"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}`); stop owner: `stop_loss_atr_mult=1.0`; candidate direction `both`. Seed: efficiency_period=10, fast_period=2, slow_period=30, slope_span=3, recursion_length=120 (133 rows of history). The registered limit is 199 rows.

## Protocol and provenance

- Protocol: `protocol.json`, sha256 `981b6ec8ac6f76db5afd2a6c0501d2350edab804d1f452a3afc90800406091b6` (frozen before scoring; `study_manifest.json` records the same hash at freeze time, sha256 `2589a9a07a8afe34f788a1c6e0f36e4591cef3009aa1dcf20984ab576b2ced01`).
- Source commit of the run: `64456817451ab5d7f96d5b962cec307a6986c425`; runtime 441.5 s.
- Inputs: the frozen files of study `chande_momentum_1661`, read in place and verified against the pins in `protocol.json` (source manifest sha256 `f5bd9d2672abf5880b8af57cb01a0ffb70b21ccdf5eab464a9d351e121d580e3`, acquired 2026-10-11T03:16:15Z at commit `0715416732ccb93275e81e3e7a096d887a1bdfed`). The run rebuilds a fresh cache from them and disables every network fetch.
- Windows (start inclusive, end exclusive): `train` 2024-01-01 to 2025-01-01; `h2023H2` 2023-07-01 to 2024-01-01; `h2025` 2025-01-01 to 2026-01-01; `oos` 2026-01-01 to 2026-10-01. Training window `train`; held-out `h2023H2, h2025, oos`. The open-ended M1 oos window is pinned to 2026-10-01.
- Warmup: every leg (candidate, comparators, incumbents) computes indicators and ATR on the 300 consecutive bars before the window start and scores only the window from a flat book (eval_windows.run_leg warmup_bars); 300 exceeds the largest candidate history requirement (185 rows) and the 200-bar EMA of the momentum_pro incumbent; a missing pre-window bar is an error.
- Costs: commission 0.045% per side, slippage 0.050% per fill, stress x2.0 slippage; funding `--funding charge`.
- Selection: highest mean drawdown-adjusted return over the three symbol legs, then highest mean Sharpe, then the smallest parameter tuple in grid key order; eligible: every symbol leg ran, no leg is funding-incomplete and every leg has at least min_training_positions positions (min 5).
- Verdict rules: each timeframe group gets its own verdict from the three held-out windows; the study verdict is pass only if both groups pass, fail only if both fail, else inconclusive. Floor: 30 entry clusters (candidate positions in scorable held-out windows, counting entries on the same bar across symbols once). Unscorable window: a held-out window where any candidate, stressed candidate, comparator or incumbent leg is missing or funding-incomplete (unpriced held hours under --funding charge) is unscorable; it cannot support a pass.

| frozen file | sha256 |
|---|---|
| `backtest/candidates/chande_momentum_1661/data/BTC_1h_candles.csv.gz` | `d107c60f72363372d03273ba6439ca42a0d867a9089c5de171e9306af3ba2d17` |
| `backtest/candidates/chande_momentum_1661/data/BTC_4h_candles.csv.gz` | `dd0758a2862955eb53b060df5fbace3e98f8bc2a3e0254842bca0d36c96f106f` |
| `backtest/candidates/chande_momentum_1661/data/BTC_funding.csv.gz` | `520fd979d5594b4c2fb950cd52aa327749a124d9971a59ec3419752dd24a27f8` |
| `backtest/candidates/chande_momentum_1661/data/ETH_1h_candles.csv.gz` | `59d08735aecb22b63bab8189c7c32b11101e80da42b2ec724f39e308548b7913` |
| `backtest/candidates/chande_momentum_1661/data/ETH_4h_candles.csv.gz` | `03df63c5dc65ba532a252e8c00118986321fcb124934bb7536e842d678fc9dd8` |
| `backtest/candidates/chande_momentum_1661/data/ETH_funding.csv.gz` | `4b7694d8fc26cb7245ffa882cbdcb2d571d17f3a9f1abd6832fb0deed4985b41` |
| `backtest/candidates/chande_momentum_1661/data/SOL_1h_candles.csv.gz` | `e4e5c15ffcce696253d78dc3b3c3448725e8cf3a0114a9fbc4b1df5f2a0efeec` |
| `backtest/candidates/chande_momentum_1661/data/SOL_4h_candles.csv.gz` | `c1e5775973d00b0b07597da3db4213d62378ef566fe2e4a3b619612b59abc8ab` |
| `backtest/candidates/chande_momentum_1661/data/SOL_funding.csv.gz` | `b29de7625f7484f0971336e31ba52f5b042127e4eef4d8ee1e4554994383cd2b` |

## Reproduce

```bash
uv run --no-sync python backtest/candidates/adaptive_moving_average_1641/run_study.py freeze
uv run --no-sync python backtest/candidates/adaptive_moving_average_1641/run_study.py run --cache-db /tmp/ama_1641_run.db
uv run --no-sync python backtest/candidates/adaptive_moving_average_1641/run_study.py parity --cache-db /tmp/ama_1641_parity.db
uv run --no-sync python backtest/candidates/adaptive_moving_average_1641/run_study.py render
```

Each run and parity call needs a cache path that does not exist yet. The ordinary M1 command for the seed on the cached path is `uv run --no-sync python backtest/eval_windows.py --registry futures --candidate-json backtest/candidates/adaptive_moving_average_1641/spec_seed.json --funding charge --warmup-bars 300` (it uses the M1 default windows and the live cache).

## 1h: FAIL

Reason: false: beats_comparators, m1_passes, net_positive_base, net_positive_stress.

| selection | params | how | eligible variants |
|---|---|---|---|
| adaptive_moving_average | efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | selected | 162 of 162 |
| ema_crossover | fast_period=20, slow_period=26 | selected | 16 of 16 |
| vol_momentum | eff_entry=0.25, entry_threshold=0.3, mom_window=48 | selected | 23 of 27 |

Held-out results (sums over BTC, ETH and SOL legs; M1 bar = median of the eight incumbents with the same warmup):

| group | window | arm | M1 verdict | Sharpe | DD-adj | bar Sharpe | bar DD-adj | net PnL USD | positions | long/short | fees USD | funding USD | turnover |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1h | h2023H2 | candidate | fail | -0.71 | -0.50 | 1.14 | 2.15 | -367.88 | 394 | 209/185 | 331.52 | -22.32 | 736.55 |
| 1h | h2023H2 | candidate x2 slippage | fail | -2.52 | -0.84 | 1.14 | 2.15 | -906.27 | 442 | 225/217 | 330.75 | -11.55 | 734.83 |
| 1h | h2023H2 | ema_crossover (long) | fail | -1.07 | -0.50 | 1.14 | 2.15 | -258.90 | 182 | 182/0 | 157.35 | -35.81 | 349.59 |
| 1h | h2023H2 | vol_momentum (both) | fail | 0.57 | 0.98 | 1.14 | 2.15 | 91.62 | 28 | 15/13 | 25.30 | -3.47 | 56.21 |
| 1h | h2025 | candidate | fail | -0.53 | -0.35 | -0.41 | -0.48 | -458.02 | 780 | 397/383 | 656.19 | -5.70 | 1457.86 |
| 1h | h2025 | candidate x2 slippage | fail | -1.34 | -0.59 | -0.41 | -0.48 | -959.45 | 802 | 410/392 | 608.64 | -6.25 | 1352.24 |
| 1h | h2025 | ema_crossover (long) | fail | -1.52 | -0.90 | -0.41 | -0.48 | -886.56 | 359 | 359/0 | 274.19 | -28.55 | 609.18 |
| 1h | h2025 | vol_momentum (both) | pass | 0.14 | 0.38 | -0.41 | -0.48 | -4.62 | 48 | 24/24 | 42.43 | -0.85 | 94.27 |
| 1h | oos | candidate | fail | -1.85 | -0.77 | 0.23 | 0.07 | -1031.78 | 569 | 278/291 | 415.99 | -0.83 | 924.20 |
| 1h | oos | candidate x2 slippage | fail | -3.08 | -0.92 | 0.23 | 0.07 | -1476.65 | 589 | 289/300 | 383.30 | -2.05 | 851.58 |
| 1h | oos | ema_crossover (long) | fail | -0.88 | -0.60 | 0.23 | 0.07 | -384.77 | 273 | 273/0 | 226.18 | -13.95 | 502.51 |
| 1h | oos | vol_momentum (both) | fail | -0.59 | -0.40 | 0.23 | 0.07 | -137.61 | 63 | 32/31 | 56.39 | -2.42 | 125.27 |

| condition | value |
|---|---|
| beats_comparators | no |
| m1_passes | no |
| net_positive_base | no |
| net_positive_stress | no |
| not_concentrated | yes |
| scorable windows | h2023H2, h2025, oos |
| entry clusters / positions (long/short) | 1510 / 1743 (884/859) |
| net PnL USD base / x2 slippage | -1857.68 / -3342.37 |
| fees USD / funding USD | 1403.69 / -28.85 |
| mean return % candidate / comparators | -20.62 / ema_crossover -17.00, vol_momentum -0.56 |
| largest position share of positive PnL | 0.0113 |
| M1 passes | 0 of 3 |

Next-bar fills: 1743 held-out candidate positions checked, 0 mismatches (each entry fills at the open of the bar after its signal, plus the modeled slippage). Every scored frame starts at its window start after 300 warmup bars: 2023-07-01 00:00:00, 2025-01-01 00:00:00, 2026-01-01 00:00:00.

Per-entry evidence: `evidence/entries_1h.csv.gz` (2714 rows, sha256 `87cd9608e48b75e8bd7da01fe7ae47715fe45da6d8377231f3fb0ec152fd264d`) lists every held-out entry signal with its input window hash and indicator values; replaying each entry on exactly its required rows reproduced it with 0 mismatches.

Parameter stability (one grid step from the selection; training rows, then held-out, exploratory after selection):

| params | train DD-adj | train Sharpe | train positions |
|---|---|---|---|
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | 0.74 | 0.43 | 800 |
| efficiency_period=5, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | -0.87 | -1.43 | 952 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | 0.05 | -0.31 | 677 |
| efficiency_period=10, fast_period=2, recursion_length=60, slope_span=3, slow_period=30 | -0.36 | -0.31 | 968 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=20 | -0.18 | -0.15 | 841 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | 0.21 | 0.33 | 784 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=1, slow_period=30 | -0.65 | -0.69 | 1026 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=5, slow_period=30 | 0.51 | 0.01 | 739 |
| efficiency_period=10, fast_period=3, recursion_length=120, slope_span=3, slow_period=30 | -0.43 | -0.34 | 780 |

| params | window | M1 verdict | Sharpe | DD-adj | net PnL USD | positions |
|---|---|---|---|---|---|---|
| efficiency_period=10, fast_period=2, recursion_length=120, slope_span=3, slow_period=30 | h2023H2 | fail | -1.72 | -0.71 | -740.89 | 512 |
| efficiency_period=10, fast_period=2, recursion_length=120, slope_span=3, slow_period=30 | h2025 | fail | -0.50 | 0.90 | 72.11 | 946 |
| efficiency_period=10, fast_period=2, recursion_length=120, slope_span=3, slow_period=30 | oos | fail | -0.91 | -0.62 | -681.87 | 661 |
| efficiency_period=10, fast_period=2, recursion_length=60, slope_span=3, slow_period=30 | h2023H2 | fail | -1.43 | -0.57 | -643.17 | 480 |
| efficiency_period=10, fast_period=2, recursion_length=60, slope_span=3, slow_period=30 | h2025 | fail | -0.56 | 0.53 | 106.14 | 949 |
| efficiency_period=10, fast_period=2, recursion_length=60, slope_span=3, slow_period=30 | oos | fail | -1.38 | -0.68 | -902.47 | 654 |
| efficiency_period=10, fast_period=3, recursion_length=120, slope_span=3, slow_period=30 | h2023H2 | fail | -0.70 | -0.21 | -327.79 | 389 |
| efficiency_period=10, fast_period=3, recursion_length=120, slope_span=3, slow_period=30 | h2025 | pass | -0.24 | 0.25 | -146.18 | 758 |
| efficiency_period=10, fast_period=3, recursion_length=120, slope_span=3, slow_period=30 | oos | fail | -1.54 | -0.76 | -922.08 | 563 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=1, slow_period=30 | h2023H2 | fail | -1.36 | -0.71 | -695.19 | 468 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=1, slow_period=30 | h2025 | pass | 0.10 | -0.03 | -25.33 | 1020 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=1, slow_period=30 | oos | fail | -1.64 | -0.86 | -1074.86 | 719 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=20 | h2023H2 | fail | -0.14 | 0.06 | -31.65 | 395 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=20 | h2025 | pass | -0.20 | -0.13 | -151.80 | 858 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=20 | oos | fail | -1.83 | -0.74 | -984.12 | 583 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | h2023H2 | fail | -0.43 | -0.33 | -178.65 | 383 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | h2025 | fail | -0.93 | -0.44 | -650.74 | 792 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | oos | fail | -1.36 | -0.74 | -769.04 | 546 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=5, slow_period=30 | h2023H2 | fail | -0.54 | -0.23 | -148.63 | 370 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=5, slow_period=30 | h2025 | pass | 0.10 | 0.61 | 276.82 | 772 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=5, slow_period=30 | oos | fail | -2.01 | -0.83 | -1092.52 | 562 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | h2023H2 | fail | -1.30 | -0.04 | -177.37 | 333 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | h2025 | fail | -0.51 | -0.21 | -333.41 | 634 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | oos | fail | -1.29 | -0.66 | -660.42 | 529 |
| efficiency_period=5, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | h2023H2 | fail | 0.04 | -0.06 | -25.49 | 479 |
| efficiency_period=5, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | h2025 | fail | -0.52 | -0.27 | -452.54 | 947 |
| efficiency_period=5, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | oos | fail | -1.57 | -0.79 | -1024.77 | 653 |

Best five candidate training variants by DD-adj (every variant, eligible or not, is in `results.json`):

| params | DD-adj | Sharpe | positions |
|---|---|---|---|
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | 0.74 | 0.43 | 800 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=5, slow_period=30 | 0.51 | 0.01 | 739 |
| efficiency_period=20, fast_period=2, recursion_length=160, slope_span=3, slow_period=20 | 0.40 | 0.45 | 856 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | 0.21 | 0.33 | 784 |
| efficiency_period=20, fast_period=2, recursion_length=120, slope_span=3, slow_period=20 | 0.12 | 0.28 | 847 |

## 4h: FAIL

Reason: false: beats_comparators, m1_passes, net_positive_base, net_positive_stress.

| selection | params | how | eligible variants |
|---|---|---|---|
| adaptive_moving_average | efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | selected | 162 of 162 |
| ema_crossover | fast_period=20, slow_period=34 | selected | 16 of 16 |
| vol_momentum | eff_entry=0.35, entry_threshold=0.2, mom_window=16 | selected | 17 of 27 |

Held-out results (sums over BTC, ETH and SOL legs; M1 bar = median of the eight incumbents with the same warmup):

| group | window | arm | M1 verdict | Sharpe | DD-adj | bar Sharpe | bar DD-adj | net PnL USD | positions | long/short | fees USD | funding USD | turnover |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 4h | h2023H2 | candidate | fail | 0.21 | 0.49 | 1.34 | 2.46 | 124.81 | 69 | 34/35 | 62.94 | -2.23 | 139.84 |
| 4h | h2023H2 | candidate x2 slippage | fail | 0.14 | 0.49 | 1.34 | 2.46 | 122.02 | 69 | 34/35 | 62.99 | -0.26 | 139.94 |
| 4h | h2023H2 | ema_crossover (long) | fail | -0.13 | -0.10 | 1.34 | 2.46 | -11.32 | 34 | 34/0 | 30.72 | -18.37 | 68.24 |
| 4h | h2023H2 | vol_momentum (both) | fail | -1.00 | -0.52 | 1.34 | 2.46 | -504.86 | 162 | 80/82 | 137.00 | 4.28 | 304.37 |
| 4h | h2025 | candidate | fail | -1.03 | -0.78 | -0.24 | -0.42 | -800.81 | 192 | 101/91 | 160.18 | -6.71 | 355.87 |
| 4h | h2025 | candidate x2 slippage | fail | -1.24 | -0.83 | -0.24 | -0.42 | -925.16 | 192 | 101/91 | 155.78 | -6.92 | 346.10 |
| 4h | h2025 | ema_crossover (long) | fail | -0.43 | -0.41 | -0.24 | -0.42 | -206.77 | 83 | 83/0 | 71.83 | -14.25 | 159.58 |
| 4h | h2025 | vol_momentum (both) | pass | 0.02 | 0.05 | -0.24 | -0.42 | -87.54 | 381 | 188/193 | 388.67 | -3.28 | 863.52 |
| 4h | oos | candidate | fail | 0.12 | 0.19 | 0.51 | 0.42 | 66.04 | 116 | 60/56 | 104.43 | -4.81 | 232.01 |
| 4h | oos | candidate x2 slippage | fail | -0.06 | 0.03 | 0.51 | 0.42 | -18.04 | 117 | 60/57 | 103.73 | -4.92 | 230.47 |
| 4h | oos | ema_crossover (long) | fail | -0.80 | -0.41 | 0.51 | 0.42 | -279.98 | 59 | 59/0 | 48.28 | -7.99 | 107.27 |
| 4h | oos | vol_momentum (both) | fail | -0.55 | -0.47 | 0.51 | 0.42 | -480.79 | 269 | 138/131 | 229.46 | -8.02 | 509.80 |

| condition | value |
|---|---|
| beats_comparators | no |
| m1_passes | no |
| net_positive_base | no |
| net_positive_stress | no |
| not_concentrated | yes |
| scorable windows | h2023H2, h2025, oos |
| entry clusters / positions (long/short) | 331 / 377 (195/182) |
| net PnL USD base / x2 slippage | -609.96 / -821.18 |
| fees USD / funding USD | 327.55 / -13.75 |
| mean return % candidate / comparators | -6.76 / ema_crossover -5.52, vol_momentum -11.91 |
| largest position share of positive PnL | 0.0209 |
| M1 passes | 0 of 3 |

Next-bar fills: 377 held-out candidate positions checked, 0 mismatches (each entry fills at the open of the bar after its signal, plus the modeled slippage). Every scored frame starts at its window start after 300 warmup bars: 2023-07-01 00:00:00, 2025-01-01 00:00:00, 2026-01-01 00:00:00.

Per-entry evidence: `evidence/entries_4h.csv.gz` (469 rows, sha256 `2db5bdacac4017a9a6ebff1c527766d8ec39a395a3b68a5fde42ed4e24208f34`) lists every held-out entry signal with its input window hash and indicator values; replaying each entry on exactly its required rows reproduced it with 0 mismatches.

Parameter stability (one grid step from the selection; training rows, then held-out, exploratory after selection):

| params | train DD-adj | train Sharpe | train positions |
|---|---|---|---|
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | 2.86 | 0.91 | 143 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | -0.35 | -0.22 | 197 |
| efficiency_period=20, fast_period=2, recursion_length=60, slope_span=3, slow_period=50 | 0.20 | 0.15 | 182 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | 1.78 | 0.74 | 167 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=1, slow_period=50 | 0.17 | -0.03 | 172 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=5, slow_period=50 | 0.42 | 0.10 | 147 |
| efficiency_period=20, fast_period=3, recursion_length=120, slope_span=3, slow_period=50 | 0.29 | -0.01 | 158 |

| params | window | M1 verdict | Sharpe | DD-adj | net PnL USD | positions |
|---|---|---|---|---|---|---|
| efficiency_period=10, fast_period=2, recursion_length=120, slope_span=3, slow_period=30 | h2023H2 | fail | -0.73 | -0.07 | -311.75 | 126 |
| efficiency_period=10, fast_period=2, recursion_length=120, slope_span=3, slow_period=30 | h2025 | fail | -0.42 | -0.53 | -515.30 | 301 |
| efficiency_period=10, fast_period=2, recursion_length=120, slope_span=3, slow_period=30 | oos | fail | 0.49 | 1.66 | 516.30 | 183 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | h2023H2 | fail | -0.52 | 0.61 | -242.21 | 89 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | h2025 | pass | -0.20 | -0.15 | -164.00 | 229 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | oos | pass | 0.97 | 1.36 | 566.59 | 134 |
| efficiency_period=20, fast_period=2, recursion_length=60, slope_span=3, slow_period=50 | h2023H2 | fail | -0.90 | -0.53 | -339.78 | 115 |
| efficiency_period=20, fast_period=2, recursion_length=60, slope_span=3, slow_period=50 | h2025 | fail | -0.53 | -0.61 | -517.24 | 212 |
| efficiency_period=20, fast_period=2, recursion_length=60, slope_span=3, slow_period=50 | oos | fail | 0.15 | 0.16 | 78.25 | 137 |
| efficiency_period=20, fast_period=3, recursion_length=120, slope_span=3, slow_period=50 | h2023H2 | fail | 0.57 | 0.87 | 218.04 | 67 |
| efficiency_period=20, fast_period=3, recursion_length=120, slope_span=3, slow_period=50 | h2025 | fail | -0.53 | -0.65 | -439.30 | 173 |
| efficiency_period=20, fast_period=3, recursion_length=120, slope_span=3, slow_period=50 | oos | fail | -1.12 | -0.53 | -481.28 | 107 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=1, slow_period=50 | h2023H2 | fail | -0.11 | -0.02 | -5.97 | 92 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=1, slow_period=50 | h2025 | pass | -0.19 | -0.38 | -309.71 | 189 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=1, slow_period=50 | oos | fail | -0.59 | -0.23 | -270.34 | 119 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | h2023H2 | fail | -0.83 | -0.57 | -249.30 | 77 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | h2025 | fail | -0.50 | -0.73 | -494.16 | 190 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | oos | pass | 0.79 | 1.04 | 484.70 | 137 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=5, slow_period=50 | h2023H2 | fail | 1.04 | 1.56 | 424.74 | 81 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=5, slow_period=50 | h2025 | fail | -0.29 | -0.39 | -316.94 | 169 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=5, slow_period=50 | oos | fail | -0.07 | -0.06 | -27.07 | 121 |

Best five candidate training variants by DD-adj (every variant, eligible or not, is in `results.json`):

| params | DD-adj | Sharpe | positions |
|---|---|---|---|
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=50 | 2.86 | 0.91 | 143 |
| efficiency_period=10, fast_period=3, recursion_length=160, slope_span=5, slow_period=20 | 1.80 | 0.91 | 199 |
| efficiency_period=10, fast_period=3, recursion_length=120, slope_span=5, slow_period=20 | 1.78 | 0.90 | 197 |
| efficiency_period=20, fast_period=3, recursion_length=60, slope_span=3, slow_period=30 | 1.78 | 0.74 | 167 |
| efficiency_period=10, fast_period=3, recursion_length=60, slope_span=5, slow_period=20 | 1.62 | 0.83 | 188 |

## Funding coverage

| group | window | arm | dataset | frame hours | priced | venue-absent | missing | unpriced held | rows sha256 |
|---|---|---|---|---|---|---|---|---|---|
| 1h | h2023H2 | candidate | BTC/USDT 1h | 4415 | None | 2 | 0 | 0 | `fcc17e246d052cd0` |
| 1h | h2023H2 | candidate | ETH/USDT 1h | 4415 | None | 2 | 0 | 0 | `f871b8fbc47840ed` |
| 1h | h2023H2 | candidate | SOL/USDT 1h | 4415 | None | 2 | 0 | 0 | `8fedf15bd5a0081b` |
| 1h | h2023H2 | candidate x2 | BTC/USDT 1h | 4415 | None | 2 | 0 | 0 | `fcc17e246d052cd0` |
| 1h | h2023H2 | candidate x2 | ETH/USDT 1h | 4415 | None | 2 | 0 | 0 | `f871b8fbc47840ed` |
| 1h | h2023H2 | candidate x2 | SOL/USDT 1h | 4415 | None | 2 | 0 | 0 | `8fedf15bd5a0081b` |
| 1h | h2023H2 | ema_crossover | BTC/USDT 1h | 4415 | None | 2 | 0 | 0 | `fcc17e246d052cd0` |
| 1h | h2023H2 | ema_crossover | ETH/USDT 1h | 4415 | None | 2 | 0 | 0 | `f871b8fbc47840ed` |
| 1h | h2023H2 | ema_crossover | SOL/USDT 1h | 4415 | None | 2 | 0 | 0 | `8fedf15bd5a0081b` |
| 1h | h2023H2 | vol_momentum | BTC/USDT 1h | 4415 | None | 2 | 0 | 0 | `fcc17e246d052cd0` |
| 1h | h2023H2 | vol_momentum | ETH/USDT 1h | 4415 | None | 2 | 0 | 0 | `f871b8fbc47840ed` |
| 1h | h2023H2 | vol_momentum | SOL/USDT 1h | 4415 | None | 2 | 0 | 0 | `8fedf15bd5a0081b` |
| 1h | h2025 | candidate | BTC/USDT 1h | 8759 | None | 0 | 0 | 0 | `50ac5ca92d5cbda7` |
| 1h | h2025 | candidate | ETH/USDT 1h | 8759 | None | 0 | 0 | 0 | `c9ea7fe5a178b743` |
| 1h | h2025 | candidate | SOL/USDT 1h | 8759 | None | 0 | 0 | 0 | `e5a6de717c3559c8` |
| 1h | h2025 | candidate x2 | BTC/USDT 1h | 8759 | None | 0 | 0 | 0 | `50ac5ca92d5cbda7` |
| 1h | h2025 | candidate x2 | ETH/USDT 1h | 8759 | None | 0 | 0 | 0 | `c9ea7fe5a178b743` |
| 1h | h2025 | candidate x2 | SOL/USDT 1h | 8759 | None | 0 | 0 | 0 | `e5a6de717c3559c8` |
| 1h | h2025 | ema_crossover | BTC/USDT 1h | 8759 | None | 0 | 0 | 0 | `50ac5ca92d5cbda7` |
| 1h | h2025 | ema_crossover | ETH/USDT 1h | 8759 | None | 0 | 0 | 0 | `c9ea7fe5a178b743` |
| 1h | h2025 | ema_crossover | SOL/USDT 1h | 8759 | None | 0 | 0 | 0 | `e5a6de717c3559c8` |
| 1h | h2025 | vol_momentum | BTC/USDT 1h | 8759 | None | 0 | 0 | 0 | `50ac5ca92d5cbda7` |
| 1h | h2025 | vol_momentum | ETH/USDT 1h | 8759 | None | 0 | 0 | 0 | `c9ea7fe5a178b743` |
| 1h | h2025 | vol_momentum | SOL/USDT 1h | 8759 | None | 0 | 0 | 0 | `e5a6de717c3559c8` |
| 1h | oos | candidate | BTC/USDT 1h | 6551 | None | 0 | 0 | 0 | `920006cfb21d6b65` |
| 1h | oos | candidate | ETH/USDT 1h | 6551 | None | 0 | 0 | 0 | `51039b1ca124e8b2` |
| 1h | oos | candidate | SOL/USDT 1h | 6551 | None | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |
| 1h | oos | candidate x2 | BTC/USDT 1h | 6551 | None | 0 | 0 | 0 | `920006cfb21d6b65` |
| 1h | oos | candidate x2 | ETH/USDT 1h | 6551 | None | 0 | 0 | 0 | `51039b1ca124e8b2` |
| 1h | oos | candidate x2 | SOL/USDT 1h | 6551 | None | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |
| 1h | oos | ema_crossover | BTC/USDT 1h | 6551 | None | 0 | 0 | 0 | `920006cfb21d6b65` |
| 1h | oos | ema_crossover | ETH/USDT 1h | 6551 | None | 0 | 0 | 0 | `51039b1ca124e8b2` |
| 1h | oos | ema_crossover | SOL/USDT 1h | 6551 | None | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |
| 1h | oos | vol_momentum | BTC/USDT 1h | 6551 | None | 0 | 0 | 0 | `920006cfb21d6b65` |
| 1h | oos | vol_momentum | ETH/USDT 1h | 6551 | None | 0 | 0 | 0 | `51039b1ca124e8b2` |
| 1h | oos | vol_momentum | SOL/USDT 1h | 6551 | None | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |
| 4h | h2023H2 | candidate | BTC/USDT 4h | 4412 | None | 2 | 0 | 0 | `e40e8a580dff0e90` |
| 4h | h2023H2 | candidate | ETH/USDT 4h | 4412 | None | 2 | 0 | 0 | `f8c768a16351073e` |
| 4h | h2023H2 | candidate | SOL/USDT 4h | 4412 | None | 2 | 0 | 0 | `c36ef8aa95c722ec` |
| 4h | h2023H2 | candidate x2 | BTC/USDT 4h | 4412 | None | 2 | 0 | 0 | `e40e8a580dff0e90` |
| 4h | h2023H2 | candidate x2 | ETH/USDT 4h | 4412 | None | 2 | 0 | 0 | `f8c768a16351073e` |
| 4h | h2023H2 | candidate x2 | SOL/USDT 4h | 4412 | None | 2 | 0 | 0 | `c36ef8aa95c722ec` |
| 4h | h2023H2 | ema_crossover | BTC/USDT 4h | 4412 | None | 2 | 0 | 0 | `e40e8a580dff0e90` |
| 4h | h2023H2 | ema_crossover | ETH/USDT 4h | 4412 | None | 2 | 0 | 0 | `f8c768a16351073e` |
| 4h | h2023H2 | ema_crossover | SOL/USDT 4h | 4412 | None | 2 | 0 | 0 | `c36ef8aa95c722ec` |
| 4h | h2023H2 | vol_momentum | BTC/USDT 4h | 4412 | None | 2 | 0 | 0 | `e40e8a580dff0e90` |
| 4h | h2023H2 | vol_momentum | ETH/USDT 4h | 4412 | None | 2 | 0 | 0 | `f8c768a16351073e` |
| 4h | h2023H2 | vol_momentum | SOL/USDT 4h | 4412 | None | 2 | 0 | 0 | `c36ef8aa95c722ec` |
| 4h | h2025 | candidate | BTC/USDT 4h | 8756 | None | 0 | 0 | 0 | `754dbc30a01fb40c` |
| 4h | h2025 | candidate | ETH/USDT 4h | 8756 | None | 0 | 0 | 0 | `609f3e8a021f5e1c` |
| 4h | h2025 | candidate | SOL/USDT 4h | 8756 | None | 0 | 0 | 0 | `0303f699aa1cd74f` |
| 4h | h2025 | candidate x2 | BTC/USDT 4h | 8756 | None | 0 | 0 | 0 | `754dbc30a01fb40c` |
| 4h | h2025 | candidate x2 | ETH/USDT 4h | 8756 | None | 0 | 0 | 0 | `609f3e8a021f5e1c` |
| 4h | h2025 | candidate x2 | SOL/USDT 4h | 8756 | None | 0 | 0 | 0 | `0303f699aa1cd74f` |
| 4h | h2025 | ema_crossover | BTC/USDT 4h | 8756 | None | 0 | 0 | 0 | `754dbc30a01fb40c` |
| 4h | h2025 | ema_crossover | ETH/USDT 4h | 8756 | None | 0 | 0 | 0 | `609f3e8a021f5e1c` |
| 4h | h2025 | ema_crossover | SOL/USDT 4h | 8756 | None | 0 | 0 | 0 | `0303f699aa1cd74f` |
| 4h | h2025 | vol_momentum | BTC/USDT 4h | 8756 | None | 0 | 0 | 0 | `754dbc30a01fb40c` |
| 4h | h2025 | vol_momentum | ETH/USDT 4h | 8756 | None | 0 | 0 | 0 | `609f3e8a021f5e1c` |
| 4h | h2025 | vol_momentum | SOL/USDT 4h | 8756 | None | 0 | 0 | 0 | `0303f699aa1cd74f` |
| 4h | oos | candidate | BTC/USDT 4h | 6548 | None | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| 4h | oos | candidate | ETH/USDT 4h | 6548 | None | 0 | 0 | 0 | `c313f79e21796506` |
| 4h | oos | candidate | SOL/USDT 4h | 6548 | None | 0 | 0 | 0 | `246de07d578592eb` |
| 4h | oos | candidate x2 | BTC/USDT 4h | 6548 | None | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| 4h | oos | candidate x2 | ETH/USDT 4h | 6548 | None | 0 | 0 | 0 | `c313f79e21796506` |
| 4h | oos | candidate x2 | SOL/USDT 4h | 6548 | None | 0 | 0 | 0 | `246de07d578592eb` |
| 4h | oos | ema_crossover | BTC/USDT 4h | 6548 | None | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| 4h | oos | ema_crossover | ETH/USDT 4h | 6548 | None | 0 | 0 | 0 | `c313f79e21796506` |
| 4h | oos | ema_crossover | SOL/USDT 4h | 6548 | None | 0 | 0 | 0 | `246de07d578592eb` |
| 4h | oos | vol_momentum | BTC/USDT 4h | 6548 | None | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| 4h | oos | vol_momentum | ETH/USDT 4h | 6548 | None | 0 | 0 | 0 | `c313f79e21796506` |
| 4h | oos | vol_momentum | SOL/USDT 4h | 6548 | None | 0 | 0 | 0 | `246de07d578592eb` |

## Decision parity

`parity_results.json` (source commit `64456817451ab5d7f96d5b962cec307a6986c425+uncommitted-changes`): clean = yes. Each run feeds the frozen BTC candles through `parity_diff.compute_parity_frame` with the paper configuration (explicit `--mode=paper`, `closed_bar_decisions: true`, `tiered_tp_pct`, one ATR stop, `direction: both`), comparing the full-series backtest decision with the live check decision on a trailing 200-row frame (window 200) or the whole prefix (window 0), solo and batched, in the flat, long and short position contexts the simulation produces. These runs prove supplied-frame parity; the closed-bar cutoff is proved separately by the subprocess checks in `shared_scripts/test_adaptive_moving_average_checks.py`.

| config | params | batched | window | bars | mismatches | batch mismatches | long entries | short entries | long closes | short closes | clean |
|---|---|---|---|---|---|---|---|---|---|---|---|
| paper_parity_1h.json | seed | no | 200 | 2000 | 0 | None | 45 | 52 | 3 | 0 | yes |
| paper_parity_1h.json | seed | no | 0 | 2170 | 0 | None | 46 | 54 | 3 | 0 | yes |
| paper_parity_1h.json | seed | yes | 200 | 2000 | 0 | 0 | 45 | 52 | 3 | 0 | yes |
| paper_parity_1h.json | seed | yes | 0 | 2170 | 0 | 0 | 46 | 54 | 3 | 0 | yes |
| paper_parity_1h.json | selected | no | 200 | 2000 | 0 | None | 38 | 38 | 4 | 1 | yes |
| paper_parity_1h.json | selected | no | 0 | 2170 | 0 | None | 44 | 45 | 4 | 1 | yes |
| paper_parity_1h.json | selected | yes | 200 | 2000 | 0 | 0 | 38 | 38 | 4 | 1 | yes |
| paper_parity_1h.json | selected | yes | 0 | 2170 | 0 | 0 | 44 | 45 | 4 | 1 | yes |
| paper_parity_4h.json | seed | no | 200 | 1989 | 0 | None | 51 | 48 | 7 | 10 | yes |
| paper_parity_4h.json | seed | no | 0 | 2159 | 0 | None | 55 | 50 | 8 | 11 | yes |
| paper_parity_4h.json | seed | yes | 200 | 1989 | 0 | 0 | 51 | 48 | 7 | 10 | yes |
| paper_parity_4h.json | seed | yes | 0 | 2159 | 0 | 0 | 55 | 50 | 8 | 11 | yes |
| paper_parity_4h.json | selected | no | 200 | 1989 | 0 | None | 34 | 34 | 6 | 2 | yes |
| paper_parity_4h.json | selected | no | 0 | 2159 | 0 | None | 39 | 36 | 7 | 2 | yes |
| paper_parity_4h.json | selected | yes | 200 | 1989 | 0 | 0 | 34 | 34 | 6 | 2 | yes |
| paper_parity_4h.json | selected | yes | 0 | 2159 | 0 | 0 | 39 | 36 | 7 | 2 | yes |

## Differences and limits

- Side populations: ema_crossover declares short_entries=False and stays long-only; vol_momentum declares short_entries=True and its futures variant ships allow_short=true, so it runs bidirectionally like the candidate.
- Every arm uses the same close owner, ATR stop, fees, slippage, funding mode, capital, windows and warmup. The incumbents keep the M1 default contract (long-only, signal exits), so the M1 bar compares a different exit policy.
- Binance US spot candles stand in for Hyperliquid perpetual prices; proxy results cannot establish Hyperliquid profitability.
- Costs are the Backtester Hyperliquid taker fee (0.045% per side) and a flat 0.05% slippage per fill; there is no separate spread, lot-size or venue-minimum model on this path.
- Funding is charged from frozen Hyperliquid rates (--funding charge); a leg with unpriced held hours is incomplete and cannot count as economic evidence.
- No higher-timeframe filter is used.
- The four windows are disjoint. Every window starts after the first Hyperliquid funding rate (2023-05-12), so a held hour without a rate is a coverage defect. The open-ended M1 oos window is pinned to end 2026-10-01. The candles were scored before by study chande_momentum_1661 for a different strategy; this candidate was never scored on them, but the held-out windows are retrospective data, not live forward evidence.

