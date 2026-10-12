# Dual Thrust study (issue 1642)

**Verdict: FAIL** (proxy 1h: pass, proxy 4h: fail, venue 4h: inconclusive). Registry metadata after this study: `edge_status="no_edge"`, `edge_source="study_fail"`, `edge_ref` = this file. The candidate stays hidden from discovery. Explicit paper mode is admitted; live use still needs `allow_no_edge: true`, and no result here adds one or approves deployment. Every command in this study is suggest-only and sends no order.

## Signal

`dual_thrust` (`shared_strategies/open/dual_thrust.py`) works on UTC daily sessions that start at `session_start_hour`. At each session boundary it freezes HH (highest high), LL (lowest low), HC (highest daily close) and LC (lowest daily close) over the immediately preceding `lookback_sessions` complete sessions, sets range = max(HH - LC, HC - LL), and anchors on the current session's opening candle open. It enters long on the first closed bar of the session whose close is strictly above anchor + `k_long` x range and short on the first closed bar strictly below anchor - `k_short` x range; each side fires at most once per session, and a crossing consumes its event even when a position or a gate suppresses the entry. A close equal to a threshold fires nothing. Every candle of the prior sessions and of the current session through the decision bar must be present, exactly `expected_interval_ms` apart, on the UTC grid and well formed; otherwise the entry holds with a reason (`dt_reason`) and close and stop management continue. The evaluator reads only the supplied frame and keeps no state, so every decision is reconstructed from its closed-bar prefix.

## Protocol

`protocol.json` (sha256 `69360a0c033bfca8b21aa9e6b1f74f8fd8ec35622de0c64075a05151646ac2b4`) was locked by `run_study.py lock` at 2026-10-11T14:48:13Z (commit `165c26492bd2c1d26374c5784bfcf6558273b7b0+uncommitted-changes`), before any window was scored; `run` and `parity` refuse a different protocol. It fixes the parameter bounds, the 80-point grid, the matched contract, the selection rule, the sample floors and the pass/fail/inconclusive rules.

Contract for the candidate, `breakout`, `session_breakout` and the M1 incumbent median: direction `both`, one `tiered_tp_pct` close (+3%, full position), one fixed entry-ATR stop (`stop_loss_atr_mult: 1.0`), `comparison_mode: strict`, `intrabar_resolution: ohlc_walk`, capital $1000 per leg. Comparators keep their registry defaults: `breakout` {"atr_multiplier": 1.5, "atr_period": 14, "lookback": 20}; `session_breakout` {"atr_multiplier": 0.0, "atr_period": 14, "lookback": 1, "session": "asian", "vol_period": 20, "volume_threshold": 1.5}.

Selection (per group, candidate only, on the selection window): every dataset scored; no funding_incomplete leg; no liquidated leg; no funding row fetched while scoring; at least min_training_clusters independent position clusters pooled across the group's datasets; venue only: candle and funding coverage complete; objective highest mean Sharpe across the group's datasets, then highest mean drawdown-adjusted return, then earliest grid order. Proxy group rule: no eligible selection -> inconclusive; oos unscorable -> inconclusive; oos candidate clusters < min_held_out_clusters -> inconclusive; any check false -> fail; every check true -> pass; else inconclusive. Venue group rule: evidence incomplete -> inconclusive; no eligible selection -> inconclusive; test candidate clusters at x1 < min_held_out_clusters -> inconclusive; any check false -> fail; every check true -> pass. Overall: pass only if proxy 1h, proxy 4h and venue 4h all pass; fail if any group is fail; otherwise inconclusive.

| proxy window | start | end (exclusive) | role |
|---|---|---|---|
| is | 2025-06-10 | 2026-01-01 | selection |
| oos | 2026-01-01 | 2026-10-01 | primary held-out |
| 2023 | 2023-01-01 | 2024-01-01 | secondary held-out |
| 2024 | 2024-01-01 | 2025-01-01 | secondary held-out |
| 2025H1 | 2025-01-01 | 2025-06-10 | secondary held-out |

Departures from the default M1 windows: oos end pinned at 2026-10-01 (M1 leaves it open); 2025H1 ends at 2025-06-10 (M1: 2025-07-01) so it cannot overlap the is selection window.

| venue window | start | end (exclusive) | role |
|---|---|---|---|
| test | 2025-09-01 | 2026-09-01 | held-out: scored once with the frozen selections |
| train | 2024-09-01 | 2025-09-01 | selection: parameter sweeps and the frozen selection rule read only this window |

## Inputs, costs and assumptions

- Proxy candles: Binance US spot USDT candles stand in for Hyperliquid perpetual prices, so this is proxy evidence and not measured Hyperliquid candle evidence. The inputs are byte-identical copies of the `chande_momentum_1661` frozen files (every proxy file equals backtest/candidates/chande_momentum_1661/study_manifest.json).
- Proxy funding: Hyperliquid hourly funding charged on every held hour (`--funding charge`, `eval_windows.run_leg(funding_mode="charge")`) for every candidate, comparator and incumbent leg.
- Proxy costs: engine Hyperliquid commission `0.00045` per side and engine slippage `0.0005` per fill. This legacy M1 screen replays no spread, lot rounding or minimum order, so it cannot by itself establish venue execution parity.
- Venue candles: Hyperliquid mainnet perpetual candles from the public info API (candleSnapshot); closed bars only (every venue file equals backtest/candidates/aroon_trend_1643/study_manifest.json), interval 4h, 300 warm-up bars read by the evaluator but never scored or traded.
- Venue costs (manifest execution specification with the explicit close owner): taker fee 0.00045, maker fee 0.00015, slippage 5.0 bps per side plus the per-coin half spread, szDecimals lot rounding, minimum notional $10.0 with a 0.03 margin; cost x2 doubles spread and slippage. Funding uses the manifest's existing attachment and coverage semantics.

| venue dataset | size decimals | half spread bps | candles sha256 | funding sha256 |
|---|---|---|---|---|
| BTC 4h | 5 | 0.06 | `89f84288d462b830ceba8108ea9d69db4d3dac417c730dc9cdd13271c10fd0f8` | `b961391682a15a619fe131abedcc638fd2c311af7cd30a67cf4af32754cabc48` |
| ETH 4h | 4 | 0.19 | `a17523ecbb30f7b594aa80002b3c5ad80b73a7bdd8122d5512683585a8eec3ae` | `1b541abb8801a444cac4e23ebe31d71cbd7dc3b7d185456830daca1c09c6ea66` |
| SOL 4h | 2 | 0.42 | `5ca4fa74940f38f180a85eb81ccaa96bb10f08988d1351c280abb2ce175fdd4c` | `a498781bca4cf017f7f772f5a0c00c0853979bacfd147303394f63a63da5af40` |

- Sizing: capital $1000 per leg, no leverage and no perps sizing block (Backtester leverage None, sizing defaults {"leverage": null, "risk_per_trade_pct": null}), so each entry commits the leg's whole available cash (engine entry fraction 1.0) unlevered; held-out candidate entry notionals ranged from $630.47 to $1933.26.
- Stop: one fixed entry-ATR stop at 1.0 x ATR. Close: one `tiered_tp_pct` tier at +3%.

| proxy file | rows | sha256 |
|---|---|---|
| `data/proxy/BTC_1h_candles.csv.gz` | 32840 | `d107c60f72363372d03273ba6439ca42a0d867a9089c5de171e9306af3ba2d17` |
| `data/proxy/BTC_4h_candles.csv.gz` | 8211 | `dd0758a2862955eb53b060df5fbace3e98f8bc2a3e0254842bca0d36c96f106f` |
| `data/proxy/BTC_funding.csv.gz` | 29142 | `520fd979d5594b4c2fb950cd52aa327749a124d9971a59ec3419752dd24a27f8` |
| `data/proxy/ETH_1h_candles.csv.gz` | 32840 | `59d08735aecb22b63bab8189c7c32b11101e80da42b2ec724f39e308548b7913` |
| `data/proxy/ETH_4h_candles.csv.gz` | 8211 | `03df63c5dc65ba532a252e8c00118986321fcb124934bb7536e842d678fc9dd8` |
| `data/proxy/ETH_funding.csv.gz` | 29142 | `4b7694d8fc26cb7245ffa882cbdcb2d571d17f3a9f1abd6832fb0deed4985b41` |
| `data/proxy/SOL_1h_candles.csv.gz` | 32840 | `e4e5c15ffcce696253d78dc3b3c3448725e8cf3a0114a9fbc4b1df5f2a0efeec` |
| `data/proxy/SOL_4h_candles.csv.gz` | 8211 | `c1e5775973d00b0b07597da3db4213d62378ef566fe2e4a3b619612b59abc8ab` |
| `data/proxy/SOL_funding.csv.gz` | 29142 | `b29de7625f7484f0971336e31ba52f5b042127e4eef4d8ee1e4554994383cd2b` |

## proxy_1h (expected_interval_ms = 3600000)

**Group verdict: pass** (every frozen proxy check is true).

| check | value |
|---|---|
| m1_oos_pass | true |
| m1_secondary_passes | true |
| oos_beats_breakout_sharpe | true |
| oos_beats_session_breakout_sharpe | true |
| oos_net_positive | true |

Selection: selected, `N=7 kL=0.5 kS=1 h=0` (mean Sharpe 0.79, mean DDadj 0.76, 28 clusters; 79 of 80 variants eligible). Grid points: 80; excluded before scoring: 0.

### Held-out `2023`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | funding_incomplete | -1.49 | -34.13 | -1026.14 | -55.92 | 1033 (572/461) | 390 | 576.72 | -25.72 | 3 |
| candidate | funding_incomplete | 1.10 | 15.82 | 474.76 | -12.40 | 115 (107/8) | 88 | 82.44 | -13.59 | 3 |
| session_breakout | funding_incomplete | -1.72 | -45.46 | -1366.74 | -65.32 | 1121 (593/528) | 365 | 542.54 | -21.75 | 3 |

### Held-out `2024`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | fail | -0.82 | -31.80 | -953.85 | -47.89 | 1041 (535/506) | 393 | 593.80 | -60.68 | 0 |
| candidate | pass | 0.40 | 4.67 | 140.23 | -10.74 | 83 (75/8) | 55 | 57.97 | -30.04 | 0 |
| session_breakout | fail | -0.70 | -30.40 | -911.90 | -54.46 | 1195 (633/562) | 413 | 683.31 | -74.40 | 0 |

### Held-out `2025H1`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | fail | -0.28 | -7.77 | -236.71 | -33.32 | 467 (220/247) | 178 | 321.87 | -6.91 | 0 |
| candidate | pass | -0.01 | -0.80 | -25.78 | -9.14 | 38 (35/3) | 24 | 24.75 | -4.39 | 0 |
| session_breakout | pass | 0.71 | 10.42 | 308.46 | -31.52 | 528 (254/274) | 192 | 379.67 | -6.92 | 0 |

### Held-out `oos`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | pass | -0.11 | -3.87 | -116.04 | -35.38 | 715 (359/356) | 224 | 496.44 | -13.24 | 0 |
| candidate | pass | 0.34 | 3.07 | 92.22 | -10.67 | 65 (59/6) | 44 | 42.99 | -5.57 | 0 |
| session_breakout | fail | -1.54 | -35.17 | -1056.24 | -49.43 | 829 (432/397) | 268 | 467.97 | -14.26 | 0 |

Concentration (candidate, oos): top dataset share of absolute net PnL 0.44, top five positions' share 0.20, long net $80.58, short net $11.61.

Unscorable windows: 2023: breakout: 3 funding_incomplete legs, breakout: 3 incumbent datasets funding_incomplete, candidate: 3 funding_incomplete legs, candidate: 3 incumbent datasets funding_incomplete, session_breakout: 3 funding_incomplete legs, session_breakout: 3 incumbent datasets funding_incomplete.

Funding coverage and provenance (candidate legs, oos):

| dataset | mode | source | complete | expected h | priced h | missing h | venue-absent h | unpriced held h | funding sha256 |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `920006cfb21d6b65` |
| ETH/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `51039b1ca124e8b2` |
| SOL/USDT 1h | charge | cache | yes | 6551 | 6551 | 0 | 0 | 0 | `2c0d96e1d7d8bf4a` |

Stability: Spearman rank correlation of train vs oos mean Sharpe across the 80 variants: 0.20; variants with positive oos net PnL: 11; variants with an M1 pass on oos: 29; one-step grid neighbours of the selection: 4 (median train Sharpe 0.21, minimum 0.01).

Start-of-window history and input holds for the selected parameters (proxy windows load no earlier bars; venue windows include the manifest warm-up):

| window | dataset | bars | first valid bar | hold reasons |
|---|---|---|---|---|
| 2023 | BTC/USDT 1h | 8753 | 2023-01-08 00:00:00 | cadence_gap 180, insufficient_history 168 |
| 2023 | ETH/USDT 1h | 8753 | 2023-01-08 00:00:00 | cadence_gap 180, insufficient_history 168 |
| 2023 | SOL/USDT 1h | 8753 | 2023-01-08 00:00:00 | cadence_gap 180, insufficient_history 168 |
| 2024 | BTC/USDT 1h | 8784 | 2024-01-08 00:00:00 | insufficient_history 168 |
| 2024 | ETH/USDT 1h | 8784 | 2024-01-08 00:00:00 | insufficient_history 168 |
| 2024 | SOL/USDT 1h | 8784 | 2024-01-08 00:00:00 | insufficient_history 168 |
| 2025H1 | BTC/USDT 1h | 3840 | 2025-01-08 00:00:00 | insufficient_history 168 |
| 2025H1 | ETH/USDT 1h | 3840 | 2025-01-08 00:00:00 | insufficient_history 168 |
| 2025H1 | SOL/USDT 1h | 3840 | 2025-01-08 00:00:00 | insufficient_history 168 |
| is | BTC/USDT 1h | 4920 | 2025-06-17 00:00:00 | insufficient_history 168 |
| is | ETH/USDT 1h | 4920 | 2025-06-17 00:00:00 | insufficient_history 168 |
| is | SOL/USDT 1h | 4920 | 2025-06-17 00:00:00 | insufficient_history 168 |
| oos | BTC/USDT 1h | 6543 | 2026-01-08 00:00:00 | cadence_gap 179, insufficient_history 168 |
| oos | ETH/USDT 1h | 6543 | 2026-01-08 00:00:00 | cadence_gap 179, insufficient_history 168 |
| oos | SOL/USDT 1h | 6543 | 2026-01-08 00:00:00 | cadence_gap 179, insufficient_history 168 |

Entry audit: 65 held-out candidate entries; each fill follows a valid raw signal on the prior closed bar, and `results.json` stores its evaluated input span hash and every indicator value. First entries:

| dataset | side | signal bar | fill bar | anchor | range | upper | lower | close | net $ |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 1h | long | 2026-01-13 18:00:00 | 2026-01-13 19:00:00 | 91307 | 4480.93 | 93547.4 | 86826.1 | 93637.7 | 28.04 |
| BTC/USDT 1h | short | 2026-01-29 17:00:00 | 2026-01-29 18:00:00 | 89283.1 | 4587.88 | 91577 | 84695.2 | 84223.3 | 32.38 |
| BTC/USDT 1h | long | 2026-02-25 14:00:00 | 2026-02-25 15:00:00 | 64072.6 | 5472.38 | 66808.8 | 58600.2 | 67006.7 | 36.64 |
| BTC/USDT 1h | long | 2026-03-02 15:00:00 | 2026-03-02 16:00:00 | 65792.3 | 6075.4 | 68830 | 59716.9 | 69121.3 | -13.90 |
| BTC/USDT 1h | long | 2026-03-04 09:00:00 | 2026-03-04 10:00:00 | 68244.8 | 5763.6 | 71126.6 | 62481.2 | 71438.3 | -13.09 |
| BTC/USDT 1h | long | 2026-04-05 23:00:00 | 2026-04-06 00:00:00 | 67306.3 | 3278.41 | 68945.5 | 64027.9 | 69007.2 | -7.02 |
| BTC/USDT 1h | long | 2026-04-07 22:00:00 | 2026-04-07 23:00:00 | 68828.6 | 3471.17 | 70564.1 | 65357.4 | 71496.4 | -10.77 |
| BTC/USDT 1h | long | 2026-04-13 22:00:00 | 2026-04-13 23:00:00 | 70693.5 | 5296.58 | 73341.8 | 65396.9 | 74674.2 | -9.98 |
| BTC/USDT 1h | long | 2026-04-17 14:00:00 | 2026-04-17 15:00:00 | 75122.2 | 5312.51 | 77778.5 | 69809.7 | 77973.8 | -8.64 |
| BTC/USDT 1h | long | 2026-04-22 13:00:00 | 2026-04-22 14:00:00 | 76373 | 4511.08 | 78628.6 | 71861.9 | 78835.6 | -6.91 |

<details><summary>Every evaluated variant (training window and held-out diagnostic)</summary>

| variant | train Sharpe | train clusters | eligible | oos Sharpe | oos return % | oos net $ | oos clusters | oos M1 |
|---|---|---|---|---|---|---|---|---|
| N=2 kL=0.25 kS=0.25 h=0 | -1.95 | 202 | yes | -0.55 | -13.85 | -415.61 | 250 | fail |
| N=2 kL=0.25 kS=0.5 h=0 | -1.67 | 198 | yes | -0.78 | -16.33 | -490.93 | 227 | fail |
| N=2 kL=0.25 kS=0.75 h=0 | -1.89 | 169 | yes | -0.82 | -13.60 | -407.82 | 205 | fail |
| N=2 kL=0.25 kS=1 h=0 | -1.82 | 157 | yes | 0.15 | 3.64 | 109.08 | 197 | pass |
| N=2 kL=0.5 kS=0.25 h=0 | -1.50 | 184 | yes | -1.19 | -25.52 | -765.60 | 220 | fail |
| N=2 kL=0.5 kS=0.5 h=0 | -1.14 | 174 | yes | -1.19 | -19.88 | -597.11 | 192 | fail |
| N=2 kL=0.5 kS=0.75 h=0 | -1.34 | 150 | yes | -1.27 | -20.28 | -608.54 | 172 | fail |
| N=2 kL=0.5 kS=1 h=0 | -1.18 | 133 | yes | -0.31 | -5.13 | -153.99 | 152 | fail |
| N=2 kL=0.75 kS=0.25 h=0 | -0.57 | 153 | yes | -1.25 | -25.97 | -779.11 | 198 | fail |
| N=2 kL=0.75 kS=0.5 h=0 | -0.22 | 141 | yes | -1.07 | -19.76 | -593.49 | 164 | fail |
| N=2 kL=0.75 kS=0.75 h=0 | -0.21 | 113 | yes | -1.18 | -18.70 | -561.00 | 144 | fail |
| N=2 kL=0.75 kS=1 h=0 | -0.03 | 93 | yes | -0.22 | -4.87 | -145.98 | 123 | pass |
| N=2 kL=1 kS=0.25 h=0 | -1.00 | 140 | yes | -1.06 | -22.30 | -668.87 | 194 | fail |
| N=2 kL=1 kS=0.5 h=0 | -0.62 | 124 | yes | -1.04 | -16.49 | -495.45 | 150 | fail |
| N=2 kL=1 kS=0.75 h=0 | -0.67 | 96 | yes | -1.26 | -18.73 | -561.80 | 127 | fail |
| N=2 kL=1 kS=1 h=0 | -0.58 | 75 | yes | -0.27 | -4.18 | -125.19 | 102 | pass |
| N=3 kL=0.25 kS=0.25 h=0 | -1.79 | 201 | yes | -0.04 | -4.49 | -135.45 | 217 | pass |
| N=3 kL=0.25 kS=0.5 h=0 | -2.10 | 185 | yes | -0.35 | -8.26 | -247.90 | 185 | fail |
| N=3 kL=0.25 kS=0.75 h=0 | -2.02 | 157 | yes | -0.73 | -13.40 | -402.02 | 174 | fail |
| N=3 kL=0.25 kS=1 h=0 | -1.86 | 146 | yes | -0.23 | -5.25 | -157.39 | 157 | fail |
| N=3 kL=0.5 kS=0.25 h=0 | -0.97 | 162 | yes | -0.14 | -5.97 | -180.02 | 180 | pass |
| N=3 kL=0.5 kS=0.5 h=0 | -1.31 | 145 | yes | -0.64 | -11.61 | -348.24 | 153 | fail |
| N=3 kL=0.5 kS=0.75 h=0 | -1.11 | 112 | yes | -1.17 | -16.41 | -492.49 | 131 | fail |
| N=3 kL=0.5 kS=1 h=0 | -0.81 | 96 | yes | -0.55 | -8.06 | -241.81 | 107 | fail |
| N=3 kL=0.75 kS=0.25 h=0 | -0.03 | 126 | yes | -0.58 | -12.91 | -387.97 | 170 | fail |
| N=3 kL=0.75 kS=0.5 h=0 | -0.01 | 100 | yes | -1.17 | -16.48 | -494.26 | 130 | fail |
| N=3 kL=0.75 kS=0.75 h=0 | 0.21 | 75 | yes | -1.77 | -21.77 | -652.95 | 109 | fail |
| N=3 kL=0.75 kS=1 h=0 | 0.73 | 57 | yes | -1.23 | -13.72 | -411.59 | 84 | fail |
| N=3 kL=1 kS=0.25 h=0 | -0.74 | 119 | yes | 0.04 | -1.38 | -42.31 | 145 | pass |
| N=3 kL=1 kS=0.5 h=0 | -0.91 | 84 | yes | -0.48 | -5.18 | -155.58 | 107 | fail |
| N=3 kL=1 kS=0.75 h=0 | -0.84 | 58 | yes | -1.12 | -12.02 | -360.59 | 78 | fail |
| N=3 kL=1 kS=1 h=0 | -0.35 | 39 | yes | -0.35 | -3.07 | -91.94 | 53 | fail |
| N=4 kL=0.25 kS=0.25 h=0 | -1.82 | 186 | yes | -1.02 | -22.03 | -661.65 | 205 | fail |
| N=4 kL=0.25 kS=0.5 h=0 | -1.69 | 161 | yes | -0.20 | -5.76 | -172.99 | 163 | pass |
| N=4 kL=0.25 kS=0.75 h=0 | -1.59 | 130 | yes | -1.00 | -15.99 | -479.76 | 152 | fail |
| N=4 kL=0.25 kS=1 h=0 | -1.93 | 128 | yes | -0.20 | -4.16 | -124.91 | 136 | pass |
| N=4 kL=0.5 kS=0.25 h=0 | -0.54 | 134 | yes | -0.67 | -12.56 | -377.55 | 169 | fail |
| N=4 kL=0.5 kS=0.5 h=0 | -0.14 | 114 | yes | -0.01 | -0.55 | -16.64 | 123 | pass |
| N=4 kL=0.5 kS=0.75 h=0 | 0.12 | 79 | yes | -1.01 | -12.81 | -384.25 | 105 | fail |
| N=4 kL=0.5 kS=1 h=0 | -0.04 | 72 | yes | -0.02 | -0.51 | -15.27 | 86 | pass |
| N=4 kL=0.75 kS=0.25 h=0 | -0.61 | 113 | yes | -0.78 | -15.09 | -453.66 | 139 | fail |
| N=4 kL=0.75 kS=0.5 h=0 | -0.01 | 77 | yes | -0.13 | -3.12 | -93.70 | 89 | pass |
| N=4 kL=0.75 kS=0.75 h=0 | 0.09 | 46 | yes | -1.31 | -14.63 | -438.84 | 69 | fail |
| N=4 kL=0.75 kS=1 h=0 | -0.05 | 38 | yes | -0.19 | -2.63 | -78.84 | 50 | fail |
| N=4 kL=1 kS=0.25 h=0 | -0.77 | 105 | yes | -0.81 | -13.71 | -412.05 | 132 | fail |
| N=4 kL=1 kS=0.5 h=0 | -0.18 | 64 | yes | -0.07 | -1.14 | -34.14 | 78 | pass |
| N=4 kL=1 kS=0.75 h=0 | -0.05 | 33 | yes | -1.54 | -13.03 | -390.75 | 56 | fail |
| N=4 kL=1 kS=1 h=0 | -0.42 | 24 | yes | -0.09 | -0.76 | -22.74 | 37 | pass |
| N=5 kL=0.25 kS=0.25 h=0 | -1.61 | 176 | yes | -0.78 | -15.49 | -465.64 | 186 | fail |
| N=5 kL=0.25 kS=0.5 h=0 | -0.86 | 144 | yes | -0.01 | -2.02 | -60.66 | 141 | pass |
| N=5 kL=0.25 kS=0.75 h=0 | -1.48 | 129 | yes | -0.55 | -9.59 | -287.54 | 128 | fail |
| N=5 kL=0.25 kS=1 h=0 | -1.64 | 118 | yes | -0.01 | -1.73 | -51.83 | 117 | pass |
| N=5 kL=0.5 kS=0.25 h=0 | -0.40 | 115 | yes | -0.47 | -9.45 | -284.20 | 150 | fail |
| N=5 kL=0.5 kS=0.5 h=0 | 0.64 | 83 | yes | 0.16 | 2.76 | 82.76 | 103 | pass |
| N=5 kL=0.5 kS=0.75 h=0 | 0.06 | 64 | yes | -0.39 | -4.57 | -137.08 | 82 | fail |
| N=5 kL=0.5 kS=1 h=0 | 0.14 | 52 | yes | 0.29 | 4.09 | 122.59 | 71 | pass |
| N=5 kL=0.75 kS=0.25 h=0 | -0.71 | 94 | yes | -0.63 | -11.07 | -333.13 | 124 | fail |
| N=5 kL=0.75 kS=0.5 h=0 | 0.34 | 55 | yes | 0.09 | 0.78 | 23.31 | 69 | pass |
| N=5 kL=0.75 kS=0.75 h=0 | -0.44 | 35 | yes | -0.67 | -6.48 | -194.32 | 47 | fail |
| N=5 kL=0.75 kS=1 h=0 | -0.52 | 23 | yes | 0.22 | 1.84 | 55.16 | 36 | pass |
| N=5 kL=1 kS=0.25 h=0 | -0.84 | 91 | yes | -0.95 | -14.82 | -445.55 | 115 | fail |
| N=5 kL=1 kS=0.5 h=0 | 0.24 | 49 | yes | -0.31 | -3.68 | -110.38 | 60 | fail |
| N=5 kL=1 kS=0.75 h=0 | -0.53 | 29 | yes | -1.36 | -10.59 | -317.81 | 38 | fail |
| N=5 kL=1 kS=1 h=0 | -0.93 | 17 | yes | -0.38 | -2.71 | -81.22 | 27 | fail |
| N=7 kL=0.25 kS=0.25 h=0 | -0.47 | 148 | yes | -0.56 | -11.67 | -350.09 | 151 | fail |
| N=7 kL=0.25 kS=0.5 h=0 | -0.25 | 108 | yes | -0.42 | -7.62 | -228.77 | 116 | fail |
| N=7 kL=0.25 kS=0.75 h=0 | -0.31 | 96 | yes | -0.93 | -13.08 | -392.37 | 103 | fail |
| N=7 kL=0.25 kS=1 h=0 | 0.01 | 88 | yes | -0.49 | -7.38 | -221.24 | 95 | fail |
| N=7 kL=0.5 kS=0.25 h=0 | 0.02 | 91 | yes | -0.07 | 0.01 | 0.39 | 117 | pass |
| N=7 kL=0.5 kS=0.5 h=0 | 0.28 | 48 | yes | 0.27 | 3.55 | 106.69 | 68 | pass |
| N=7 kL=0.5 kS=0.75 h=0 | 0.42 | 36 | yes | -0.36 | -2.69 | -80.88 | 52 | pass |
| N=7 kL=0.5 kS=1 h=0 | 0.79 | 28 | yes | 0.34 | 3.07 | 92.22 | 44 | pass |
| N=7 kL=0.75 kS=0.25 h=0 | -0.35 | 76 | yes | -0.27 | -3.00 | -90.03 | 97 | pass |
| N=7 kL=0.75 kS=0.5 h=0 | -0.25 | 31 | yes | 0.06 | 0.75 | 22.37 | 45 | pass |
| N=7 kL=0.75 kS=0.75 h=0 | -0.43 | 19 | yes | -0.87 | -5.38 | -161.37 | 29 | fail |
| N=7 kL=0.75 kS=1 h=0 | 0.27 | 11 | yes | 0.08 | 0.32 | 9.50 | 21 | pass |
| N=7 kL=1 kS=0.25 h=0 | -0.43 | 74 | yes | -0.34 | -3.68 | -110.34 | 88 | pass |
| N=7 kL=1 kS=0.5 h=0 | -0.39 | 27 | yes | -0.03 | 0.11 | 3.33 | 36 | pass |
| N=7 kL=1 kS=0.75 h=0 | -0.67 | 15 | yes | -1.35 | -5.95 | -178.66 | 20 | fail |
| N=7 kL=1 kS=1 h=0 | 0.10 | 7 | no | 0.05 | -0.25 | -7.37 | 12 | pass |

</details>

## proxy_4h (expected_interval_ms = 14400000)

**Group verdict: fail** (false: oos_net_positive).

| check | value |
|---|---|
| m1_oos_pass | true |
| m1_secondary_passes | unknown |
| oos_beats_breakout_sharpe | true |
| oos_beats_session_breakout_sharpe | true |
| oos_net_positive | false |

Selection: selected, `N=5 kL=0.5 kS=0.5 h=0` (mean Sharpe 1.63, mean DDadj 3.22, 52 clusters; 79 of 80 variants eligible). Grid points: 80; excluded before scoring: 0.

### Held-out `2023`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | funding_incomplete | 0.38 | 9.77 | 293.18 | -44.51 | 289 (172/117) | 117 | 203.11 | -32.42 | 3 |
| candidate | funding_incomplete | 0.50 | 30.90 | 927.08 | -30.06 | 227 (138/89) | 98 | 173.78 | -59.53 | 3 |
| session_breakout | funding_incomplete | -0.39 | -13.06 | -391.76 | -50.76 | 427 (232/195) | 140 | 285.65 | -58.08 | 3 |

### Held-out `2024`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | pass | 0.04 | -3.61 | -108.26 | -46.63 | 294 (155/139) | 122 | 197.20 | -73.92 | 0 |
| candidate | fail | -0.47 | -14.88 | -446.31 | -38.48 | 221 (117/104) | 114 | 129.61 | -41.40 | 0 |
| session_breakout | fail | -0.23 | -11.65 | -349.53 | -57.17 | 457 (234/223) | 163 | 276.07 | -72.77 | 0 |

### Held-out `2025H1`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | pass | 0.13 | 4.54 | 164.57 | -27.57 | 134 (66/68) | 62 | 93.91 | -5.65 | 0 |
| candidate | pass | 0.12 | 4.25 | 137.37 | -25.34 | 99 (45/54) | 50 | 67.49 | -3.11 | 0 |
| session_breakout | fail | -0.54 | -13.13 | -366.36 | -33.57 | 188 (92/96) | 74 | 116.87 | 6.81 | 0 |

### Held-out `oos`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | pass | -0.37 | -10.18 | -305.42 | -35.08 | 201 (104/97) | 81 | 123.47 | -5.97 | 0 |
| candidate | pass | -0.12 | -4.32 | -129.62 | -25.32 | 151 (74/77) | 73 | 98.37 | -4.65 | 0 |
| session_breakout | fail | -0.92 | -25.92 | -779.17 | -46.56 | 325 (168/157) | 106 | 197.61 | -12.01 | 0 |

Concentration (candidate, oos): top dataset share of absolute net PnL 0.55, top five positions' share 0.09, long net $110.03, short net $-239.68.

Unscorable windows: 2023: breakout: 3 funding_incomplete legs, breakout: 3 incumbent datasets funding_incomplete, candidate: 3 funding_incomplete legs, candidate: 3 incumbent datasets funding_incomplete, session_breakout: 3 funding_incomplete legs, session_breakout: 3 incumbent datasets funding_incomplete.

Funding coverage and provenance (candidate legs, oos):

| dataset | mode | source | complete | expected h | priced h | missing h | venue-absent h | unpriced held h | funding sha256 |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `c9efdb7d129a9a70` |
| ETH/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `c313f79e21796506` |
| SOL/USDT 4h | charge | cache | yes | 6548 | 6548 | 0 | 0 | 0 | `246de07d578592eb` |

Stability: Spearman rank correlation of train vs oos mean Sharpe across the 80 variants: -0.15; variants with positive oos net PnL: 30; variants with an M1 pass on oos: 76; one-step grid neighbours of the selection: 6 (median train Sharpe 1.08, minimum 0.35).

Start-of-window history and input holds for the selected parameters (proxy windows load no earlier bars; venue windows include the manifest warm-up):

| window | dataset | bars | first valid bar | hold reasons |
|---|---|---|---|---|
| 2023 | BTC/USDT 4h | 2189 | 2023-01-06 00:00:00 | cadence_gap 33, insufficient_history 30 |
| 2023 | ETH/USDT 4h | 2189 | 2023-01-06 00:00:00 | cadence_gap 33, insufficient_history 30 |
| 2023 | SOL/USDT 4h | 2189 | 2023-01-06 00:00:00 | cadence_gap 33, insufficient_history 30 |
| 2024 | BTC/USDT 4h | 2196 | 2024-01-06 00:00:00 | insufficient_history 30 |
| 2024 | ETH/USDT 4h | 2196 | 2024-01-06 00:00:00 | insufficient_history 30 |
| 2024 | SOL/USDT 4h | 2196 | 2024-01-06 00:00:00 | insufficient_history 30 |
| 2025H1 | BTC/USDT 4h | 960 | 2025-01-06 00:00:00 | insufficient_history 30 |
| 2025H1 | ETH/USDT 4h | 960 | 2025-01-06 00:00:00 | insufficient_history 30 |
| 2025H1 | SOL/USDT 4h | 960 | 2025-01-06 00:00:00 | insufficient_history 30 |
| is | BTC/USDT 4h | 1230 | 2025-06-15 00:00:00 | insufficient_history 30 |
| is | ETH/USDT 4h | 1230 | 2025-06-15 00:00:00 | insufficient_history 30 |
| is | SOL/USDT 4h | 1230 | 2025-06-15 00:00:00 | insufficient_history 30 |
| oos | BTC/USDT 4h | 1636 | 2026-01-06 00:00:00 | cadence_gap 33, insufficient_history 30 |
| oos | ETH/USDT 4h | 1636 | 2026-01-06 00:00:00 | cadence_gap 33, insufficient_history 30 |
| oos | SOL/USDT 4h | 1636 | 2026-01-06 00:00:00 | cadence_gap 33, insufficient_history 30 |

Entry audit: 151 held-out candidate entries; each fill follows a valid raw signal on the prior closed bar, and `results.json` stores its evaluated input span hash and every indicator value. First entries:

| dataset | side | signal bar | fill bar | anchor | range | upper | lower | close | net $ |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 4h | long | 2026-01-13 12:00:00 | 2026-01-13 16:00:00 | 91307 | 2037.34 | 92325.6 | 90288.3 | 93440.3 | 30.88 |
| BTC/USDT 4h | short | 2026-01-20 16:00:00 | 2026-01-20 20:00:00 | 92628.8 | 4533.92 | 94895.7 | 90361.8 | 89728.3 | -12.86 |
| BTC/USDT 4h | short | 2026-01-25 16:00:00 | 2026-01-25 20:00:00 | 89248.8 | 4445.63 | 91471.6 | 87026 | 86584.7 | -9.88 |
| BTC/USDT 4h | short | 2026-01-29 12:00:00 | 2026-01-29 16:00:00 | 89283.1 | 3911.87 | 91239 | 87327.1 | 84885.6 | 39.62 |
| BTC/USDT 4h | short | 2026-01-31 16:00:00 | 2026-01-31 20:00:00 | 84200 | 8116.87 | 88258.4 | 80141.6 | 77805.8 | 33.07 |
| BTC/USDT 4h | short | 2026-02-05 12:00:00 | 2026-02-05 16:00:00 | 73152.7 | 11098.5 | 78702 | 67603.5 | 67495.9 | 32.19 |
| BTC/USDT 4h | short | 2026-02-23 00:00:00 | 2026-02-23 04:00:00 | 67669.8 | 2373.18 | 68856.4 | 66483.2 | 64950.8 | -14.23 |
| BTC/USDT 4h | long | 2026-02-25 12:00:00 | 2026-02-25 16:00:00 | 64072.6 | 5472.38 | 66808.8 | 61336.4 | 67410.1 | -21.48 |
| BTC/USDT 4h | long | 2026-03-02 12:00:00 | 2026-03-02 16:00:00 | 65792.3 | 4910.1 | 68247.3 | 63337.2 | 69121.3 | -25.96 |
| BTC/USDT 4h | long | 2026-03-04 12:00:00 | 2026-03-04 16:00:00 | 68244.8 | 5763.6 | 71126.6 | 65363 | 73377.1 | -26.54 |

<details><summary>Every evaluated variant (training window and held-out diagnostic)</summary>

| variant | train Sharpe | train clusters | eligible | oos Sharpe | oos return % | oos net $ | oos clusters | oos M1 |
|---|---|---|---|---|---|---|---|---|
| N=2 kL=0.25 kS=0.25 h=0 | -0.83 | 63 | yes | 0.32 | 5.81 | 161.88 | 82 | pass |
| N=2 kL=0.25 kS=0.5 h=0 | -0.52 | 60 | yes | 0.22 | 2.78 | 71.28 | 89 | pass |
| N=2 kL=0.25 kS=0.75 h=0 | -0.09 | 72 | yes | 0.31 | 3.27 | 97.18 | 96 | pass |
| N=2 kL=0.25 kS=1 h=0 | -0.67 | 77 | yes | 0.48 | 9.12 | 272.62 | 98 | pass |
| N=2 kL=0.5 kS=0.25 h=0 | -0.66 | 75 | yes | -0.16 | -8.53 | -265.85 | 95 | pass |
| N=2 kL=0.5 kS=0.5 h=0 | -0.37 | 72 | yes | 0.06 | 0.44 | 2.08 | 97 | pass |
| N=2 kL=0.5 kS=0.75 h=0 | 0.01 | 83 | yes | 0.07 | -1.09 | -32.65 | 92 | pass |
| N=2 kL=0.5 kS=1 h=0 | -0.25 | 75 | yes | 0.12 | -0.46 | -13.82 | 94 | pass |
| N=2 kL=0.75 kS=0.25 h=0 | -0.61 | 75 | yes | -0.19 | -5.74 | -183.67 | 97 | pass |
| N=2 kL=0.75 kS=0.5 h=0 | -0.18 | 71 | yes | -0.10 | -1.15 | -46.77 | 94 | pass |
| N=2 kL=0.75 kS=0.75 h=0 | 0.35 | 73 | yes | -0.10 | -2.93 | -88.10 | 86 | pass |
| N=2 kL=0.75 kS=1 h=0 | 0.05 | 64 | yes | 0.01 | -1.87 | -56.23 | 84 | pass |
| N=2 kL=1 kS=0.25 h=0 | -0.57 | 75 | yes | -0.51 | -16.16 | -493.15 | 99 | pass |
| N=2 kL=1 kS=0.5 h=0 | 0.06 | 64 | yes | -0.23 | -7.55 | -236.06 | 94 | pass |
| N=2 kL=1 kS=0.75 h=0 | 0.71 | 63 | yes | -0.25 | -7.60 | -227.98 | 80 | pass |
| N=2 kL=1 kS=1 h=0 | 0.48 | 54 | yes | -0.30 | -7.21 | -216.26 | 74 | pass |
| N=3 kL=0.25 kS=0.25 h=0 | -0.68 | 79 | yes | -0.04 | 0.99 | 16.47 | 89 | pass |
| N=3 kL=0.25 kS=0.5 h=0 | -0.52 | 80 | yes | 0.03 | -3.66 | -109.80 | 89 | pass |
| N=3 kL=0.25 kS=0.75 h=0 | -0.12 | 74 | yes | -0.16 | -5.94 | -178.12 | 96 | pass |
| N=3 kL=0.25 kS=1 h=0 | -0.66 | 73 | yes | -0.27 | -8.86 | -265.89 | 95 | pass |
| N=3 kL=0.5 kS=0.25 h=0 | -0.33 | 67 | yes | -0.00 | 0.44 | 0.86 | 95 | pass |
| N=3 kL=0.5 kS=0.5 h=0 | 0.04 | 66 | yes | 0.10 | -1.19 | -35.66 | 87 | pass |
| N=3 kL=0.5 kS=0.75 h=0 | 0.39 | 63 | yes | -0.26 | -7.89 | -236.65 | 79 | pass |
| N=3 kL=0.5 kS=1 h=0 | -0.01 | 59 | yes | -0.37 | -9.30 | -278.90 | 74 | pass |
| N=3 kL=0.75 kS=0.25 h=0 | 0.14 | 67 | yes | 0.11 | 5.37 | 147.23 | 96 | pass |
| N=3 kL=0.75 kS=0.5 h=0 | 0.57 | 60 | yes | -0.08 | -4.30 | -129.17 | 81 | pass |
| N=3 kL=0.75 kS=0.75 h=0 | 1.04 | 52 | yes | -0.50 | -10.24 | -307.22 | 70 | fail |
| N=3 kL=0.75 kS=1 h=0 | 0.81 | 44 | yes | -0.68 | -10.76 | -322.94 | 61 | fail |
| N=3 kL=1 kS=0.25 h=0 | -0.21 | 66 | yes | 0.32 | 6.74 | 190.06 | 87 | pass |
| N=3 kL=1 kS=0.5 h=0 | 0.33 | 52 | yes | 0.27 | 2.62 | 78.65 | 66 | pass |
| N=3 kL=1 kS=0.75 h=0 | 0.82 | 42 | yes | -0.12 | -2.24 | -67.31 | 54 | pass |
| N=3 kL=1 kS=1 h=0 | 0.57 | 33 | yes | -0.38 | -4.64 | -139.15 | 43 | pass |
| N=4 kL=0.25 kS=0.25 h=0 | -0.91 | 77 | yes | -0.42 | -10.47 | -324.79 | 95 | pass |
| N=4 kL=0.25 kS=0.5 h=0 | -0.36 | 74 | yes | -0.44 | -14.10 | -423.07 | 92 | fail |
| N=4 kL=0.25 kS=0.75 h=0 | -0.46 | 70 | yes | -0.38 | -11.88 | -356.57 | 93 | pass |
| N=4 kL=0.25 kS=1 h=0 | -1.47 | 67 | yes | -0.32 | -9.74 | -292.17 | 84 | pass |
| N=4 kL=0.5 kS=0.25 h=0 | 0.06 | 71 | yes | 0.21 | 6.28 | 175.31 | 93 | pass |
| N=4 kL=0.5 kS=0.5 h=0 | 0.92 | 58 | yes | -0.16 | -5.58 | -167.35 | 80 | pass |
| N=4 kL=0.5 kS=0.75 h=0 | 0.92 | 52 | yes | -0.31 | -7.48 | -224.47 | 78 | pass |
| N=4 kL=0.5 kS=1 h=0 | -0.24 | 47 | yes | -0.19 | -3.96 | -118.74 | 64 | pass |
| N=4 kL=0.75 kS=0.25 h=0 | 0.10 | 70 | yes | -0.09 | -4.01 | -130.46 | 83 | pass |
| N=4 kL=0.75 kS=0.5 h=0 | 1.23 | 48 | yes | -0.46 | -9.61 | -288.32 | 60 | pass |
| N=4 kL=0.75 kS=0.75 h=0 | 1.49 | 39 | yes | -0.57 | -8.07 | -242.20 | 51 | fail |
| N=4 kL=0.75 kS=1 h=0 | 0.10 | 32 | yes | -0.43 | -4.99 | -149.78 | 37 | pass |
| N=4 kL=1 kS=0.25 h=0 | -0.26 | 60 | yes | 0.22 | 2.49 | 64.37 | 77 | pass |
| N=4 kL=1 kS=0.5 h=0 | 0.69 | 38 | yes | -0.06 | -2.41 | -72.45 | 52 | pass |
| N=4 kL=1 kS=0.75 h=0 | 0.76 | 28 | yes | -0.12 | -0.87 | -26.22 | 42 | pass |
| N=4 kL=1 kS=1 h=0 | -1.17 | 21 | yes | 0.15 | 2.36 | 70.95 | 28 | pass |
| N=5 kL=0.25 kS=0.25 h=0 | -0.16 | 74 | yes | 0.08 | 0.06 | -9.26 | 97 | pass |
| N=5 kL=0.25 kS=0.5 h=0 | 0.35 | 64 | yes | -0.03 | -4.59 | -137.88 | 86 | pass |
| N=5 kL=0.25 kS=0.75 h=0 | -0.19 | 62 | yes | 0.20 | 2.45 | 73.41 | 80 | pass |
| N=5 kL=0.25 kS=1 h=0 | -0.74 | 56 | yes | 0.31 | 3.73 | 111.83 | 74 | pass |
| N=5 kL=0.5 kS=0.25 h=0 | 0.77 | 63 | yes | 0.15 | 2.34 | 59.02 | 96 | pass |
| N=5 kL=0.5 kS=0.5 h=0 | 1.63 | 52 | yes | -0.12 | -4.32 | -129.62 | 73 | pass |
| N=5 kL=0.5 kS=0.75 h=0 | 1.30 | 42 | yes | 0.39 | 4.88 | 146.43 | 61 | pass |
| N=5 kL=0.5 kS=1 h=0 | 0.69 | 34 | yes | 0.56 | 7.08 | 212.38 | 51 | pass |
| N=5 kL=0.75 kS=0.25 h=0 | 0.27 | 58 | yes | -0.01 | -2.83 | -94.66 | 75 | pass |
| N=5 kL=0.75 kS=0.5 h=0 | 1.24 | 37 | yes | -0.50 | -8.69 | -260.58 | 50 | pass |
| N=5 kL=0.75 kS=0.75 h=0 | 0.81 | 27 | yes | -0.01 | 0.63 | 18.97 | 34 | pass |
| N=5 kL=0.75 kS=1 h=0 | -0.07 | 18 | yes | 0.20 | 2.99 | 89.82 | 25 | pass |
| N=5 kL=1 kS=0.25 h=0 | 0.09 | 52 | yes | 0.10 | -0.52 | -25.24 | 71 | pass |
| N=5 kL=1 kS=0.5 h=0 | 0.88 | 34 | yes | -0.37 | -6.14 | -184.40 | 46 | pass |
| N=5 kL=1 kS=0.75 h=0 | 0.38 | 23 | yes | 0.02 | 1.39 | 41.57 | 32 | pass |
| N=5 kL=1 kS=1 h=0 | -0.80 | 14 | yes | 0.33 | 3.32 | 99.63 | 22 | pass |
| N=7 kL=0.25 kS=0.25 h=0 | 0.63 | 65 | yes | 0.08 | -1.30 | -39.06 | 95 | pass |
| N=7 kL=0.25 kS=0.5 h=0 | 1.17 | 58 | yes | 0.16 | 2.13 | 63.96 | 77 | pass |
| N=7 kL=0.25 kS=0.75 h=0 | 1.10 | 45 | yes | 0.30 | 4.79 | 143.64 | 69 | pass |
| N=7 kL=0.25 kS=1 h=0 | 0.70 | 41 | yes | 0.23 | 3.44 | 103.18 | 64 | pass |
| N=7 kL=0.5 kS=0.25 h=0 | 0.71 | 53 | yes | -0.17 | -5.94 | -178.29 | 77 | pass |
| N=7 kL=0.5 kS=0.5 h=0 | 1.45 | 37 | yes | -0.12 | -2.52 | -75.56 | 51 | pass |
| N=7 kL=0.5 kS=0.75 h=0 | 1.54 | 25 | yes | 0.08 | 0.42 | 12.71 | 39 | pass |
| N=7 kL=0.5 kS=1 h=0 | 1.05 | 22 | yes | -0.04 | -0.89 | -26.76 | 33 | pass |
| N=7 kL=0.75 kS=0.25 h=0 | 0.08 | 46 | yes | -0.19 | -5.65 | -169.59 | 68 | pass |
| N=7 kL=0.75 kS=0.5 h=0 | 0.94 | 26 | yes | -0.04 | -1.71 | -51.31 | 38 | pass |
| N=7 kL=0.75 kS=0.75 h=0 | 0.82 | 13 | yes | 0.18 | 1.28 | 38.46 | 26 | pass |
| N=7 kL=0.75 kS=1 h=0 | -0.11 | 10 | yes | 0.06 | -0.14 | -4.17 | 20 | pass |
| N=7 kL=1 kS=0.25 h=0 | 0.11 | 42 | yes | 0.07 | -0.74 | -22.15 | 60 | pass |
| N=7 kL=1 kS=0.5 h=0 | 0.82 | 23 | yes | 0.35 | 3.28 | 98.21 | 28 | pass |
| N=7 kL=1 kS=0.75 h=0 | 0.57 | 10 | yes | 0.70 | 6.45 | 193.33 | 16 | pass |
| N=7 kL=1 kS=1 h=0 | -0.54 | 7 | no | 0.66 | 4.99 | 149.76 | 10 | pass |

</details>

## venue_4h (expected_interval_ms = 14400000)

**Group verdict: inconclusive** (test independent position clusters 18 < 30).

| check | value |
|---|---|
| beats_breakout_sharpe_x1 | true |
| beats_session_breakout_sharpe_x1 | true |
| m1_test_pass_x1 | true |
| net_positive_x1 | true |
| net_positive_x2 | true |

Selection: selected, `N=7 kL=1 kS=0.75 h=0` (mean Sharpe 1.56, mean DDadj 4.90, 15 clusters; 80 of 80 variants eligible). Grid points: 80; excluded before scoring: 0.

### Held-out `test`, cost base

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | pass | -0.30 | -13.17 | -394.99 | -44.87 | 276 (145/131) | 107 | 163.75 | -12.06 | 0 |
| candidate | pass | 0.97 | 13.51 | 405.41 | -10.07 | 32 (9/23) | 18 | 22.32 | -5.87 | 0 |
| session_breakout | fail | -1.06 | -37.50 | -1126.19 | -54.03 | 446 (207/239) | 126 | 240.40 | -17.43 | 0 |

### Held-out `test`, cost stress

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|
| breakout | pass | -0.48 | -17.69 | -530.68 | -47.14 | 276 (145/131) | 108 | 159.27 | -10.78 | 0 |
| candidate | pass | 0.94 | 13.07 | 392.00 | -10.32 | 32 (9/23) | 18 | 22.34 | -5.80 | 0 |
| session_breakout | fail | -1.59 | -49.02 | -1472.28 | -64.95 | 446 (208/238) | 128 | 216.08 | -13.62 | 0 |

Concentration (candidate, cost x1): top dataset share of absolute net PnL 0.50, top five positions' share 0.31, long net $49.06, short net $356.33.

Coverage and execution (candidate, cost x1):

| dataset | candles complete | funding complete | rejected entries | min entry notional $ |
|---|---|---|---|---|
| BTC 4h | yes | yes | 0 | 991.99 |
| ETH 4h | yes | yes | 0 | 982.30 |
| SOL 4h | yes | yes | 0 | 998.01 |

Stability: Spearman rank correlation of train vs test mean Sharpe across the 80 variants: 0.47; variants with positive test net PnL: 24; variants with an M1 pass on test: 74; one-step grid neighbours of the selection: 4 (median train Sharpe 0.92, minimum 0.74).

Start-of-window history and input holds for the selected parameters (proxy windows load no earlier bars; venue windows include the manifest warm-up):

| window | dataset | bars | first valid bar | hold reasons |
|---|---|---|---|---|
| test | BTC 4h | 2490 | 2025-07-20 00:00:00 | insufficient_history 42 |
| test | ETH 4h | 2490 | 2025-07-20 00:00:00 | insufficient_history 42 |
| test | SOL 4h | 2490 | 2025-07-20 00:00:00 | insufficient_history 42 |
| train | BTC 4h | 2490 | 2024-07-20 00:00:00 | insufficient_history 42 |
| train | ETH 4h | 2490 | 2024-07-20 00:00:00 | insufficient_history 42 |
| train | SOL 4h | 2490 | 2024-07-20 00:00:00 | insufficient_history 42 |

Entry audit: 32 held-out candidate entries; each fill follows a valid raw signal on the prior closed bar, and `results.json` stores its evaluated input span hash and every indicator value. First entries:

| dataset | side | signal bar | fill bar | anchor | range | upper | lower | close | net $ |
|---|---|---|---|---|---|---|---|---|---|
| BTC 4h | short | 2025-09-22 04:00:00 | 2025-09-22 08:00:00 | 115284 | 2743 | 118027 | 113227 | 112506 | -7.34 |
| BTC 4h | short | 2025-10-10 16:00:00 | 2025-10-10 20:00:00 | 121615 | 5495 | 127110 | 117494 | 116632 | 31.54 |
| BTC 4h | short | 2025-12-01 12:00:00 | 2025-12-01 16:00:00 | 90325 | 6079 | 96404 | 85765.8 | 84645 | -15.42 |
| BTC 4h | short | 2026-01-29 12:00:00 | 2026-01-29 16:00:00 | 89276 | 4569 | 93845 | 85849.2 | 84911 | 41.78 |
| BTC 4h | short | 2026-01-31 16:00:00 | 2026-01-31 20:00:00 | 84240 | 8276 | 92516 | 78033 | 77730 | 36.27 |
| BTC 4h | short | 2026-02-23 16:00:00 | 2026-02-23 20:00:00 | 67602 | 3663 | 71265 | 64854.8 | 64270 | -17.32 |
| BTC 4h | long | 2026-07-14 20:00:00 | 2026-07-15 00:00:00 | 62331 | 2605 | 64936 | 60377.2 | 65000 | -14.04 |
| BTC 4h | short | 2026-07-31 12:00:00 | 2026-07-31 16:00:00 | 64755 | 2663 | 67418 | 62757.8 | 62705 | -13.66 |
| BTC 4h | long | 2026-08-19 12:00:00 | 2026-08-19 16:00:00 | 64696 | 2173 | 66869 | 63066.2 | 68532 | 48.84 |
| ETH 4h | long | 2025-09-12 20:00:00 | 2025-09-13 00:00:00 | 4460.1 | 233.3 | 4693.4 | 4285.12 | 4713.5 | -16.91 |

<details><summary>Every evaluated variant (training window and held-out diagnostic)</summary>

| variant | train Sharpe | train clusters | eligible | test Sharpe | test return % | test net $ | test clusters | test M1 |
|---|---|---|---|---|---|---|---|---|
| N=2 kL=0.25 kS=0.25 h=0 | -1.67 | 132 | yes | -0.03 | -8.28 | -251.26 | 107 | pass |
| N=2 kL=0.25 kS=0.5 h=0 | -0.74 | 154 | yes | -0.35 | -18.04 | -543.40 | 118 | pass |
| N=2 kL=0.25 kS=0.75 h=0 | -0.33 | 165 | yes | -0.02 | -5.37 | -163.91 | 136 | pass |
| N=2 kL=0.25 kS=1 h=0 | -0.64 | 160 | yes | 0.10 | -2.30 | -71.75 | 130 | pass |
| N=2 kL=0.5 kS=0.25 h=0 | -0.73 | 123 | yes | -0.45 | -23.57 | -712.85 | 136 | pass |
| N=2 kL=0.5 kS=0.5 h=0 | 0.12 | 144 | yes | -0.42 | -17.95 | -546.18 | 136 | pass |
| N=2 kL=0.5 kS=0.75 h=0 | 0.59 | 148 | yes | -0.26 | -12.24 | -375.53 | 133 | pass |
| N=2 kL=0.5 kS=1 h=0 | 0.37 | 133 | yes | -0.21 | -10.78 | -331.58 | 131 | pass |
| N=2 kL=0.75 kS=0.25 h=0 | -1.24 | 145 | yes | -0.31 | -16.45 | -494.28 | 132 | pass |
| N=2 kL=0.75 kS=0.5 h=0 | -0.27 | 146 | yes | -0.29 | -11.94 | -358.22 | 126 | pass |
| N=2 kL=0.75 kS=0.75 h=0 | 0.08 | 141 | yes | -0.12 | -5.83 | -174.98 | 119 | pass |
| N=2 kL=0.75 kS=1 h=0 | -0.19 | 124 | yes | 0.16 | 0.20 | 5.92 | 108 | pass |
| N=2 kL=1 kS=0.25 h=0 | -1.31 | 143 | yes | -0.44 | -21.73 | -652.75 | 130 | pass |
| N=2 kL=1 kS=0.5 h=0 | -0.17 | 139 | yes | -0.42 | -17.06 | -511.92 | 123 | pass |
| N=2 kL=1 kS=0.75 h=0 | 0.28 | 125 | yes | -0.26 | -10.39 | -311.74 | 113 | pass |
| N=2 kL=1 kS=1 h=0 | -0.03 | 103 | yes | 0.04 | -2.65 | -79.52 | 97 | pass |
| N=3 kL=0.25 kS=0.25 h=0 | -1.09 | 141 | yes | -0.19 | -8.84 | -269.47 | 123 | pass |
| N=3 kL=0.25 kS=0.5 h=0 | -0.24 | 146 | yes | -0.28 | -15.76 | -478.19 | 130 | pass |
| N=3 kL=0.25 kS=0.75 h=0 | -0.15 | 146 | yes | -0.49 | -20.11 | -608.13 | 131 | pass |
| N=3 kL=0.25 kS=1 h=0 | 0.11 | 132 | yes | -0.68 | -24.55 | -741.47 | 129 | fail |
| N=3 kL=0.5 kS=0.25 h=0 | -0.87 | 131 | yes | -0.03 | -3.84 | -121.58 | 123 | pass |
| N=3 kL=0.5 kS=0.5 h=0 | -0.05 | 141 | yes | -0.06 | -8.57 | -264.51 | 112 | pass |
| N=3 kL=0.5 kS=0.75 h=0 | 0.01 | 122 | yes | -0.30 | -13.36 | -407.77 | 113 | pass |
| N=3 kL=0.5 kS=1 h=0 | 0.42 | 105 | yes | -0.35 | -13.18 | -401.44 | 100 | pass |
| N=3 kL=0.75 kS=0.25 h=0 | -0.89 | 138 | yes | -0.13 | -4.98 | -149.48 | 127 | pass |
| N=3 kL=0.75 kS=0.5 h=0 | -0.00 | 130 | yes | -0.38 | -15.39 | -461.75 | 108 | pass |
| N=3 kL=0.75 kS=0.75 h=0 | 0.10 | 104 | yes | -0.68 | -18.91 | -567.15 | 102 | fail |
| N=3 kL=0.75 kS=1 h=0 | 0.66 | 83 | yes | -0.78 | -17.93 | -538.05 | 82 | fail |
| N=3 kL=1 kS=0.25 h=0 | -0.73 | 132 | yes | 0.07 | -0.03 | -0.99 | 113 | pass |
| N=3 kL=1 kS=0.5 h=0 | 0.31 | 109 | yes | -0.12 | -7.80 | -234.09 | 85 | pass |
| N=3 kL=1 kS=0.75 h=0 | 0.51 | 79 | yes | -0.25 | -8.54 | -256.05 | 74 | pass |
| N=3 kL=1 kS=1 h=0 | 1.22 | 58 | yes | -0.28 | -7.80 | -234.13 | 53 | pass |
| N=4 kL=0.25 kS=0.25 h=0 | -0.73 | 143 | yes | -0.58 | -26.77 | -807.73 | 132 | fail |
| N=4 kL=0.25 kS=0.5 h=0 | -0.39 | 153 | yes | -0.08 | -9.14 | -280.31 | 120 | pass |
| N=4 kL=0.25 kS=0.75 h=0 | -0.23 | 136 | yes | -0.41 | -16.77 | -509.11 | 124 | pass |
| N=4 kL=0.25 kS=1 h=0 | -0.31 | 130 | yes | -0.79 | -26.06 | -787.10 | 111 | fail |
| N=4 kL=0.5 kS=0.25 h=0 | -0.54 | 137 | yes | -0.40 | -18.95 | -568.51 | 124 | pass |
| N=4 kL=0.5 kS=0.5 h=0 | 0.15 | 123 | yes | -0.15 | -8.94 | -268.21 | 104 | pass |
| N=4 kL=0.5 kS=0.75 h=0 | 0.34 | 96 | yes | -0.44 | -13.87 | -415.95 | 100 | pass |
| N=4 kL=0.5 kS=1 h=0 | 0.23 | 88 | yes | -0.92 | -21.17 | -635.00 | 85 | fail |
| N=4 kL=0.75 kS=0.25 h=0 | -0.30 | 130 | yes | -0.27 | -13.71 | -411.21 | 113 | pass |
| N=4 kL=0.75 kS=0.5 h=0 | 0.23 | 107 | yes | 0.21 | 1.60 | 47.93 | 80 | pass |
| N=4 kL=0.75 kS=0.75 h=0 | 0.59 | 72 | yes | 0.08 | -0.84 | -25.25 | 67 | pass |
| N=4 kL=0.75 kS=1 h=0 | 0.47 | 61 | yes | -0.40 | -9.17 | -275.11 | 51 | pass |
| N=4 kL=1 kS=0.25 h=0 | -0.40 | 117 | yes | -0.23 | -12.30 | -368.84 | 106 | pass |
| N=4 kL=1 kS=0.5 h=0 | 0.23 | 88 | yes | 0.33 | 4.36 | 130.87 | 67 | pass |
| N=4 kL=1 kS=0.75 h=0 | 0.58 | 53 | yes | 0.21 | 2.08 | 62.21 | 54 | pass |
| N=4 kL=1 kS=1 h=0 | 0.44 | 42 | yes | -0.28 | -6.85 | -205.54 | 38 | pass |
| N=5 kL=0.25 kS=0.25 h=0 | -0.11 | 136 | yes | -0.01 | -5.19 | -160.96 | 133 | pass |
| N=5 kL=0.25 kS=0.5 h=0 | 0.28 | 141 | yes | 0.30 | 5.67 | 163.62 | 118 | pass |
| N=5 kL=0.25 kS=0.75 h=0 | 0.22 | 123 | yes | 0.12 | 1.28 | 31.93 | 111 | pass |
| N=5 kL=0.25 kS=1 h=0 | 0.19 | 119 | yes | -0.16 | -8.91 | -273.38 | 103 | pass |
| N=5 kL=0.5 kS=0.25 h=0 | -0.18 | 124 | yes | -0.11 | -6.97 | -209.06 | 120 | pass |
| N=5 kL=0.5 kS=0.5 h=0 | 0.79 | 105 | yes | 0.31 | 5.92 | 177.70 | 96 | pass |
| N=5 kL=0.5 kS=0.75 h=0 | 0.71 | 77 | yes | 0.27 | 4.64 | 139.32 | 79 | pass |
| N=5 kL=0.5 kS=1 h=0 | 0.67 | 69 | yes | -0.11 | -4.36 | -130.91 | 67 | pass |
| N=5 kL=0.75 kS=0.25 h=0 | -0.16 | 109 | yes | -0.12 | -7.03 | -210.98 | 103 | pass |
| N=5 kL=0.75 kS=0.5 h=0 | 0.79 | 82 | yes | 0.36 | 6.52 | 195.44 | 67 | pass |
| N=5 kL=0.75 kS=0.75 h=0 | 0.70 | 51 | yes | 0.38 | 5.61 | 168.33 | 47 | pass |
| N=5 kL=0.75 kS=1 h=0 | 0.69 | 42 | yes | -0.01 | -2.36 | -70.75 | 36 | pass |
| N=5 kL=1 kS=0.25 h=0 | -0.21 | 101 | yes | -0.12 | -6.96 | -208.76 | 98 | pass |
| N=5 kL=1 kS=0.5 h=0 | 0.76 | 69 | yes | 0.39 | 6.89 | 206.62 | 62 | pass |
| N=5 kL=1 kS=0.75 h=0 | 0.74 | 38 | yes | 0.24 | 3.76 | 112.65 | 43 | pass |
| N=5 kL=1 kS=1 h=0 | 0.79 | 29 | yes | -0.16 | -4.71 | -141.47 | 32 | pass |
| N=7 kL=0.25 kS=0.25 h=0 | 0.37 | 125 | yes | -0.04 | -7.19 | -221.38 | 129 | pass |
| N=7 kL=0.25 kS=0.5 h=0 | 0.53 | 123 | yes | 0.21 | 3.69 | 104.52 | 103 | pass |
| N=7 kL=0.25 kS=0.75 h=0 | 0.66 | 106 | yes | 0.49 | 11.53 | 338.87 | 89 | pass |
| N=7 kL=0.25 kS=1 h=0 | 0.36 | 104 | yes | 0.26 | 4.80 | 137.52 | 83 | pass |
| N=7 kL=0.5 kS=0.25 h=0 | 0.11 | 103 | yes | -0.10 | -6.28 | -188.37 | 105 | pass |
| N=7 kL=0.5 kS=0.5 h=0 | 0.37 | 73 | yes | 0.25 | 4.61 | 138.27 | 68 | pass |
| N=7 kL=0.5 kS=0.75 h=0 | 0.64 | 51 | yes | 0.72 | 12.61 | 378.17 | 49 | pass |
| N=7 kL=0.5 kS=1 h=0 | 0.05 | 48 | yes | 0.39 | 5.76 | 172.88 | 43 | pass |
| N=7 kL=0.75 kS=0.25 h=0 | 0.20 | 87 | yes | -0.25 | -9.52 | -285.75 | 92 | pass |
| N=7 kL=0.75 kS=0.5 h=0 | 0.61 | 52 | yes | 0.07 | 1.27 | 38.21 | 48 | pass |
| N=7 kL=0.75 kS=0.75 h=0 | 1.11 | 28 | yes | 0.59 | 8.97 | 269.15 | 29 | pass |
| N=7 kL=0.75 kS=1 h=0 | 0.43 | 25 | yes | 0.20 | 2.43 | 72.87 | 23 | pass |
| N=7 kL=1 kS=0.25 h=0 | 0.31 | 77 | yes | -0.12 | -5.94 | -178.33 | 83 | pass |
| N=7 kL=1 kS=0.5 h=0 | 0.82 | 39 | yes | 0.30 | 5.41 | 162.44 | 37 | pass |
| N=7 kL=1 kS=0.75 h=0 | 1.56 | 15 | yes | 0.97 | 13.51 | 405.41 | 18 | pass |
| N=7 kL=1 kS=1 h=0 | 1.01 | 12 | yes | 0.58 | 6.60 | 198.00 | 12 | pass |

</details>

## Decision parity

`run_study.py parity` ran the explicit-paper configurations `paper_parity_1h.json` and `paper_parity_4h.json` through `backtest/parity_diff.compute_parity_frame` on frozen BTC frames: non-batched and batched (solo vs batch slots through the real Hyperliquid check evaluator), on the trailing 200-bar window and on every prefix. Overall clean: yes.

| source | params | batched | window | bars compared | mismatches | batch mismatches | long/short signals | clean |
|---|---|---|---|---|---|---|---|---|
| proxy BTC/USDT 1h 2026-07-01..2026-10-01 | seed | no | 200 | 2000 | 0 | - | 8/5 | yes |
| proxy BTC/USDT 1h 2026-07-01..2026-10-01 | seed | no | every prefix | 2170 | 0 | - | 8/6 | yes |
| proxy BTC/USDT 1h 2026-07-01..2026-10-01 | seed | yes | 200 | 2000 | 0 | 0 | 8/5 | yes |
| proxy BTC/USDT 1h 2026-07-01..2026-10-01 | seed | yes | every prefix | 2170 | 0 | 0 | 8/6 | yes |
| proxy BTC/USDT 1h 2026-07-01..2026-10-01 | selected | no | 200 | 2000 | 0 | - | 6/5 | yes |
| proxy BTC/USDT 1h 2026-07-01..2026-10-01 | selected | no | every prefix | 2170 | 0 | - | 6/5 | yes |
| proxy BTC/USDT 1h 2026-07-01..2026-10-01 | selected | yes | 200 | 2000 | 0 | 0 | 6/5 | yes |
| proxy BTC/USDT 1h 2026-07-01..2026-10-01 | selected | yes | every prefix | 2170 | 0 | 0 | 6/5 | yes |
| proxy BTC/USDT 4h 2025-10-01..2026-10-01 | seed | no | 200 | 1989 | 0 | - | 35/35 | yes |
| proxy BTC/USDT 4h 2025-10-01..2026-10-01 | seed | no | every prefix | 2159 | 0 | - | 37/39 | yes |
| proxy BTC/USDT 4h 2025-10-01..2026-10-01 | seed | yes | 200 | 1989 | 0 | 0 | 35/35 | yes |
| proxy BTC/USDT 4h 2025-10-01..2026-10-01 | seed | yes | every prefix | 2159 | 0 | 0 | 37/39 | yes |
| proxy BTC/USDT 4h 2025-10-01..2026-10-01 | selected | no | 200 | 1989 | 0 | - | 27/30 | yes |
| proxy BTC/USDT 4h 2025-10-01..2026-10-01 | selected | no | every prefix | 2159 | 0 | - | 29/33 | yes |
| proxy BTC/USDT 4h 2025-10-01..2026-10-01 | selected | yes | 200 | 1989 | 0 | 0 | 27/30 | yes |
| proxy BTC/USDT 4h 2025-10-01..2026-10-01 | selected | yes | every prefix | 2159 | 0 | 0 | 29/33 | yes |
| venue BTC 4h test window with warmup | seed | no | 200 | 2291 | 0 | - | 38/36 | yes |
| venue BTC 4h test window with warmup | seed | no | every prefix | 2461 | 0 | - | 42/39 | yes |
| venue BTC 4h test window with warmup | seed | yes | 200 | 2291 | 0 | 0 | 38/36 | yes |
| venue BTC 4h test window with warmup | seed | yes | every prefix | 2461 | 0 | 0 | 42/39 | yes |
| venue BTC 4h test window with warmup | selected | no | 200 | 2291 | 0 | - | 7/6 | yes |
| venue BTC 4h test window with warmup | selected | no | every prefix | 2461 | 0 | - | 7/6 | yes |
| venue BTC 4h test window with warmup | selected | yes | 200 | 2291 | 0 | 0 | 7/6 | yes |
| venue BTC 4h test window with warmup | selected | yes | every prefix | 2461 | 0 | 0 | 7/6 | yes |

## Limits

- Proxy legs use Binance US spot candles; basis, venue wicks and funding-driven price effects are not modelled. The legacy M1 screen has no spread, lot rounding or minimum order, so venue execution claims rest on the venue group only.
- Hyperliquid funding starts in May 2023; a proxy 2023 leg that holds a position before the first rate is funding_incomplete, and such a window can never support a pass.
- The venue windows cover 2024-09-01 to 2026-09-01 only (the venue serves the most recent 5,000 candles). Spread is a per-coin half spread from one frozen L2 snapshot plus flat slippage; queue position, partial fills and market impact are not modelled.
- The selection and the verdict read only frozen inputs with network acquisition disabled. A research result alone cannot approve deployment.

## Reproduction

```
uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py lock
uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py run --cache-db /tmp/dt1642_run.db
uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py parity --cache-db /tmp/dt1642_parity.db
uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py render
```

Results: `results.json` (source commit `d0c719b18a26a2ce2b0fa69ea284029baf5da7e1`, runtime 324.8 s, manifest sha256 `1add7d1cdd46af1fdfe61420f5dc28386f7dbddf36c5d5f31b8cc2029bf6bb87`, venue manifest sha256 `8e21fa954d56207fc0a36994a8fa6283f711a9a8e18774844459ca63b52f7bae`), `parity_results.json` (source commit `d0c719b18a26a2ce2b0fa69ea284029baf5da7e1+uncommitted-changes`).
