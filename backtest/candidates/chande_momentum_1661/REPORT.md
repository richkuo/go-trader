# Chande Momentum Oscillator study (issue 1661)

**Verdict: FAIL.** Registry metadata after this study: `edge_status="no_edge"`, `edge_source="study_fail"`, `edge_ref` = this file. The candidate stays hidden from discovery. Explicit paper mode is admitted; live use still needs `allow_no_edge: true`, and no result here adds one. Every command in this study is suggest-only and sends no order.

## Signal

`chande_momentum` computes `100 * (G - L) / (G + L)` over `period` unsmoothed close changes, where G sums the up changes and L sums the absolute down changes. It enters long when the value rises strictly above `threshold` from at or below it, and short when it falls strictly below `-threshold` from at or above it. A flat window (G + L = 0) reads 0 and fires nothing on that bar. Each decision reads exactly `period + 2` closes; every close must be finite and positive and every step inside both windows must equal the fixed `expected_interval_ms`, else the entry holds with a reason and close/stop management continues.

For a nonzero denominator the value equals `2 * RSI_sma - 100`, where `RSI_sma` uses simple averages of the gains and losses over the window. The registry `rsi` strategy is different: it uses recursive Wilder averages (`indicators_core.wilder_rsi`) that carry state from all earlier bars, and it trades reversion crossings of the oversold and overbought levels. `chande_momentum` trades momentum crossings and its value depends only on its bounded window.

## Protocol

`protocol.json` (sha256 `f8faab5d54af4e993ed9f31dc79a8b89e83d2fc7535908ca4f9e23f0daefe2dc`) was written and frozen before any held-out window was scored. It fixes the grids, the selection rule on the `is` window, the confirmation windows, the sample floor, the funding rule and the pass/fail/inconclusive rules. Selection read only `is`; the stability sweep ran after confirmation and does not change the verdict.

Amendment made before any scoring: Acquisition found no Hyperliquid funding rate before the venue's first record (2023-05-12 for BTC), so legs holding a position in early 2023 are funding-incomplete under charge. Before any scoring, the verdict rule changed from 'any incomplete confirmation leg gives inconclusive' to the three-valued rule below, so incomplete funding can never support a pass but complete windows can still establish a fail.

| window | start | end (exclusive) | role |
|---|---|---|---|
| is | 2025-06-10 | 2026-01-01 | training/selection |
| oos | 2026-01-01 | 2026-10-01 | protocol out-of-sample |
| 2023 | 2023-01-01 | 2024-01-01 | held-out |
| 2024 | 2024-01-01 | 2025-01-01 | held-out |
| 2025H1 | 2025-01-01 | 2025-07-01 | held-out |

These are the M1 window definitions, with the open-ended `oos` end pinned to 2026-10-01. The M1 `is` window (from 2025-06-10) overlaps the last three weeks of `2025H1`; the `2025H1` result is therefore not fully independent of selection.

Exits for the candidate and both matched-exit comparators: one `tiered_tp_pct` tier at +3% closing the whole position, plus the single stop owner `stop_loss_atr_mult: 1.0`, `direction: both`, `intrabar_resolution: ohlc_walk`, funding `charge`, strict close capability checks. The M1 incumbent median uses the eight fixed incumbents with registry defaults and default exits, unchanged.

## Inputs, costs and their limits

- Candles: Binance US spot USDT pairs through the M1 non-manifest path. They proxy Hyperliquid perpetual prices; basis and venue-specific wicks are not modeled.
- Funding: Hyperliquid hourly funding through the shared funding helper, charged on every held hour (`--funding charge`).
- Fees: the engine's Hyperliquid fee model (commission_pct `0.00045` per side as reported by the Backtester). Slippage: engine default `0.0005` per fill, which stands in for the spread; no order book was replayed.
- Capital 1000.0 USD per leg; notionals below are the engine's actual entry notionals. Lot size, size decimals and the venue minimum notional are not modeled.
- Frozen inputs: `study_manifest.json` (acquired 2026-10-11T03:16:15Z from commit `0715416732ccb93275e81e3e7a096d887a1bdfed`); `run_study.py run` rebuilds a fresh cache from `data/`, checks every sha256 and disables network acquisition.

| file | rows | sha256 |
|---|---|---|
| `data/BTC_1h_candles.csv.gz` | 32840 | `d107c60f72363372d03273ba6439ca42a0d867a9089c5de171e9306af3ba2d17` |
| `data/BTC_4h_candles.csv.gz` | 8211 | `dd0758a2862955eb53b060df5fbace3e98f8bc2a3e0254842bca0d36c96f106f` |
| `data/BTC_funding.csv.gz` | 29142 | `520fd979d5594b4c2fb950cd52aa327749a124d9971a59ec3419752dd24a27f8` |
| `data/ETH_1h_candles.csv.gz` | 32840 | `59d08735aecb22b63bab8189c7c32b11101e80da42b2ec724f39e308548b7913` |
| `data/ETH_4h_candles.csv.gz` | 8211 | `03df63c5dc65ba532a252e8c00118986321fcb124934bb7536e842d678fc9dd8` |
| `data/ETH_funding.csv.gz` | 29142 | `4b7694d8fc26cb7245ffa882cbdcb2d571d17f3a9f1abd6832fb0deed4985b41` |
| `data/SOL_1h_candles.csv.gz` | 32840 | `e4e5c15ffcce696253d78dc3b3c3448725e8cf3a0114a9fbc4b1df5f2a0efeec` |
| `data/SOL_4h_candles.csv.gz` | 8211 | `c1e5775973d00b0b07597da3db4213d62378ef566fe2e4a3b619612b59abc8ab` |
| `data/SOL_funding.csv.gz` | 29142 | `b29de7625f7484f0971336e31ba52f5b042127e4eef4d8ee1e4554994383cd2b` |

Input gaps: BTC/USDT 1h: 2 gaps, 16 missing bars; BTC/USDT 4h: 2 gaps, 3 missing bars; BTC funding: 3144 hours before the first venue rate, 570 venue-absent hours; ETH/USDT 1h: 2 gaps, 16 missing bars; ETH/USDT 4h: 2 gaps, 3 missing bars; ETH funding: 3144 hours before the first venue rate, 570 venue-absent hours; SOL/USDT 1h: 2 gaps, 16 missing bars; SOL/USDT 4h: 2 gaps, 3 missing bars; SOL funding: 3144 hours before the first venue rate, 570 venue-absent hours. The candidate holds its entry for the bars whose windows contain a candle gap; the engine still manages open positions.

## 1h group (expected_interval_ms = 3600000)

### Training selection (`is`)

| strategy | selected params | how | eligible combos | is mean DDadj | is mean Sharpe |
|---|---|---|---|---|---|
| chande_momentum | period=20, threshold=10.0 | selected | 28 | 0.43 | 0.22 |
| momentum | roc_period=30, threshold=5.0 | selected | 7 | -0.05 | -0.44 |
| rsi | overbought=75, oversold=25, period=21 | selected | 27 | 0.24 | 0.07 |

Every training combination (unsuccessful variants included):

| strategy | params | M1 verdict | mean Sharpe | bar Sharpe | mean DDadj | bar DDadj | min trades |
|---|---|---|---|---|---|---|---|
| chande_momentum | period=10, threshold=10.0 | fail | -0.57 | -0.26 | -0.56 | -0.18 | 205 |
| chande_momentum | period=10, threshold=15.0 | fail | -0.86 | -0.26 | -0.54 | -0.18 | 235 |
| chande_momentum | period=10, threshold=25.0 | fail | -1.89 | -0.26 | -0.89 | -0.18 | 242 |
| chande_momentum | period=10, threshold=35.0 | fail | -1.96 | -0.26 | -0.94 | -0.18 | 227 |
| chande_momentum | period=10, threshold=50.0 | fail | -2.31 | -0.26 | -0.89 | -0.18 | 186 |
| chande_momentum | period=14, threshold=10.0 | fail | -1.43 | -0.26 | -0.68 | -0.18 | 226 |
| chande_momentum | period=14, threshold=15.0 | fail | -1.24 | -0.26 | -0.69 | -0.18 | 232 |
| chande_momentum | period=14, threshold=25.0 | fail | -1.49 | -0.26 | -0.78 | -0.18 | 231 |
| chande_momentum | period=14, threshold=35.0 | fail | -2.20 | -0.26 | -0.87 | -0.18 | 186 |
| chande_momentum | period=14, threshold=50.0 | fail | -2.09 | -0.26 | -0.90 | -0.18 | 128 |
| chande_momentum | period=20, threshold=10.0 | pass | 0.22 | -0.26 | 0.43 | -0.18 | 219 |
| chande_momentum | period=20, threshold=15.0 | fail | -0.62 | -0.26 | -0.41 | -0.18 | 235 |
| chande_momentum | period=20, threshold=25.0 | fail | -1.21 | -0.26 | -0.67 | -0.18 | 199 |
| chande_momentum | period=20, threshold=35.0 | fail | -1.96 | -0.26 | -0.88 | -0.18 | 139 |
| chande_momentum | period=20, threshold=50.0 | fail | -1.02 | -0.26 | -0.70 | -0.18 | 64 |
| chande_momentum | period=30, threshold=10.0 | pass | -0.22 | -0.26 | -0.10 | -0.18 | 206 |
| chande_momentum | period=30, threshold=15.0 | fail | -0.87 | -0.26 | -0.66 | -0.18 | 181 |
| chande_momentum | period=30, threshold=25.0 | fail | -1.29 | -0.26 | -0.64 | -0.18 | 152 |
| chande_momentum | period=30, threshold=35.0 | fail | -1.42 | -0.26 | -0.70 | -0.18 | 93 |
| chande_momentum | period=30, threshold=50.0 | fail | -1.61 | -0.26 | -0.50 | -0.18 | 20 |
| chande_momentum | period=40, threshold=10.0 | fail | -0.64 | -0.26 | -0.33 | -0.18 | 169 |
| chande_momentum | period=40, threshold=15.0 | fail | -0.89 | -0.26 | -0.58 | -0.18 | 164 |
| chande_momentum | period=40, threshold=25.0 | fail | -1.08 | -0.26 | -0.68 | -0.18 | 120 |
| chande_momentum | period=40, threshold=35.0 | fail | -0.52 | -0.26 | 0.06 | -0.18 | 48 |
| chande_momentum | period=40, threshold=50.0 | fail | -1.64 | -0.26 | -0.66 | -0.18 | 6 |
| chande_momentum | period=60, threshold=10.0 | fail | -0.70 | -0.26 | -0.55 | -0.18 | 143 |
| chande_momentum | period=60, threshold=15.0 | fail | -1.19 | -0.26 | -0.70 | -0.18 | 126 |
| chande_momentum | period=60, threshold=25.0 | fail | -1.97 | -0.26 | -0.86 | -0.18 | 58 |
| chande_momentum | period=60, threshold=35.0 | fail | -1.97 | -0.26 | -0.89 | -0.18 | 19 |
| chande_momentum | period=60, threshold=50.0 | degenerate | -0.35 | -0.26 | -0.33 | -0.18 | 0 |
| momentum | roc_period=10, threshold=3.0 | fail | -1.00 | -0.26 | -0.54 | -0.18 | 55 |
| momentum | roc_period=10, threshold=5.0 | fail | -1.65 | -0.26 | -0.66 | -0.18 | 8 |
| momentum | roc_period=10, threshold=7.0 | fail | -0.79 | -0.26 | -0.56 | -0.18 | 1 |
| momentum | roc_period=10, threshold=10.0 | fail | -1.41 | -0.26 | -0.58 | -0.18 | 0 |
| momentum | roc_period=14, threshold=3.0 | fail | -1.55 | -0.26 | -0.81 | -0.18 | 64 |
| momentum | roc_period=14, threshold=5.0 | fail | -0.26 | -0.26 | -0.17 | -0.18 | 12 |
| momentum | roc_period=14, threshold=7.0 | pass | 0.29 | -0.26 | 0.51 | -0.18 | 1 |
| momentum | roc_period=14, threshold=10.0 | fail | -0.52 | -0.26 | -0.26 | -0.18 | 0 |
| momentum | roc_period=21, threshold=3.0 | fail | -1.78 | -0.26 | -0.84 | -0.18 | 81 |
| momentum | roc_period=21, threshold=5.0 | pass | -0.23 | -0.26 | -0.12 | -0.18 | 15 |
| momentum | roc_period=21, threshold=7.0 | fail | -0.78 | -0.26 | -0.57 | -0.18 | 6 |
| momentum | roc_period=21, threshold=10.0 | fail | -2.28 | -0.26 | -0.93 | -0.18 | 1 |
| momentum | roc_period=30, threshold=3.0 | fail | -0.87 | -0.26 | -0.58 | -0.18 | 90 |
| momentum | roc_period=30, threshold=5.0 | fail | -0.44 | -0.26 | -0.05 | -0.18 | 22 |
| momentum | roc_period=30, threshold=7.0 | fail | -0.80 | -0.26 | -0.42 | -0.18 | 9 |
| momentum | roc_period=30, threshold=10.0 | fail | -1.89 | -0.26 | -0.89 | -0.18 | 1 |
| rsi | overbought=65, oversold=25, period=10 | fail | -0.76 | -0.26 | -0.37 | -0.18 | 188 |
| rsi | overbought=65, oversold=30, period=10 | fail | -1.34 | -0.26 | -0.71 | -0.18 | 203 |
| rsi | overbought=65, oversold=35, period=10 | fail | -1.42 | -0.26 | -0.73 | -0.18 | 217 |
| rsi | overbought=70, oversold=25, period=10 | fail | -0.82 | -0.26 | -0.42 | -0.18 | 146 |
| rsi | overbought=70, oversold=30, period=10 | fail | -1.28 | -0.26 | -0.64 | -0.18 | 170 |
| rsi | overbought=70, oversold=35, period=10 | fail | -1.42 | -0.26 | -0.69 | -0.18 | 190 |
| rsi | overbought=75, oversold=25, period=10 | fail | -0.90 | -0.26 | -0.47 | -0.18 | 118 |
| rsi | overbought=75, oversold=30, period=10 | fail | -1.45 | -0.26 | -0.65 | -0.18 | 150 |
| rsi | overbought=75, oversold=35, period=10 | fail | -1.35 | -0.26 | -0.71 | -0.18 | 172 |
| rsi | overbought=65, oversold=25, period=14 | fail | -1.33 | -0.26 | -0.62 | -0.18 | 118 |
| rsi | overbought=65, oversold=30, period=14 | fail | -1.33 | -0.26 | -0.61 | -0.18 | 166 |
| rsi | overbought=65, oversold=35, period=14 | fail | -1.73 | -0.26 | -0.76 | -0.18 | 177 |
| rsi | overbought=70, oversold=25, period=14 | fail | -1.88 | -0.26 | -0.73 | -0.18 | 86 |
| rsi | overbought=70, oversold=30, period=14 | fail | -1.69 | -0.26 | -0.66 | -0.18 | 125 |
| rsi | overbought=70, oversold=35, period=14 | fail | -2.05 | -0.26 | -0.83 | -0.18 | 153 |
| rsi | overbought=75, oversold=25, period=14 | fail | -1.06 | -0.26 | -0.47 | -0.18 | 52 |
| rsi | overbought=75, oversold=30, period=14 | fail | -1.03 | -0.26 | -0.36 | -0.18 | 89 |
| rsi | overbought=75, oversold=35, period=14 | fail | -1.35 | -0.26 | -0.69 | -0.18 | 129 |
| rsi | overbought=65, oversold=25, period=21 | fail | -1.85 | -0.26 | -0.73 | -0.18 | 64 |
| rsi | overbought=65, oversold=30, period=21 | fail | -1.46 | -0.26 | -0.70 | -0.18 | 84 |
| rsi | overbought=65, oversold=35, period=21 | fail | -1.71 | -0.26 | -0.74 | -0.18 | 120 |
| rsi | overbought=70, oversold=25, period=21 | fail | -1.11 | -0.26 | -0.49 | -0.18 | 29 |
| rsi | overbought=70, oversold=30, period=21 | fail | -0.80 | -0.26 | -0.46 | -0.18 | 49 |
| rsi | overbought=70, oversold=35, period=21 | fail | -1.21 | -0.26 | -0.62 | -0.18 | 89 |
| rsi | overbought=75, oversold=25, period=21 | pass | 0.07 | -0.26 | 0.24 | -0.18 | 10 |
| rsi | overbought=75, oversold=30, period=21 | pass | 0.11 | -0.26 | 0.05 | -0.18 | 29 |
| rsi | overbought=75, oversold=35, period=21 | fail | -0.54 | -0.26 | -0.30 | -0.18 | 63 |

### Confirmation (locked selections)

| window | leg | M1 verdict | mean Sharpe | bar Sharpe | mean DDadj | bar DDadj | mean return % | worst max DD % | positions (L/S) | net PnL USD | fees USD | funding USD | turnover | mean entry notional |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 2023 | candidate | funding_incomplete | -1.95 | 1.47 | -0.82 | 4.18 | -53.12 | -70.94 | 1394 (697/697) | -1594.48 | 823.88 | -1.17 | 610.14 | 652.92 |
| 2023 | momentum | funding_incomplete | -1.13 | 1.47 | -0.70 | 4.18 | -28.15 | -62.94 | 397 (228/169) | -844.56 | 283.27 | -6.52 | 209.78 | 877.96 |
| 2023 | rsi | funding_incomplete | -0.28 | 1.47 | -0.20 | 4.18 | -10.34 | -44.26 | 165 (35/130) | -310.16 | 135.49 | 45.05 | 100.34 | 902.91 |
| 2024 | candidate | fail | -0.95 | 0.54 | -0.79 | 0.45 | -40.89 | -62.11 | 1475 (752/723) | -1227.47 | 1164.44 | -48.73 | 862.35 | 866.20 |
| 2024 | momentum | fail | -0.91 | 0.54 | -0.63 | 0.45 | -28.12 | -61.85 | 544 (291/253) | -843.50 | 427.45 | -55.04 | 316.55 | 891.33 |
| 2024 | rsi | fail | -0.35 | 0.54 | -0.33 | 0.45 | -6.18 | -19.82 | 130 (58/72) | -185.25 | 111.37 | 18.51 | 82.48 | 954.98 |
| 2025H1 | candidate | fail | -1.13 | -0.44 | -0.52 | -0.42 | -22.73 | -46.24 | 744 (372/372) | -683.20 | 550.46 | -13.86 | 407.66 | 816.95 |
| 2025H1 | momentum | pass | 0.40 | -0.44 | 0.29 | -0.42 | 3.43 | -20.11 | 266 (125/141) | 102.85 | 231.68 | -7.79 | 171.57 | 984.39 |
| 2025H1 | rsi | pass | -0.21 | -0.44 | -0.22 | -0.42 | -4.18 | -18.24 | 76 (43/33) | -125.30 | 66.53 | 0.76 | 49.27 | 971.02 |
| oos | candidate | fail | -1.20 | 0.18 | -0.64 | 0.02 | -29.07 | -48.93 | 973 (504/469) | -873.26 | 734.03 | -3.75 | 543.60 | 829.69 |
| oos | momentum | fail | -0.90 | 0.18 | -0.71 | 0.02 | -16.85 | -27.02 | 269 (140/129) | -505.59 | 212.81 | -7.33 | 157.60 | 896.83 |
| oos | rsi | fail | -1.18 | 0.18 | -0.65 | 0.02 | -12.48 | -26.11 | 98 (52/46) | -374.54 | 81.11 | 1.98 | 60.07 | 919.71 |

Per-dataset candidate legs:

| window | dataset | return % | max DD % | Sharpe | DDadj | positions | net PnL USD | B&H % | exits |
|---|---|---|---|---|---|---|---|---|---|
| 2023 | BTC/USDT 1h | -68.41 | -70.94 | -3.61 | -0.96 | 417 | -684.42 | 155.69 | end_of_data, sl, tiered_tp_pct |
| 2023 | ETH/USDT 1h | -40.93 | -61.05 | -1.40 | -0.67 | 435 | -409.86 | 91.05 | end_of_data, sl, tiered_tp_pct |
| 2023 | SOL/USDT 1h | -50.02 | -60.91 | -0.83 | -0.82 | 542 | -500.20 | 916.68 | sl, tiered_tp_pct |
| 2024 | BTC/USDT 1h | -31.09 | -39.57 | -0.80 | -0.79 | 447 | -311.55 | 120.06 | end_of_data, sl, tiered_tp_pct |
| 2024 | ETH/USDT 1h | -53.62 | -55.72 | -1.53 | -0.96 | 488 | -536.23 | 45.38 | sl, tiered_tp_pct |
| 2024 | SOL/USDT 1h | -37.97 | -62.11 | -0.51 | -0.61 | 540 | -379.69 | 85.82 | sl, tiered_tp_pct |
| 2025H1 | BTC/USDT 1h | -39.98 | -44.03 | -2.78 | -0.91 | 209 | -400.37 | 13.62 | end_of_data, sl, tiered_tp_pct |
| 2025H1 | ETH/USDT 1h | -25.13 | -46.24 | -0.83 | -0.54 | 257 | -252.05 | -25.98 | end_of_data, sl, tiered_tp_pct |
| 2025H1 | SOL/USDT 1h | -3.08 | -32.90 | 0.22 | -0.09 | 278 | -30.78 | -19.52 | sl, tiered_tp_pct |
| oos | BTC/USDT 1h | -46.67 | -48.93 | -2.37 | -0.95 | 306 | -466.73 | -4.87 | sl, tiered_tp_pct |
| oos | ETH/USDT 1h | -39.74 | -42.62 | -1.44 | -0.93 | 309 | -397.44 | -9.85 | sl, tiered_tp_pct |
| oos | SOL/USDT 1h | -0.81 | -30.84 | 0.20 | -0.03 | 358 | -9.09 | -5.87 | end_of_data, sl, tiered_tp_pct |

**1h verdict: FAIL** (false: oos_pass, held_out_passes).

| condition | value |
|---|---|
| oos_pass | false |
| held_out_passes | false |
| net_pnl_positive | unknown |
| beats_comparators | unknown |
| not_concentrated | unknown |

- scorable confirmation windows: oos, 2024, 2025H1
- unscorable `2023` (funding incomplete or missing): candidate BTC/USDT 1h, candidate ETH/USDT 1h, candidate SOL/USDT 1h, incumbent median BTC/USDT 1h, incumbent median ETH/USDT 1h, incumbent median SOL/USDT 1h, momentum BTC/USDT 1h, momentum ETH/USDT 1h, momentum SOL/USDT 1h, rsi BTC/USDT 1h, rsi ETH/USDT 1h, rsi SOL/USDT 1h
- held-out M1 passes among scorable windows: 0
- summed candidate net PnL on scorable windows: -2783.93 USD over 3192 independent positions
- mean return % on scorable windows: candidate -30.90; momentum -13.85; rsi -7.61
- largest single position share of summed winning PnL: 0.0050

### Stability sweep (exploratory, after confirmation)

| window | combos | M1 pass | positive mean return | median mean return % |
|---|---|---|---|---|
| 2023 | 30 | 0 | 6 | -46.12 |
| 2024 | 30 | 1 | 4 | -30.66 |
| 2025H1 | 30 | 13 | 10 | -9.79 |
| oos | 30 | 1 | 3 | -28.82 |

## 4h group (expected_interval_ms = 14400000)

### Training selection (`is`)

| strategy | selected params | how | eligible combos | is mean DDadj | is mean Sharpe |
|---|---|---|---|---|---|
| chande_momentum | period=14, threshold=15.0 | selected | 26 | 1.28 | 0.67 |
| momentum | roc_period=10, threshold=3.0 | selected | 10 | 1.66 | 0.34 |
| rsi | overbought=65, oversold=25, period=14 | selected | 25 | 1.44 | 0.76 |

Every training combination (unsuccessful variants included):

| strategy | params | M1 verdict | mean Sharpe | bar Sharpe | mean DDadj | bar DDadj | min trades |
|---|---|---|---|---|---|---|---|
| chande_momentum | period=10, threshold=10.0 | fail | -0.35 | -0.13 | -0.11 | -0.19 | 92 |
| chande_momentum | period=10, threshold=15.0 | pass | 0.12 | -0.13 | 0.43 | -0.19 | 88 |
| chande_momentum | period=10, threshold=25.0 | fail | -0.20 | -0.13 | 0.43 | -0.19 | 94 |
| chande_momentum | period=10, threshold=35.0 | fail | -0.45 | -0.13 | -0.47 | -0.19 | 87 |
| chande_momentum | period=10, threshold=50.0 | pass | 0.03 | -0.13 | 0.27 | -0.19 | 61 |
| chande_momentum | period=14, threshold=10.0 | pass | 0.01 | -0.13 | 0.94 | -0.19 | 81 |
| chande_momentum | period=14, threshold=15.0 | pass | 0.67 | -0.13 | 1.28 | -0.19 | 78 |
| chande_momentum | period=14, threshold=25.0 | pass | 0.59 | -0.13 | 0.63 | -0.19 | 73 |
| chande_momentum | period=14, threshold=35.0 | fail | -0.36 | -0.13 | 0.34 | -0.19 | 66 |
| chande_momentum | period=14, threshold=50.0 | fail | -0.92 | -0.13 | -0.46 | -0.19 | 51 |
| chande_momentum | period=20, threshold=10.0 | fail | -0.21 | -0.13 | -0.38 | -0.19 | 70 |
| chande_momentum | period=20, threshold=15.0 | fail | -0.57 | -0.13 | -0.59 | -0.19 | 70 |
| chande_momentum | period=20, threshold=25.0 | fail | -0.36 | -0.13 | -0.23 | -0.19 | 61 |
| chande_momentum | period=20, threshold=35.0 | fail | -0.30 | -0.13 | -0.40 | -0.19 | 48 |
| chande_momentum | period=20, threshold=50.0 | fail | -0.34 | -0.13 | -0.01 | -0.19 | 22 |
| chande_momentum | period=30, threshold=10.0 | fail | -1.03 | -0.13 | -0.51 | -0.19 | 61 |
| chande_momentum | period=30, threshold=15.0 | fail | -0.97 | -0.13 | -0.57 | -0.19 | 56 |
| chande_momentum | period=30, threshold=25.0 | pass | 0.17 | -0.13 | 0.12 | -0.19 | 49 |
| chande_momentum | period=30, threshold=35.0 | fail | -0.83 | -0.13 | -0.47 | -0.19 | 25 |
| chande_momentum | period=30, threshold=50.0 | pass | 0.95 | -0.13 | 1.13 | -0.19 | 6 |
| chande_momentum | period=40, threshold=10.0 | pass | -0.09 | -0.13 | 0.03 | -0.19 | 58 |
| chande_momentum | period=40, threshold=15.0 | pass | 0.38 | -0.13 | 0.46 | -0.19 | 42 |
| chande_momentum | period=40, threshold=25.0 | pass | 0.26 | -0.13 | 0.35 | -0.19 | 32 |
| chande_momentum | period=40, threshold=35.0 | fail | -0.43 | -0.13 | -0.47 | -0.19 | 15 |
| chande_momentum | period=40, threshold=50.0 | pass | 0.69 | -0.13 | 2.09 | -0.19 | 2 |
| chande_momentum | period=60, threshold=10.0 | pass | -0.08 | -0.13 | -0.12 | -0.19 | 40 |
| chande_momentum | period=60, threshold=15.0 | pass | 0.26 | -0.13 | 0.18 | -0.19 | 31 |
| chande_momentum | period=60, threshold=25.0 | fail | -0.36 | -0.13 | -0.18 | -0.19 | 16 |
| chande_momentum | period=60, threshold=35.0 | fail | -0.52 | -0.13 | -0.23 | -0.19 | 4 |
| chande_momentum | period=60, threshold=50.0 | degenerate | -0.61 | -0.13 | -0.29 | -0.19 | 0 |
| momentum | roc_period=10, threshold=3.0 | pass | 0.34 | -0.13 | 1.66 | -0.19 | 56 |
| momentum | roc_period=10, threshold=5.0 | pass | 0.40 | -0.13 | 0.35 | -0.19 | 15 |
| momentum | roc_period=10, threshold=7.0 | pass | -0.09 | -0.13 | 0.28 | -0.19 | 6 |
| momentum | roc_period=10, threshold=10.0 | fail | -0.52 | -0.13 | -0.15 | -0.19 | 0 |
| momentum | roc_period=14, threshold=3.0 | pass | 0.76 | -0.13 | 1.26 | -0.19 | 63 |
| momentum | roc_period=14, threshold=5.0 | fail | -0.22 | -0.13 | -0.16 | -0.19 | 23 |
| momentum | roc_period=14, threshold=7.0 | pass | 0.83 | -0.13 | 0.92 | -0.19 | 8 |
| momentum | roc_period=14, threshold=10.0 | fail | -1.76 | -0.13 | -0.75 | -0.19 | 3 |
| momentum | roc_period=21, threshold=3.0 | pass | -0.00 | -0.13 | 0.05 | -0.19 | 51 |
| momentum | roc_period=21, threshold=5.0 | pass | 0.02 | -0.13 | 0.30 | -0.19 | 29 |
| momentum | roc_period=21, threshold=7.0 | fail | -0.26 | -0.13 | 0.10 | -0.19 | 16 |
| momentum | roc_period=21, threshold=10.0 | fail | -1.35 | -0.13 | -0.76 | -0.19 | 2 |
| momentum | roc_period=30, threshold=3.0 | fail | -1.12 | -0.13 | -0.75 | -0.19 | 58 |
| momentum | roc_period=30, threshold=5.0 | fail | -0.34 | -0.13 | -0.25 | -0.19 | 34 |
| momentum | roc_period=30, threshold=7.0 | pass | 0.33 | -0.13 | 0.28 | -0.19 | 14 |
| momentum | roc_period=30, threshold=10.0 | pass | 0.56 | -0.13 | 0.63 | -0.19 | 7 |
| rsi | overbought=65, oversold=25, period=10 | pass | 0.32 | -0.13 | 0.41 | -0.19 | 55 |
| rsi | overbought=65, oversold=30, period=10 | pass | 0.39 | -0.13 | 0.25 | -0.19 | 65 |
| rsi | overbought=65, oversold=35, period=10 | fail | -0.16 | -0.13 | -0.04 | -0.19 | 79 |
| rsi | overbought=70, oversold=25, period=10 | pass | 0.50 | -0.13 | 0.67 | -0.19 | 42 |
| rsi | overbought=70, oversold=30, period=10 | pass | 0.67 | -0.13 | 0.47 | -0.19 | 51 |
| rsi | overbought=70, oversold=35, period=10 | pass | 0.14 | -0.13 | 0.26 | -0.19 | 65 |
| rsi | overbought=75, oversold=25, period=10 | pass | 0.05 | -0.13 | -0.08 | -0.19 | 32 |
| rsi | overbought=75, oversold=30, period=10 | pass | 0.17 | -0.13 | 0.69 | -0.19 | 36 |
| rsi | overbought=75, oversold=35, period=10 | fail | -0.35 | -0.13 | 0.01 | -0.19 | 54 |
| rsi | overbought=65, oversold=25, period=14 | pass | 0.76 | -0.13 | 1.44 | -0.19 | 32 |
| rsi | overbought=65, oversold=30, period=14 | pass | 0.64 | -0.13 | 0.76 | -0.19 | 43 |
| rsi | overbought=65, oversold=35, period=14 | pass | 0.49 | -0.13 | 0.49 | -0.19 | 48 |
| rsi | overbought=70, oversold=25, period=14 | pass | 0.58 | -0.13 | 0.64 | -0.19 | 19 |
| rsi | overbought=70, oversold=30, period=14 | pass | 0.50 | -0.13 | 0.41 | -0.19 | 31 |
| rsi | overbought=70, oversold=35, period=14 | pass | 0.25 | -0.13 | 0.31 | -0.19 | 37 |
| rsi | overbought=75, oversold=25, period=14 | pass | 0.07 | -0.13 | 0.01 | -0.19 | 16 |
| rsi | overbought=75, oversold=30, period=14 | pass | 0.14 | -0.13 | 0.56 | -0.19 | 22 |
| rsi | overbought=75, oversold=35, period=14 | fail | -0.17 | -0.13 | 0.25 | -0.19 | 28 |
| rsi | overbought=65, oversold=25, period=21 | fail | -0.28 | -0.13 | -0.24 | -0.19 | 10 |
| rsi | overbought=65, oversold=30, period=21 | pass | 0.89 | -0.13 | 0.73 | -0.19 | 18 |
| rsi | overbought=65, oversold=35, period=21 | pass | 0.43 | -0.13 | 0.46 | -0.19 | 32 |
| rsi | overbought=70, oversold=25, period=21 | fail | -0.22 | -0.13 | 0.14 | -0.19 | 5 |
| rsi | overbought=70, oversold=30, period=21 | pass | 0.87 | -0.13 | 0.87 | -0.19 | 13 |
| rsi | overbought=70, oversold=35, period=21 | pass | 0.38 | -0.13 | 0.42 | -0.19 | 23 |
| rsi | overbought=75, oversold=25, period=21 | fail | -0.86 | -0.13 | -0.48 | -0.19 | 5 |
| rsi | overbought=75, oversold=30, period=21 | pass | 0.75 | -0.13 | 0.50 | -0.19 | 13 |
| rsi | overbought=75, oversold=35, period=21 | pass | 0.23 | -0.13 | 0.33 | -0.19 | 21 |

### Confirmation (locked selections)

| window | leg | M1 verdict | mean Sharpe | bar Sharpe | mean DDadj | bar DDadj | mean return % | worst max DD % | positions (L/S) | net PnL USD | fees USD | funding USD | turnover | mean entry notional |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 2023 | candidate | funding_incomplete | -0.82 | 1.29 | -0.71 | 2.93 | -32.54 | -57.30 | 466 (244/222) | -963.68 | 365.67 | -20.33 | 270.81 | 868.48 |
| 2023 | momentum | funding_incomplete | 0.32 | 1.29 | 0.35 | 2.93 | 15.64 | -48.26 | 372 (196/176) | 467.14 | 413.80 | -64.81 | 306.45 | 1181.65 |
| 2023 | rsi | funding_incomplete | -0.19 | 1.29 | 0.17 | 2.93 | 0.94 | -37.67 | 198 (27/171) | 28.16 | 167.55 | 80.93 | 124.09 | 936.06 |
| 2024 | candidate | fail | -0.14 | 0.74 | -0.32 | 0.74 | -13.91 | -45.10 | 490 (240/250) | -417.33 | 421.88 | -47.95 | 312.43 | 968.15 |
| 2024 | momentum | fail | -0.39 | 0.74 | -0.54 | 0.74 | -22.71 | -50.20 | 487 (253/234) | -681.43 | 419.98 | -77.42 | 311.03 | 956.67 |
| 2024 | rsi | fail | -0.37 | 0.74 | -0.50 | 0.74 | -13.23 | -34.76 | 203 (37/166) | -396.83 | 162.81 | 102.21 | 120.58 | 890.21 |
| 2025H1 | candidate | fail | -0.68 | -0.37 | -0.47 | -0.28 | -17.73 | -47.05 | 241 (113/128) | -532.73 | 180.98 | -2.76 | 134.03 | 840.01 |
| 2025H1 | momentum | pass | 0.57 | -0.37 | 0.44 | -0.28 | 7.07 | -44.17 | 234 (127/107) | 200.49 | 206.74 | -10.38 | 153.11 | 994.89 |
| 2025H1 | rsi | pass | 0.60 | -0.37 | 0.67 | -0.28 | 10.70 | -20.61 | 95 (28/67) | 321.10 | 92.51 | 13.11 | 68.51 | 1086.21 |
| oos | candidate | fail | -0.43 | 0.43 | -0.28 | 0.30 | -12.36 | -41.10 | 354 (173/181) | -371.51 | 330.00 | -7.69 | 244.39 | 1030.13 |
| oos | momentum | fail | -0.59 | 0.43 | -0.59 | 0.30 | -18.39 | -33.03 | 310 (157/153) | -551.89 | 273.98 | -13.60 | 202.90 | 985.62 |
| oos | rsi | fail | -0.42 | 0.43 | -0.42 | 0.30 | -10.42 | -29.02 | 147 (33/114) | -312.51 | 127.39 | 18.79 | 94.34 | 963.39 |

Per-dataset candidate legs:

| window | dataset | return % | max DD % | Sharpe | DDadj | positions | net PnL USD | B&H % | exits |
|---|---|---|---|---|---|---|---|---|---|
| 2023 | BTC/USDT 4h | -36.35 | -43.91 | -1.29 | -0.83 | 142 | -364.10 | 155.76 | end_of_data, sl, tiered_tp_pct |
| 2023 | ETH/USDT 4h | -29.55 | -39.91 | -0.93 | -0.74 | 146 | -296.17 | 90.98 | end_of_data, sl, tiered_tp_pct |
| 2023 | SOL/USDT 4h | -31.71 | -57.30 | -0.25 | -0.55 | 178 | -303.41 | 919.90 | end_of_data, sl, tiered_tp_pct |
| 2024 | BTC/USDT 4h | -7.12 | -27.03 | -0.03 | -0.26 | 158 | -71.24 | 120.93 | sl, tiered_tp_pct |
| 2024 | ETH/USDT 4h | 8.09 | -32.20 | 0.39 | 0.25 | 153 | 80.93 | 46.53 | sl, tiered_tp_pct |
| 2024 | SOL/USDT 4h | -42.70 | -45.10 | -0.79 | -0.95 | 179 | -427.02 | 84.88 | sl, tiered_tp_pct |
| 2025H1 | BTC/USDT 4h | -17.15 | -27.84 | -1.02 | -0.62 | 73 | -172.30 | 14.45 | end_of_data, sl, tiered_tp_pct |
| 2025H1 | ETH/USDT 4h | -33.90 | -47.05 | -1.25 | -0.72 | 89 | -338.99 | -25.86 | sl, tiered_tp_pct |
| 2025H1 | SOL/USDT 4h | -2.14 | -30.12 | 0.23 | -0.07 | 79 | -21.44 | -18.74 | sl, tiered_tp_pct |
| oos | BTC/USDT 4h | -25.98 | -41.10 | -1.23 | -0.63 | 112 | -260.55 | -4.96 | end_of_data, sl, tiered_tp_pct |
| oos | ETH/USDT 4h | -16.21 | -37.80 | -0.44 | -0.43 | 112 | -162.11 | -9.81 | sl, tiered_tp_pct |
| oos | SOL/USDT 4h | 5.11 | -23.83 | 0.37 | 0.21 | 130 | 51.15 | -5.98 | sl, tiered_tp_pct |

**4h verdict: FAIL** (false: oos_pass, held_out_passes).

| condition | value |
|---|---|
| oos_pass | false |
| held_out_passes | false |
| net_pnl_positive | unknown |
| beats_comparators | unknown |
| not_concentrated | unknown |

- scorable confirmation windows: oos, 2024, 2025H1
- unscorable `2023` (funding incomplete or missing): candidate BTC/USDT 4h, candidate ETH/USDT 4h, candidate SOL/USDT 4h, incumbent median BTC/USDT 4h, incumbent median ETH/USDT 4h, incumbent median SOL/USDT 4h, momentum BTC/USDT 4h, momentum ETH/USDT 4h, momentum SOL/USDT 4h, rsi BTC/USDT 4h, rsi ETH/USDT 4h, rsi SOL/USDT 4h
- held-out M1 passes among scorable windows: 0
- summed candidate net PnL on scorable windows: -1321.57 USD over 1085 independent positions
- mean return % on scorable windows: candidate -14.67; momentum -11.35; rsi -4.31
- largest single position share of summed winning PnL: 0.0084

### Stability sweep (exploratory, after confirmation)

| window | combos | M1 pass | positive mean return | median mean return % |
|---|---|---|---|---|
| 2023 | 30 | 0 | 11 | -6.83 |
| 2024 | 30 | 0 | 13 | -6.23 |
| 2025H1 | 30 | 16 | 12 | -4.70 |
| oos | 30 | 7 | 7 | -9.89 |

## Funding coverage

`frame hours` counts every hour of the leg's candle frame the helper tried to price (all fetched hours); `unpriced held hours` counts only hours a position was actually held without a funding rate. A charge leg with any unpriced held hour is funding-incomplete and cannot support a pass. Source `cache` means the rows came from the frozen inputs.

| group | window | leg | dataset | source | frame hours | priced | venue-absent | missing | unpriced held hours | rows sha256 |
|---|---|---|---|---|---|---|---|---|---|---|
| 1h | 2023 | candidate | BTC/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 1593 | `c6c30ca2d838093e` |
| 1h | 2023 | candidate | ETH/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 1470 | `7fa99349de673ff4` |
| 1h | 2023 | candidate | SOL/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 1238 | `d6f6490e8f5cf84e` |
| 1h | 2023 | momentum | BTC/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 277 | `c6c30ca2d838093e` |
| 1h | 2023 | momentum | ETH/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 367 | `7fa99349de673ff4` |
| 1h | 2023 | momentum | SOL/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 444 | `d6f6490e8f5cf84e` |
| 1h | 2023 | rsi | BTC/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 426 | `c6c30ca2d838093e` |
| 1h | 2023 | rsi | ETH/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 291 | `7fa99349de673ff4` |
| 1h | 2023 | rsi | SOL/USDT 1h | cache | 8759 | 5046 | 569 | 3144 | 101 | `d6f6490e8f5cf84e` |
| 1h | 2024 | candidate | BTC/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `b2e2ddcc6f2a44f3` |
| 1h | 2024 | candidate | ETH/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `c7d926be627c19bf` |
| 1h | 2024 | candidate | SOL/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `42c14bb7eb37828d` |
| 1h | 2024 | momentum | BTC/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `b2e2ddcc6f2a44f3` |
| 1h | 2024 | momentum | ETH/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `c7d926be627c19bf` |
| 1h | 2024 | momentum | SOL/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `42c14bb7eb37828d` |
| 1h | 2024 | rsi | BTC/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `b2e2ddcc6f2a44f3` |
| 1h | 2024 | rsi | ETH/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `c7d926be627c19bf` |
| 1h | 2024 | rsi | SOL/USDT 1h | cache | 8783 | 8782 | 1 | 0 | 0 | `42c14bb7eb37828d` |
| 1h | 2025H1 | candidate | BTC/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `595a6424c0ebbb44` |
| 1h | 2025H1 | candidate | ETH/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `d06074c21f26d5cd` |
| 1h | 2025H1 | candidate | SOL/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `9405202db8ec4662` |
| 1h | 2025H1 | momentum | BTC/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `595a6424c0ebbb44` |
| 1h | 2025H1 | momentum | ETH/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `d06074c21f26d5cd` |
| 1h | 2025H1 | momentum | SOL/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `9405202db8ec4662` |
| 1h | 2025H1 | rsi | BTC/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `595a6424c0ebbb44` |
| 1h | 2025H1 | rsi | ETH/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `d06074c21f26d5cd` |
| 1h | 2025H1 | rsi | SOL/USDT 1h | cache | 4343 | 4343 | 0 | 0 | 0 | `9405202db8ec4662` |
| 1h | oos | candidate | BTC/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `920006cfb21d6b65` |
| 1h | oos | candidate | ETH/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `51039b1ca124e8b2` |
| 1h | oos | candidate | SOL/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |
| 1h | oos | momentum | BTC/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `920006cfb21d6b65` |
| 1h | oos | momentum | ETH/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `51039b1ca124e8b2` |
| 1h | oos | momentum | SOL/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |
| 1h | oos | rsi | BTC/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `920006cfb21d6b65` |
| 1h | oos | rsi | ETH/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `51039b1ca124e8b2` |
| 1h | oos | rsi | SOL/USDT 1h | cache | 6551 | 6551 | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |
| 4h | 2023 | candidate | BTC/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 1324 | `4c0bf587b1d879e8` |
| 4h | 2023 | candidate | ETH/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 1328 | `0188d64ffab972ad` |
| 4h | 2023 | candidate | SOL/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 1144 | `cd5f4986815a930d` |
| 4h | 2023 | momentum | BTC/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 928 | `4c0bf587b1d879e8` |
| 4h | 2023 | momentum | ETH/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 1064 | `0188d64ffab972ad` |
| 4h | 2023 | momentum | SOL/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 1136 | `cd5f4986815a930d` |
| 4h | 2023 | rsi | BTC/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 528 | `4c0bf587b1d879e8` |
| 4h | 2023 | rsi | ETH/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 552 | `0188d64ffab972ad` |
| 4h | 2023 | rsi | SOL/USDT 4h | cache | 8756 | 5043 | 569 | 3144 | 484 | `cd5f4986815a930d` |
| 4h | 2024 | candidate | BTC/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `ca0029420e9ac503` |
| 4h | 2024 | candidate | ETH/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `e6411bdf8f858849` |
| 4h | 2024 | candidate | SOL/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `e14e1c00b458473a` |
| 4h | 2024 | momentum | BTC/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `ca0029420e9ac503` |
| 4h | 2024 | momentum | ETH/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `e6411bdf8f858849` |
| 4h | 2024 | momentum | SOL/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `e14e1c00b458473a` |
| 4h | 2024 | rsi | BTC/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `ca0029420e9ac503` |
| 4h | 2024 | rsi | ETH/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `e6411bdf8f858849` |
| 4h | 2024 | rsi | SOL/USDT 4h | cache | 8780 | 8779 | 1 | 0 | 0 | `e14e1c00b458473a` |
| 4h | 2025H1 | candidate | BTC/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `7852b7bb343db7fa` |
| 4h | 2025H1 | candidate | ETH/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `251464b189b2ce91` |
| 4h | 2025H1 | candidate | SOL/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `cd7d671c0329d5f0` |
| 4h | 2025H1 | momentum | BTC/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `7852b7bb343db7fa` |
| 4h | 2025H1 | momentum | ETH/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `251464b189b2ce91` |
| 4h | 2025H1 | momentum | SOL/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `cd7d671c0329d5f0` |
| 4h | 2025H1 | rsi | BTC/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `7852b7bb343db7fa` |
| 4h | 2025H1 | rsi | ETH/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `251464b189b2ce91` |
| 4h | 2025H1 | rsi | SOL/USDT 4h | cache | 4340 | 4340 | 0 | 0 | 0 | `cd7d671c0329d5f0` |
| 4h | oos | candidate | BTC/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| 4h | oos | candidate | ETH/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `c313f79e21796506` |
| 4h | oos | candidate | SOL/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `246de07d578592eb` |
| 4h | oos | momentum | BTC/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| 4h | oos | momentum | ETH/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `c313f79e21796506` |
| 4h | oos | momentum | SOL/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `246de07d578592eb` |
| 4h | oos | rsi | BTC/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| 4h | oos | rsi | ETH/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `c313f79e21796506` |
| 4h | oos | rsi | SOL/USDT 4h | cache | 6548 | 6548 | 0 | 0 | 0 | `246de07d578592eb` |

## Candle frames scored

| dataset window | rows | first | last | sha256 |
|---|---|---|---|---|
| BTC/USDT 1h 2023 | 8753 | 2023-01-01 00:00:00 | 2023-12-31 23:00:00 | `db5c7ef2f011447d` |
| BTC/USDT 1h 2024 | 8784 | 2024-01-01 00:00:00 | 2024-12-31 23:00:00 | `adb5a8b0ac83aee4` |
| BTC/USDT 1h 2025H1 | 4344 | 2025-01-01 00:00:00 | 2025-06-30 23:00:00 | `5bedf8b0dd4be9b8` |
| BTC/USDT 1h is | 4920 | 2025-06-10 00:00:00 | 2025-12-31 23:00:00 | `5f64cee7ead42ea8` |
| BTC/USDT 1h oos | 6543 | 2026-01-01 00:00:00 | 2026-09-30 23:00:00 | `7dab5d01e8a43516` |
| BTC/USDT 4h 2023 | 2189 | 2023-01-01 00:00:00 | 2023-12-31 20:00:00 | `768b14d1d4e50f37` |
| BTC/USDT 4h 2024 | 2196 | 2024-01-01 00:00:00 | 2024-12-31 20:00:00 | `f353da565ed02040` |
| BTC/USDT 4h 2025H1 | 1086 | 2025-01-01 00:00:00 | 2025-06-30 20:00:00 | `3954bdb7a7a921e5` |
| BTC/USDT 4h is | 1230 | 2025-06-10 00:00:00 | 2025-12-31 20:00:00 | `b4e8a0918c98daf3` |
| BTC/USDT 4h oos | 1636 | 2026-01-01 00:00:00 | 2026-09-30 20:00:00 | `2b5cedb35ee3e6c0` |
| ETH/USDT 1h 2023 | 8753 | 2023-01-01 00:00:00 | 2023-12-31 23:00:00 | `0a65a04c055ca9c7` |
| ETH/USDT 1h 2024 | 8784 | 2024-01-01 00:00:00 | 2024-12-31 23:00:00 | `03088df0216f6c5e` |
| ETH/USDT 1h 2025H1 | 4344 | 2025-01-01 00:00:00 | 2025-06-30 23:00:00 | `8f519f7762e4238e` |
| ETH/USDT 1h is | 4920 | 2025-06-10 00:00:00 | 2025-12-31 23:00:00 | `6e02d651b1504ce3` |
| ETH/USDT 1h oos | 6543 | 2026-01-01 00:00:00 | 2026-09-30 23:00:00 | `3995bc640b7247c6` |
| ETH/USDT 4h 2023 | 2189 | 2023-01-01 00:00:00 | 2023-12-31 20:00:00 | `8ee541184b389051` |
| ETH/USDT 4h 2024 | 2196 | 2024-01-01 00:00:00 | 2024-12-31 20:00:00 | `1e88c6ef8b7e4ef5` |
| ETH/USDT 4h 2025H1 | 1086 | 2025-01-01 00:00:00 | 2025-06-30 20:00:00 | `1d01ba14f30dbd98` |
| ETH/USDT 4h is | 1230 | 2025-06-10 00:00:00 | 2025-12-31 20:00:00 | `89f12277d1ab562c` |
| ETH/USDT 4h oos | 1636 | 2026-01-01 00:00:00 | 2026-09-30 20:00:00 | `ec8931475a49d94d` |
| SOL/USDT 1h 2023 | 8753 | 2023-01-01 00:00:00 | 2023-12-31 23:00:00 | `c3860f5cee7d157b` |
| SOL/USDT 1h 2024 | 8784 | 2024-01-01 00:00:00 | 2024-12-31 23:00:00 | `b67cdf3981b862a4` |
| SOL/USDT 1h 2025H1 | 4344 | 2025-01-01 00:00:00 | 2025-06-30 23:00:00 | `c8a00220a35d3df0` |
| SOL/USDT 1h is | 4920 | 2025-06-10 00:00:00 | 2025-12-31 23:00:00 | `e8a006105e8ee3c1` |
| SOL/USDT 1h oos | 6543 | 2026-01-01 00:00:00 | 2026-09-30 23:00:00 | `b39eb5672a542a83` |
| SOL/USDT 4h 2023 | 2189 | 2023-01-01 00:00:00 | 2023-12-31 20:00:00 | `30660b28e1f66fb0` |
| SOL/USDT 4h 2024 | 2196 | 2024-01-01 00:00:00 | 2024-12-31 20:00:00 | `5106a31e7936b634` |
| SOL/USDT 4h 2025H1 | 1086 | 2025-01-01 00:00:00 | 2025-06-30 20:00:00 | `0c229d53effc9fe2` |
| SOL/USDT 4h is | 1230 | 2025-06-10 00:00:00 | 2025-12-31 20:00:00 | `f729673d38030807` |
| SOL/USDT 4h oos | 1636 | 2026-01-01 00:00:00 | 2026-09-30 20:00:00 | `3d42598533493bb4` |

## Decision parity (paper configurations)

`run_study.py parity` reads `paper_parity_1h.json` / `paper_parity_4h.json` through `parity_diff.config_from_live_config` (the `--config` parser), feeds the frozen BTC/USDT candles, and runs `compute_parity_frame` non-batched and `--batched` at window 200 and window 0 (every prefix). Strict mode reports close parity `unverified` with `clean = true`; that is the strict-mode label for an eligible close set with no approximation.

| config | params | batched | window | candles | bars compared | mismatches | batch mismatches | long/short live signals | clean |
|---|---|---|---|---|---|---|---|---|---|
| paper_parity_1h.json | seed (period=20, threshold=25.0) | no | 200 | 2199 | 2000 | 0 | - | 25/27 | yes |
| paper_parity_1h.json | seed (period=20, threshold=25.0) | no | 0 | 2199 | 2170 | 0 | - | 27/29 | yes |
| paper_parity_1h.json | seed (period=20, threshold=25.0) | yes | 200 | 2199 | 2000 | 0 | 0 | 25/27 | yes |
| paper_parity_1h.json | seed (period=20, threshold=25.0) | yes | 0 | 2199 | 2170 | 0 | 0 | 27/29 | yes |
| paper_parity_1h.json | selected (period=20, threshold=10.0) | no | 200 | 2199 | 2000 | 0 | - | 36/34 | yes |
| paper_parity_1h.json | selected (period=20, threshold=10.0) | no | 0 | 2199 | 2170 | 0 | - | 39/37 | yes |
| paper_parity_1h.json | selected (period=20, threshold=10.0) | yes | 200 | 2199 | 2000 | 0 | 0 | 36/34 | yes |
| paper_parity_1h.json | selected (period=20, threshold=10.0) | yes | 0 | 2199 | 2170 | 0 | 0 | 39/37 | yes |
| paper_parity_4h.json | seed (period=20, threshold=25.0) | no | 200 | 2188 | 1989 | 0 | - | 41/41 | yes |
| paper_parity_4h.json | seed (period=20, threshold=25.0) | no | 0 | 2188 | 2159 | 0 | - | 46/46 | yes |
| paper_parity_4h.json | seed (period=20, threshold=25.0) | yes | 200 | 2188 | 1989 | 0 | 0 | 41/41 | yes |
| paper_parity_4h.json | seed (period=20, threshold=25.0) | yes | 0 | 2188 | 2159 | 0 | 0 | 46/46 | yes |
| paper_parity_4h.json | selected (period=14, threshold=15.0) | no | 200 | 2188 | 1989 | 0 | - | 64/60 | yes |
| paper_parity_4h.json | selected (period=14, threshold=15.0) | no | 0 | 2188 | 2159 | 0 | - | 71/66 | yes |
| paper_parity_4h.json | selected (period=14, threshold=15.0) | yes | 200 | 2188 | 1989 | 0 | 0 | 64/60 | yes |
| paper_parity_4h.json | selected (period=14, threshold=15.0) | yes | 0 | 2188 | 2159 | 0 | 0 | 71/66 | yes |

All parity runs clean: yes.

## Reproduce

```bash
uv run --no-sync python backtest/candidates/chande_momentum_1661/run_study.py acquire --cache-db /tmp/cmo_acquire.db  # network; rewrites data/
uv run --no-sync python backtest/candidates/chande_momentum_1661/run_study.py run --cache-db /tmp/cmo_run.db
uv run --no-sync python backtest/candidates/chande_momentum_1661/run_study.py parity --cache-db /tmp/cmo_parity.db
uv run --no-sync python backtest/candidates/chande_momentum_1661/run_study.py render
```

`run` and `parity` refuse an existing cache path and rebuild it from `data/`. The interval specs `spec_1h.json` / `spec_4h.json` (seeds) and `selected_1h.json` / `selected_4h.json` (locked selections) also run through the stock M1 CLI, for example:

```bash
GO_TRADER_OHLCV_CACHE_DB=/tmp/cmo_run.db uv run --no-sync python backtest/eval_windows.py \
  --candidate-json backtest/candidates/chande_momentum_1661/selected_1h.json --registry futures \
  --datasets BTC/USDT:1h,ETH/USDT:1h,SOL/USDT:1h --windows 2023,2024,2025H1 --funding charge
```

The stock CLI keeps `oos` open-ended, so `run_study.py` pins its end to 2026-10-01 instead.

Study runtime: 292.6 s on commit `ed2ee31ae8108964a421282d53afac55a8e7396a`.
