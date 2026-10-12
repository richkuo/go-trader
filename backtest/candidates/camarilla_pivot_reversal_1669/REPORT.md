# Camarilla pivot reversal study (issue 1669)

**Verdict: FAIL** (proxy 1h: fail, proxy 4h: fail, venue 4h: pass). Registry metadata after this study: `edge_status="no_edge"`, `edge_source="study_fail"`, `edge_ref` = this file. The candidate stays hidden from discovery. Explicit paper mode is admitted; live use still needs `allow_no_edge: true`, and no result here adds one or approves deployment. Every command in this study is suggest-only and sends no order. The proxy groups are proxy evidence (Binance US candles with Hyperliquid fees and funding); the venue group is Hyperliquid candle evidence.

Group reasons: proxy_1h: fail (false: m1_oos_pass, m1_secondary_passes, oos_beats_mean_reversion_pro_sharpe, oos_beats_prior_day_reclaim_sharpe, oos_net_positive); proxy_4h: fail (false: oos_beats_prior_day_reclaim_sharpe); venue_4h: pass (every frozen venue check is true). The frozen overall rule needs every group to pass, so one group's pass cannot offset another group's fail; the venue windows were also used by earlier studies, so venue evidence is retrospective.

## Signal

`camarilla_pivot_reversal` (`shared_strategies/open/camarilla_pivot_reversal.py`) reads the previous complete UTC day (00:00 to 24:00, by bar opening time) and freezes its high H, low L and final close C for the current UTC day: S3 = C - 1.1 x (H - L) / 4, R3 = C + 1.1 x (H - L) / 4, S4 = C - 1.1 x (H - L) / 2, R4 = C + 1.1 x (H - L) / 2. It enters long when the preceding closed candle closes at or below S3 and the current closed candle closes strictly above S3 and strictly below R3 with both lows strictly above S4, and short on the mirror at R3 with both highs strictly below R4. Both candles are consecutive bars of the current UTC day; a jump across the opposite level holds. Every bar of the prior day must be present, aligned and well formed; the frame must declare its bar interval and timestamp kind; a later defect never changes an earlier decision; levels never carry over a missing day. Bad entry data gives a reasoned hold (`cpr_reason`), and close and stop management continue. The evaluator tunes nothing.

## Protocol

`protocol.json` (sha256 `168599acb779a5ee1fd6a5c83bae60c45e8717d1a9ccdbd61c30c4e51d21c898`) was locked by `run_study.py lock` at 2026-10-12T13:27:06Z (commit `7275d441eb3e0b2aa677acca776a949fabc9dc0f+uncommitted-changes`), before any window was scored; `run` and `parity` refuse a different protocol. It fixes the formula, the matched contract, the windows, the warm-up policy, the sample floor and the pass/fail/inconclusive rules. No parameter is selected.

Contract for the candidate, the three comparators and the M1 incumbent median: direction `both`, one `tiered_tp_pct` close (+3%, full position), one fixed entry-ATR stop (`stop_loss_atr_mult: 1.0`), `comparison_mode: strict`, `intrabar_resolution: ohlc_walk`, no regime gate, capital $1000 per leg. Comparators keep their registry defaults: `anchored_vwap_reversion` {"atr_period": 14, "buffer_atr_mult": 0.25, "confirm_bars": 2, "entry_atr_mult": 1.5, "pivot_strength": 5}; `mean_reversion_pro` {"adx_max": 25.0, "adx_period": 14, "confirm_window": 3, "entry_std": 2.0, "lookback": 30, "rsi_overbought": 70.0, "rsi_oversold": 30.0, "rsi_period": 14, "touch_entry": 0, "turn_entry": 0}. `prior_day_reclaim_1669` is the study-local prior-day reclaim: uses the candidate's complete prior UTC day (H, L, C), the same consecutive current-day crossing pair and the same outer S4/R4 invalidation; long when the preceding close is at or below L and the current close is strictly above L and strictly below H, with both lows strictly above S4; short when the preceding close is at or above H and the current close is strictly below H and strictly above L, with both highs strictly below R4; every candidate hold (timing, defect, missing or incomplete prior day, first bar of day) holds it too. run_study.py installs its callable into the futures research registry instance that eval_windows uses in this process only, and removes it in a finally block after the comparison, also after a failure; it is never added to shared_strategies, PLATFORM_ORDER, discovery or any Go table (removed after the run: yes).

Warm-up: every arm and every incumbent of a window loads the same 300 verified consecutive bars before the window start (eval_windows warmup_bars on the cached path, the manifest warmup_bars on the venue path); the evaluator reads them, the engine neither scores nor trades them. The proxy candles start at 2023-01-01, so the 2023 window has no prehistory; it runs every arm with warmup 0 and its first complete prior day plus crossing pair holds with insufficient_history; this is a coverage restriction, never evidence of an economic failure.

Proxy group rule: oos unscorable -> inconclusive; oos candidate clusters < min_held_out_clusters -> inconclusive; any check false -> fail; every check true -> pass; else inconclusive. Venue group rule: evidence incomplete -> inconclusive; test candidate clusters at x1 < min_held_out_clusters -> inconclusive; any check false -> fail; every check true -> pass. Overall: pass only if proxy 1h, proxy 4h and venue 4h all pass; fail if any group is fail; otherwise inconclusive. The proxy groups are proxy evidence and the venue group is venue evidence; the report states both.

| proxy window | start | end (exclusive) | role |
|---|---|---|---|
| is | 2025-06-10 | 2026-01-01 | diagnostic |
| oos | 2026-01-01 | 2026-10-01 | primary held-out |
| 2023 | 2023-01-01 | 2024-01-01 | secondary held-out |
| 2024 | 2024-01-01 | 2025-01-01 | secondary held-out |
| 2025H1 | 2025-01-01 | 2025-07-01 | secondary held-out |

Departures from the default M1 windows: oos end pinned at 2026-10-01, the end of the frozen candles (M1 leaves it open). The other M1 windows are unchanged. The #1642 study ended 2025H1 at 2025-06-10 so it could not overlap its parameter-selection window; this study selects nothing, so that cause does not apply and the M1 end 2025-07-01 stays.

| venue window | start | end (exclusive) | role |
|---|---|---|---|
| test | 2025-09-01 | 2026-09-01 | primary held-out: scored once under the frozen protocol; previously the held-out window of the #1642 and earlier studies, so this is retrospective candidate evidence |
| train | 2024-09-01 | 2025-09-01 | secondary held-out evaluation: this study tunes nothing, so no window selects parameters; previously the selection window of the #1642 and earlier studies, so this is retrospective candidate evidence |

## Inputs, costs and assumptions

- Proxy candles: Binance US spot USDT candles stand in for Hyperliquid perpetual prices, so this is proxy evidence and not measured Hyperliquid candle evidence (every proxy file equals backtest/candidates/dual_thrust_1642/study_manifest.json (read in place)).
- Proxy funding: Hyperliquid hourly funding charged on every held hour (`--funding charge`, `eval_windows.run_leg(funding_mode="charge")`) for every candidate, comparator and incumbent leg.
- Proxy costs: engine Hyperliquid commission `0.00045` per side and engine slippage `0.0005` per fill; the slippage term stands in for the spread. This legacy M1 screen replays no spread, lot rounding or minimum order, so it cannot by itself establish venue execution parity.
- Venue candles: Hyperliquid mainnet perpetual candles from the public info API (candleSnapshot); closed bars only (every venue file equals backtest/candidates/dual_thrust_1642/venue_manifest.json (read in place)), interval 4h, 300 warm-up bars read by the evaluator but never scored or traded.
- Venue costs (manifest execution specification with the explicit close owner): taker fee 0.00045, maker fee 0.00015, slippage 5.0 bps per side plus the per-coin half spread from one frozen L2 snapshot, szDecimals lot rounding, minimum notional $10.0 with a 0.03 margin; cost x2 doubles spread and slippage. Funding uses the manifest's existing attachment and coverage semantics.

| venue dataset | size decimals | half spread bps | candles sha256 | funding sha256 |
|---|---|---|---|---|
| BTC 4h | 5 | 0.06 | `89f84288d462b830ceba8108ea9d69db4d3dac417c730dc9cdd13271c10fd0f8` | `b961391682a15a619fe131abedcc638fd2c311af7cd30a67cf4af32754cabc48` |
| ETH 4h | 4 | 0.19 | `a17523ecbb30f7b594aa80002b3c5ad80b73a7bdd8122d5512683585a8eec3ae` | `1b541abb8801a444cac4e23ebe31d71cbd7dc3b7d185456830daca1c09c6ea66` |
| SOL 4h | 2 | 0.42 | `5ca4fa74940f38f180a85eb81ccaa96bb10f08988d1351c280abb2ce175fdd4c` | `a498781bca4cf017f7f772f5a0c00c0853979bacfd147303394f63a63da5af40` |

- Sizing: capital $1000 per leg, no leverage and no perps sizing block (Backtester leverage None, sizing defaults {"leverage": null, "risk_per_trade_pct": null}), so each entry commits the leg's available cash unlevered; candidate entry notionals ranged from $690.92 to $1490.46. Venue lot rounding and the $10 minimum rejected 0 candidate entries in the held-out venue window at cost x1, so the minimum does not bind at this size; lot rounding trims each order by at most one szDecimals step.
- Stop: one fixed entry-ATR stop at 1.0 x ATR. Close: one `tiered_tp_pct` tier at +3%.

| proxy file (read in place) | rows | sha256 |
|---|---|---|
| `backtest/candidates/dual_thrust_1642/data/proxy/BTC_1h_candles.csv.gz` | 32840 | `d107c60f72363372d03273ba6439ca42a0d867a9089c5de171e9306af3ba2d17` |
| `backtest/candidates/dual_thrust_1642/data/proxy/BTC_4h_candles.csv.gz` | 8211 | `dd0758a2862955eb53b060df5fbace3e98f8bc2a3e0254842bca0d36c96f106f` |
| `backtest/candidates/dual_thrust_1642/data/proxy/BTC_funding.csv.gz` | 29142 | `520fd979d5594b4c2fb950cd52aa327749a124d9971a59ec3419752dd24a27f8` |
| `backtest/candidates/dual_thrust_1642/data/proxy/ETH_1h_candles.csv.gz` | 32840 | `59d08735aecb22b63bab8189c7c32b11101e80da42b2ec724f39e308548b7913` |
| `backtest/candidates/dual_thrust_1642/data/proxy/ETH_4h_candles.csv.gz` | 8211 | `03df63c5dc65ba532a252e8c00118986321fcb124934bb7536e842d678fc9dd8` |
| `backtest/candidates/dual_thrust_1642/data/proxy/ETH_funding.csv.gz` | 29142 | `4b7694d8fc26cb7245ffa882cbdcb2d571d17f3a9f1abd6832fb0deed4985b41` |
| `backtest/candidates/dual_thrust_1642/data/proxy/SOL_1h_candles.csv.gz` | 32840 | `e4e5c15ffcce696253d78dc3b3c3448725e8cf3a0114a9fbc4b1df5f2a0efeec` |
| `backtest/candidates/dual_thrust_1642/data/proxy/SOL_4h_candles.csv.gz` | 8211 | `c1e5775973d00b0b07597da3db4213d62378ef566fe2e4a3b619612b59abc8ab` |
| `backtest/candidates/dual_thrust_1642/data/proxy/SOL_funding.csv.gz` | 29142 | `b29de7625f7484f0971336e31ba52f5b042127e4eef4d8ee1e4554994383cd2b` |

## proxy_1h (expected_interval_ms = 3600000)

**Group verdict: fail** (false: m1_oos_pass, m1_secondary_passes, oos_beats_mean_reversion_pro_sharpe, oos_beats_prior_day_reclaim_sharpe, oos_net_positive).

| check | value |
|---|---|
| m1_oos_pass | false |
| m1_secondary_passes | false |
| oos_beats_anchored_vwap_reversion_sharpe | true |
| oos_beats_mean_reversion_pro_sharpe | false |
| oos_beats_prior_day_reclaim_sharpe | false |
| oos_net_positive | false |

### `oos` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | fail | -1.72 | -24.27 | -728.84 | -46.47 | 429 (207/222) | 251 | 243.28 | 328.51 | 5.30 | 0 |
| candidate | fail | -0.66 | -14.91 | -448.90 | -33.63 | 501 (274/227) | 235 | 305.41 | 412.39 | -5.13 | 0 |
| mean_reversion_pro | pass | 0.56 | 3.89 | 115.80 | -7.17 | 72 (34/38) | 57 | 47.99 | 64.80 | 2.97 | 0 |
| prior_day_reclaim_1669 | pass | 0.11 | 2.88 | 86.39 | -24.67 | 285 (145/140) | 174 | 199.66 | 269.60 | -1.05 | 0 |

### `2023` (warm-up 0 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | funding_incomplete | -0.29 | -1.30 | -39.10 | -31.84 | 559 (329/230) | 272 | 348.72 | 470.88 | -9.13 | 3 |
| candidate | funding_incomplete | -0.09 | -8.18 | -258.73 | -36.38 | 642 (318/324) | 266 | 427.08 | 576.69 | -5.80 | 3 |
| mean_reversion_pro | funding_incomplete | -0.33 | -0.27 | -8.21 | -10.29 | 63 (26/37) | 56 | 41.80 | 56.45 | 4.18 | 3 |
| prior_day_reclaim_1669 | funding_incomplete | 0.04 | -2.51 | -76.31 | -25.97 | 296 (145/151) | 193 | 193.36 | 261.09 | -11.93 | 3 |

### `2024` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | fail | -0.86 | -23.33 | -699.84 | -39.34 | 542 (318/224) | 316 | 309.36 | 417.72 | -12.31 | 0 |
| candidate | fail | -0.54 | -19.71 | -593.44 | -47.67 | 740 (330/410) | 370 | 469.75 | 634.31 | 25.21 | 0 |
| mean_reversion_pro | pass | -0.19 | -2.87 | -85.99 | -17.70 | 74 (29/45) | 60 | 48.27 | 65.18 | 19.14 | 0 |
| prior_day_reclaim_1669 | fail | -0.60 | -15.05 | -451.57 | -33.83 | 440 (187/253) | 281 | 287.33 | 387.98 | 32.10 | 0 |

### `2025H1` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | fail | -0.98 | -13.03 | -391.63 | -36.97 | 226 (110/116) | 146 | 134.52 | 181.64 | -1.48 | 0 |
| candidate | fail | -0.38 | -0.75 | -22.96 | -31.23 | 402 (204/198) | 199 | 277.18 | 374.28 | -2.83 | 0 |
| mean_reversion_pro | pass | 0.54 | 2.61 | 78.25 | -9.20 | 47 (18/29) | 41 | 31.64 | 42.73 | 2.08 | 0 |
| prior_day_reclaim_1669 | pass | 0.24 | 1.17 | 34.12 | -22.83 | 227 (125/102) | 137 | 160.23 | 216.35 | -2.86 | 0 |

### `is` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | pass | -0.74 | -13.24 | -397.99 | -28.94 | 282 (156/126) | 170 | 175.25 | 236.64 | 0.29 | 0 |
| candidate | pass | 0.28 | 2.18 | 62.22 | -30.25 | 399 (202/197) | 175 | 266.46 | 359.80 | 10.84 | 0 |
| mean_reversion_pro | pass | 0.21 | 1.33 | 39.87 | -9.99 | 35 (11/24) | 29 | 23.03 | 31.10 | 1.78 | 0 |
| prior_day_reclaim_1669 | pass | -0.39 | -8.16 | -244.68 | -26.28 | 227 (95/132) | 138 | 141.89 | 191.60 | 9.07 | 0 |

Concentration (candidate, oos): top dataset share of absolute net PnL 0.36, top five positions' share 0.05, long net $-215.03, short net $-233.90.

Unscorable windows: 2023: anchored_vwap_reversion: 3 funding_incomplete legs, anchored_vwap_reversion: 3 incumbent datasets funding_incomplete, candidate: 3 funding_incomplete legs, candidate: 3 incumbent datasets funding_incomplete, mean_reversion_pro: 3 funding_incomplete legs, mean_reversion_pro: 3 incumbent datasets funding_incomplete, prior_day_reclaim_1669: 3 funding_incomplete legs, prior_day_reclaim_1669: 3 incumbent datasets funding_incomplete.

Funding coverage and provenance (candidate legs, oos):

| dataset | mode | source | complete | expected h | priced h | missing h | venue-absent h | unpriced held h | funding sha256 |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `920006cfb21d6b65` |
| ETH/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `51039b1ca124e8b2` |
| SOL/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |

Start-of-window history and input holds (scored bars only; warm-up bars are read but not counted):

| window | dataset | scored bars | first valid bar | entries L/S | hold reasons |
|---|---|---|---|---|---|
| 2023 | BTC/USDT 1h | 8753 | 2023-01-02 01:00:00 | 156/135 | cadence_gap 1, insufficient_history 24, prior_day_incomplete 23 |
| 2023 | ETH/USDT 1h | 8753 | 2023-01-02 01:00:00 | 163/135 | cadence_gap 1, insufficient_history 24, prior_day_incomplete 23 |
| 2023 | SOL/USDT 1h | 8753 | 2023-01-02 01:00:00 | 173/206 | cadence_gap 1, insufficient_history 24, prior_day_incomplete 23 |
| 2024 | BTC/USDT 1h | 8784 | 2024-01-01 01:00:00 | 149/163 | - |
| 2024 | ETH/USDT 1h | 8784 | 2024-01-01 01:00:00 | 154/215 | - |
| 2024 | SOL/USDT 1h | 8784 | 2024-01-01 01:00:00 | 164/210 | - |
| 2025H1 | BTC/USDT 1h | 4344 | 2025-01-01 01:00:00 | 89/90 | - |
| 2025H1 | ETH/USDT 1h | 4344 | 2025-01-01 01:00:00 | 115/94 | - |
| 2025H1 | SOL/USDT 1h | 4344 | 2025-01-01 01:00:00 | 91/108 | - |
| is | BTC/USDT 1h | 4920 | 2025-06-10 01:00:00 | 123/125 | - |
| is | ETH/USDT 1h | 4920 | 2025-06-10 01:00:00 | 86/90 | - |
| is | SOL/USDT 1h | 4920 | 2025-06-10 01:00:00 | 93/111 | - |
| oos | BTC/USDT 1h | 6543 | 2026-01-01 01:00:00 | 118/113 | cadence_gap 1, prior_day_incomplete 23 |
| oos | ETH/USDT 1h | 6543 | 2026-01-01 01:00:00 | 124/114 | cadence_gap 1, prior_day_incomplete 23 |
| oos | SOL/USDT 1h | 6543 | 2026-01-01 01:00:00 | 145/112 | cadence_gap 1, prior_day_incomplete 23 |

Entry audit: 501 held-out candidate entries; each fill follows a valid raw signal on the prior closed bar, its S3/R3/S4/R4 equal an independent recomputation from the prior-day candles, and `results.json` stores the input span hash and every level. First entries:

| dataset | side | signal bar | fill bar | prior H | prior L | prior C | S3 | R3 | prev close | close | net $ |
|---|---|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 1h | short | 2026-01-01 21:00:00 | 2026-01-01 22:00:00 | 89111.8 | 87324.2 | 87709.9 | 87218.3 | 88201.5 | 88328.3 | 88176 | -5.32 |
| BTC/USDT 1h | short | 2026-01-03 22:00:00 | 2026-01-03 23:00:00 | 91000 | 88422.7 | 89945.2 | 89236.4 | 90653.9 | 90682.3 | 90418.8 | -5.00 |
| BTC/USDT 1h | short | 2026-01-04 03:00:00 | 2026-01-04 04:00:00 | 90729.5 | 89312 | 90711.4 | 90321.6 | 91101.2 | 91314.6 | 91020.9 | -5.44 |
| BTC/USDT 1h | long | 2026-01-06 21:00:00 | 2026-01-06 22:00:00 | 95500 | 91524.4 | 94115 | 93021.7 | 95208.3 | 92471.7 | 93539.1 | -8.58 |
| BTC/USDT 1h | long | 2026-01-09 10:00:00 | 2026-01-09 11:00:00 | 91669.9 | 89313.5 | 91110.4 | 90462.3 | 91758.4 | 90240.6 | 90541.5 | -6.27 |
| BTC/USDT 1h | long | 2026-01-09 15:00:00 | 2026-01-09 16:00:00 | 91669.9 | 89313.5 | 91110.4 | 90462.3 | 91758.4 | 90239 | 90912.6 | -7.44 |
| BTC/USDT 1h | long | 2026-01-12 09:00:00 | 2026-01-12 10:00:00 | 91302.1 | 90396.6 | 90982.4 | 90733.4 | 91231.4 | 90673.2 | 91017.1 | -7.36 |
| BTC/USDT 1h | long | 2026-01-15 04:00:00 | 2026-01-15 05:00:00 | 97891.4 | 94595.3 | 97013.7 | 96107.2 | 97920.1 | 95858 | 96190.6 | -9.25 |
| BTC/USDT 1h | long | 2026-01-15 15:00:00 | 2026-01-15 16:00:00 | 97891.4 | 94595.3 | 97013.7 | 96107.2 | 97920.1 | 96045.6 | 96714 | -6.77 |
| BTC/USDT 1h | long | 2026-01-16 17:00:00 | 2026-01-16 18:00:00 | 97162.7 | 95069 | 95583.1 | 95007.3 | 96158.8 | 94940.9 | 95111.7 | -5.78 |

## proxy_4h (expected_interval_ms = 14400000)

**Group verdict: fail** (false: oos_beats_prior_day_reclaim_sharpe).

| check | value |
|---|---|
| m1_oos_pass | true |
| m1_secondary_passes | unknown |
| oos_beats_anchored_vwap_reversion_sharpe | true |
| oos_beats_mean_reversion_pro_sharpe | true |
| oos_beats_prior_day_reclaim_sharpe | false |
| oos_net_positive | true |

### `oos` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | pass | -0.06 | 1.17 | 34.27 | -22.57 | 98 (44/54) | 62 | 62.11 | 83.87 | -1.07 | 0 |
| candidate | pass | -0.01 | 0.61 | 18.35 | -25.81 | 155 (79/76) | 85 | 105.34 | 142.24 | -2.66 | 0 |
| mean_reversion_pro | pass | -0.45 | -0.24 | -7.24 | -9.50 | 15 (4/11) | 13 | 10.07 | 13.60 | 2.10 | 0 |
| prior_day_reclaim_1669 | pass | 0.26 | 7.61 | 228.22 | -16.97 | 90 (48/42) | 57 | 63.99 | 86.40 | -0.17 | 0 |

### `2023` (warm-up 0 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | funding_incomplete | -0.46 | -11.66 | -350.53 | -23.85 | 162 (84/78) | 97 | 102.21 | 138.02 | 0.45 | 3 |
| candidate | funding_incomplete | 0.15 | 2.08 | 60.59 | -19.59 | 163 (82/81) | 105 | 111.79 | 150.95 | -3.51 | 3 |
| mean_reversion_pro | funding_incomplete | 0.70 | 4.57 | 137.11 | -9.24 | 14 (6/8) | 13 | 9.58 | 12.94 | -2.89 | 3 |
| prior_day_reclaim_1669 | funding_incomplete | -0.34 | -11.51 | -351.35 | -34.14 | 101 (45/56) | 74 | 62.24 | 84.05 | -1.33 | 3 |

### `2024` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | fail | -0.24 | -8.61 | -258.44 | -46.21 | 156 (79/77) | 115 | 96.85 | 130.78 | 22.27 | 0 |
| candidate | fail | -0.17 | -6.88 | -206.55 | -39.21 | 190 (99/91) | 124 | 121.48 | 164.03 | 20.02 | 0 |
| mean_reversion_pro | fail | -0.68 | -5.46 | -163.94 | -16.75 | 26 (9/17) | 24 | 16.77 | 22.65 | 20.45 | 0 |
| prior_day_reclaim_1669 | fail | -0.49 | -14.44 | -433.04 | -41.15 | 151 (72/79) | 107 | 89.63 | 121.02 | 11.94 | 0 |

### `2025H1` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | pass | 0.40 | 3.15 | 94.48 | -17.85 | 51 (26/25) | 38 | 35.73 | 48.25 | -0.59 | 0 |
| candidate | pass | 0.96 | 13.31 | 405.99 | -31.61 | 104 (49/55) | 61 | 71.39 | 96.40 | -0.77 | 0 |
| mean_reversion_pro | fail | -0.76 | -1.87 | -56.06 | -10.81 | 15 (10/5) | 12 | 9.60 | 12.96 | -2.25 | 0 |
| prior_day_reclaim_1669 | fail | -0.41 | -7.79 | -234.64 | -30.38 | 76 (38/38) | 46 | 48.82 | 65.92 | -0.29 | 0 |

### `is` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | fail | -0.67 | -10.06 | -303.63 | -25.38 | 67 (39/28) | 52 | 41.89 | 56.56 | -3.44 | 0 |
| candidate | pass | 1.48 | 21.09 | 632.82 | -23.67 | 105 (56/49) | 55 | 78.88 | 106.51 | -1.69 | 0 |
| mean_reversion_pro | fail | -0.70 | -1.58 | -47.47 | -11.04 | 7 (3/4) | 7 | 4.50 | 6.07 | -0.15 | 0 |
| prior_day_reclaim_1669 | fail | -0.32 | -4.71 | -141.26 | -23.92 | 71 (22/49) | 53 | 45.12 | 60.92 | 8.21 | 0 |

Concentration (candidate, oos): top dataset share of absolute net PnL 0.47, top five positions' share 0.10, long net $167.86, short net $-149.56.

Unscorable windows: 2023: anchored_vwap_reversion: 3 funding_incomplete legs, anchored_vwap_reversion: 3 incumbent datasets funding_incomplete, candidate: 3 funding_incomplete legs, candidate: 3 incumbent datasets funding_incomplete, mean_reversion_pro: 3 funding_incomplete legs, mean_reversion_pro: 3 incumbent datasets funding_incomplete, prior_day_reclaim_1669: 3 funding_incomplete legs, prior_day_reclaim_1669: 3 incumbent datasets funding_incomplete.

Funding coverage and provenance (candidate legs, oos):

| dataset | mode | source | complete | expected h | priced h | missing h | venue-absent h | unpriced held h | funding sha256 |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| ETH/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `c313f79e21796506` |
| SOL/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `246de07d578592eb` |

Start-of-window history and input holds (scored bars only; warm-up bars are read but not counted):

| window | dataset | scored bars | first valid bar | entries L/S | hold reasons |
|---|---|---|---|---|---|
| 2023 | BTC/USDT 4h | 2189 | 2023-01-02 04:00:00 | 37/31 | cadence_gap 1, insufficient_history 6, prior_day_incomplete 5 |
| 2023 | ETH/USDT 4h | 2189 | 2023-01-02 04:00:00 | 42/30 | cadence_gap 1, insufficient_history 6, prior_day_incomplete 5 |
| 2023 | SOL/USDT 4h | 2189 | 2023-01-02 04:00:00 | 33/41 | cadence_gap 1, insufficient_history 6, prior_day_incomplete 5 |
| 2024 | BTC/USDT 4h | 2196 | 2024-01-01 04:00:00 | 35/31 | - |
| 2024 | ETH/USDT 4h | 2196 | 2024-01-01 04:00:00 | 41/42 | - |
| 2024 | SOL/USDT 4h | 2196 | 2024-01-01 04:00:00 | 44/45 | - |
| 2025H1 | BTC/USDT 4h | 1086 | 2025-01-01 04:00:00 | 16/17 | - |
| 2025H1 | ETH/USDT 4h | 1086 | 2025-01-01 04:00:00 | 26/23 | - |
| 2025H1 | SOL/USDT 4h | 1086 | 2025-01-01 04:00:00 | 18/30 | - |
| is | BTC/USDT 4h | 1230 | 2025-06-10 04:00:00 | 23/25 | - |
| is | ETH/USDT 4h | 1230 | 2025-06-10 04:00:00 | 22/17 | - |
| is | SOL/USDT 4h | 1230 | 2025-06-10 04:00:00 | 17/26 | - |
| oos | BTC/USDT 4h | 1636 | 2026-01-01 04:00:00 | 27/32 | cadence_gap 1, prior_day_incomplete 5 |
| oos | ETH/USDT 4h | 1636 | 2026-01-01 04:00:00 | 31/28 | cadence_gap 1, prior_day_incomplete 5 |
| oos | SOL/USDT 4h | 1636 | 2026-01-01 04:00:00 | 36/28 | cadence_gap 1, prior_day_incomplete 5 |

Entry audit: 155 held-out candidate entries; each fill follows a valid raw signal on the prior closed bar, its S3/R3/S4/R4 equal an independent recomputation from the prior-day candles, and `results.json` stores the input span hash and every level. First entries:

| dataset | side | signal bar | fill bar | prior H | prior L | prior C | S3 | R3 | prev close | close | net $ |
|---|---|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 4h | long | 2026-01-16 20:00:00 | 2026-01-17 00:00:00 | 97162.7 | 95069 | 95583.1 | 95007.3 | 96158.8 | 95006.9 | 95571.7 | -13.30 |
| BTC/USDT 4h | short | 2026-01-21 20:00:00 | 2026-01-22 00:00:00 | 92862.5 | 87952.9 | 88416.9 | 87066.7 | 89767 | 90354.7 | 89616 | -16.71 |
| BTC/USDT 4h | short | 2026-01-28 12:00:00 | 2026-01-28 16:00:00 | 89535.7 | 87320.3 | 89260.5 | 88651.3 | 89869.7 | 90095.7 | 89534.6 | -12.31 |
| BTC/USDT 4h | long | 2026-01-30 08:00:00 | 2026-01-30 12:00:00 | 89285.8 | 83386.1 | 84684.4 | 83062 | 86306.8 | 82936.1 | 83206.9 | -20.04 |
| BTC/USDT 4h | short | 2026-02-12 12:00:00 | 2026-02-12 16:00:00 | 69292.1 | 65760.4 | 67087.9 | 66116.7 | 68059.1 | 68170.6 | 67098.6 | -20.72 |
| BTC/USDT 4h | short | 2026-02-14 12:00:00 | 2026-02-14 16:00:00 | 69441.4 | 65867.9 | 68866.6 | 67883.9 | 69849.3 | 70479.9 | 69789.6 | -16.03 |
| BTC/USDT 4h | long | 2026-02-16 20:00:00 | 2026-02-17 00:00:00 | 70968.6 | 68074.6 | 68818 | 68022.1 | 69613.8 | 67903 | 68833.3 | -14.21 |
| BTC/USDT 4h | short | 2026-02-19 08:00:00 | 2026-02-19 12:00:00 | 68446.9 | 65863.3 | 66445.6 | 65735.1 | 67156.1 | 67186.3 | 66504 | -14.26 |
| BTC/USDT 4h | short | 2026-02-21 20:00:00 | 2026-02-22 00:00:00 | 68300 | 66483.7 | 68022.9 | 67523.4 | 68522.3 | 68560.6 | 68023.2 | 37.18 |
| BTC/USDT 4h | short | 2026-03-02 04:00:00 | 2026-03-02 08:00:00 | 68200 | 65050.4 | 65721.2 | 64855 | 66587.3 | 66818.8 | 65982.5 | -20.80 |

## venue_4h (expected_interval_ms = 14400000)

**Group verdict: pass** (every frozen venue check is true).

| check | value |
|---|---|
| beats_anchored_vwap_reversion_sharpe_x1 | true |
| beats_mean_reversion_pro_sharpe_x1 | true |
| beats_prior_day_reclaim_sharpe_x1 | true |
| m1_test_pass_x1 | true |
| net_positive_x1 | true |
| net_positive_x2 | true |

### Held-out `test`, cost base

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | pass | -0.08 | -0.93 | -28.79 | -26.64 | 140 (71/69) | 93 | 91.36 | 123.33 | -0.99 | 0 |
| candidate | pass | 0.59 | 13.80 | 413.83 | -27.72 | 196 (93/103) | 112 | 144.72 | 195.38 | 7.06 | 0 |
| mean_reversion_pro | pass | -0.26 | -2.48 | -74.48 | -10.09 | 18 (4/14) | 15 | 11.82 | 15.95 | 2.09 | 0 |
| prior_day_reclaim_1669 | pass | 0.30 | 12.90 | 386.85 | -19.86 | 117 (52/65) | 73 | 84.12 | 113.56 | 3.35 | 0 |

### Held-out `test`, cost stress

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | pass | -0.07 | -0.67 | -21.23 | -28.29 | 139 (71/68) | 92 | 90.43 | 122.08 | -1.29 | 0 |
| candidate | pass | 0.33 | 5.96 | 178.65 | -28.62 | 198 (95/103) | 115 | 142.48 | 192.35 | 5.85 | 0 |
| mean_reversion_pro | pass | -0.47 | -4.47 | -134.12 | -10.24 | 18 (4/14) | 15 | 11.78 | 15.91 | 1.56 | 0 |
| prior_day_reclaim_1669 | pass | 0.09 | 7.72 | 231.61 | -21.34 | 117 (52/65) | 73 | 82.92 | 111.94 | 3.12 | 0 |

### Secondary `train`, cost base (diagnostic verdict: pass, every frozen venue check is true)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| anchored_vwap_reversion | fail | -0.37 | -10.08 | -302.36 | -32.10 | 128 (72/56) | 86 | 88.77 | 119.84 | -8.68 | 0 |
| candidate | pass | 0.75 | 20.50 | 613.74 | -21.00 | 191 (90/101) | 123 | 135.43 | 182.83 | 7.48 | 0 |
| mean_reversion_pro | fail | 0.06 | 0.26 | 7.87 | -11.33 | 23 (11/12) | 18 | 15.23 | 20.55 | 5.97 | 0 |
| prior_day_reclaim_1669 | fail | -0.18 | -6.24 | -188.12 | -30.28 | 141 (66/75) | 98 | 95.96 | 129.55 | 17.59 | 0 |

Concentration (candidate, test, cost x1): top dataset share of absolute net PnL 0.57, top five positions' share 0.07, long net $206.06, short net $207.80.

| dataset | candles complete | funding complete | rejected entries | min entry notional $ |
|---|---|---|---|---|
| BTC 4h | yes | yes | 0 | 966.62 |
| ETH 4h | yes | yes | 0 | 952.70 |
| SOL 4h | yes | yes | 0 | 832.74 |

Start-of-window history and input holds (scored bars only; warm-up bars are read but not counted):

| window | dataset | scored bars | first valid bar | entries L/S | hold reasons |
|---|---|---|---|---|---|
| test | BTC 4h | 2190 | 2025-09-01 04:00:00 | 36/45 | - |
| test | ETH 4h | 2190 | 2025-09-01 04:00:00 | 34/40 | - |
| test | SOL 4h | 2190 | 2025-09-01 04:00:00 | 43/42 | - |
| train | BTC 4h | 2190 | 2024-09-01 04:00:00 | 29/38 | - |
| train | ETH 4h | 2190 | 2024-09-01 04:00:00 | 41/39 | - |
| train | SOL 4h | 2190 | 2024-09-01 04:00:00 | 34/42 | - |

Entry audit: 196 held-out candidate entries; each fill follows a valid raw signal on the prior closed bar, its S3/R3/S4/R4 equal an independent recomputation from the prior-day candles, and `results.json` stores the input span hash and every level. First entries:

| dataset | side | signal bar | fill bar | prior H | prior L | prior C | S3 | R3 | prev close | close | net $ |
|---|---|---|---|---|---|---|---|---|---|---|---|
| BTC 4h | short | 2025-09-03 20:00:00 | 2025-09-04 00:00:00 | 111752 | 108350 | 111225 | 110289 | 112161 | 112186 | 111670 | -12.09 |
| BTC 4h | short | 2025-09-05 20:00:00 | 2025-09-06 00:00:00 | 112171 | 109324 | 110741 | 109958 | 111524 | 111570 | 110646 | -12.67 |
| BTC 4h | short | 2025-09-09 08:00:00 | 2025-09-09 12:00:00 | 112871 | 110578 | 112035 | 111404 | 112666 | 112967 | 112412 | -7.79 |
| BTC 4h | long | 2025-09-30 16:00:00 | 2025-09-30 20:00:00 | 114379 | 111490 | 114274 | 113480 | 115068 | 113072 | 114330 | 33.59 |
| BTC 4h | long | 2025-10-03 08:00:00 | 2025-10-03 12:00:00 | 121050 | 118203 | 120516 | 119733 | 121299 | 119647 | 120363 | 34.12 |
| BTC 4h | long | 2025-10-07 08:00:00 | 2025-10-07 12:00:00 | 126297 | 123184 | 124711 | 123855 | 125567 | 123727 | 124521 | -12.13 |
| BTC 4h | short | 2025-10-08 12:00:00 | 2025-10-08 16:00:00 | 125244 | 120564 | 121401 | 120114 | 122688 | 122959 | 122242 | -13.54 |
| BTC 4h | long | 2025-10-26 04:00:00 | 2025-10-26 08:00:00 | 112000 | 110704 | 111683 | 111327 | 112039 | 111271 | 111756 | 32.03 |
| BTC 4h | short | 2025-10-30 08:00:00 | 2025-10-30 12:00:00 | 113627 | 109305 | 110102 | 108913 | 111291 | 111430 | 109784 | 38.21 |
| BTC 4h | long | 2025-11-08 20:00:00 | 2025-11-09 00:00:00 | 104124 | 99237 | 103300 | 101956 | 104644 | 101943 | 102269 | 37.62 |

## Stability across intervals and windows

| group | window | M1 | mean Sharpe | net $ | positions | clusters | long net $ | short net $ | funding-incomplete legs |
|---|---|---|---|---|---|---|---|---|---|
| proxy_1h | oos | fail | -0.66 | -448.90 | 501 | 235 | -215.03 | -233.90 | 0 |
| proxy_1h | 2023 | funding_incomplete | -0.09 | -258.73 | 642 | 266 | 164.92 | -423.66 | 3 |
| proxy_1h | 2024 | fail | -0.54 | -593.44 | 740 | 370 | -19.19 | -574.22 | 0 |
| proxy_1h | 2025H1 | fail | -0.38 | -22.96 | 402 | 199 | -16.99 | -6.12 | 0 |
| proxy_1h | is | pass | 0.28 | 62.22 | 399 | 175 | -206.12 | 268.24 | 0 |
| proxy_4h | oos | pass | -0.01 | 18.35 | 155 | 85 | 167.86 | -149.56 | 0 |
| proxy_4h | 2023 | funding_incomplete | 0.15 | 60.59 | 163 | 105 | 128.82 | -68.28 | 3 |
| proxy_4h | 2024 | fail | -0.17 | -206.55 | 190 | 124 | 464.45 | -671.04 | 0 |
| proxy_4h | 2025H1 | pass | 0.96 | 405.99 | 104 | 61 | 91.33 | 314.64 | 0 |
| proxy_4h | is | pass | 1.48 | 632.82 | 105 | 55 | 318.61 | 314.20 | 0 |
| venue_4h | test | pass | 0.59 | 413.83 | 196 | 112 | 206.06 | 207.80 | 0 |
| venue_4h | train | pass | 0.75 | 613.74 | 191 | 123 | 413.45 | 200.28 | 0 |

The regime and close settings were frozen as one configuration (no regime gate, one +3% close, one 1.0 x ATR stop), so the only variation measured is across intervals and windows.

Verdict guards (each altered copy must not pass): proxy_1h: proxy_below_sample_floor -> inconclusive, proxy_incumbent_funding_incomplete -> inconclusive, proxy_oos_funding_incomplete -> inconclusive, proxy_oos_missing_comparator_dataset -> inconclusive; proxy_4h: proxy_below_sample_floor -> inconclusive, proxy_incumbent_funding_incomplete -> inconclusive, proxy_oos_funding_incomplete -> inconclusive, proxy_oos_missing_comparator_dataset -> inconclusive; venue_4h: venue_below_sample_floor -> inconclusive, venue_coverage_incomplete -> inconclusive, venue_funding_incomplete -> inconclusive, venue_proxy_provenance -> inconclusive.

## Decision parity

`run_study.py parity` ran the explicit-paper configurations `paper_parity_<interval>.json` through `backtest/parity_diff.compute_parity_frame` with `--registry futures --platform hyperliquid`, strict close comparison and charged funding: non-batched and batched (solo vs batch slots through the real Hyperliquid check evaluator), on the trailing 200-bar window and on every prefix. Intervals covered: 15m, 30m, 1h, 2h, 4h, 8h, 12h. Overall clean: yes.

| interval | source | batched | window | bars compared | mismatches | batch mismatches | long/short signals | close eligibility | clean |
|---|---|---|---|---|---|---|---|---|---|
| 15m | synthetic seeded walk 15m (2200 bars, seed 1669) | no | 200 | 2001 | 0 | - | 5/6 | eligible (parity unverified) | yes |
| 15m | synthetic seeded walk 15m (2200 bars, seed 1669) | no | every prefix | 2171 | 0 | - | 5/7 | eligible (parity unverified) | yes |
| 15m | synthetic seeded walk 15m (2200 bars, seed 1669) | yes | 200 | 2001 | 0 | 0 | 5/6 | eligible (parity unverified) | yes |
| 15m | synthetic seeded walk 15m (2200 bars, seed 1669) | yes | every prefix | 2171 | 0 | 0 | 5/7 | eligible (parity unverified) | yes |
| 30m | synthetic seeded walk 30m (2200 bars, seed 1669) | no | 200 | 2001 | 0 | - | 8/10 | eligible (parity unverified) | yes |
| 30m | synthetic seeded walk 30m (2200 bars, seed 1669) | no | every prefix | 2171 | 0 | - | 8/12 | eligible (parity unverified) | yes |
| 30m | synthetic seeded walk 30m (2200 bars, seed 1669) | yes | 200 | 2001 | 0 | 0 | 8/10 | eligible (parity unverified) | yes |
| 30m | synthetic seeded walk 30m (2200 bars, seed 1669) | yes | every prefix | 2171 | 0 | 0 | 8/12 | eligible (parity unverified) | yes |
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | no | 200 | 2000 | 0 | - | 10/14 | eligible (parity unverified) | yes |
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | no | every prefix | 2170 | 0 | - | 11/14 | eligible (parity unverified) | yes |
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | yes | 200 | 2000 | 0 | 0 | 10/14 | eligible (parity unverified) | yes |
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | yes | every prefix | 2170 | 0 | 0 | 11/14 | eligible (parity unverified) | yes |
| 2h | proxy BTC/USDT 1h aggregated to 2h (complete groups of 2) 2026-03-31..2026-10-01 | no | 200 | 2001 | 0 | - | 18/17 | eligible (parity unverified) | yes |
| 2h | proxy BTC/USDT 1h aggregated to 2h (complete groups of 2) 2026-03-31..2026-10-01 | no | every prefix | 2171 | 0 | - | 18/18 | eligible (parity unverified) | yes |
| 2h | proxy BTC/USDT 1h aggregated to 2h (complete groups of 2) 2026-03-31..2026-10-01 | yes | 200 | 2001 | 0 | 0 | 18/17 | eligible (parity unverified) | yes |
| 2h | proxy BTC/USDT 1h aggregated to 2h (complete groups of 2) 2026-03-31..2026-10-01 | yes | every prefix | 2171 | 0 | 0 | 18/18 | eligible (parity unverified) | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | no | 200 | 2001 | 0 | - | 33/29 | eligible (parity unverified) | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | no | every prefix | 2171 | 0 | - | 35/32 | eligible (parity unverified) | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | yes | 200 | 2001 | 0 | 0 | 33/29 | eligible (parity unverified) | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | yes | every prefix | 2171 | 0 | 0 | 35/32 | eligible (parity unverified) | yes |
| 8h | proxy BTC/USDT 1h aggregated to 8h (complete groups of 8) 2024-09-27..2026-10-01 | no | 200 | 2001 | 0 | - | 27/26 | eligible (parity unverified) | yes |
| 8h | proxy BTC/USDT 1h aggregated to 8h (complete groups of 8) 2024-09-27..2026-10-01 | no | every prefix | 2171 | 0 | - | 28/27 | eligible (parity unverified) | yes |
| 8h | proxy BTC/USDT 1h aggregated to 8h (complete groups of 8) 2024-09-27..2026-10-01 | yes | 200 | 2001 | 0 | 0 | 27/26 | eligible (parity unverified) | yes |
| 8h | proxy BTC/USDT 1h aggregated to 8h (complete groups of 8) 2024-09-27..2026-10-01 | yes | every prefix | 2171 | 0 | 0 | 28/27 | eligible (parity unverified) | yes |
| 12h | proxy BTC/USDT 1h aggregated to 12h (complete groups of 12) 2023-09-26..2026-10-01 | no | 200 | 2001 | 0 | - | 15/15 | eligible (parity unverified) | yes |
| 12h | proxy BTC/USDT 1h aggregated to 12h (complete groups of 12) 2023-09-26..2026-10-01 | no | every prefix | 2171 | 0 | - | 15/17 | eligible (parity unverified) | yes |
| 12h | proxy BTC/USDT 1h aggregated to 12h (complete groups of 12) 2023-09-26..2026-10-01 | yes | 200 | 2001 | 0 | 0 | 15/15 | eligible (parity unverified) | yes |
| 12h | proxy BTC/USDT 1h aggregated to 12h (complete groups of 12) 2023-09-26..2026-10-01 | yes | every prefix | 2171 | 0 | 0 | 15/17 | eligible (parity unverified) | yes |
| 4h | venue BTC 4h test window with warmup | no | 200 | 2291 | 0 | - | 35/31 | eligible (parity unverified) | yes |
| 4h | venue BTC 4h test window with warmup | no | every prefix | 2461 | 0 | - | 38/34 | eligible (parity unverified) | yes |
| 4h | venue BTC 4h test window with warmup | yes | 200 | 2291 | 0 | 0 | 35/31 | eligible (parity unverified) | yes |
| 4h | venue BTC 4h test window with warmup | yes | every prefix | 2461 | 0 | 0 | 38/34 | eligible (parity unverified) | yes |

Strict mode with an eligible close set and no approximation is the exact supported comparison mode; the runner labels it `unverified` because it never claims more than decision agreement plus an eligible close set. The 15m and 30m runs use a seeded synthetic walk and the 2h, 8h and 12h runs aggregate frozen 1h proxy candles; they prove decision parity only.

## Limits

- Proxy legs use Binance US spot candles; basis, venue wicks and funding-driven price effects are not modelled. The legacy M1 screen has no spread, lot rounding or minimum order, so venue execution claims rest on the venue group only.
- Hyperliquid funding starts in May 2023; a proxy 2023 leg that holds a position before the first rate is funding_incomplete, and such a window can never support a pass. The 2023 window also has no prehistory for the warm-up.
- The venue windows cover 2024-09-01 to 2026-09-01 only (the venue serves the most recent 5,000 candles), and earlier studies already used them, so the venue result is retrospective. Spread is a per-coin half spread from one frozen L2 snapshot plus flat slippage; queue position, partial fills and market impact are not modelled.
- Only 1h and 4h are scored; the other supported intervals have no frozen candles.
- Every score reads only frozen inputs with network acquisition disabled. A research result alone cannot approve deployment.

## Reproduction

```
uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py lock
uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py run --cache-db /tmp/cpr1669_run.db
uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py parity --cache-db /tmp/cpr1669_parity.db
uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py render
```

Results: `results.json` (source commit `0d21b651dcf710ecff127fb586061c9995d710f0`, runtime 122.3 s, manifest sha256 `7823cafaa17b635060c5188dba709cb8758aaf8c78aa08404be7cdf3d06394ee`, venue manifest sha256 `ce0795d9f2ff57c649c2d75e4ecb4b8fa0a7b4e086a0d875f6db7aa08d16550f`), `parity_results.json` (source commit `0d21b651dcf710ecff127fb586061c9995d710f0`).
