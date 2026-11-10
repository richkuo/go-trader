# breakout_2014: trend-quality entry gate on the deployed Hyperliquid perps shape

Issue 2014. Suggest-only: no command writes a live or paper config, a registry default or edge_status, and none sends an order.

## Verdict

**gate adopted for all (gate_p21: BNB/USDT 30m, BNB/USDT 1h, BTC/USDT 30m, BTC/USDT 1h, ETH/USDT 30m, ETH/USDT 1h, SOL/USDT 30m, SOL/USDT 1h).**

- set `1h`: primary arm `gate_p21`; adopted yes; every frozen adoption check is true.
  Checks: oos_beats_control_ddadj yes, oos_beats_control_sharpe yes, oos_min_clusters yes, stitched_mean_return_not_lower yes, stitched_shallower_worst_dd yes, venue_not_fail yes.
  Venue verdict: inconclusive (gate arm venue clusters 25 < 30).
- set `30m`: primary arm `None`; adopted no; no primary-family gate arm met the frozen is shortlist rule.
- set `all`: primary arm `gate_p21`; adopted yes; every frozen adoption check is true.
  Checks: oos_beats_control_ddadj yes, oos_beats_control_sharpe yes, oos_min_clusters yes, stitched_mean_return_not_lower yes, stitched_shallower_worst_dd yes, venue_not_fail yes.
  Venue verdict: pass (beats control on mean Sharpe and mean ddadj at cost x1 and x2).

Suggested config: `suggested_paper_config.json` adds `breakout-bnb-1h-gated`, `breakout-bnb-30m-gated`, `breakout-btc-1h-gated`, `breakout-btc-30m-gated`, `breakout-eth-1h-gated`, `breakout-eth-30m-gated`, `breakout-sol-1h-gated`, `breakout-sol-30m-gated`. Apply it while flat; it is a suggestion only.

## Arms

| arm | config | directional policy | entries | gate and short entries | gate window |
|---|---|---|---|---|---|
| control | `arms/control.json` | off, as live | long (policy off, as live) | no short entry exists | `composite_medium` |
| gate_p21 | `arms/gate_p21.json` | off, as live | long (policy off, as live) | no short entry exists | `comp_p21` |
| gate_p350 | `arms/gate_p350.json` | off, as live | long (policy off, as live) | no short entry exists | `composite_medium` |
| policy_control | `arms/control.json` | on (research replay) | per composite_medium label (research replay of the configured policy) | no gate | `composite_medium` |
| policy_gate_p21 | `arms/gate_p21.json` | on (research replay) | per composite_medium label | one label set blocks both sides: a short fires only when the period 21 label is trending_up_clean | `comp_p21` |
| policy_gate_p350 | `arms/gate_p350.json` | on (research replay) | long in effect | every short is blocked: the gate passes only on trending_up_clean, where the policy is long | `composite_medium` |
| ref_long | `backtest/candidates/breakout_1165/baseline.json` (read in place) | none | long, open signal as close | reference only | first-study spec |
| ref_long_p21 | `backtest/candidates/breakout_1165/comp_up_clean_p21.json` (read in place) | none | long, open signal as close | reference only | first-study spec |

Every config arm keeps `regime_atr_window: composite_medium`, so the `trailing_tp_ratchet_regime` ladder and the 2.5 ATR trail read that window; the period 21 window feeds the gate only.

## Coverage

| group | dataset | status | reason |
|---|---|---|---|
| proxy | BNB 30m | included | warm-up prefix and funding complete on is, oos, stitched |
| proxy | BNB 1h | included | warm-up prefix and funding complete on is, oos, stitched |
| proxy | BTC 30m | included | warm-up prefix and funding complete on is, oos, stitched |
| proxy | BTC 1h | included | warm-up prefix and funding complete on is, oos, stitched |
| proxy | ETH 30m | included | warm-up prefix and funding complete on is, oos, stitched |
| proxy | ETH 1h | included | warm-up prefix and funding complete on is, oos, stitched |
| proxy | HYPE 30m | excluded | is: warm-up prefix lacks 3011 consecutive bars (2841 missing, first 2025-04-08 06:30:00); stitched: warm-up prefix lacks 3011 consecutive bars (2841 missing, first 2025-04-08 06:30:00) |
| proxy | HYPE 1h | excluded | is: warm-up prefix lacks 3011 consecutive bars (2926 missing, first 2025-02-04 13:00:00); stitched: warm-up prefix lacks 3011 consecutive bars (2926 missing, first 2025-02-04 13:00:00) |
| proxy | SOL 30m | included | warm-up prefix and funding complete on is, oos, stitched |
| proxy | SOL 1h | included | warm-up prefix and funding complete on is, oos, stitched |
| venue | BNB 30m | included | complete candles, warm-up and funding on every window |
| venue | BNB 1h | included | complete candles, warm-up and funding on every window |
| venue | BTC 30m | included | complete candles, warm-up and funding on every window |
| venue | BTC 1h | included | complete candles, warm-up and funding on every window |
| venue | ETH 30m | included | complete candles, warm-up and funding on every window |
| venue | ETH 1h | included | complete candles, warm-up and funding on every window |
| venue | HYPE 30m | included | complete candles, warm-up and funding on every window |
| venue | HYPE 1h | included | complete candles, warm-up and funding on every window |
| venue | SOL 30m | included | complete candles, warm-up and funding on every window |
| venue | SOL 1h | included | complete candles, warm-up and funding on every window |

## Selection on is

Set `1h` (4 datasets): shortlist ['gate_p21', 'policy_gate_p21'], primary arm `gate_p21`, policy-family arm `policy_gate_p21`.

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 4 | fail | -12.56 | -0.99 | -0.41 | -28.38 | 417 | 324 (324/0) | 105 | 7.02 | 1.29 | 0 |
| gate_p21 | 4 | fail | -0.30 | -0.29 | 0.05 | -9.47 | 69 | 52 (52/0) | 41 | 1.19 | 0.43 | 0 |
| gate_p350 | 4 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 4 | fail | -3.58 | -0.36 | 0.04 | -27.93 | 694 | 480 (256/224) | 150 | 10.79 | 0.80 | 0 |
| policy_gate_p21 | 4 | fail | 2.37 | -0.07 | 1.02 | -9.47 | 71 | 53 (48/5) | 42 | 1.22 | 0.39 | 0 |
| policy_gate_p350 | 4 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

| gate arm | beats_control_ddadj | beats_control_sharpe | min_clusters | no_funding_incomplete | not_degenerate |
|---|---|---|---|---|---|
| gate_p21 | yes | yes | yes | yes | yes |
| gate_p350 | yes | yes | no | yes | no |
| policy_gate_p21 | yes | yes | yes | yes | yes |
| policy_gate_p350 | no | yes | no | yes | no |

Set `30m` (4 datasets): shortlist ['policy_gate_p21'], primary arm `None`, policy-family arm `policy_gate_p21`.

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 4 | fail | -21.97 | -1.58 | -0.80 | -33.20 | 816 | 630 (630/0) | 222 | 12.72 | 1.00 | 0 |
| gate_p21 | 4 | fail | -7.92 | -1.41 | -0.80 | -12.17 | 83 | 72 (72/0) | 50 | 1.58 | 0.21 | 0 |
| gate_p350 | 4 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 4 | fail | -35.95 | -2.30 | -0.93 | -52.05 | 1333 | 937 (506/431) | 299 | 17.60 | 0.46 | 0 |
| policy_gate_p21 | 4 | fail | -5.53 | -0.89 | -0.60 | -11.72 | 84 | 73 (60/13) | 50 | 1.60 | 0.16 | 0 |
| policy_gate_p350 | 4 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

| gate arm | beats_control_ddadj | beats_control_sharpe | min_clusters | no_funding_incomplete | not_degenerate |
|---|---|---|---|---|---|
| gate_p21 | no | yes | yes | yes | yes |
| gate_p350 | yes | yes | no | yes | no |
| policy_gate_p21 | yes | yes | yes | yes | yes |
| policy_gate_p350 | yes | yes | no | yes | no |

Set `all` (8 datasets): shortlist ['gate_p21', 'policy_gate_p21'], primary arm `gate_p21`, policy-family arm `policy_gate_p21`.

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 8 | fail | -17.27 | -1.29 | -0.61 | -33.20 | 1233 | 954 (954/0) | 154 | 9.87 | 1.15 | 0 |
| gate_p21 | 8 | fail | -4.11 | -0.85 | -0.37 | -12.17 | 152 | 124 (124/0) | 65 | 1.38 | 0.32 | 0 |
| gate_p350 | 8 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 8 | fail | -19.76 | -1.33 | -0.45 | -52.05 | 2027 | 1417 (762/655) | 172 | 14.20 | 0.63 | 0 |
| policy_gate_p21 | 8 | fail | -1.58 | -0.48 | 0.21 | -11.72 | 155 | 126 (108/18) | 65 | 1.41 | 0.28 | 0 |
| policy_gate_p350 | 8 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

| gate arm | beats_control_ddadj | beats_control_sharpe | min_clusters | no_funding_incomplete | not_degenerate |
|---|---|---|---|---|---|
| gate_p21 | yes | yes | yes | yes | yes |
| gate_p350 | yes | yes | no | yes | no |
| policy_gate_p21 | yes | yes | yes | yes | yes |
| policy_gate_p350 | yes | yes | no | yes | no |

## Proxy group

Candle source: Binance US spot USDT pairs through ccxt (proxy for Hyperliquid perpetual prices); Hyperliquid fees and funding. Funding charged on every leg; warm-up 3011 bars before every window.

### Set `1h`

| arm | is M1 | oos M1 | 2023H2 M1 | 2024 M1 | 2025H1 M1 | stitched mean return % | stitched mean Sharpe | stitched worst DD % | stitched trades | stitched fee drag % | stitched funding drag % | stitched clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | fail | pass | fail | fail | fail | -19.55 | -0.51 | -44.17 | 1096 | 16.32 | 2.07 | 264 |
| gate_p21 | fail | pass | fail | fail | pass | 6.60 | 0.27 | -14.24 | 177 | 2.93 | 0.60 | 83 |
| gate_p350 | degenerate | degenerate | degenerate | degenerate | degenerate | 0.00 | 0.00 | 0.00 | 0 | 0.00 | 0.00 | 0 |
| policy_control | fail | pass | fail | fail | pass | 0.12 | -0.08 | -33.79 | 1767 | 26.62 | 1.43 | 376 |
| policy_gate_p21 | fail | pass | fail | fail | pass | 5.80 | 0.29 | -13.76 | 183 | 3.07 | 0.54 | 86 |
| policy_gate_p350 | degenerate | degenerate | degenerate | degenerate | degenerate | 0.00 | 0.00 | 0.00 | 0 | 0.00 | 0.00 | 0 |
| ref_long | pass | pass | not scored | not scored | not scored | 6.06 | 0.21 | -48.99 | 421 | 10.95 | 7.21 | 95 |
| ref_long_p21 | pass | pass | not scored | not scored | not scored | 15.73 | 0.46 | -23.09 | 98 | 2.37 | 2.45 | 48 |

`oos` window:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 4 | pass | -7.27 | -0.25 | -0.30 | -25.14 | 679 | 507 (507/0) | 159 | 10.57 | 0.85 | 0 |
| gate_p21 | 4 | pass | 6.68 | 0.64 | 1.18 | -12.36 | 108 | 77 (77/0) | 42 | 1.75 | 0.17 | 0 |
| gate_p350 | 4 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 4 | pass | 5.55 | 0.08 | 0.27 | -27.64 | 1073 | 736 (410/326) | 226 | 16.66 | 0.64 | 0 |
| policy_gate_p21 | 4 | pass | 3.56 | 0.49 | 0.50 | -12.06 | 112 | 81 (67/14) | 44 | 1.80 | 0.14 | 0 |
| policy_gate_p350 | 4 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| ref_long | 4 | pass | -7.13 | -0.07 | -0.27 | -32.71 | 272 | 272 (272/0) | 62 | 5.98 | 2.66 | 0 |
| ref_long_p21 | 4 | pass | 6.65 | 0.43 | 0.39 | -17.37 | 61 | 61 (61/0) | 27 | 1.37 | 0.78 | 0 |

### Set `30m`

| arm | is M1 | oos M1 | 2023H2 M1 | 2024 M1 | 2025H1 M1 | stitched mean return % | stitched mean Sharpe | stitched worst DD % | stitched trades | stitched fee drag % | stitched funding drag % | stitched clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | fail | fail | fail | fail | fail | -51.82 | -2.04 | -63.22 | 2208 | 27.28 | 1.56 | 568 |
| gate_p21 | fail | pass | fail | fail | pass | -8.40 | -0.53 | -16.18 | 265 | 4.56 | 0.34 | 137 |
| gate_p350 | degenerate | degenerate | degenerate | degenerate | degenerate | 0.00 | 0.00 | 0.00 | 0 | 0.00 | 0.00 | 0 |
| policy_control | fail | fail | fail | fail | fail | -59.80 | -2.19 | -69.81 | 3472 | 34.43 | 0.73 | 785 |
| policy_gate_p21 | fail | fail | fail | fail | fail | -8.56 | -0.60 | -17.87 | 268 | 4.62 | 0.29 | 137 |
| policy_gate_p350 | degenerate | degenerate | degenerate | degenerate | degenerate | 0.00 | 0.00 | 0.00 | 0 | 0.00 | 0.00 | 0 |
| ref_long | fail | fail | not scored | not scored | not scored | -24.16 | -0.46 | -70.03 | 890 | 17.70 | 5.62 | 211 |
| ref_long_p21 | fail | pass | not scored | not scored | not scored | -0.15 | -0.10 | -27.52 | 174 | 3.85 | 1.73 | 92 |

`oos` window:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 4 | fail | -37.70 | -2.43 | -0.94 | -50.58 | 1392 | 1078 (1078/0) | 346 | 18.89 | 0.72 | 0 |
| gate_p21 | 4 | pass | -0.51 | 0.02 | -0.02 | -10.39 | 182 | 145 (145/0) | 87 | 3.24 | 0.14 | 0 |
| gate_p350 | 4 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 4 | fail | -35.79 | -2.12 | -0.74 | -54.71 | 2139 | 1515 (848/667) | 486 | 26.57 | 0.43 | 0 |
| policy_gate_p21 | 4 | fail | -3.20 | -0.40 | -0.26 | -13.39 | 184 | 147 (130/17) | 87 | 3.21 | 0.15 | 0 |
| policy_gate_p350 | 4 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| ref_long | 4 | fail | -16.24 | -0.48 | -0.40 | -37.99 | 543 | 543 (543/0) | 134 | 10.70 | 2.45 | 0 |
| ref_long_p21 | 4 | pass | 2.21 | 0.06 | 0.27 | -19.21 | 114 | 114 (114/0) | 58 | 2.52 | 0.80 | 0 |

### Set `all`

| arm | is M1 | oos M1 | 2023H2 M1 | 2024 M1 | 2025H1 M1 | stitched mean return % | stitched mean Sharpe | stitched worst DD % | stitched trades | stitched fee drag % | stitched funding drag % | stitched clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | fail | fail | fail | fail | fail | -35.69 | -1.27 | -63.22 | 3304 | 21.80 | 1.81 | 412 |
| gate_p21 | fail | pass | fail | fail | pass | -0.90 | -0.13 | -16.18 | 442 | 3.75 | 0.47 | 155 |
| gate_p350 | degenerate | degenerate | degenerate | degenerate | degenerate | 0.00 | 0.00 | 0.00 | 0 | 0.00 | 0.00 | 0 |
| policy_control | fail | fail | fail | fail | fail | -29.84 | -1.14 | -69.81 | 5239 | 30.53 | 1.08 | 498 |
| policy_gate_p21 | fail | pass | fail | fail | pass | -1.38 | -0.16 | -17.87 | 451 | 3.84 | 0.42 | 157 |
| policy_gate_p350 | degenerate | degenerate | degenerate | degenerate | degenerate | 0.00 | 0.00 | 0.00 | 0 | 0.00 | 0.00 | 0 |
| ref_long | pass | pass | not scored | not scored | not scored | -9.05 | -0.13 | -70.03 | 1311 | 14.33 | 6.41 | 129 |
| ref_long_p21 | pass | pass | not scored | not scored | not scored | 7.79 | 0.18 | -27.52 | 272 | 3.11 | 2.09 | 84 |

`oos` window:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 8 | fail | -22.49 | -1.34 | -0.62 | -50.58 | 2071 | 1585 (1585/0) | 258 | 14.73 | 0.78 | 0 |
| gate_p21 | 8 | pass | 3.09 | 0.33 | 0.58 | -12.36 | 290 | 222 (222/0) | 90 | 2.49 | 0.16 | 0 |
| gate_p350 | 8 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 8 | fail | -15.12 | -1.02 | -0.23 | -54.71 | 3212 | 2251 (1258/993) | 327 | 21.62 | 0.54 | 0 |
| policy_gate_p21 | 8 | pass | 0.18 | 0.05 | 0.12 | -13.39 | 296 | 228 (197/31) | 92 | 2.50 | 0.14 | 0 |
| policy_gate_p350 | 8 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| ref_long | 8 | pass | -11.69 | -0.27 | -0.34 | -37.99 | 815 | 815 (815/0) | 86 | 8.34 | 2.55 | 0 |
| ref_long_p21 | 8 | pass | 4.43 | 0.25 | 0.33 | -19.21 | 175 | 175 (175/0) | 52 | 1.95 | 0.79 | 0 |

## Venue group

Candle source: Hyperliquid mainnet perpetual candles from the public info API (candleSnapshot), closed bars only, frozen once. Window `tail`; warm-up 400 bars; manifest execution specification (spread, slippage, lot rounding, venue minimum).

### Set `1h`, cost base (x1)

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | fail | -4.98 | -0.28 | -0.24 | -24.01 | 465 | 346 (346/0) | 97 | 6.29 | 0.83 | 0 |
| gate_p21 | 5 | pass | 6.40 | 0.96 | 1.42 | -11.29 | 90 | 67 (67/0) | 35 | 1.25 | 0.22 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | -5.66 | -0.28 | -0.27 | -31.55 | 764 | 517 (288/229) | 128 | 8.89 | 0.26 | 0 |
| policy_gate_p21 | 5 | pass | 6.16 | 1.14 | 1.73 | -8.84 | 92 | 69 (58/11) | 35 | 1.28 | 0.20 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

### Set `1h`, cost stress (x2)

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | fail | -10.33 | -0.80 | -0.49 | -25.71 | 460 | 344 (344/0) | 96 | 6.06 | 0.81 | 0 |
| gate_p21 | 5 | pass | 5.20 | 0.78 | 1.14 | -11.64 | 90 | 67 (67/0) | 34 | 1.24 | 0.22 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | -16.55 | -1.12 | -0.63 | -38.02 | 755 | 517 (287/230) | 126 | 8.38 | 0.26 | 0 |
| policy_gate_p21 | 5 | pass | 4.78 | 0.93 | 1.37 | -9.16 | 92 | 69 (58/11) | 34 | 1.27 | 0.20 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

### Set `30m`, cost base (x1)

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | fail | -8.13 | -1.18 | -0.46 | -19.13 | 478 | 361 (361/0) | 105 | 6.52 | 0.43 | 0 |
| gate_p21 | 5 | pass | 4.41 | 1.07 | 0.80 | -12.00 | 89 | 67 (67/0) | 44 | 1.25 | 0.09 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | -4.91 | -0.91 | -0.06 | -28.05 | 735 | 509 (305/204) | 126 | 8.80 | 0.13 | 0 |
| policy_gate_p21 | 5 | pass | 4.52 | 1.04 | 0.80 | -11.75 | 89 | 67 (61/6) | 44 | 1.25 | 0.09 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

### Set `30m`, cost stress (x2)

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | fail | -15.88 | -2.60 | -0.70 | -26.59 | 478 | 368 (368/0) | 112 | 6.34 | 0.40 | 0 |
| gate_p21 | 5 | pass | 3.27 | 0.76 | 0.55 | -12.86 | 87 | 67 (67/0) | 44 | 1.24 | 0.09 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | -14.70 | -2.36 | -0.52 | -34.29 | 722 | 512 (310/202) | 141 | 8.38 | 0.13 | 0 |
| policy_gate_p21 | 5 | pass | 3.35 | 0.73 | 0.56 | -12.52 | 87 | 67 (61/6) | 44 | 1.25 | 0.09 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

### Set `all`, cost base (x1)

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 10 | fail | -6.56 | -0.73 | -0.35 | -24.01 | 943 | 707 (707/0) | 120 | 6.41 | 0.63 | 0 |
| gate_p21 | 10 | pass | 5.41 | 1.02 | 1.11 | -12.00 | 179 | 134 (134/0) | 57 | 1.25 | 0.15 | 0 |
| gate_p350 | 10 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 10 | fail | -5.29 | -0.60 | -0.17 | -31.55 | 1499 | 1026 (593/433) | 137 | 8.85 | 0.20 | 0 |
| policy_gate_p21 | 10 | pass | 5.34 | 1.09 | 1.27 | -11.75 | 181 | 136 (119/17) | 57 | 1.26 | 0.14 | 0 |
| policy_gate_p350 | 10 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

### Set `all`, cost stress (x2)

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 10 | fail | -13.10 | -1.70 | -0.59 | -26.59 | 938 | 712 (712/0) | 120 | 6.20 | 0.60 | 0 |
| gate_p21 | 10 | pass | 4.23 | 0.77 | 0.85 | -12.86 | 177 | 134 (134/0) | 55 | 1.24 | 0.15 | 0 |
| gate_p350 | 10 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 10 | fail | -15.62 | -1.74 | -0.57 | -38.02 | 1477 | 1029 (597/432) | 139 | 8.38 | 0.20 | 0 |
| policy_gate_p21 | 10 | pass | 4.06 | 0.83 | 0.97 | -12.52 | 179 | 136 (119/17) | 55 | 1.26 | 0.14 | 0 |
| policy_gate_p350 | 10 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

### Bridge (Binance US candles on the venue windows)

Same windows, warm-up and execution specification as the venue group; only the candle source differs.

Set `1h`, cost base:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | pass | 5.79 | 0.46 | 0.38 | -19.68 | 530 | 392 (392/0) | 117 | 7.41 | 0.80 | 0 |
| gate_p21 | 5 | pass | 10.96 | 1.69 | 2.07 | -7.76 | 85 | 61 (61/0) | 35 | 1.18 | 0.17 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | 1.49 | 0.00 | 0.11 | -28.90 | 845 | 586 (320/266) | 160 | 10.23 | 0.23 | 0 |
| policy_gate_p21 | 5 | pass | 9.14 | 1.61 | 2.44 | -7.76 | 88 | 64 (55/9) | 36 | 1.22 | 0.15 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

Set `30m`, cost base:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | fail | -12.06 | -1.82 | -0.58 | -26.35 | 603 | 468 (468/0) | 128 | 8.21 | 0.40 | 0 |
| gate_p21 | 5 | fail | 2.18 | 0.68 | 0.36 | -10.43 | 94 | 77 (77/0) | 48 | 1.41 | 0.10 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | -8.45 | -1.34 | -0.31 | -23.82 | 870 | 629 (370/259) | 157 | 10.84 | 0.13 | 0 |
| policy_gate_p21 | 5 | pass | 2.82 | 0.76 | 0.43 | -10.83 | 94 | 77 (69/8) | 48 | 1.42 | 0.09 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

Set `all`, cost base:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 10 | fail | -3.13 | -0.68 | -0.10 | -26.35 | 1133 | 860 (860/0) | 152 | 7.81 | 0.60 | 0 |
| gate_p21 | 10 | pass | 6.57 | 1.19 | 1.22 | -10.43 | 179 | 138 (138/0) | 60 | 1.30 | 0.14 | 0 |
| gate_p350 | 10 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 10 | fail | -3.48 | -0.67 | -0.10 | -28.90 | 1715 | 1215 (690/525) | 183 | 10.54 | 0.18 | 0 |
| policy_gate_p21 | 10 | pass | 5.98 | 1.18 | 1.44 | -10.83 | 182 | 141 (124/17) | 62 | 1.32 | 0.12 | 0 |
| policy_gate_p350 | 10 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

### Common dates (30m tail dates scored at 30m and 1h)

bridge `1h common`, cost base:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | pass | 3.89 | 0.75 | 0.57 | -19.68 | 251 | 200 (200/0) | 63 | 3.80 | 0.43 | 0 |
| gate_p21 | 5 | pass | 7.30 | 2.31 | 2.59 | -5.83 | 47 | 35 (35/0) | 18 | 0.67 | 0.10 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | pass | 10.56 | 1.10 | 1.21 | -18.85 | 379 | 278 (165/113) | 81 | 5.28 | 0.18 | 0 |
| policy_gate_p21 | 5 | pass | 7.30 | 2.31 | 2.59 | -5.83 | 47 | 35 (35/0) | 18 | 0.67 | 0.10 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

bridge `30m tail`, cost base:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | fail | -12.06 | -1.82 | -0.58 | -26.35 | 603 | 468 (468/0) | 128 | 8.21 | 0.40 | 0 |
| gate_p21 | 5 | fail | 2.18 | 0.68 | 0.36 | -10.43 | 94 | 77 (77/0) | 48 | 1.41 | 0.10 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | -8.45 | -1.34 | -0.31 | -23.82 | 870 | 629 (370/259) | 157 | 10.84 | 0.13 | 0 |
| policy_gate_p21 | 5 | pass | 2.82 | 0.76 | 0.43 | -10.83 | 94 | 77 (69/8) | 48 | 1.42 | 0.09 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

venue `1h common`, cost base:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | fail | -4.30 | -0.42 | 0.25 | -23.94 | 226 | 179 (179/0) | 50 | 3.28 | 0.44 | 0 |
| gate_p21 | 5 | pass | 6.13 | 1.89 | 1.92 | -7.37 | 51 | 37 (37/0) | 15 | 0.70 | 0.15 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | 1.62 | 0.39 | 0.27 | -29.45 | 355 | 253 (151/102) | 59 | 4.56 | 0.19 | 0 |
| policy_gate_p21 | 5 | pass | 6.57 | 2.07 | 2.00 | -7.37 | 51 | 37 (36/1) | 15 | 0.70 | 0.14 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

venue `30m tail`, cost base:

| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) | clusters | fee drag % | funding drag % | funding incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | 5 | fail | -8.13 | -1.18 | -0.46 | -19.13 | 478 | 361 (361/0) | 105 | 6.52 | 0.43 | 0 |
| gate_p21 | 5 | pass | 4.41 | 1.07 | 0.80 | -12.00 | 89 | 67 (67/0) | 44 | 1.25 | 0.09 | 0 |
| gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |
| policy_control | 5 | fail | -4.91 | -0.91 | -0.06 | -28.05 | 735 | 509 (305/204) | 126 | 8.80 | 0.13 | 0 |
| policy_gate_p21 | 5 | pass | 4.52 | 1.04 | 0.80 | -11.75 | 89 | 67 (61/6) | 44 | 1.25 | 0.09 | 0 |
| policy_gate_p350 | 5 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 | 0 (0/0) | 0 | 0.00 | 0.00 | 0 |

## Live-label fidelity

`validate_frames` on frames built from the frozen files, lookback 3011 bars, pass at agreement >= 0.95 on at least 30 bars. Engine check: {'rows_checked': 52, 'rows_with_mismatch': 0, 'rows_without_engine_labels': 0, 'sampled_bars': 13514}.

| group | dataset | window | regime window | n | agreement | passes | shortest bounded slice | engine labels sampled / equal |
|---|---|---|---|---|---|---|---|---|
| proxy | BNB 1h | is | `comp_p21` | 4892 | 1.0000 | yes | 3011 | 197 / 197 |
| proxy | BNB 1h | is | `composite_medium` | 4571 | 1.0000 | yes | 3011 | 197 / 197 |
| proxy | BNB 1h | oos | `comp_p21` | 7475 | 1.0000 | yes | 3011 | 301 / 301 |
| proxy | BNB 1h | oos | `composite_medium` | 7154 | 1.0000 | yes | 3011 | 301 / 301 |
| proxy | BNB 30m | is | `comp_p21` | 9812 | 0.9996 | yes | 3011 | 394 / 394 |
| proxy | BNB 30m | is | `composite_medium` | 9491 | 1.0000 | yes | 3011 | 394 / 394 |
| proxy | BNB 30m | oos | `comp_p21` | 14978 | 1.0000 | yes | 3011 | 601 / 601 |
| proxy | BNB 30m | oos | `composite_medium` | 14657 | 1.0000 | yes | 3011 | 601 / 601 |
| proxy | BTC 1h | is | `comp_p21` | 4892 | 0.9984 | yes | 3011 | 197 / 197 |
| proxy | BTC 1h | is | `composite_medium` | 4571 | 1.0000 | yes | 3011 | 197 / 197 |
| proxy | BTC 1h | oos | `comp_p21` | 7475 | 0.9995 | yes | 3011 | 301 / 301 |
| proxy | BTC 1h | oos | `composite_medium` | 7154 | 1.0000 | yes | 3011 | 301 / 301 |
| proxy | BTC 30m | is | `comp_p21` | 9812 | 0.9990 | yes | 3011 | 394 / 394 |
| proxy | BTC 30m | is | `composite_medium` | 9491 | 1.0000 | yes | 3011 | 394 / 394 |
| proxy | BTC 30m | oos | `comp_p21` | 14978 | 0.9999 | yes | 3011 | 601 / 601 |
| proxy | BTC 30m | oos | `composite_medium` | 14657 | 1.0000 | yes | 3011 | 601 / 601 |
| proxy | ETH 1h | is | `comp_p21` | 4892 | 0.9980 | yes | 3011 | 197 / 197 |
| proxy | ETH 1h | is | `composite_medium` | 4571 | 1.0000 | yes | 3011 | 197 / 197 |
| proxy | ETH 1h | oos | `comp_p21` | 7475 | 0.9995 | yes | 3011 | 301 / 301 |
| proxy | ETH 1h | oos | `composite_medium` | 7154 | 1.0000 | yes | 3011 | 301 / 301 |
| proxy | ETH 30m | is | `comp_p21` | 9812 | 0.9999 | yes | 3011 | 394 / 394 |
| proxy | ETH 30m | is | `composite_medium` | 9491 | 1.0000 | yes | 3011 | 394 / 394 |
| proxy | ETH 30m | oos | `comp_p21` | 14978 | 1.0000 | yes | 3011 | 601 / 601 |
| proxy | ETH 30m | oos | `composite_medium` | 14657 | 1.0000 | yes | 3011 | 601 / 601 |
| proxy | SOL 1h | is | `comp_p21` | 4892 | 1.0000 | yes | 3011 | 197 / 197 |
| proxy | SOL 1h | is | `composite_medium` | 4571 | 1.0000 | yes | 3011 | 197 / 197 |
| proxy | SOL 1h | oos | `comp_p21` | 7475 | 0.9997 | yes | 3011 | 301 / 301 |
| proxy | SOL 1h | oos | `composite_medium` | 7154 | 1.0000 | yes | 3011 | 301 / 301 |
| proxy | SOL 30m | is | `comp_p21` | 9812 | 0.9994 | yes | 3011 | 394 / 394 |
| proxy | SOL 30m | is | `composite_medium` | 9491 | 1.0000 | yes | 3011 | 394 / 394 |
| proxy | SOL 30m | oos | `comp_p21` | 14978 | 1.0000 | yes | 3011 | 601 / 601 |
| proxy | SOL 30m | oos | `composite_medium` | 14657 | 1.0000 | yes | 3011 | 601 / 601 |
| venue | BNB 1h | tail | `comp_p21` | 4556 | 0.9996 | yes | 401 | 79 / 79 |
| venue | BNB 1h | tail | `composite_medium` | 4235 | 1.0000 | yes | 401 | 79 / 79 |
| venue | BNB 30m | tail | `comp_p21` | 4532 | 1.0000 | yes | 401 | 78 / 78 |
| venue | BNB 30m | tail | `composite_medium` | 4211 | 1.0000 | yes | 401 | 78 / 78 |
| venue | BTC 1h | tail | `comp_p21` | 4556 | 0.9989 | yes | 401 | 79 / 79 |
| venue | BTC 1h | tail | `composite_medium` | 4235 | 1.0000 | yes | 401 | 79 / 79 |
| venue | BTC 30m | tail | `comp_p21` | 4532 | 0.9985 | yes | 401 | 78 / 78 |
| venue | BTC 30m | tail | `composite_medium` | 4211 | 1.0000 | yes | 401 | 78 / 78 |
| venue | ETH 1h | tail | `comp_p21` | 4556 | 1.0000 | yes | 401 | 79 / 79 |
| venue | ETH 1h | tail | `composite_medium` | 4235 | 1.0000 | yes | 401 | 79 / 79 |
| venue | ETH 30m | tail | `comp_p21` | 4532 | 0.9996 | yes | 401 | 78 / 78 |
| venue | ETH 30m | tail | `composite_medium` | 4211 | 1.0000 | yes | 401 | 78 / 78 |
| venue | HYPE 1h | tail | `comp_p21` | 4556 | 1.0000 | yes | 401 | 79 / 79 |
| venue | HYPE 1h | tail | `composite_medium` | 4235 | 1.0000 | yes | 401 | 79 / 79 |
| venue | HYPE 30m | tail | `comp_p21` | 4532 | 1.0000 | yes | 401 | 78 / 78 |
| venue | HYPE 30m | tail | `composite_medium` | 4211 | 1.0000 | yes | 401 | 78 / 78 |
| venue | SOL 1h | tail | `comp_p21` | 4556 | 1.0000 | yes | 401 | 79 / 79 |
| venue | SOL 1h | tail | `composite_medium` | 4235 | 1.0000 | yes | 401 | 79 / 79 |
| venue | SOL 30m | tail | `comp_p21` | 4532 | 0.9991 | yes | 401 | 78 / 78 |
| venue | SOL 30m | tail | `composite_medium` | 4211 | 1.0000 | yes | 401 | 78 / 78 |

## Reference arms and the first-study rerun

The first study's committed legs were scored without funding. Rerun with funding off on the same start and the committed last bar:

| arm and dataset | committed Sharpe / return % / DD % / trades | rerun | difference |
|---|---|---|---|
| ref_long BTC/USDT 1h | -0.67 / -20.61 / -27.19 / 66 | -0.41 / -14.62 / -23.12 / 66 | 0.26 / 5.99 / 4.07 / 0 |
| ref_long ETH/USDT 1h | 1.05 / 44.63 / -29.68 / 72 | 1.22 / 56.09 / -25.62 / 72 | 0.17 / 11.46 / 4.06 / 0 |
| ref_long SOL/USDT 1h | -0.17 / -18.90 / -47.85 / 73 | -0.02 / -12.62 / -45.74 / 73 | 0.15 / 6.28 / 2.11 / 0 |
| ref_long_p21 BTC/USDT 1h | -0.59 / -8.51 / -15.83 / 17 | -0.45 / -6.78 / -14.71 / 17 | 0.14 / 1.73 / 1.12 / 0 |
| ref_long_p21 ETH/USDT 1h | 1.56 / 41.13 / -11.67 / 18 | 1.64 / 43.95 / -11.28 / 18 | 0.08 / 2.82 / 0.39 / 0 |
| ref_long_p21 SOL/USDT 1h | 0.23 / 2.56 / -18.43 / 17 | 0.30 / 4.49 / -17.43 / 17 | 0.07 / 1.93 / 1.00 / 0 |

`policy_gate_p350` against `gate_p350`: 108 of 108 legs equal.

Label cache parity on BTC/USDT 1h `is`: identical yes (control: yes, gate_p21: yes).

## Disclosures

- The deployed `regime_directional_policy` is off live: the scheduler honours a policy entry only for a certified (asset, timeframe, classifier) cell and none is certified, so all ten strategies trade the base `direction: long`. The primary arms are long-only; the configured-policy arms are a research replay.
- `use_defaults: true` is not read by the Python evaluator; with no `tp_tiers`, the system ladders of `trailing_tp_ratchet_regime` apply, chosen by the entry label of `composite_medium`.
- `atr_method: wilder` does not change these legs: `breakout` writes its own `atr` column, which sets the entry ATR in live and in the backtest.
- Binance US candles are a proxy for Hyperliquid prices; only fees and funding are Hyperliquid on the proxy group.
- The venue window lies inside the proxy `oos` period, so the two groups are not independent samples.
- The first study already read `oos` through June 2026 for the period 21 gate on BTC, ETH and SOL 1h.
- Closed-bar decisions are opt-in live (`closed_bar_decisions`), and the deployed strategies do not set them, while the backtest decides on closed bars.
- No `apply-regime-gate` preset exists for period 350; the `comp_up_clean_p21` preset edits a strategy in place and makes no copy.
- Venue labels at the start of the venue window come from slices shorter than the live depth; only the ADX seed differs, and the fidelity rows measure it.
- Every leg charged funding; legs that fetched funding while scoring: 0.
- Run-time projection measured before the full run: 0.07972 s per labelled window-bar on BTC 1h `is`.
- `run_log.json` records 1 judge run(s). An earlier `judge` start ran at reduced CPU priority and was stopped before any unit finished, so it wrote no judged result.
- On `is`, `gate_p21` did not shortlist on the 30m set alone (beats_control_ddadj: mean ddadj -0.7995 against control -0.7980). The frozen rule adopts the `all` set when it passes, so the suggested config still gates the 30m strategies.
- The first-study rerun keeps every committed trade count but not the committed returns; it ran on the newly frozen candles with today's engine, and this study does not trace the difference.
- The Backtester constructor fix in this change (regime stop labels validated against the ATR-window vocabulary) was needed before any deployed-shape leg could run; every leg here ran with it.
- HYPE is scored on venue candles only: Binance US lists HYPE/USDT from 2025-06-06, which leaves no 3,011-bar warm-up before `is`, so HYPE is not in the suggested config.

## Reproduction

Every command is in `README.md`. Results: `results.json` (source commit `d85b4f268d78ce67d3772916bc926746af3449fb`, protocol sha256 `a68c58e20ac58392d59622152e9a3698fc06eef60dc3483daf307094c90bcdf2`), `selection.json`, `fidelity.json`, `verdict.json`, `coverage_report.json`.
