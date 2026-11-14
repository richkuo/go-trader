# Dual Thrust Hyperliquid 4h confirmation (issue 2019)

**Primary verdict: FAIL** (false: beats_breakout_sharpe_base, m1_pass_base, net_positive_base, net_positive_stress). Only the additional-coin 4h population decides this verdict. Registry metadata, strategy defaults, live and paper configuration, acknowledgements and deployment decisions are unchanged: `dual_thrust` keeps `edge_status="no_edge"` and `edge_source="study_fail"`. Every command in this study is suggest-only and sends no order.

## Question

The `dual_thrust_1642` venue 4h group (BTC, ETH, SOL; test window 2025-09-01 to 2026-09-01) was `inconclusive` (test independent position clusters 18 < 30) while every performance check passed: base mean return 13.51%, mean Sharpe 0.97, net $405.41; stress mean return 13.07%, net $392.00. This study scores the same frozen settings, once, on additional Hyperliquid coins chosen by liquidity and coverage only, to see whether the pooled sample reaches the 30-cluster floor and what the frozen checks say there. An inconclusive result is a valid outcome.

## Protocol and lock

- `protocol.json` sha256 `1d07e46208cb33a5a2dd792d899d2049156e50c78c2704ceef77219fc7412f81`; input inventory `study_manifest.json` sha256 `c7d595fc7646c7a8dc586ff6da1b3f2cabe7943bad84fc7a8a3e19071f83f20c`; `screen.json` sha256 `af3500c4d742ff1f6b2de72c97d0e5161f4db6f655f98b2432b49b7d7042a779`.
- `run_study.py lock` wrote `protocol_lock.json` at 2026-11-14T17:43:41Z (working tree `092385f0cb34529829c15ae4037a09bb3fb00620+uncommitted-changes`); the lock commit is `f4d188bef7556a0e3aa7908b7b3ca941e22346ae` and it precedes every scored artifact in branch history (the runner refuses to score otherwise). The lock also records the sha256 of 42 input files, 3 manifests and 117 scoring source files.
- Scored at source commit `f4d188bef7556a0e3aa7908b7b3ca941e22346ae` with network access blocked and a fresh research cache; runtime 37.9 s.
- Frozen candidate: `dual_thrust` {"k_long": 1.0, "k_short": 0.75, "lookback_sessions": 7, "session_start_hour": 0} with `expected_interval_ms` bound to each interval (the original venue selection). Comparators: `breakout` {"atr_multiplier": 1.5, "atr_period": 14, "lookback": 20}; `session_breakout` {"atr_multiplier": 0.0, "atr_period": 14, "lookback": 1, "session": "asian", "vol_period": 20, "volume_threshold": 1.5}. Incumbents (M1 median): `momentum_pro`, `chart_pattern`, `squeeze_momentum`, `mean_reversion_pro`, `donchian_breakout`, `ichimoku_cloud`, `range_scalper`, `sma_crossover`, each with the effective parameters read from commit `d0c719b18a26a2ce2b0fa69ea284029baf5da7e1` (source check: verified). No grid, re-selection, alternative-setting diagnostic or seed fallback runs.
- Matched contract for every arm and incumbent: direction `both`, one `tiered_tp_pct` close (+3%, full position), one fixed entry-ATR stop (`stop_loss_atr_mult: 1.0`), `comparison_mode: strict`, `intrabar_resolution: ohlc_walk`, $1000 per leg.
- Costs (manifest execution specification): taker fee 0.00045, maker fee 0.00015, slippage 5.0 bps plus the per-coin half spread, szDecimals lot rounding, minimum notional $10 with a 0.03 margin. Stress (cost x2) doubles spread and slippage; fees stay fixed. Hourly funding is charged from the frozen series.
- Evaluator source files that differ from the original study's recorded versions: `backtest/backtester.py`, `backtest/eval_windows.py`, `backtest/offline_manifest.py`, `backtest/optimizer.py`, `backtest/parity_diff.py`, `shared_scripts/check_hyperliquid.py`, `shared_strategies/open/registry.py`, `shared_tools/closed_bar.py`, `shared_tools/data_fetcher.py`, `shared_tools/strategy_composition.py`. The current lock records every scoring source hash. Registry default parameters that differ from the frozen values: none.

Ordered verdict rule (primary population only):

- 1. evidence: inconclusive when the primary manifest provenance is not venue; the manifest roster differs from the screen roster; any primary dataset fails the candle, warm-up or funding checks; any candidate, comparator or incumbent leg is missing, errored, duplicated or unexpected at either cost; any leg's candle or funding coverage is incomplete; any required metric is non-finite; or the candidate entry audit fails
- 2. sample: inconclusive when pooled candidate base-cost clusters < 30
- 3. performance: pass only when every check is true; fail when any check is false

## Population screen

Liquidity source: the dayNtlVlm (24h notional volume, USD) field of one recorded metaAndAssetCtxs response; the observation time and the raw response are frozen in data/venue/. Observation time 2026-11-14T17:36:09Z (raw response `data/venue/meta_and_asset_ctxs.json`, sha256 `b396a97dd66cc21d70cbd1556097364cdc8b5fd5eb37ac863f89824191d71da4`). Rule: walk the ranking in order; screen each coin's 4h candles and hourly funding; select a coin when every requirement holds; stop after max_coins selections; coins below the last selection are not screened. The screen read only liquidity, listing and coverage information.

| rank | coin | 24h notional volume $ | 4h window bars | warm-up bars | invalid bars | funding hours | selected | reasons |
|---|---|---|---|---|---|---|---|---|
| 1 | HYPE | 845,546,545 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 2 | ZEC | 351,954,682 | 2556/2556 | 300/300 | 0 | 9466/10224 | no | 758 funding hours missing (first 2025-09-01 00:00:00) |
| 3 | XRP | 109,465,194 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 4 | NEAR | 67,100,241 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 5 | PUMP | 43,905,372 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 6 | kPEPE | 37,199,898 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 7 | LIT | 33,623,722 | 1881/2556 | 0/300 | 0 | 7520/10224 | no | 675 window bars missing (first 2025-09-01 00:00:00); 300 warm-up bars missing (first 2025-07-13 00:00:00); 2704 funding hours missing (first 2025-09-01 00:00:00) |
| 8 | XMR | 26,938,656 | 2556/2556 | 181/300 | 0 | 6951/10224 | no | 119 warm-up bars missing (first 2025-07-13 00:00:00); 3273 funding hours missing (first 2025-09-01 00:00:00) |
| 9 | ENA | 21,693,260 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 10 | UNI | 21,357,862 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 11 | AVAX | 16,627,282 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 12 | WLD | 14,247,643 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |
| 13 | DOGE | 13,385,208 | 2556/2556 | 300/300 | 0 | 10224/10224 | yes | - |

Not screened (ranked below the last selection): 163 coins. Excluded before ranking: 59 (BTC: original study coin, ETH: original study coin, SOL: original study coin; 56 delisted). Named candidates: BNB rank 14, selected no (ranked below the last selection; not screened); HYPE rank 1, selected yes.

## Populations

| population | role | interval | window (end exclusive) | warm-up | roster |
|---|---|---|---|---|---|
| primary_4h | primary: the sole verdict population | 4h | 2025-09-01 to 2026-11-01 | 300 bars | HYPE, XRP, NEAR, PUMP, kPEPE, ENA, UNI, AVAX, WLD, DOGE |
| forward_4h | descriptive: original coins on bars after every original window; contributes nothing to the verdict | 4h | 2026-10-01 to 2026-11-01 | 300 bars | BTC, ETH, SOL |
| descriptive_1h | descriptive: the 1h arm; contributes nothing to the verdict | 1h | 2026-06-01 to 2026-11-01 | 300 bars | HYPE, XRP, NEAR, PUMP, kPEPE, ENA, UNI, AVAX, WLD, DOGE, BTC, ETH, SOL |

The primary window repeats the original venue test calendar from 2025-09-01 to 2026-09-01 (12 of its 14 months) on different coins, and its warm-up reads July and August 2025 bars that fall in the original train calendar; crypto returns are correlated across coins, so the confirmation is cross-sectional and not independent in time.

## Inputs

| population | dataset | size decimals | half spread bps | candles complete | warm-up range | funding hours | candles sha256 | funding sha256 |
|---|---|---|---|---|---|---|---|---|
| primary_4h | AVAX 4h | 2 | 0.266 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `6162e90663bcb096` | `9477bb407656f75f` |
| primary_4h | DOGE 4h | 0 | 0.2475 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `7444a57e0cbd078b` | `86ea8ebc4ae46982` |
| primary_4h | ENA 4h | 0 | 1.2398 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `4a604f2de5caeb9d` | `c00c4015a806f806` |
| primary_4h | HYPE 4h | 2 | 1.1337 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `16f3181d31bfcb2c` | `65f1de6a9770ab14` |
| primary_4h | NEAR 4h | 1 | 0.317 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `ca670730cc7dbcda` | `0c0e8df6be23b772` |
| primary_4h | PUMP 4h | 0 | 1.6375 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `0a146c19cffddefb` | `8a51484a4e4f1bdf` |
| primary_4h | UNI 4h | 1 | 0.4951 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `de01853c604751c5` | `6af1a6535c79e333` |
| primary_4h | WLD 4h | 1 | 0.9379 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `f5136a6f8d6af0a8` | `43d1d23efb944af9` |
| primary_4h | XRP 4h | 0 | 0.0519 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `3f0a5f39062d8837` | `580e2599b02f2f60` |
| primary_4h | kPEPE 4h | 0 | 5.0163 | yes | 2025-07-13 00:00:00 to 2025-08-31 20:00:00 | 10224/10224 | `6f9c6d612ccf24d8` | `f73517aa710aed17` |
| forward_4h | BTC 4h | 5 | 0.0759 | yes | 2026-08-12 00:00:00 to 2026-09-30 20:00:00 | 744/744 | `7764ca98d0a78f3d` | `c35aca259a13fbfb` |
| forward_4h | ETH 4h | 4 | 0.2707 | yes | 2026-08-12 00:00:00 to 2026-09-30 20:00:00 | 744/744 | `3a99226a9afb7710` | `6d51304affd576af` |
| forward_4h | SOL 4h | 2 | 0.0685 | yes | 2026-08-12 00:00:00 to 2026-09-30 20:00:00 | 744/744 | `8ce4b105ec03c242` | `ba288b82d48187ab` |
| descriptive_1h | AVAX 1h | 2 | 0.266 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `fea76d5b8b5cdc86` | `9477bb407656f75f` |
| descriptive_1h | BTC 1h | 5 | 0.0759 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `642a8f0137451f35` | `c35aca259a13fbfb` |
| descriptive_1h | DOGE 1h | 0 | 0.2475 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `b1bd267fed2fc8a7` | `86ea8ebc4ae46982` |
| descriptive_1h | ENA 1h | 0 | 1.2398 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `20da6559842a5661` | `c00c4015a806f806` |
| descriptive_1h | ETH 1h | 4 | 0.2707 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `e79efcfd376615ad` | `6d51304affd576af` |
| descriptive_1h | HYPE 1h | 2 | 1.1337 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `470dfbc3226cbb8a` | `65f1de6a9770ab14` |
| descriptive_1h | NEAR 1h | 1 | 0.317 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `12c2149cdd33381f` | `0c0e8df6be23b772` |
| descriptive_1h | PUMP 1h | 0 | 1.6375 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `20f9a0b922d004eb` | `8a51484a4e4f1bdf` |
| descriptive_1h | SOL 1h | 2 | 0.0685 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `8f0053fbc46b3119` | `ba288b82d48187ab` |
| descriptive_1h | UNI 1h | 1 | 0.4951 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `b78fe6e070d4b7aa` | `6af1a6535c79e333` |
| descriptive_1h | WLD 1h | 1 | 0.9379 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `f8bab79968768901` | `43d1d23efb944af9` |
| descriptive_1h | XRP 1h | 0 | 0.0519 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `c9b1441a5741cd19` | `580e2599b02f2f60` |
| descriptive_1h | kPEPE 1h | 0 | 5.0163 | yes | 2026-05-19 12:00:00 to 2026-05-31 23:00:00 | 3672/3672 | `abafff33d94ea3ac` | `f73517aa710aed17` |

Every file and its full sha256, row count, scored range and warm-up range is in `study_manifest.json` (42 files, acquired 2026-11-14T17:43:06Z).

## Primary population (verdict)

Evidence complete: yes.

Pooled candidate base-cost clusters: **73** (floor 30), merged from position spans across all primary coins. Per-coin clusters: AVAX 4h 17, DOGE 4h 17, ENA 4h 9, HYPE 4h 10, NEAR 4h 15, PUMP 4h 11, UNI 4h 19, WLD 4h 11, XRP 4h 14, kPEPE 4h 20 (sum 143, which is never used as the pooled total).

| check | value |
|---|---|
| net_positive_base | false |
| net_positive_stress | false |
| m1_pass_base | false |
| beats_breakout_sharpe_base | false |
| beats_session_breakout_sharpe_base | true |

### Cost base (x1)

| arm | datasets | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | pooled clusters | summed per-coin clusters | fees $ | funding $ | liquidated legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| breakout | 10 | -0.106 | -14.41 | -1,441.75 | -61.65 | 1041 (541/500) | 188 | 935 | 979.71 | -83.41 | 0 |
| candidate | 10 | -0.156 | -4.33 | -434.36 | -33.55 | 143 (61/82) | 73 | 143 | 121.46 | -6.44 | 0 |
| session_breakout | 10 | -0.186 | -23.50 | -2,351.73 | -73.23 | 1707 (845/862) | 125 | 1594 | 1,411.98 | -89.45 | 0 |

M1 verdict `fail`: candidate mean Sharpe -0.156 vs incumbent median 0.000; candidate mean drawdown-adjusted return 0.122 vs -0.118; traded datasets 10 of 10.

| dataset | incumbent median Sharpe | incumbent median DDadj | incumbents |
|---|---|---|---|
| AVAX 4h | 0.050 | 0.022 | 8 |
| DOGE 4h | 0.169 | 0.113 | 8 |
| ENA 4h | 0.057 | -0.060 | 8 |
| HYPE 4h | -0.107 | -0.221 | 8 |
| NEAR 4h | 0.230 | 0.095 | 8 |
| PUMP 4h | -0.288 | -0.483 | 8 |
| UNI 4h | 0.001 | -0.095 | 8 |
| WLD 4h | -0.062 | -0.246 | 8 |
| XRP 4h | -0.086 | -0.220 | 8 |
| kPEPE 4h | 0.040 | -0.083 | 8 |

Candidate per dataset, cost base:

| dataset | Sharpe | return % | net PnL $ | max DD % | positions (L/S) | clusters | fees $ | funding $ | rejected entries |
|---|---|---|---|---|---|---|---|---|---|
| AVAX 4h | -0.599 | -15.90 | -159.03 | -23.16 | 17 (5/12) | 17 | 13.93 | -3.44 | 0 |
| DOGE 4h | 0.788 | 15.76 | 156.41 | -10.11 | 17 (7/10) | 17 | 15.33 | -0.24 | 0 |
| ENA 4h | 0.142 | 0.80 | 8.03 | -18.52 | 9 (5/4) | 9 | 8.46 | -0.34 | 0 |
| HYPE 4h | 0.710 | 11.26 | 112.57 | -5.93 | 10 (5/5) | 10 | 9.20 | -0.08 | 0 |
| NEAR 4h | 0.275 | 4.21 | 42.07 | -11.74 | 15 (7/8) | 15 | 13.11 | -1.22 | 0 |
| PUMP 4h | -0.133 | -4.49 | -44.86 | -18.34 | 11 (5/6) | 11 | 9.66 | -1.24 | 0 |
| UNI 4h | 0.764 | 21.66 | 216.62 | -20.51 | 19 (7/12) | 19 | 17.37 | 5.14 | 0 |
| WLD 4h | -1.107 | -30.10 | -301.01 | -33.55 | 11 (5/6) | 11 | 7.88 | -1.74 | 0 |
| XRP 4h | -1.528 | -18.76 | -187.57 | -21.76 | 14 (4/10) | 14 | 11.10 | -0.21 | 0 |
| kPEPE 4h | -0.874 | -27.76 | -277.59 | -27.76 | 20 (11/9) | 20 | 15.41 | -3.07 | 0 |

### Cost stress (x2)

| arm | datasets | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | pooled clusters | summed per-coin clusters | fees $ | funding $ | liquidated legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| breakout | 10 | -0.305 | -22.87 | -2,288.02 | -63.22 | 1041 (541/500) | 194 | 939 | 934.62 | -73.39 | 0 |
| candidate | 10 | -0.245 | -6.88 | -689.56 | -33.74 | 143 (61/82) | 72 | 143 | 119.78 | -6.99 | 0 |
| session_breakout | 10 | -0.451 | -35.32 | -3,533.74 | -78.77 | 1707 (845/862) | 127 | 1595 | 1,314.08 | -84.67 | 0 |

M1 verdict `fail`: candidate mean Sharpe -0.245 vs incumbent median -0.072; candidate mean drawdown-adjusted return -0.017 vs -0.221; traded datasets 10 of 10.

| dataset | incumbent median Sharpe | incumbent median DDadj | incumbents |
|---|---|---|---|
| AVAX 4h | -0.057 | -0.166 | 8 |
| DOGE 4h | 0.092 | 0.032 | 8 |
| ENA 4h | 0.049 | -0.073 | 8 |
| HYPE 4h | -0.131 | -0.254 | 8 |
| NEAR 4h | 0.201 | 0.028 | 8 |
| PUMP 4h | -0.389 | -0.518 | 8 |
| UNI 4h | -0.166 | -0.285 | 8 |
| WLD 4h | -0.134 | -0.406 | 8 |
| XRP 4h | -0.137 | -0.318 | 8 |
| kPEPE 4h | -0.047 | -0.246 | 8 |

Candidate per dataset, cost stress:

| dataset | Sharpe | return % | net PnL $ | max DD % | positions (L/S) | clusters | fees $ | funding $ | rejected entries |
|---|---|---|---|---|---|---|---|---|---|
| AVAX 4h | -0.635 | -16.78 | -167.82 | -23.63 | 17 (5/12) | 17 | 13.86 | -3.19 | 0 |
| DOGE 4h | 0.727 | 14.25 | 140.78 | -10.31 | 17 (7/10) | 17 | 15.24 | -0.23 | 0 |
| ENA 4h | 0.114 | -0.02 | -0.18 | -18.68 | 9 (5/4) | 9 | 8.43 | -0.29 | 0 |
| HYPE 4h | 0.656 | 10.24 | 102.35 | -6.00 | 10 (5/5) | 10 | 9.16 | -0.08 | 0 |
| NEAR 4h | 0.226 | 3.02 | 30.22 | -11.93 | 15 (7/8) | 15 | 13.05 | -1.21 | 0 |
| PUMP 4h | -0.183 | -5.48 | -54.79 | -18.84 | 11 (5/6) | 11 | 9.62 | -1.23 | 0 |
| UNI 4h | 0.309 | 5.65 | 56.53 | -20.82 | 19 (7/12) | 19 | 16.29 | 3.95 | 0 |
| WLD 4h | -1.131 | -30.66 | -306.64 | -33.74 | 11 (5/6) | 11 | 7.85 | -1.74 | 0 |
| XRP 4h | -1.580 | -19.42 | -194.24 | -22.29 | 14 (4/10) | 14 | 11.06 | -0.21 | 0 |
| kPEPE 4h | -0.949 | -29.58 | -295.77 | -29.58 | 20 (11/9) | 20 | 15.22 | -2.76 | 0 |

## Descriptive population `descriptive_1h` (1h, 2026-06-01 to 2026-11-01)

Role: descriptive: the 1h arm; contributes nothing to the verdict. Evidence complete: yes.

Cost base:

| arm | datasets | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | pooled clusters | summed per-coin clusters | fees $ | funding $ | liquidated legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| breakout | 13 | -1.781 | -28.48 | -3,703.88 | -73.04 | 1737 (896/841) | 117 | 1590 | 1,339.77 | -26.94 | 0 |
| candidate | 13 | 0.088 | 2.39 | 311.18 | -12.61 | 87 (45/42) | 40 | 87 | 79.39 | -1.34 | 0 |
| session_breakout | 13 | -1.484 | -24.10 | -3,136.19 | -62.64 | 1984 (1026/958) | 89 | 1865 | 1,470.16 | -37.58 | 0 |

M1 verdict `pass`: candidate mean Sharpe 0.088 vs incumbent median -0.849; candidate mean drawdown-adjusted return 0.675 vs -0.398; traded datasets 13 of 13.

Cost stress:

| arm | datasets | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | pooled clusters | summed per-coin clusters | fees $ | funding $ | liquidated legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| breakout | 13 | -2.697 | -39.68 | -5,159.64 | -75.61 | 1757 (903/854) | 124 | 1616 | 1,261.97 | -27.47 | 0 |
| candidate | 13 | -0.039 | 1.87 | 242.60 | -13.40 | 87 (45/42) | 40 | 87 | 79.19 | -1.24 | 0 |
| session_breakout | 13 | -2.570 | -37.18 | -4,838.13 | -71.46 | 2024 (1044/980) | 98 | 1912 | 1,385.82 | -36.45 | 0 |

M1 verdict `pass`: candidate mean Sharpe -0.039 vs incumbent median -1.496; candidate mean drawdown-adjusted return 0.577 vs -0.622; traded datasets 13 of 13.

Candidate per dataset, cost base:

| dataset | Sharpe | return % | net PnL $ | max DD % | positions (L/S) | clusters | fees $ | funding $ | rejected entries |
|---|---|---|---|---|---|---|---|---|---|
| AVAX 1h | -1.209 | -6.01 | -60.12 | -6.38 | 8 (3/5) | 8 | 7.06 | -0.44 | 0 |
| BTC 1h | -3.468 | -8.38 | -83.77 | -8.38 | 9 (4/5) | 9 | 7.79 | -0.24 | 0 |
| DOGE 1h | 0.735 | 4.24 | 42.40 | -5.80 | 10 (5/5) | 10 | 9.27 | 0.12 | 0 |
| ENA 1h | 2.116 | 15.30 | 153.01 | -5.33 | 5 (4/1) | 5 | 4.80 | -0.26 | 0 |
| ETH 1h | 1.834 | 9.83 | 98.29 | -4.66 | 6 (3/3) | 6 | 5.71 | 0.31 | 0 |
| HYPE 1h | 1.275 | 4.38 | 43.77 | -2.43 | 3 (2/1) | 3 | 2.73 | -0.02 | 0 |
| NEAR 1h | 2.011 | 15.50 | 154.99 | -5.49 | 6 (4/2) | 6 | 5.82 | -0.98 | 0 |
| PUMP 1h | -0.193 | -1.31 | -13.11 | -7.33 | 5 (2/3) | 5 | 4.35 | -0.12 | 0 |
| SOL 1h | 0.045 | -0.06 | -0.56 | -9.42 | 8 (2/6) | 8 | 7.45 | -0.14 | 0 |
| UNI 1h | 2.089 | 12.43 | 124.32 | -5.65 | 8 (5/3) | 8 | 7.71 | 0.09 | 0 |
| WLD 1h | -1.765 | -4.03 | -40.29 | -4.29 | 2 (1/1) | 2 | 1.78 | -0.04 | 0 |
| XRP 1h | 0.374 | 1.76 | 17.57 | -5.79 | 8 (4/4) | 8 | 7.14 | 0.57 | 0 |
| kPEPE 1h | -2.705 | -12.53 | -125.32 | -12.61 | 9 (6/3) | 9 | 7.78 | -0.17 | 0 |

## Descriptive population `forward_4h` (4h, 2026-10-01 to 2026-11-01)

Role: descriptive: original coins on bars after every original window; contributes nothing to the verdict. Evidence complete: yes.

Cost base:

| arm | datasets | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | pooled clusters | summed per-coin clusters | fees $ | funding $ | liquidated legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| breakout | 3 | -1.218 | -1.36 | -42.68 | -9.39 | 23 (6/17) | 8 | 20 | 20.78 | 2.84 | 0 |
| candidate | 3 | 2.479 | 2.94 | 87.27 | -4.26 | 5 (0/5) | 2 | 4 | 4.55 | 1.08 | 0 |
| session_breakout | 3 | -1.412 | -4.49 | -136.64 | -11.34 | 37 (9/28) | 7 | 31 | 32.33 | 4.46 | 0 |

M1 verdict `pass`: candidate mean Sharpe 2.479 vs incumbent median -1.570; candidate mean drawdown-adjusted return 2.390 vs -0.334; traded datasets 3 of 3.

Cost stress:

| arm | datasets | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | pooled clusters | summed per-coin clusters | fees $ | funding $ | liquidated legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| breakout | 3 | -1.468 | -1.81 | -57.15 | -9.78 | 23 (6/17) | 8 | 20 | 20.74 | 2.83 | 0 |
| candidate | 3 | 2.372 | 2.82 | 83.22 | -4.41 | 5 (0/5) | 2 | 4 | 4.55 | 1.08 | 0 |
| session_breakout | 3 | -1.864 | -5.70 | -173.65 | -12.77 | 38 (10/28) | 7 | 32 | 32.99 | 3.96 | 0 |

M1 verdict `pass`: candidate mean Sharpe 2.372 vs incumbent median -1.623; candidate mean drawdown-adjusted return 2.318 vs -0.335; traded datasets 3 of 3.

Candidate per dataset, cost base:

| dataset | Sharpe | return % | net PnL $ | max DD % | positions (L/S) | clusters | fees $ | funding $ | rejected entries |
|---|---|---|---|---|---|---|---|---|---|
| BTC 4h | 1.075 | 0.48 | 3.88 | -0.81 | 1 (0/1) | 1 | 0.90 | 0.50 | 0 |
| ETH 4h | 5.052 | 6.60 | 66.03 | -1.07 | 1 (0/1) | 1 | 0.87 | 0.30 | 0 |
| SOL 4h | 1.309 | 1.74 | 17.36 | -4.26 | 3 (0/3) | 2 | 2.78 | 0.28 | 0 |

## Entry audit

Primary candidate base-cost entries audited: 143; failures: 0. Each fill must follow a valid raw signal on the prior closed bar, enter inside the window (never in the warm-up) and exit before the window end. First entries:

| dataset | side | signal bar | fill bar | exit bar | anchor | upper | lower | close |
|---|---|---|---|---|---|---|---|---|
| HYPE 4h | short | 2025-10-10 20:00:00 | 2025-10-11 00:00:00 | 2025-10-11 04:00:00 | 43.976 | 51.5 | 38.333 | 37.366 |
| HYPE 4h | short | 2025-11-21 04:00:00 | 2025-11-21 08:00:00 | 2025-11-21 12:00:00 | 37.549 | 41.735 | 34.4095 | 34.272 |
| HYPE 4h | short | 2026-01-20 20:00:00 | 2026-01-21 00:00:00 | 2026-01-21 04:00:00 | 23.727 | 26.879 | 21.363 | 20.978 |
| HYPE 4h | long | 2026-01-27 20:00:00 | 2026-01-28 00:00:00 | 2026-01-28 04:00:00 | 24.929 | 29.393 | 21.581 | 30.814 |
| HYPE 4h | short | 2026-02-23 16:00:00 | 2026-02-23 20:00:00 | 2026-02-24 16:00:00 | 29.008 | 32.505 | 26.3852 | 26.284 |
| HYPE 4h | long | 2026-03-09 16:00:00 | 2026-03-09 20:00:00 | 2026-03-11 16:00:00 | 30.687 | 34.067 | 28.152 | 34.105 |
| HYPE 4h | long | 2026-05-14 16:00:00 | 2026-05-14 20:00:00 | 2026-05-14 20:00:00 | 38.817 | 44.414 | 34.6193 | 44.653 |
| HYPE 4h | short | 2026-07-16 16:00:00 | 2026-07-16 20:00:00 | 2026-07-17 04:00:00 | 66.86 | 72.779 | 62.4207 | 62.264 |
| HYPE 4h | long | 2026-08-19 16:00:00 | 2026-08-19 20:00:00 | 2026-08-19 20:00:00 | 58.483 | 63.635 | 54.619 | 70.27 |
| HYPE 4h | long | 2026-10-29 08:00:00 | 2026-10-29 12:00:00 | 2026-10-29 16:00:00 | 80.565 | 87.41 | 75.4313 | 87.993 |
| XRP 4h | short | 2025-09-22 04:00:00 | 2025-09-22 08:00:00 | 2025-09-22 12:00:00 | 2.9708 | 3.1418 | 2.84255 | 2.8054 |
| XRP 4h | short | 2025-10-10 20:00:00 | 2025-10-11 00:00:00 | 2025-10-12 16:00:00 | 2.8029 | 3.0974 | 2.58203 | 2.3637 |

## Verification

`run_study.py verify` ran the real loader, evaluator and verdict paths on disposable copies and on saved leg evidence (all checks passed: yes; results sha256 `1192865ec839c4c4`). Fixture rows test verdict branches and are not market evidence.

| case | kind | expected | observed | ok |
|---|---|---|---|---|
| canonical lock order in branch history | git | lock committed once, before results, sources clean | {"lock_commit": "f4d188bef7556a0e3aa7908b7b3ca941e22346ae", "head": "f4d188bef7556a0e3aa7908b7b3ca941e22346ae"} | yes |
| screen replay from the frozen liquidity response and coverage records | frozen inputs | ranking order, stop rule and selection reproduce | {"order_ok": true, "recheck": {"AVAX": {"complete": true, "fetched_hashes_match": true}, "DOGE": {"complete": true, "fetched_hashes_match": true}, "ENA": {"complete": true, "fetched_hashes_match": true}, "HYPE": {"comple... | yes |
| changed protocol hash | refusal | refused (protocol.json differs from the lock) | protocol.json differs from the lock | yes |
| changed input inventory hash | refusal | refused (differs from the lock) | study_manifest.json (input inventory) differs from the lock | yes |
| changed input file hash | refusal | refused (registered input data/venue/HYPE_funding.csv.gz) | registered input data/venue/HYPE_funding.csv.gz sha256 b653dc59c26e5dadbb74a8af9aeefac8441837ea8145bee68b9cc7b225d7ab8c != inventory 65f1de6a9770ab14b6daea30d082217abca7bb51e6655c5922041411c34e5968 | yes |
| unregistered input file | refusal | refused (unregistered files under data/) | unregistered files under data/: data/venue/EXTRA_4h_candles.csv.gz | yes |
| missing lock | refusal | refused (protocol_lock.json is missing) | protocol_lock.json is missing; run `run_study.py lock` before scoring | yes |
| changed scoring source hash | refusal | refused (scoring sources differ from the lock) | scoring sources differ from the lock (check out the lock's source commit to reproduce): backtest/eval_windows.py | yes |
| changed manifest | refusal | refused (differs from the inventory) | primary_4h: manifest venue_primary_4h.json differs from the inventory | yes |
| lock written after scores | refusal | refused (results exist; the lock must precede every score) | results exist; the lock must precede every score | yes |
| existing research cache | refusal | refused (refusing to reuse an existing research cache) | refusing to reuse an existing research cache /private/tmp/claude-501/-Users-richardkuo-Work-go-trader/ad682292-f47e-4c7c-a006-3e8625d153af/scratchpad/dt2019_verify1/caches/reused.db; pass a new path | yes |
| missing registered dataset | real runner on a disposable copy | inconclusive (dataset roster differs) | inconclusive: evidence incomplete: dataset roster differs from the frozen roster (missing ['DOGE'], unexpected []); base candidate: no legs; base breakout: no legs; base session_breakout: no legs; base momentum_pro: no l... | yes |
| same-size roster with a wrong coin | real runner on a disposable copy | inconclusive (dataset roster differs) | inconclusive: evidence incomplete: dataset roster differs from the frozen roster (missing ['DOGE'], unexpected ['BTC']); base candidate: no legs; base breakout: no legs; base session_breakout: no legs; base momentum_pro:... | yes |
| missing funding series | real runner on a disposable copy | inconclusive (no funding series attached) | inconclusive: evidence incomplete: HYPE 4h funding: no funding series attached; base candidate: no legs; base breakout: no legs; base session_breakout: no legs; base momentum_pro: no legs; base chart_pattern: no legs; ba... | yes |
| interior funding gap | real runner on a disposable copy | inconclusive (funding hours missing) | inconclusive: evidence incomplete: HYPE 4h funding: 1 funding hours missing (first 2026-04-02 00:00:00); base candidate: no legs; base breakout: no legs; base session_breakout: no legs; base momentum_pro: no legs; base c... | yes |
| malformed candle | real runner on a disposable copy | inconclusive (invalid OHLCV bars) | inconclusive: evidence incomplete: HYPE 4h candles: 1 invalid OHLCV bars (first 2025-09-07 16:00:00); base candidate: no legs; base breakout: no legs; base session_breakout: no legs; base momentum_pro: no legs; base char... | yes |
| warm-up gap | real runner on a disposable copy | inconclusive (warm-up bars missing) | inconclusive: evidence incomplete: HYPE 4h candles: 1 warm-up bars missing (first 2025-08-23 16:00:00); base candidate: no legs; base breakout: no legs; base session_breakout: no legs; base momentum_pro: no legs; base ch... | yes |
| verdict recomputed from saved leg evidence | saved evidence | fail | fail: false: beats_breakout_sharpe_base, m1_pass_base, net_positive_base, net_positive_stress | yes |
| missing incumbent leg at base cost | saved evidence | inconclusive (base momentum_pro AVAX 4h: leg missing) | inconclusive: evidence incomplete: base momentum_pro: datasets ['DOGE 4h', 'ENA 4h', 'HYPE 4h', 'NEAR 4h', 'PUMP 4h', 'UNI 4h', 'WLD 4h', 'XRP 4h', 'kPEPE 4h'] != frozen ['AVAX 4h', 'DOGE 4h', 'ENA 4h', 'HYPE 4h', 'NEAR ... | yes |
| missing incumbent leg at stress cost | saved evidence | inconclusive (stress momentum_pro AVAX 4h: leg missing) | inconclusive: evidence incomplete: stress momentum_pro: datasets ['DOGE 4h', 'ENA 4h', 'HYPE 4h', 'NEAR 4h', 'PUMP 4h', 'UNI 4h', 'WLD 4h', 'XRP 4h', 'kPEPE 4h'] != frozen ['AVAX 4h', 'DOGE 4h', 'ENA 4h', 'HYPE 4h', 'NEA... | yes |
| errored candidate leg | saved evidence | inconclusive (TimeoutError) | inconclusive: evidence incomplete: base candidate AVAX 4h: TimeoutError: fixture | yes |
| non-finite candidate metric | saved evidence | inconclusive (non-finite ['sharpe']) | inconclusive: evidence incomplete: stress candidate AVAX 4h: non-finite ['sharpe'] | yes |
| unexpected comparator dataset | saved evidence | inconclusive (!= frozen) | inconclusive: evidence incomplete: base breakout: datasets ['AVAX 4h', 'DOGE 4h', 'ENA 4h', 'HYPE 4h', 'NEAR 4h', 'PUMP 4h', 'UNI 4h', 'WLD 4h', 'XRP 4h', 'ZZZ 4h', 'kPEPE 4h'] != frozen ['AVAX 4h', 'DOGE 4h', 'ENA 4h', ... | yes |
| incumbent funding coverage incomplete | saved evidence | inconclusive (funding coverage incomplete) | inconclusive: evidence incomplete: base sma_crossover AVAX 4h: funding coverage incomplete | yes |
| entry audit failure | saved evidence | inconclusive (entry audit) | inconclusive: evidence incomplete: candidate entry audit: 1 failures | yes |
| incumbent arm absent | saved evidence | inconclusive (chart_pattern: no legs) | inconclusive: evidence incomplete: base chart_pattern: no legs | yes |
| valid sample below the floor (positions removed) | saved evidence | inconclusive (< 30) | inconclusive: pooled candidate base-cost clusters 29 < 30 | yes |
| all frozen checks true | isolated verdict fixture (not market evidence) | pass (every frozen check is true) | pass: every frozen check is true | yes |
| performance failure: net_positive_base | isolated verdict fixture (not market evidence) | fail (net_positive_base) | fail: false: net_positive_base | yes |
| performance failure: net_positive_stress | isolated verdict fixture (not market evidence) | fail (net_positive_stress) | fail: false: net_positive_stress | yes |
| performance failure: m1_pass_base | isolated verdict fixture (not market evidence) | fail (m1_pass_base) | fail: false: m1_pass_base | yes |
| performance failure: beats_breakout_sharpe_base | isolated verdict fixture (not market evidence) | fail (beats_breakout_sharpe_base) | fail: false: beats_breakout_sharpe_base | yes |
| performance failure: beats_session_breakout_sharpe_base | isolated verdict fixture (not market evidence) | fail (beats_session_breakout_sharpe_base) | fail: false: beats_session_breakout_sharpe_base | yes |
| all checks true but a stress incumbent leg missing | isolated verdict fixture (not market evidence) | inconclusive (leg missing) | inconclusive: evidence incomplete: stress range_scalper: datasets ['DOGE 4h', 'ENA 4h', 'HYPE 4h', 'NEAR 4h', 'PUMP 4h', 'UNI 4h', 'WLD 4h', 'XRP 4h', 'kPEPE 4h'] != frozen ['AVAX 4h', 'DOGE 4h', 'ENA 4h', 'HYPE 4h', 'NE... | yes |
| all checks true but an input check failed | isolated verdict fixture (not market evidence) | inconclusive (funding hours missing) | inconclusive: evidence incomplete: AVAX 4h funding: 1 funding hours missing (fixture) | yes |
| fixture provenance | note | - | saved evidence; 0 fixture positions added | yes |
| span bridge: pooled clusters differ from summed per-coin clusters | synthetic spans | per-coin 1+1+1=3, pooled 1 (overlap and touching spans merge) | per-coin [1, 1, 1] sum 3, pooled 1 | yes |
| scored primary: pooled vs summed per-coin clusters | saved evidence | pooled <= summed, recomputed independently | pooled 73, summed 143 | yes |
| descriptive outputs replaced: primary verdict unchanged | saved evidence | fail | fail | yes |
| forward population roster changed: primary evidence and verdict unchanged | real runner on a disposable copy | identical primary legs and verdict; forward evidence incomplete | primary identical True; verdict fail; forward evidence complete False | yes |
| full reproduction with a separate fresh research cache | real runner on the canonical study | identical populations, legs, reductions and verdict (timing excluded) | identical True; rerun verdict fail | yes |

## Limits

- The roster comes from one 24h notional volume snapshot taken after the scored window, so it favours coins that are liquid and listed today (survivorship). The screen never read strategy outcomes.
- The primary window repeats the original venue test calendar from 2025-09-01 to 2026-09-01 (12 of its 14 months) on different coins, and its warm-up reads July and August 2025 bars that fall in the original train calendar; crypto returns are correlated across coins, so the confirmation is cross-sectional and not independent in time.
- Pooling more coins can merge existing holding spans into one cluster, so more coins do not guarantee more clusters; the pooled count above is merged from spans and never summed.
- Spread is one frozen L2 snapshot per coin plus flat slippage; queue position, partial fills and market impact are not modelled. The venue serves only the most recent 5,000 candles per interval, so a later acquisition cannot reproduce the earliest bars; reproduction uses the committed files.
- The descriptive populations are short or use a different interval; they never enter the verdict.
- A research result alone cannot approve deployment.

## Reproduction

```
uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py acquire            # network; already done, refuses to overwrite frozen inputs
uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py lock               # already done; confirms the existing lock
uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py run --cache-db <fresh-research-path>/cache.db --out <path>/rerun.json
uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py verify --work-dir <fresh-empty-directory>
uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py render
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/dual_thrust_2019/venue_primary_4h.json
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/dual_thrust_2019/venue_forward_4h.json
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/dual_thrust_2019/venue_descriptive_1h.json
```
