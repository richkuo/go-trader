# Mass Index reversal study (issue 1670)

**Verdict: INCONCLUSIVE** (proxy 1h: inconclusive, proxy 4h: inconclusive, venue 4h: inconclusive). Registry metadata after this study: `edge_status="no_edge"`, `edge_source="study_inconclusive"`, `edge_ref` = this file. The candidate stays hidden from discovery. Explicit paper mode is admitted; live use still needs `allow_no_edge: true`, and no result here adds one or approves deployment. Every command in this study is suggest-only and sends no order. The proxy groups are proxy evidence (Binance US candles with Hyperliquid fees and funding); the venue group is Hyperliquid candle evidence.

Group reasons: proxy_1h: inconclusive (held-out independent position clusters 9 < 30); proxy_4h: inconclusive (held-out independent position clusters 4 < 30); venue_4h: inconclusive (held-out independent position clusters 6 < 30).

## Signal

`mass_index_reversal` (`shared_strategies/open/mass_index_reversal.py`, Go short name `massrev`) is entry-only and futures-only. Finite-recursion research variant: for every bar x the range high - low is smoothed by two exponential averages of period ema_period (weight 2 / (ema_period + 1)), both seeded with the range at x - recursion_length and updated exactly recursion_length times, so each ratio depends only on the trailing recursion_length + 1 bars; the Mass Index is the sum of sum_period consecutive ratios (first average / second average). It is not an exact copy of the infinite-history published formula. An upper crossing (previous Mass Index <= upper_threshold, current > upper_threshold) clears any older bulge and arms a new one only when the trend slope (close SMA of trend_period minus its value slope_span bars earlier) at that bar is nonzero; a rising slope permits a later short and a falling slope a later long. The first strict lower crossing (previous >= lower_threshold, current < lower_threshold) at age 1 to max_bulge_age completes and consumes the bulge; age max_bulge_age + 1 expires it. On the completion bar a long needs the close strictly above the previous confirm_lookback highs and a short strictly below their lows; a failed confirmation consumes the bulge without an entry. The Mass Index itself has no direction. Seed parameters: `ema_period=9`, `sum_period=25`, `upper_threshold=27.0`, `lower_threshold=26.5`, `trend_period=50`, `slope_span=5`, `confirm_lookback=5`, `max_bulge_age=50`, `recursion_length=72`, `bulge_filter=1`; a decision needs 148 clean consecutive rows with declared bar cadence (at most 199, inside the 200-row check window). Bad candles, timestamp defects, cadence gaps and a non-positive second average hold the entry with a reason (`mir_reason`); close and stop management continue.

## Protocol

`protocol.json` (sha256 `732a94b7a773bbe07f007e44f47333d6727d5657880391cc2ae3753e4443a67e`) was locked by `run_study.py lock` at 2026-10-12T15:34:17Z (commit `0b96bf9502590a8fe140e366c607f70c426921a0`), before any window was scored; `run` and `parity` refuse a different protocol. It fixes the seed, the matched contract, the windows, the warm-up policy, the stability, concentration and sample rules and the pass/fail/inconclusive rules. The seed is never re-selected. The optimizer grid has 324 combinations, every one passes the parameter validator (worst required history 153 rows); the stability gate uses its 10 one-parameter neighbors of the seed.

Contract for the candidate, both comparators and the M1 incumbent median: direction `both`, one `tiered_tp_pct` close (+3%, full position), one fixed entry-ATR stop (`stop_loss_atr_mult: 1.0`), `comparison_mode: strict`, `intrabar_resolution: ohlc_walk`, no regime gate, capital $1000 per leg. Comparators: `mass_index_reversal_filter_off` (the same evaluator, composer, contract and costs with the Mass Index filter removed: every valid bar is a completion bar, the side comes from the current trend slope (falling permits a long, rising a short, zero holds) and the same confirm_lookback price confirmation decides the entry) and `mean_reversion_pro` with its registry defaults {"adx_max": 25.0, "adx_period": 14, "confirm_window": 3, "entry_std": 2.0, "lookback": 30, "rsi_overbought": 70.0, "rsi_oversold": 30.0, "rsi_period": 14, "touch_entry": 0, "turn_entry": 0}. The M1 incumbent median is its own reference under the same contract and is not a matched both-side comparison.

Warm-up: every arm and every incumbent of a window loads the same 300 verified consecutive bars before the window start (eval_windows warmup_bars on the cached path, the manifest warmup_bars on the venue path); 300 bars cover the 148-row decision requirement plus the entry ATR; the evaluator reads them, the engine neither scores nor trades them. The proxy candles start at 2023-01-01, so the 2023 window has no prehistory; it runs every arm with warmup 0 and its first 147 bars hold with insufficient_history; this is a coverage restriction, never evidence of an economic failure.

Proxy group rule: oos unscorable -> inconclusive; oos candidate clusters < min_held_out_clusters -> inconclusive; any check false -> fail; every check true -> pass; else inconclusive. Venue group rule: evidence incomplete -> inconclusive; test candidate clusters at x1 < min_held_out_clusters -> inconclusive; any check false -> fail; every check true -> pass; else inconclusive. Overall: pass only if proxy 1h, proxy 4h and venue 4h all pass; fail if any group is fail; otherwise inconclusive. The proxy groups are proxy evidence and the venue group is venue evidence; the report states both.

| proxy window | start | end (exclusive) | role |
|---|---|---|---|
| is | 2025-06-10 | 2026-01-01 | training (stability gate, diagnostic) |
| oos | 2026-01-01 | 2026-10-01 | primary held-out |
| 2023 | 2023-01-01 | 2024-01-01 | secondary held-out |
| 2024 | 2024-01-01 | 2025-01-01 | secondary held-out |
| 2025H1 | 2025-01-01 | 2025-07-01 | secondary held-out |

Departures from the default M1 windows: oos end pinned at 2026-10-01, the end of the frozen candles (M1 leaves it open). The other M1 windows are unchanged. 2025H1 overlaps the is training window from 2025-06-10 to 2025-07-01; a trade in that overlap appears in both windows and is never counted twice as independent held-out evidence (only oos carries the sample floor).

| venue window | start | end (exclusive) | role |
|---|---|---|---|
| test | 2025-09-01 | 2026-09-01 | primary held-out: scored once under the frozen protocol after the stability gate; previously the held-out window of the #1642 and earlier studies, so this is retrospective candidate evidence |
| train | 2024-09-01 | 2025-09-01 | training window: the fixed seed is never re-selected here; it scores only the frozen one-parameter neighbor stability gate before the held-out window is scored, and a diagnostic seed result; previously the selection window of the #1642 and earlier studies |

## Inputs, costs and assumptions

- Proxy candles: Binance US spot USDT candles stand in for Hyperliquid perpetual prices, so this is proxy evidence and not measured Hyperliquid candle evidence (every proxy file equals backtest/candidates/dual_thrust_1642/study_manifest.json (read in place)).
- Proxy funding: Hyperliquid hourly funding charged on every held hour (`--funding charge`, `eval_windows.run_leg(funding_mode="charge")`) for every candidate, comparator, neighbor and incumbent leg.
- Proxy costs: engine Hyperliquid commission `0.00045` per side and engine slippage `0.0005` per fill; the slippage term stands in for the spread. Lot sizes and venue minimums are unmodeled, so the proxy result cannot claim Hyperliquid execution parity.
- Venue candles: Hyperliquid mainnet perpetual candles from the public info API (candleSnapshot); closed bars only (every venue file equals backtest/candidates/dual_thrust_1642/venue_manifest.json (read in place)), interval 4h, 300 warm-up bars read by the evaluator but never scored or traded.
- Venue costs (manifest execution specification with the explicit close owner): taker fee 0.00045, maker fee 0.00015, slippage 5.0 bps per side plus the per-coin half spread from one frozen L2 snapshot, szDecimals lot rounding, minimum notional $10.0 with a 0.03 margin; cost x2 doubles spread and slippage. Funding uses the manifest's existing attachment and coverage semantics.

| venue dataset | size decimals | half spread bps | candles sha256 | funding sha256 |
|---|---|---|---|---|
| BTC 4h | 5 | 0.06 | `89f84288d462b830ceba8108ea9d69db4d3dac417c730dc9cdd13271c10fd0f8` | `b961391682a15a619fe131abedcc638fd2c311af7cd30a67cf4af32754cabc48` |
| ETH 4h | 4 | 0.19 | `a17523ecbb30f7b594aa80002b3c5ad80b73a7bdd8122d5512683585a8eec3ae` | `1b541abb8801a444cac4e23ebe31d71cbd7dc3b7d185456830daca1c09c6ea66` |
| SOL 4h | 2 | 0.42 | `5ca4fa74940f38f180a85eb81ccaa96bb10f08988d1351c280abb2ce175fdd4c` | `a498781bca4cf017f7f772f5a0c00c0853979bacfd147303394f63a63da5af40` |

- Sizing: capital $1000 per leg, no leverage and no perps sizing block (Backtester leverage None, sizing defaults {"leverage": null, "risk_per_trade_pct": null}), so each entry commits the leg's available cash unlevered. Venue lot rounding and the minimum rejected 0 candidate entries in the held-out venue window at cost x1.
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

## proxy_1h (bar interval 3600000 ms)

**Group verdict: inconclusive** (held-out independent position clusters 9 < 30).

| check | value |
|---|---|
| m1_oos_pass | false |
| m1_secondary_passes | unknown |
| oos_beats_filter_off_sharpe | false |
| oos_beats_mean_reversion_pro_sharpe | false |
| oos_concentration | false |
| oos_net_positive | false |
| training_stability | true |

Stability gate on `is` (scored before the held-out windows): 7 of 10 one-parameter neighbors had positive summed net PnL; 7 needed; 0 unscorable; result true.

| neighbor | net PnL $ | mean Sharpe | positions | clusters | M1 | unscorable |
|---|---|---|---|---|---|---|
| sum_period = 20 | 0.00 | 0.00 | 0 | 0 | degenerate | - |
| sum_period = 30 | -10.26 | -0.75 | 2 | 2 | degenerate | - |
| upper_threshold = 26.75 | 22.35 | -0.21 | 7 | 7 | pass | - |
| upper_threshold = 27.5 | 0.90 | -0.46 | 4 | 4 | pass | - |
| lower_threshold = 26.0 | -137.56 | -1.36 | 11 | 11 | fail | - |
| trend_period = 30 | 22.69 | -0.40 | 7 | 6 | pass | - |
| trend_period = 70 | 31.34 | -0.24 | 6 | 5 | pass | - |
| confirm_lookback = 3 | 18.89 | 0.12 | 9 | 8 | pass | - |
| confirm_lookback = 8 | 1.97 | -0.26 | 4 | 4 | pass | - |
| max_bulge_age = 25 | 30.59 | -0.10 | 6 | 6 | pass | - |

### `oos` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | fail | -1.19 | -1.54 | -46.23 | -3.97 | 11 (6/5) | 9 | 1.00 | 7.24 | 9.78 | 0.27 | 0 |
| mass_index_reversal_filter_off | fail | -0.79 | -21.53 | -645.89 | -38.68 | 780 (387/393) | 204 | 0.07 | 492.13 | 664.53 | 2.39 | 0 |
| mean_reversion_pro | pass | 0.56 | 3.89 | 115.80 | -7.17 | 72 (34/38) | 57 | 0.13 | 47.99 | 64.80 | 2.97 | 0 |

### `2023` (warm-up 0 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | funding_incomplete | -0.40 | -1.83 | -55.08 | -8.62 | 20 (8/12) | 19 | 0.22 | 13.12 | 17.71 | 2.66 | 3 |
| mass_index_reversal_filter_off | funding_incomplete | -2.10 | -54.41 | -1633.34 | -63.90 | 1068 (531/537) | 285 | 0.06 | 451.28 | 609.37 | 11.06 | 3 |
| mean_reversion_pro | funding_incomplete | -0.33 | -0.27 | -8.21 | -10.29 | 63 (26/37) | 56 | 0.12 | 41.80 | 56.45 | 4.18 | 3 |

### `2024` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | fail | -1.29 | -4.47 | -134.09 | -9.85 | 16 (10/6) | 14 | 1.00 | 10.38 | 14.02 | -0.36 | 0 |
| mass_index_reversal_filter_off | fail | -1.78 | -55.36 | -1661.55 | -78.83 | 1199 (589/610) | 349 | 0.04 | 570.23 | 769.98 | 0.06 | 0 |
| mean_reversion_pro | pass | -0.19 | -2.87 | -85.99 | -17.70 | 74 (29/45) | 60 | 0.14 | 48.27 | 65.18 | 19.14 | 0 |

### `2025H1` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | pass | 0.88 | 3.28 | 98.38 | -6.44 | 11 (7/4) | 10 | 0.46 | 7.50 | 10.13 | -0.87 | 0 |
| mass_index_reversal_filter_off | pass | 0.17 | 0.31 | 13.05 | -33.59 | 632 (315/317) | 157 | 0.14 | 432.19 | 583.59 | -8.66 | 0 |
| mean_reversion_pro | pass | 0.54 | 2.61 | 78.25 | -9.20 | 47 (18/29) | 41 | 0.09 | 31.64 | 42.73 | 2.08 | 0 |

### `is` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | pass | -0.10 | 1.02 | 30.59 | -3.57 | 6 (2/4) | 6 | 0.54 | 4.00 | 5.41 | 0.42 | 0 |
| mass_index_reversal_filter_off | pass | -0.61 | -17.47 | -525.03 | -40.75 | 635 (335/300) | 171 | 0.09 | 396.13 | 534.90 | -5.23 | 0 |
| mean_reversion_pro | pass | 0.21 | 1.33 | 39.87 | -9.99 | 35 (11/24) | 29 | 0.13 | 23.03 | 31.10 | 1.78 | 0 |

Concentration (candidate, oos): top dataset share of absolute net PnL 0.52, largest positive cluster $33.96 of $33.96 positive cluster PnL (share 1.00, rule met: no), long net $-50.21, short net $3.99.

Unscorable windows: 2023: candidate: 3 funding_incomplete legs, candidate: 3 incumbent datasets funding_incomplete, mass_index_reversal_filter_off: 3 funding_incomplete legs, mass_index_reversal_filter_off: 3 incumbent datasets funding_incomplete, mean_reversion_pro: 3 funding_incomplete legs, mean_reversion_pro: 3 incumbent datasets funding_incomplete.

Funding coverage and provenance (candidate legs, oos):

| dataset | mode | source | complete | expected h | priced h | missing h | venue-absent h | unpriced held h | funding sha256 |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `920006cfb21d6b65` |
| ETH/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `51039b1ca124e8b2` |
| SOL/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |

Bulge events and holds per window (scored bars only; warm-up bars are read but not counted). Filtered = completed bulges whose price confirmation failed:

| window | dataset | scored bars | valid bars | first valid bar | upper crossings | completed bulges | filtered | entries L/S | hold reasons |
|---|---|---|---|---|---|---|---|---|---|
| 2023 | BTC/USDT 1h | 8753 | 8459 | 2023-01-07 03:00:00 | 78 | 69 | 63 | 2/4 | cadence_gap 147, insufficient_history 147 |
| 2023 | ETH/USDT 1h | 8753 | 8459 | 2023-01-07 03:00:00 | 68 | 61 | 58 | 2/1 | cadence_gap 147, insufficient_history 147 |
| 2023 | SOL/USDT 1h | 8753 | 8459 | 2023-01-07 03:00:00 | 75 | 68 | 57 | 4/7 | cadence_gap 147, insufficient_history 147 |
| 2024 | BTC/USDT 1h | 8784 | 8784 | 2024-01-01 00:00:00 | 80 | 67 | 58 | 6/3 | - |
| 2024 | ETH/USDT 1h | 8784 | 8784 | 2024-01-01 00:00:00 | 74 | 65 | 61 | 2/2 | - |
| 2024 | SOL/USDT 1h | 8784 | 8784 | 2024-01-01 00:00:00 | 62 | 53 | 50 | 2/1 | - |
| 2025H1 | BTC/USDT 1h | 4344 | 4344 | 2025-01-01 00:00:00 | 34 | 33 | 28 | 3/2 | - |
| 2025H1 | ETH/USDT 1h | 4344 | 4344 | 2025-01-01 00:00:00 | 34 | 30 | 27 | 2/1 | - |
| 2025H1 | SOL/USDT 1h | 4344 | 4344 | 2025-01-01 00:00:00 | 35 | 32 | 29 | 2/1 | - |
| is | BTC/USDT 1h | 4920 | 4920 | 2025-06-10 00:00:00 | 40 | 37 | 36 | 1/0 | - |
| is | ETH/USDT 1h | 4920 | 4920 | 2025-06-10 00:00:00 | 36 | 33 | 31 | 1/1 | - |
| is | SOL/USDT 1h | 4920 | 4920 | 2025-06-10 00:00:00 | 45 | 40 | 37 | 0/3 | - |
| oos | BTC/USDT 1h | 6543 | 6396 | 2026-01-01 00:00:00 | 49 | 47 | 44 | 1/2 | cadence_gap 147 |
| oos | ETH/USDT 1h | 6543 | 6396 | 2026-01-01 00:00:00 | 59 | 52 | 48 | 2/2 | cadence_gap 147 |
| oos | SOL/USDT 1h | 6543 | 6396 | 2026-01-01 00:00:00 | 58 | 55 | 51 | 3/1 | cadence_gap 147 |

Filter-off comparison signal totals per window: 2023: 1156 long / 1131 short; 2024: 1353 long / 1272 short; 2025H1: 825 long / 772 short; is: 888 long / 736 short; oos: 942 long / 942 short.

Entry audit: 11 held-out candidate entries; each fill is on the bar after a valid completed-bulge signal, and the Mass Index, arming bar, frozen slope and confirmation equal an independent scalar recomputation from the raw candles; `results.json` stores the input span hash and values. First entries:

| dataset | side | armed at | signal bar | fill bar | age | Mass Index | close | confirm high | confirm low | net $ |
|---|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 1h | short | 2026-06-15 00:00:00 | 2026-06-15 22:00:00 | 2026-06-15 23:00:00 | 22 | 25.9275 | 66216.5 | 67084.3 | 66354.1 | -6.92 |
| BTC/USDT 1h | long | 2026-06-24 21:00:00 | 2026-06-25 07:00:00 | 2026-06-25 08:00:00 | 10 | 26.435 | 61869.7 | 61832.3 | 60690.9 | -8.24 |
| BTC/USDT 1h | short | 2026-08-03 15:00:00 | 2026-08-04 00:00:00 | 2026-08-04 01:00:00 | 9 | 26.4372 | 63354.4 | 64054.2 | 63389.1 | -6.44 |
| ETH/USDT 1h | short | 2026-01-02 14:00:00 | 2026-01-03 06:00:00 | 2026-01-03 07:00:00 | 16 | 26.4447 | 3099.24 | 3133.25 | 3104.53 | -7.15 |
| ETH/USDT 1h | long | 2026-04-12 14:00:00 | 2026-04-12 18:00:00 | 2026-04-12 19:00:00 | 4 | 26.1906 | 2205.37 | 2204.41 | 2176.05 | -6.30 |
| ETH/USDT 1h | long | 2026-05-26 16:00:00 | 2026-05-27 00:00:00 | 2026-05-27 01:00:00 | 8 | 26.4265 | 2083.16 | 2079.05 | 2066.22 | -8.69 |
| ETH/USDT 1h | short | 2026-09-14 20:00:00 | 2026-09-15 03:00:00 | 2026-09-15 04:00:00 | 7 | 26.3386 | 2498.88 | 2556.26 | 2508.29 | 33.96 |
| SOL/USDT 1h | long | 2026-01-19 00:00:00 | 2026-01-19 16:00:00 | 2026-01-19 17:00:00 | 16 | 26.3383 | 134.61 | 134.29 | 133.33 | -5.00 |
| SOL/USDT 1h | short | 2026-05-06 09:00:00 | 2026-05-07 00:00:00 | 2026-05-07 01:00:00 | 15 | 26.4259 | 88.43 | 89.58 | 88.72 | -9.47 |
| SOL/USDT 1h | long | 2026-05-26 18:00:00 | 2026-05-27 00:00:00 | 2026-05-27 01:00:00 | 6 | 26.2219 | 84.14 | 84 | 83.38 | -8.36 |

## proxy_4h (bar interval 14400000 ms)

**Group verdict: inconclusive** (held-out independent position clusters 4 < 30).

| check | value |
|---|---|
| m1_oos_pass | true |
| m1_secondary_passes | unknown |
| oos_beats_filter_off_sharpe | true |
| oos_beats_mean_reversion_pro_sharpe | true |
| oos_concentration | false |
| oos_net_positive | true |
| training_stability | false |

Stability gate on `is` (scored before the held-out windows): 2 of 10 one-parameter neighbors had positive summed net PnL; 7 needed; 0 unscorable; result false.

| neighbor | net PnL $ | mean Sharpe | positions | clusters | M1 | unscorable |
|---|---|---|---|---|---|---|
| sum_period = 20 | 0.00 | 0.00 | 0 | 0 | degenerate | - |
| sum_period = 30 | 0.00 | 0.00 | 0 | 0 | degenerate | - |
| upper_threshold = 26.75 | -44.74 | -0.56 | 5 | 5 | fail | - |
| upper_threshold = 27.5 | 7.55 | 0.08 | 2 | 2 | degenerate | - |
| lower_threshold = 26.0 | -2.13 | 0.06 | 7 | 6 | pass | - |
| trend_period = 30 | -27.93 | -0.50 | 4 | 4 | fail | - |
| trend_period = 70 | 14.14 | -0.02 | 5 | 5 | pass | - |
| confirm_lookback = 3 | -52.71 | -0.63 | 5 | 5 | fail | - |
| confirm_lookback = 8 | -2.89 | -0.32 | 3 | 3 | fail | - |
| max_bulge_age = 25 | -27.93 | -0.50 | 4 | 4 | fail | - |

### `oos` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | pass | 0.37 | 3.51 | 105.41 | -3.88 | 5 (3/2) | 4 | 0.70 | 3.37 | 4.55 | 0.47 | 0 |
| mass_index_reversal_filter_off | fail | -0.99 | -19.30 | -588.25 | -38.03 | 231 (119/112) | 74 | 0.13 | 144.91 | 195.68 | 6.05 | 0 |
| mean_reversion_pro | pass | -0.45 | -0.24 | -7.24 | -9.50 | 15 (4/11) | 13 | 0.48 | 10.07 | 13.60 | 2.10 | 0 |

### `2023` (warm-up 0 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | funding_incomplete | 0.02 | -0.64 | -19.14 | -5.05 | 3 (2/1) | 3 | 1.00 | 1.99 | 2.69 | 0.21 | 1 |
| mass_index_reversal_filter_off | funding_incomplete | -0.24 | -13.67 | -412.39 | -47.44 | 289 (138/151) | 87 | 0.16 | 170.78 | 230.60 | 45.94 | 3 |
| mean_reversion_pro | funding_incomplete | 0.70 | 4.57 | 137.11 | -9.24 | 14 (6/8) | 13 | 0.21 | 9.58 | 12.94 | -2.89 | 3 |

### `2024` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | pass | 0.15 | 0.52 | 15.50 | -8.22 | 6 (4/2) | 5 | 0.68 | 3.97 | 5.36 | -2.03 | 0 |
| mass_index_reversal_filter_off | fail | -0.65 | -26.56 | -796.78 | -45.26 | 382 (189/193) | 133 | 0.09 | 224.57 | 303.23 | 20.27 | 0 |
| mean_reversion_pro | fail | -0.68 | -5.46 | -163.94 | -16.75 | 26 (9/17) | 24 | 0.22 | 16.77 | 22.65 | 20.45 | 0 |

### `2025H1` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | fail | -0.84 | -2.43 | -72.78 | -4.74 | 2 (2/0) | 2 | - | 1.31 | 1.77 | -0.12 | 0 |
| mass_index_reversal_filter_off | fail | -0.60 | -7.55 | -227.25 | -51.27 | 192 (112/80) | 61 | 0.12 | 119.90 | 161.90 | 1.47 | 0 |
| mean_reversion_pro | fail | -0.76 | -1.87 | -56.06 | -10.81 | 15 (10/5) | 12 | 0.46 | 9.60 | 12.96 | -2.25 | 0 |

### `is` (warm-up 300 bars)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | fail | -0.50 | -0.93 | -27.93 | -4.76 | 4 (2/2) | 4 | 1.00 | 2.67 | 3.61 | -0.05 | 0 |
| mass_index_reversal_filter_off | pass | 0.35 | 12.14 | 361.66 | -23.91 | 209 (116/93) | 65 | 0.10 | 162.42 | 219.32 | 3.12 | 0 |
| mean_reversion_pro | fail | -0.70 | -1.58 | -47.47 | -11.04 | 7 (3/4) | 7 | 1.00 | 4.50 | 6.07 | -0.15 | 0 |

Concentration (candidate, oos): top dataset share of absolute net PnL 0.47, largest positive cluster $98.60 of $141.11 positive cluster PnL (share 0.70, rule met: no), long net $78.80, short net $26.61.

Unscorable windows: 2023: candidate: 1 funding_incomplete legs, candidate: 3 incumbent datasets funding_incomplete, mass_index_reversal_filter_off: 3 funding_incomplete legs, mass_index_reversal_filter_off: 3 incumbent datasets funding_incomplete, mean_reversion_pro: 3 funding_incomplete legs, mean_reversion_pro: 3 incumbent datasets funding_incomplete.

Funding coverage and provenance (candidate legs, oos):

| dataset | mode | source | complete | expected h | priced h | missing h | venue-absent h | unpriced held h | funding sha256 |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| ETH/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `c313f79e21796506` |
| SOL/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `246de07d578592eb` |

Bulge events and holds per window (scored bars only; warm-up bars are read but not counted). Filtered = completed bulges whose price confirmation failed:

| window | dataset | scored bars | valid bars | first valid bar | upper crossings | completed bulges | filtered | entries L/S | hold reasons |
|---|---|---|---|---|---|---|---|---|---|
| 2023 | BTC/USDT 4h | 2189 | 1895 | 2023-01-25 12:00:00 | 25 | 23 | 22 | 1/0 | cadence_gap 147, insufficient_history 147 |
| 2023 | ETH/USDT 4h | 2189 | 1895 | 2023-01-25 12:00:00 | 16 | 14 | 14 | 0/0 | cadence_gap 147, insufficient_history 147 |
| 2023 | SOL/USDT 4h | 2189 | 1895 | 2023-01-25 12:00:00 | 24 | 22 | 20 | 1/1 | cadence_gap 147, insufficient_history 147 |
| 2024 | BTC/USDT 4h | 2196 | 2196 | 2024-01-01 00:00:00 | 22 | 22 | 22 | 0/0 | - |
| 2024 | ETH/USDT 4h | 2196 | 2196 | 2024-01-01 00:00:00 | 19 | 19 | 17 | 2/0 | - |
| 2024 | SOL/USDT 4h | 2196 | 2196 | 2024-01-01 00:00:00 | 17 | 16 | 12 | 2/2 | - |
| 2025H1 | BTC/USDT 4h | 1086 | 1086 | 2025-01-01 00:00:00 | 13 | 13 | 13 | 0/0 | - |
| 2025H1 | ETH/USDT 4h | 1086 | 1086 | 2025-01-01 00:00:00 | 11 | 11 | 10 | 1/0 | - |
| 2025H1 | SOL/USDT 4h | 1086 | 1086 | 2025-01-01 00:00:00 | 10 | 10 | 9 | 1/0 | - |
| is | BTC/USDT 4h | 1230 | 1230 | 2025-06-10 00:00:00 | 14 | 14 | 13 | 0/1 | - |
| is | ETH/USDT 4h | 1230 | 1230 | 2025-06-10 00:00:00 | 13 | 13 | 10 | 2/1 | - |
| is | SOL/USDT 4h | 1230 | 1230 | 2025-06-10 00:00:00 | 17 | 15 | 15 | 0/0 | - |
| oos | BTC/USDT 4h | 1636 | 1489 | 2026-01-01 00:00:00 | 14 | 13 | 11 | 1/1 | cadence_gap 147 |
| oos | ETH/USDT 4h | 1636 | 1489 | 2026-01-01 00:00:00 | 16 | 16 | 15 | 1/0 | cadence_gap 147 |
| oos | SOL/USDT 4h | 1636 | 1489 | 2026-01-01 00:00:00 | 14 | 13 | 11 | 1/1 | cadence_gap 147 |

Filter-off comparison signal totals per window: 2023: 224 long / 241 short; 2024: 314 long / 304 short; 2025H1: 164 long / 141 short; is: 191 long / 165 short; oos: 196 long / 184 short.

Entry audit: 5 held-out candidate entries; each fill is on the bar after a valid completed-bulge signal, and the Mass Index, arming bar, frozen slope and confirmation equal an independent scalar recomputation from the raw candles; `results.json` stores the input span hash and values. First entries:

| dataset | side | armed at | signal bar | fill bar | age | Mass Index | close | confirm high | confirm low | net $ |
|---|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 4h | short | 2026-01-05 20:00:00 | 2026-01-06 16:00:00 | 2026-01-06 20:00:00 | 5 | 26.3027 | 92100 | 94882.5 | 93182.5 | -15.90 |
| BTC/USDT 4h | long | 2026-06-25 12:00:00 | 2026-06-27 12:00:00 | 2026-06-27 16:00:00 | 12 | 26.2871 | 60841 | 60566.9 | 59567.5 | -19.80 |
| ETH/USDT 4h | long | 2026-06-03 20:00:00 | 2026-06-07 00:00:00 | 2026-06-07 04:00:00 | 19 | 26.2974 | 1594.67 | 1592.64 | 1505.85 | 57.73 |
| SOL/USDT 4h | short | 2026-02-26 08:00:00 | 2026-02-27 12:00:00 | 2026-02-27 16:00:00 | 7 | 26.4908 | 82.73 | 88.29 | 83.37 | 42.51 |
| SOL/USDT 4h | long | 2026-06-03 16:00:00 | 2026-06-07 00:00:00 | 2026-06-07 04:00:00 | 20 | 26.1632 | 63.76 | 63.57 | 60.16 | 40.86 |

## venue_4h (bar interval 14400000 ms)

**Group verdict: inconclusive** (held-out independent position clusters 6 < 30).

| check | value |
|---|---|
| beats_filter_off_sharpe_x1 | true |
| beats_mean_reversion_pro_sharpe_x1 | true |
| concentration_x1 | true |
| m1_test_pass_x1 | true |
| net_positive_x1 | true |
| net_positive_x2 | true |
| training_stability | false |

Stability gate on `train` (scored before the held-out windows): 3 of 10 one-parameter neighbors had positive summed net PnL; 7 needed; 0 unscorable; result false.

| neighbor | net PnL $ | mean Sharpe | positions | clusters | M1 | unscorable |
|---|---|---|---|---|---|---|
| sum_period = 20 | 0.00 | 0.00 | 0 | 0 | degenerate | - |
| sum_period = 30 | 0.00 | 0.00 | 0 | 0 | degenerate | - |
| upper_threshold = 26.75 | -4.61 | 0.15 | 5 | 4 | pass | - |
| upper_threshold = 27.5 | 49.91 | 0.42 | 1 | 1 | degenerate | - |
| lower_threshold = 26.0 | 26.39 | 0.15 | 4 | 4 | pass | - |
| trend_period = 30 | -4.61 | 0.15 | 5 | 4 | pass | - |
| trend_period = 70 | -4.61 | 0.15 | 5 | 4 | pass | - |
| confirm_lookback = 3 | 85.44 | 0.61 | 9 | 7 | pass | - |
| confirm_lookback = 8 | -52.33 | -0.22 | 4 | 4 | fail | - |
| max_bulge_age = 25 | -4.61 | 0.15 | 5 | 4 | pass | - |

### Held-out `test`, cost base

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | pass | 0.33 | 3.31 | 99.27 | -6.45 | 9 (7/2) | 6 | 0.47 | 5.97 | 8.06 | 0.49 | 0 |
| mass_index_reversal_filter_off | fail | -1.07 | -28.97 | -869.17 | -49.61 | 343 (192/151) | 115 | 0.07 | 208.71 | 281.76 | 5.25 | 0 |
| mean_reversion_pro | pass | -0.26 | -2.48 | -74.48 | -10.09 | 18 (4/14) | 15 | 0.54 | 11.82 | 15.95 | 2.09 | 0 |

### Held-out `test`, cost stress

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | pass | 0.29 | 3.06 | 91.89 | -6.49 | 9 (7/2) | 6 | 0.47 | 5.97 | 8.05 | 0.49 | 0 |
| mass_index_reversal_filter_off | fail | -1.39 | -35.29 | -1058.71 | -53.65 | 348 (195/153) | 117 | 0.13 | 202.99 | 274.03 | 4.66 | 0 |
| mean_reversion_pro | pass | -0.47 | -4.47 | -134.12 | -10.24 | 18 (4/14) | 15 | 0.79 | 11.78 | 15.91 | 1.56 | 0 |

### Training `train`, cost base (diagnostic)

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | pass | 0.15 | -0.16 | -4.61 | -6.35 | 5 (4/1) | 4 | 0.51 | 3.36 | 4.54 | -1.71 | 0 |
| mass_index_reversal_filter_off | fail | -0.71 | -25.33 | -762.06 | -58.52 | 363 (182/181) | 126 | 0.09 | 201.75 | 272.36 | 10.11 | 0 |
| mean_reversion_pro | fail | 0.06 | 0.26 | 7.87 | -11.33 | 23 (11/12) | 18 | 0.27 | 15.23 | 20.55 | 5.97 | 0 |

Concentration (candidate, test, cost x1): top dataset share of absolute net PnL 0.49, largest positive cluster $99.01 of $212.63 positive cluster PnL (share 0.47, rule met: yes), long net $21.77, short net $77.52.

| dataset | candles complete | funding complete | rejected entries | min entry notional $ |
|---|---|---|---|---|
| BTC 4h | yes | yes | 0 | 998.43 |
| ETH 4h | yes | yes | 0 | 969.94 |
| SOL 4h | yes | yes | 0 | 963.89 |

Bulge events and holds per window (scored bars only; warm-up bars are read but not counted). Filtered = completed bulges whose price confirmation failed:

| window | dataset | scored bars | valid bars | first valid bar | upper crossings | completed bulges | filtered | entries L/S | hold reasons |
|---|---|---|---|---|---|---|---|---|---|
| test | BTC 4h | 2190 | 2190 | 2025-09-01 00:00:00 | 25 | 23 | 20 | 2/1 | - |
| test | ETH 4h | 2190 | 2190 | 2025-09-01 00:00:00 | 23 | 23 | 20 | 3/0 | - |
| test | SOL 4h | 2190 | 2190 | 2025-09-01 00:00:00 | 15 | 15 | 12 | 2/1 | - |
| train | BTC 4h | 2190 | 2190 | 2024-09-01 00:00:00 | 25 | 24 | 23 | 1/0 | - |
| train | ETH 4h | 2190 | 2190 | 2024-09-01 00:00:00 | 19 | 19 | 17 | 1/1 | - |
| train | SOL 4h | 2190 | 2190 | 2024-09-01 00:00:00 | 21 | 20 | 18 | 2/0 | - |

Filter-off comparison signal totals per window: test: 310 long / 252 short; train: 277 long / 295 short.

Entry audit: 9 held-out candidate entries; each fill is on the bar after a valid completed-bulge signal, and the Mass Index, arming bar, frozen slope and confirmation equal an independent scalar recomputation from the raw candles; `results.json` stores the input span hash and values. First entries:

| dataset | side | armed at | signal bar | fill bar | age | Mass Index | close | confirm high | confirm low | net $ |
|---|---|---|---|---|---|---|---|---|---|---|
| BTC 4h | short | 2025-09-18 20:00:00 | 2025-09-19 08:00:00 | 2025-09-19 12:00:00 | 3 | 26.3626 | 116425 | 117960 | 116652 | 32.66 |
| BTC 4h | long | 2025-10-10 20:00:00 | 2025-10-12 20:00:00 | 2025-10-13 00:00:00 | 12 | 26.4879 | 114940 | 114709 | 109515 | -28.13 |
| BTC 4h | long | 2026-06-25 12:00:00 | 2026-06-27 12:00:00 | 2026-06-27 16:00:00 | 12 | 26.2321 | 60813 | 60546 | 59522 | -20.51 |
| ETH 4h | long | 2025-10-10 20:00:00 | 2025-10-13 16:00:00 | 2025-10-13 20:00:00 | 17 | 26.4901 | 4248.8 | 4217.2 | 4038.7 | -29.62 |
| ETH 4h | long | 2025-11-05 12:00:00 | 2025-11-07 16:00:00 | 2025-11-07 20:00:00 | 13 | 26.2578 | 3442.7 | 3368.5 | 3193 | 36.11 |
| ETH 4h | long | 2026-06-03 20:00:00 | 2026-06-07 00:00:00 | 2026-06-07 04:00:00 | 19 | 26.2314 | 1594.2 | 1592.4 | 1504.4 | 58.30 |
| SOL 4h | long | 2025-10-10 20:00:00 | 2025-10-13 16:00:00 | 2025-10-13 20:00:00 | 17 | 26.4479 | 207.01 | 199.94 | 189.94 | -35.09 |
| SOL 4h | short | 2026-02-26 08:00:00 | 2026-02-27 12:00:00 | 2026-02-27 16:00:00 | 7 | 26.3995 | 82.65 | 88.26 | 83.305 | 44.86 |
| SOL 4h | long | 2026-06-03 20:00:00 | 2026-06-07 00:00:00 | 2026-06-07 04:00:00 | 19 | 26.099 | 63.794 | 63.555 | 60.062 | 40.70 |

## Stability across intervals and windows

| group | window | M1 | mean Sharpe | net $ | positions | clusters | long net $ | short net $ | funding-incomplete legs |
|---|---|---|---|---|---|---|---|---|---|
| proxy_1h | oos | fail | -1.19 | -46.23 | 11 | 9 | -50.21 | 3.99 | 0 |
| proxy_1h | 2023 | funding_incomplete | -0.40 | -55.08 | 20 | 19 | 30.91 | -86.02 | 3 |
| proxy_1h | 2024 | fail | -1.29 | -134.09 | 16 | 14 | -74.16 | -59.93 | 0 |
| proxy_1h | 2025H1 | pass | 0.88 | 98.38 | 11 | 10 | 47.11 | 51.27 | 0 |
| proxy_1h | is | pass | -0.10 | 30.59 | 6 | 6 | -18.19 | 48.77 | 0 |
| proxy_4h | oos | pass | 0.37 | 105.41 | 5 | 4 | 78.80 | 26.61 | 0 |
| proxy_4h | 2023 | funding_incomplete | 0.02 | -19.14 | 3 | 3 | 2.99 | -22.12 | 1 |
| proxy_4h | 2024 | pass | 0.15 | 15.50 | 6 | 5 | 5.53 | 9.96 | 0 |
| proxy_4h | 2025H1 | fail | -0.84 | -72.78 | 2 | 2 | -72.78 | 0 | 0 |
| proxy_4h | is | fail | -0.50 | -27.93 | 4 | 4 | 8.38 | -36.31 | 0 |
| venue_4h | test | pass | 0.33 | 99.27 | 9 | 6 | 21.77 | 77.52 | 0 |
| venue_4h | train | pass | 0.15 | -4.61 | 5 | 4 | 13.78 | -18.39 | 0 |

Searched variants: the seed, the filter-off comparison and the one-parameter neighbors listed in each group's stability table; no other variant was run.

Verdict guards (each altered copy must not pass): proxy_1h: proxy_below_sample_floor -> inconclusive, proxy_incumbent_funding_incomplete -> inconclusive, proxy_isolated_seed -> inconclusive, proxy_oos_funding_incomplete -> inconclusive, proxy_oos_missing_comparator_dataset -> inconclusive, proxy_unknown_stability -> inconclusive; proxy_4h: proxy_below_sample_floor -> inconclusive, proxy_incumbent_funding_incomplete -> inconclusive, proxy_isolated_seed -> inconclusive, proxy_oos_funding_incomplete -> inconclusive, proxy_oos_missing_comparator_dataset -> inconclusive, proxy_unknown_stability -> inconclusive; venue_4h: venue_below_sample_floor -> inconclusive, venue_coverage_incomplete -> inconclusive, venue_funding_incomplete -> inconclusive, venue_isolated_seed -> inconclusive, venue_proxy_provenance -> inconclusive.

## Decision parity

`run_study.py parity` checks raw indicator and event equality first: for every bar of each source the evaluator's last row on the expanding prefix, and on trailing frames of the required history, 199 and 200 rows wherever the full-series row is valid, must equal the full-series row in every `mir_` value, the reason and the signal (filter on and filter off). It then runs the explicit-paper configurations `paper_parity_<interval>.json` through `backtest/parity_diff.compute_parity_frame` with `--registry futures --platform hyperliquid`, strict close comparison and charged funding: non-batched and batched, on the trailing 200-bar window and on every prefix. Overall clean: yes.

| interval | source | mode | prefix rows | required-history tails | 199 tails | 200 tails | mismatches | completed bulges | long/short | clean |
|---|---|---|---|---|---|---|---|---|---|---|
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | filter_on | 2199 | 1905 | 1854 | 1853 | 0 | 13 | 0/1 | yes |
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | filter_off | 2199 | 1905 | 1854 | 1853 | 0 | 13 | 106/110 | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | filter_on | 2200 | 1906 | 1855 | 1854 | 0 | 18 | 1/1 | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | filter_off | 2200 | 1906 | 1855 | 1854 | 0 | 18 | 83/82 | yes |
| 4h | venue BTC 4h test window with warmup | filter_on | 2490 | 2343 | 2292 | 2291 | 0 | 24 | 2/1 | yes |
| 4h | venue BTC 4h test window with warmup | filter_off | 2490 | 2343 | 2292 | 2291 | 0 | 24 | 107/92 | yes |

| interval | source | batched | window | bars compared | mismatches | batch mismatches | long/short signals | close eligibility | clean |
|---|---|---|---|---|---|---|---|---|---|
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | no | 200 | 2000 | 0 | - | 0/1 | eligible (parity unverified) | yes |
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | no | every prefix | 2170 | 0 | - | 0/1 | eligible (parity unverified) | yes |
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | yes | 200 | 2000 | 0 | 0 | 0/1 | eligible (parity unverified) | yes |
| 1h | proxy BTC/USDT 1h 2026-07-01..2026-10-01 | yes | every prefix | 2170 | 0 | 0 | 0/1 | eligible (parity unverified) | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | no | 200 | 2001 | 0 | - | 2/2 | eligible (parity unverified) | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | no | every prefix | 2171 | 0 | - | 2/2 | eligible (parity unverified) | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | yes | 200 | 2001 | 0 | 0 | 2/2 | eligible (parity unverified) | yes |
| 4h | proxy BTC/USDT 4h 2025-09-29..2026-10-01 | yes | every prefix | 2171 | 0 | 0 | 2/2 | eligible (parity unverified) | yes |
| 4h | venue BTC 4h test window with warmup | no | 200 | 2291 | 0 | - | 2/1 | eligible (parity unverified) | yes |
| 4h | venue BTC 4h test window with warmup | no | every prefix | 2461 | 0 | - | 2/1 | eligible (parity unverified) | yes |
| 4h | venue BTC 4h test window with warmup | yes | 200 | 2291 | 0 | 0 | 2/1 | eligible (parity unverified) | yes |
| 4h | venue BTC 4h test window with warmup | yes | every prefix | 2461 | 0 | 0 | 2/1 | eligible (parity unverified) | yes |

The parity adapter uses prebuilt frames without closed-bar timing; the closed-bar boundary is proved separately by the timed-candle check tests in `shared_scripts/test_mass_index_reversal_checks.py`.

## Limits

- The Mass Index here is the finite-recursion research variant; it is not an exact copy of the infinite-history published formula.
- Proxy legs use Binance US spot candles; basis, venue wicks and funding-driven price effects are not modelled. The legacy M1 screen has no spread, lot rounding or minimum order, so venue execution claims rest on the venue group only.
- Hyperliquid funding starts in May 2023; a proxy 2023 leg that holds a position before the first rate is funding_incomplete, and such a window can never support a pass. The 2023 window also has no prehistory for the warm-up.
- The venue windows cover 2024-09-01 to 2026-09-01 only, and earlier studies already used them, so the venue result is retrospective. Spread is a per-coin half spread from one frozen L2 snapshot plus flat slippage; queue position, partial fills and market impact are not modelled.
- Only 1h and 4h are scored. Every score reads only frozen inputs with network acquisition disabled. A research result alone cannot approve deployment.

## Reproduction

```
uv run --no-sync python backtest/candidates/mass_index_reversal_1670/run_study.py lock
uv run --no-sync python backtest/candidates/mass_index_reversal_1670/run_study.py run --cache-db /tmp/mir1670_run.db
uv run --no-sync python backtest/candidates/mass_index_reversal_1670/run_study.py parity --cache-db /tmp/mir1670_parity.db
uv run --no-sync python backtest/candidates/mass_index_reversal_1670/run_study.py render
```

Results: `results.json` (source commit `39ac920bb1c822d90ecef1cdb13d539953856ca1`, runtime 210.3 s, manifest sha256 `6bc52404179e2e7ff3785a99c3fb8cfbb5540fe0807025aab73fca0550bd97ae`, venue manifest sha256 `ca32978f37c08510ef7c3e2a07180c182540a6c9716471c190c801658a34e287`), `parity_results.json` (source commit `39ac920bb1c822d90ecef1cdb13d539953856ca1`).
