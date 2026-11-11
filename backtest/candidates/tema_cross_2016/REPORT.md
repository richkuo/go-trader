# tema_cross_2016 report

Issue 2016: does an entry filter keep the `tema_cross` edge after fees, on the spot long frame and on the two Hyperliquid perps shapes that run today? Suggest-only: no command writes a live or paper config, a registry default or `edge_status`, and none sends an order. Every number below renders from the committed JSON files in this folder.

## Verdict per frame

| frame | verdict | arm | net %/trade (base) | net %/trade (stress) | trades | clusters | p_decision | BH | venue | reason |
|---|---|---|---|---|---|---|---|---|---|---|
| spot | **negative** | best judged: bar_4h | +0.475 | +0.375 | 246 | 132 | 0.1428 | no | - | no judged arm met every frozen verdict check |
| paper | **negative** | best judged: bar_1h | -0.179 | -0.272 | 1099 | 528 | 0.9983 | no | fail | no judged arm met every frozen verdict check |
| live | **inconclusive** | best judged: bar_1h | - | - | 0 | 0 | 1.0000 | no | inconclusive | coverage not met: 0 of 1 datasets remain after the frozen exclusions |

- paper `bar_1h` before the exclusions (diagnostic only, not a verdict input): net -0.179%/trade over 1099 positions, stress -0.272%, gross +0.048%.
- paper `gate_cm_up` before the exclusions (diagnostic only, not a verdict input): net -0.164%/trade over 1389 positions, stress -0.273%, gross +0.053%.
- live `bar_1h` before the exclusions (diagnostic only, not a verdict input): net -0.126%/trade over 178 positions, stress -0.230%, gross +0.101%.
- live `live_long` before the exclusions (diagnostic only, not a verdict input): net -0.139%/trade over 304 positions, stress -0.264%, gross +0.060%.

Frames agree: no. The perps frames decide the suggestion; the spot frame is reported beside them.
No suggested config is written.

## Protocol amendment (before any judged window)

Written 2026-11-11T08:40:21Z; read before it: only the is window (select stage); no judged window (oos, 2023H2, 2024, 2025H1, stitched, venue, bridge) of any arm had been scored.

- Change: the per-trade unit of every per-trade statistic is the opened position (all close records of one entry), not the close record. net %/trade = net_pnl / entry_notional (fees, slippage and funding); stress %/trade = the same on the stress leg; gross %/trade = side x (exit notional - entry notional) / entry notional on the zero-cost leg (price only, no funding, the gross_edge_noise.py convention per position). The noise gate, the pooled verdict samples, the sample floors (100 trades, 30 clusters) and trades per year count positions. Record-level means stay in results.json as net_per_record_pct and gross_per_record_pct.
- Reason: the deployed ratchet ladders close part of a position at a tier and the rest later, so one position can produce several close records. A mean over records weights a partly closed position more than a position closed at once. On the is window the paper control had 893 records from 675 positions: the record mean was +0.164% net and the position mean was -0.048% net, while five of its eight legs lost money. A record-level verdict would overstate the edge of any arm that closes in parts.
- Unchanged: the selection rule (mean drawdown-adjusted return, record-count and cluster floors on is) and selection.json, the arms, windows, costs, family size, correction, venue rule and fidelity rule.

## What `direction: both` does for `tema_cross`

`tema_cross` is registered with `short_entries=False`, but that flag is catalog metadata only: no scheduler file, no `load_strategy_config` path and no backtester path reads it. The strategy emits `+1` on a bullish fast/mid cross while the mid EMA is above the long EMA, and `-1` when its own position state leaves long (a bearish cross). Live maps `-1` to a short open when the book is flat and `direction` allows shorts (`both`); the ratchet close owns every exit, so there is no flip. The higher-timeframe filter passes a `-1` only when the 4h trend is `-1` or `0`. The backtester does the same: `-1` is zeroed only under `direction: long`. So the live entry opens real shorts on the bearish cross that follows a bullish cross whenever the book is flat at that bar. The tables report long and short positions separately (L/S).

## Noise gate (gross edge of each baseline, before any filter verdict)

Windows: is, oos, 2023H2, 2024, 2025H1; field: per-position price return of the zero-cost (gross) leg: side x (exit notional - entry notional) / entry notional over all close records of one entry.

| baseline | n | mean gross %/trade | p (sign flip) | p (cluster sign flip) | clusters | verdict |
|---|---|---|---|---|---|---|
| live:live_both | 552 | +0.0686 | 0.1370 | 0.1370 | 552 | indistinguishable_from_zero |
| live:live_long | 362 | +0.0997 | 0.1194 | 0.1194 | 362 | indistinguishable_from_zero |
| paper:control | 3889 | +0.0749 | 0.0025 | 0.0289 | 998 | distinguishable_positive |
| spot:spot_control | 1628 | +0.2989 | 0.0003 | 0.0148 | 467 | distinguishable_positive |

## Selection window (`is`, written once to `selection.json` before any judged window)

The shortlist and picks come from `selection.json`; the table numbers are the position-level `is` summaries in `results.json` (trades = opened positions).

### spot

M1 audit frame: Binance US spot candles for BTC, ETH and SOL at 1h and 4h, the registry close (the strategy's own exit signal), long only, Hyperliquid fees, funding charged as the M1 harness does.

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| spot_control | control | 6 | 261 | 77.5 | +0.141 | -0.088 | -0.188 | +0.229 | 4.08 | 1.75 | -31.36 | -0.221 | -5.09 | 261/0 | 83 |
| bar_4h | bar | 3 | 49 | 29.1 | +0.033 | -0.265 | -0.364 | +0.297 | 1.55 | 1.87 | -31.36 | -0.158 | -5.23 | 49/0 | 25 |
| combo | combo | 6 | 44 | 13.1 | +0.897 | +0.551 | +0.450 | +0.347 | 0.71 | 1.18 | -21.67 | 0.185 | 3.19 | 44/0 | 13 |
| gate_adx_up | gate | 6 | 0 | 0.0 | - | - | - | - | 0.00 | 0.00 | 0.00 | 0.000 | 0.00 | 0/0 | 0 |
| gate_cm_not_down | gate | 6 | 191 | 56.8 | +0.301 | +0.063 | -0.037 | +0.238 | 3.00 | 1.59 | -23.59 | 0.042 | 0.83 | 191/0 | 61 |
| gate_cm_up | gate | 6 | 84 | 25.0 | +0.183 | -0.068 | -0.168 | +0.251 | 1.31 | 0.86 | -23.59 | 0.068 | -1.35 | 84/0 | 36 |
| gate_p21_up_clean | gate | 6 | 0 | 0.0 | - | - | - | - | 0.00 | 0.00 | 0.00 | 0.000 | 0.00 | 0/0 | 0 |
| htf | htf | 6 | 23 | 6.8 | -2.281 | -3.806 | -3.903 | +1.526 | 0.35 | 5.15 | -58.10 | -0.377 | -16.62 | 23/0 | 1 |
| p_13_34_89 | period | 6 | 79 | 23.5 | +0.724 | +0.407 | +0.307 | +0.317 | 1.32 | 1.76 | -27.19 | 0.233 | 4.54 | 79/0 | 28 |
| p_21_55_144 | period | 6 | 50 | 14.9 | +0.276 | -0.077 | -0.177 | +0.354 | 0.80 | 1.37 | -32.17 | -0.172 | -2.15 | 50/0 | 15 |
| p_34_89_233 | period | 6 | 40 | 11.9 | +1.872 | +1.309 | +1.207 | +0.563 | 0.63 | 2.42 | -31.04 | 0.264 | 5.40 | 40/0 | 6 |
| p_55_144_377 | period | 6 | 22 | 6.5 | +2.698 | +1.828 | +1.726 | +0.870 | 0.36 | 2.52 | -38.91 | 0.193 | 4.67 | 22/0 | 3 |
| p_8_21_55 | period | 6 | 152 | 45.2 | +0.312 | +0.054 | -0.046 | +0.258 | 2.42 | 1.79 | -26.85 | 0.168 | 0.46 | 152/0 | 50 |

| arm | control | control mean ddadj | failed checks | shortlisted |
|---|---|---|---|---|
| bar_4h | spot_control | -0.284 | none | yes |
| combo | spot_control | -0.221 | none | yes |
| gate_adx_up | spot_control | -0.221 | min_clusters, min_trades, trades_on_half_the_datasets | no |
| gate_cm_not_down | spot_control | -0.221 | none | yes |
| gate_cm_up | spot_control | -0.221 | none | yes |
| gate_p21_up_clean | spot_control | -0.221 | min_clusters, min_trades, trades_on_half_the_datasets | no |
| htf | spot_control | -0.221 | beats_control_ddadj, min_clusters, min_trades | no |
| p_13_34_89 | spot_control | -0.221 | none | yes |
| p_21_55_144 | spot_control | -0.221 | none | yes |
| p_34_89_233 | spot_control | -0.221 | min_clusters | no |
| p_55_144_377 | spot_control | -0.221 | min_clusters, min_trades | no |
| p_8_21_55 | spot_control | -0.221 | none | yes |

Picks: bar: bar_4h, gate: gate_cm_up, htf: None, period: p_13_34_89. Combination: ['gate_cm_up', 'p_13_34_89']. Judged arms: bar_4h, combo, gate_cm_up, p_13_34_89.

### paper

the ten deployed paper strategies (BNB, BTC, ETH, HYPE, SOL at 30m and 1h) on frozen Binance US proxy candles with Hyperliquid fees and funding.

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| control | control | 8 | 675 | 150.3 | +0.143 | -0.048 | -0.154 | +0.191 | 7.74 | 0.94 | -31.01 | -0.046 | -4.01 | 675/0 | 177 |
| bar_1h | bar | 4 | 223 | 99.3 | +0.214 | +0.022 | -0.091 | +0.192 | 5.12 | 0.85 | -19.53 | 0.143 | 0.44 | 223/0 | 105 |
| gate_adx_up | gate | 8 | 0 | 0.0 | - | - | - | - | 0.00 | 0.00 | 0.00 | 0.000 | 0.00 | 0/0 | 0 |
| gate_cm_not_down | gate | 8 | 543 | 121.0 | +0.143 | -0.043 | -0.142 | +0.186 | 6.17 | 0.82 | -23.23 | -0.168 | -3.29 | 543/0 | 158 |
| gate_cm_up | gate | 8 | 254 | 56.6 | +0.349 | +0.176 | +0.073 | +0.173 | 2.96 | 0.61 | -14.02 | 0.800 | 5.32 | 254/0 | 97 |
| gate_p21_up_clean | gate | 8 | 0 | 0.0 | - | - | - | - | 0.00 | 0.00 | 0.00 | 0.000 | 0.00 | 0/0 | 0 |
| htf | htf | 8 | 448 | 99.8 | +0.145 | -0.038 | -0.145 | +0.183 | 5.06 | 0.75 | -17.64 | -0.149 | -2.61 | 448/0 | 127 |
| p_13_34_89 | period | 8 | 261 | 58.1 | +0.097 | -0.099 | -0.180 | +0.196 | 2.91 | 0.37 | -20.76 | -0.147 | -3.23 | 261/0 | 108 |
| p_21_55_144 | period | 8 | 172 | 38.3 | -0.077 | -0.289 | -0.366 | +0.213 | 1.90 | 0.25 | -22.89 | -0.452 | -6.07 | 172/0 | 94 |
| p_34_89_233 | period | 8 | 119 | 26.5 | -0.187 | -0.354 | -0.461 | +0.167 | 1.31 | 0.16 | -22.08 | -0.064 | -4.93 | 119/0 | 61 |
| p_55_144_377 | period | 8 | 77 | 17.1 | +0.080 | -0.168 | -0.269 | +0.248 | 0.86 | 0.12 | -11.60 | 0.329 | -1.51 | 77/0 | 52 |
| p_8_21_55 | period | 8 | 423 | 94.2 | +0.185 | -0.010 | -0.133 | +0.195 | 4.89 | 0.54 | -26.04 | 0.123 | -0.36 | 423/0 | 146 |

| arm | control | control mean ddadj | failed checks | shortlisted |
|---|---|---|---|---|
| bar_1h | control | -0.236 | none | yes |
| gate_adx_up | control | -0.046 | min_clusters, min_trades, trades_on_half_the_datasets | no |
| gate_cm_not_down | control | -0.046 | beats_control_ddadj | no |
| gate_cm_up | control | -0.046 | none | yes |
| gate_p21_up_clean | control | -0.046 | min_clusters, min_trades, trades_on_half_the_datasets | no |
| htf | control | -0.046 | beats_control_ddadj | no |
| p_13_34_89 | control | -0.046 | beats_control_ddadj, plateau_neighbours_beat_control | no |
| p_21_55_144 | control | -0.046 | beats_control_ddadj, plateau_neighbours_beat_control | no |
| p_34_89_233 | control | -0.046 | beats_control_ddadj, plateau_neighbours_beat_control | no |
| p_55_144_377 | control | -0.046 | plateau_neighbours_beat_control | no |
| p_8_21_55 | control | -0.046 | plateau_neighbours_beat_control | no |

Picks: bar: bar_1h, gate: gate_cm_up, htf: None, period: None. Combination: not built (fewer than two picks). Judged arms: bar_1h, gate_cm_up.

### live

the one deployed live strategy (ETH 30m, direction both, htf_filter true, explicit ratchet ladder, isolated margin at leverage 20 with a fixed margin per trade) on frozen Binance US proxy candles with Hyperliquid fees and funding.

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| live_both | control | 1 | 92 | 163.9 | +0.187 | +0.018 | -0.024 | +0.168 | 8.29 | 0.56 | -17.69 | 0.095 | 1.68 | 61/31 | 92 |
| bar_1h | bar | 1 | 32 | 57.0 | +0.406 | +0.205 | +0.044 | +0.201 | 2.88 | 0.79 | -9.75 | 0.682 | 6.65 | 32/0 | 32 |
| live_long | direction | 1 | 61 | 108.7 | +0.297 | +0.129 | +0.115 | +0.168 | 5.50 | 0.74 | -14.41 | 0.545 | 7.86 | 61/0 | 61 |
| gate_adx_up | gate | 1 | 0 | 0.0 | - | - | - | - | 0.00 | 0.00 | 0.00 | 0.000 | 0.00 | 0/0 | 0 |
| gate_cm_not_down | gate | 1 | 60 | 106.9 | +0.240 | +0.073 | +0.061 | +0.167 | 5.40 | 0.73 | -14.86 | 0.295 | 4.38 | 60/0 | 60 |
| gate_cm_up | gate | 1 | 49 | 87.3 | +0.234 | +0.081 | +0.088 | +0.154 | 4.41 | 0.65 | -11.51 | 0.343 | 3.95 | 49/0 | 49 |
| gate_p21_up_clean | gate | 1 | 0 | 0.0 | - | - | - | - | 0.00 | 0.00 | 0.00 | 0.000 | 0.00 | 0/0 | 0 |
| p_13_34_89 | period | 1 | 25 | 44.5 | +0.269 | +0.063 | -0.023 | +0.206 | 2.25 | 0.30 | -12.08 | 0.130 | 1.57 | 25/0 | 25 |
| p_21_55_144 | period | 1 | 21 | 37.4 | -0.186 | -0.345 | -0.388 | +0.159 | 1.89 | 0.24 | -10.35 | -0.700 | -7.25 | 21/0 | 21 |
| p_34_89_233 | period | 1 | 17 | 30.3 | -0.767 | -0.927 | -1.000 | +0.160 | 1.52 | 0.20 | -20.20 | -0.780 | -15.76 | 17/0 | 17 |
| p_55_144_377 | period | 1 | 12 | 21.4 | -0.649 | -0.820 | -0.918 | +0.171 | 1.08 | 0.19 | -12.69 | -0.775 | -9.84 | 12/0 | 12 |
| p_8_21_55 | period | 1 | 41 | 73.1 | +0.268 | +0.080 | -0.224 | +0.188 | 3.69 | 0.44 | -14.07 | 0.233 | 3.28 | 41/0 | 41 |
| live_both_nohtf | reference | 1 | 133 | 237.0 | +0.232 | +0.067 | -0.001 | +0.165 | 11.98 | 0.08 | -15.91 | 0.567 | 9.02 | 72/61 | 133 |
| live_long_nohtf | reference | 1 | 99 | 176.4 | +0.250 | +0.066 | +0.020 | +0.185 | 8.92 | 1.03 | -20.71 | 0.313 | 6.49 | 99/0 | 99 |

| arm | control | control mean ddadj | failed checks | shortlisted |
|---|---|---|---|---|
| bar_1h | live_long | 0.545 | none | yes |
| gate_adx_up | live_long | 0.545 | beats_control_ddadj, min_clusters, min_trades, trades_on_half_the_datasets | no |
| gate_cm_not_down | live_long | 0.545 | beats_control_ddadj | no |
| gate_cm_up | live_long | 0.545 | beats_control_ddadj | no |
| gate_p21_up_clean | live_long | 0.545 | beats_control_ddadj, min_clusters, min_trades, trades_on_half_the_datasets | no |
| live_long | live_both | 0.095 | none | yes |
| p_13_34_89 | live_long | 0.545 | beats_control_ddadj, min_trades, plateau_neighbours_beat_control | no |
| p_21_55_144 | live_long | 0.545 | beats_control_ddadj, min_trades, plateau_neighbours_beat_control | no |
| p_34_89_233 | live_long | 0.545 | beats_control_ddadj, min_trades, plateau_neighbours_beat_control | no |
| p_55_144_377 | live_long | 0.545 | beats_control_ddadj, min_trades, plateau_neighbours_beat_control | no |
| p_8_21_55 | live_long | 0.545 | beats_control_ddadj, plateau_neighbours_beat_control | no |

Picks: bar: bar_1h, direction: live_long, gate: None, period: None. Combination: not built (fewer than two picks). Judged arms: bar_1h, live_long.

## Judged windows

Pooled windows for the statistics: oos, 2023H2, 2024, 2025H1. `stitched` is one continuous run from a flat book and is reported only.

### spot

`oos`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_4h | bar | 3 | 65 | 25.3 | -0.459 | -0.679 | -0.778 | +0.220 | 1.85 | 0.63 | -30.99 | -0.435 | -13.46 | 65/0 | 39 |
| combo | combo | 6 | 61 | 11.9 | -0.523 | -0.753 | -0.853 | +0.230 | 0.89 | 0.39 | -18.60 | -0.606 | -7.46 | 61/0 | 32 |
| gate_cm_not_down | gate | 6 | 282 | 54.9 | -0.045 | -0.247 | -0.347 | +0.202 | 3.91 | 0.51 | -34.13 | -0.459 | -11.54 | 282/0 | 103 |
| gate_cm_up | gate | 6 | 138 | 26.9 | -0.387 | -0.591 | -0.691 | +0.204 | 1.89 | 0.31 | -28.73 | -0.678 | -12.98 | 138/0 | 66 |
| p_13_34_89 | period | 6 | 138 | 26.9 | -0.126 | -0.354 | -0.453 | +0.228 | 2.00 | 0.83 | -30.49 | -0.284 | -7.90 | 138/0 | 46 |
| p_21_55_144 | period | 6 | 79 | 15.4 | -0.183 | -0.433 | -0.533 | +0.250 | 1.12 | 0.75 | -22.55 | -0.273 | -5.81 | 79/0 | 31 |
| p_8_21_55 | period | 6 | 230 | 44.8 | +0.212 | -0.004 | -0.105 | +0.217 | 3.38 | 0.98 | -40.84 | 0.163 | -0.74 | 230/0 | 73 |
| spot_control | control | 6 | 375 | 73.0 | -0.038 | -0.241 | -0.341 | +0.203 | 4.98 | 0.73 | -43.44 | -0.401 | -14.30 | 375/0 | 116 |

`2023H2`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_4h | bar | 3 | 51 | 33.8 | +2.734 | +2.271 | +2.168 | +0.463 | 1.73 | 6.30 | -18.76 | 3.445 | 66.25 | 51/0 | 25 |
| combo | combo | 6 | 66 | 21.9 | +1.432 | +0.897 | +0.796 | +0.535 | 1.07 | 3.78 | -22.73 | 0.288 | 10.68 | 66/0 | 19 |
| gate_cm_not_down | gate | 6 | 230 | 76.1 | +1.077 | +0.788 | +0.688 | +0.288 | 3.69 | 4.81 | -25.89 | 2.050 | 45.73 | 230/0 | 55 |
| gate_cm_up | gate | 6 | 141 | 46.7 | +1.671 | +1.345 | +1.244 | +0.325 | 2.40 | 4.05 | -19.81 | 2.587 | 45.70 | 141/0 | 46 |
| p_13_34_89 | period | 6 | 103 | 34.1 | +2.670 | +2.164 | +2.061 | +0.506 | 1.83 | 7.10 | -24.01 | 2.082 | 45.51 | 103/0 | 21 |
| p_21_55_144 | period | 6 | 52 | 17.2 | +6.727 | +5.717 | +5.611 | +1.011 | 0.93 | 9.38 | -23.52 | 2.935 | 65.53 | 52/0 | 13 |
| p_8_21_55 | period | 6 | 165 | 54.6 | +0.995 | +0.657 | +0.557 | +0.338 | 2.54 | 4.20 | -29.77 | 0.833 | 19.57 | 165/0 | 47 |
| spot_control | control | 6 | 258 | 85.4 | +0.918 | +0.640 | +0.540 | +0.278 | 4.01 | 4.78 | -25.89 | 2.100 | 43.52 | 258/0 | 61 |

`2024`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_4h | bar | 3 | 87 | 28.9 | +1.510 | +0.995 | +0.893 | +0.515 | 3.11 | 10.85 | -30.56 | 1.660 | 29.27 | 87/0 | 48 |
| combo | combo | 6 | 104 | 17.3 | +1.006 | +0.424 | +0.323 | +0.583 | 1.69 | 6.95 | -32.85 | 0.660 | 7.38 | 104/0 | 30 |
| gate_cm_not_down | gate | 6 | 450 | 74.9 | +0.376 | +0.080 | -0.020 | +0.296 | 6.65 | 8.41 | -44.36 | 0.636 | 6.48 | 450/0 | 117 |
| gate_cm_up | gate | 6 | 270 | 44.9 | +0.316 | -0.007 | -0.107 | +0.323 | 3.85 | 5.83 | -26.32 | -0.056 | -1.37 | 270/0 | 101 |
| p_13_34_89 | period | 6 | 167 | 27.8 | +0.751 | +0.260 | +0.160 | +0.491 | 2.79 | 8.61 | -36.00 | 0.681 | 7.33 | 167/0 | 51 |
| p_21_55_144 | period | 6 | 94 | 15.6 | +2.420 | +1.602 | +1.501 | +0.818 | 1.71 | 10.50 | -30.99 | 1.530 | 24.70 | 94/0 | 24 |
| p_8_21_55 | period | 6 | 314 | 52.2 | +0.664 | +0.295 | +0.195 | +0.369 | 5.36 | 10.19 | -32.36 | 0.521 | 13.14 | 314/0 | 79 |
| spot_control | control | 6 | 519 | 86.3 | +0.407 | +0.120 | +0.020 | +0.287 | 7.70 | 8.86 | -39.15 | 0.748 | 9.97 | 519/0 | 135 |

`2025H1`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_4h | bar | 3 | 43 | 32.8 | -0.737 | -0.961 | -1.060 | +0.224 | 1.17 | 0.48 | -26.50 | -0.592 | -13.25 | 43/0 | 20 |
| combo | combo | 6 | 41 | 15.6 | -0.285 | -0.562 | -0.662 | +0.277 | 0.58 | 0.58 | -19.48 | 0.042 | -3.63 | 41/0 | 17 |
| gate_cm_not_down | gate | 6 | 171 | 65.1 | -0.024 | -0.239 | -0.338 | +0.214 | 2.49 | 0.67 | -28.54 | -0.461 | -8.00 | 171/0 | 66 |
| gate_cm_up | gate | 6 | 89 | 33.9 | -0.136 | -0.354 | -0.454 | +0.218 | 1.32 | 0.40 | -22.22 | -0.407 | -5.41 | 89/0 | 29 |
| p_13_34_89 | period | 6 | 70 | 26.6 | +0.425 | +0.170 | +0.069 | +0.255 | 1.01 | 0.75 | -19.76 | 0.189 | 0.85 | 70/0 | 23 |
| p_21_55_144 | period | 6 | 45 | 17.1 | -0.903 | -1.140 | -1.239 | +0.237 | 0.61 | 0.35 | -22.79 | -0.512 | -8.26 | 45/0 | 20 |
| p_8_21_55 | period | 6 | 124 | 47.2 | +0.388 | +0.160 | +0.060 | +0.229 | 1.77 | 0.78 | -22.76 | 0.380 | 1.29 | 124/0 | 36 |
| spot_control | control | 6 | 215 | 81.8 | +0.075 | -0.134 | -0.234 | +0.209 | 3.09 | 0.67 | -29.68 | -0.333 | -6.16 | 215/0 | 72 |

`stitched`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_4h | bar | 3 | 114 | 26.8 | -0.188 | -0.443 | -0.542 | +0.255 | 3.40 | 2.56 | -46.17 | -0.506 | -17.47 | 114/0 | 63 |
| combo | combo | 6 | 105 | 12.3 | +0.072 | -0.207 | -0.307 | +0.279 | 1.65 | 1.59 | -35.95 | -0.200 | -4.41 | 105/0 | 45 |
| gate_cm_not_down | gate | 6 | 473 | 55.6 | +0.095 | -0.122 | -0.222 | +0.216 | 6.96 | 2.11 | -41.63 | -0.326 | -11.48 | 473/0 | 164 |
| gate_cm_up | gate | 6 | 222 | 26.1 | -0.171 | -0.393 | -0.493 | +0.222 | 3.26 | 1.18 | -37.21 | -0.593 | -14.57 | 222/0 | 102 |
| p_13_34_89 | period | 6 | 217 | 25.5 | +0.217 | -0.044 | -0.144 | +0.261 | 3.45 | 2.67 | -42.48 | 0.075 | -2.62 | 217/0 | 71 |
| p_21_55_144 | period | 6 | 129 | 15.2 | +0.110 | -0.183 | -0.283 | +0.293 | 1.96 | 2.16 | -42.35 | -0.284 | -6.16 | 129/0 | 45 |
| p_8_21_55 | period | 6 | 382 | 44.9 | +0.252 | +0.019 | -0.081 | +0.233 | 5.83 | 2.77 | -51.13 | 0.078 | -1.46 | 382/0 | 123 |
| spot_control | control | 6 | 636 | 74.8 | +0.046 | -0.168 | -0.268 | +0.214 | 8.86 | 2.49 | -51.62 | -0.471 | -18.64 | 636/0 | 197 |

### paper

`oos`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 4 | 322 | 94.0 | -0.035 | -0.237 | -0.338 | +0.203 | 6.50 | 0.59 | -33.35 | -0.752 | -17.95 | 322/0 | 156 |
| control | control | 8 | 978 | 142.7 | +0.014 | -0.188 | -0.299 | +0.202 | 9.41 | 0.61 | -44.73 | -0.738 | -20.73 | 978/0 | 257 |
| gate_cm_up | gate | 8 | 367 | 53.5 | +0.028 | -0.169 | -0.283 | +0.198 | 3.95 | 0.35 | -28.55 | -0.160 | -6.59 | 367/0 | 141 |

`2023H2`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 4 | 200 | 99.3 | -0.107 | -0.312 | -0.392 | +0.205 | 4.13 | 1.18 | -30.80 | -0.631 | -14.15 | 200/0 | 98 |
| control | control | 8 | 593 | 147.2 | -0.009 | -0.218 | -0.320 | +0.209 | 5.99 | 1.12 | -39.71 | -0.518 | -14.19 | 593/0 | 157 |
| gate_cm_up | gate | 8 | 273 | 67.8 | +0.174 | -0.048 | -0.163 | +0.222 | 3.04 | 0.83 | -21.75 | -0.117 | -1.17 | 273/0 | 106 |

`2024`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 4 | 405 | 101.0 | +0.204 | -0.059 | -0.153 | +0.263 | 8.91 | 4.06 | -26.75 | -0.096 | -6.49 | 405/0 | 193 |
| control | control | 8 | 1161 | 144.8 | +0.145 | -0.077 | -0.181 | +0.222 | 11.99 | 3.73 | -33.24 | -0.273 | -11.33 | 1161/0 | 288 |
| gate_cm_up | gate | 8 | 567 | 70.7 | +0.123 | -0.109 | -0.207 | +0.232 | 6.08 | 2.67 | -29.68 | -0.217 | -7.91 | 567/0 | 204 |

`2025H1`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 4 | 172 | 98.2 | +0.019 | -0.199 | -0.286 | +0.218 | 3.66 | 0.36 | -21.17 | -0.605 | -9.00 | 172/0 | 81 |
| control | control | 8 | 485 | 138.4 | +0.039 | -0.158 | -0.267 | +0.197 | 5.21 | 0.32 | -25.74 | -0.645 | -10.08 | 485/0 | 124 |
| gate_cm_up | gate | 8 | 182 | 51.9 | -0.299 | -0.501 | -0.618 | +0.202 | 1.93 | 0.19 | -27.32 | -0.703 | -11.04 | 182/0 | 67 |

`stitched`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 4 | 545 | 96.1 | +0.069 | -0.130 | -0.234 | +0.199 | 11.63 | 1.44 | -42.08 | -0.569 | -17.48 | 545/0 | 260 |
| control | control | 8 | 1653 | 145.7 | +0.067 | -0.131 | -0.239 | +0.198 | 16.65 | 1.52 | -60.15 | -0.588 | -23.14 | 1653/0 | 433 |
| gate_cm_up | gate | 8 | 621 | 54.7 | +0.160 | -0.028 | -0.139 | +0.188 | 7.11 | 0.97 | -34.11 | 0.281 | -1.53 | 621/0 | 238 |

### live

`oos`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 1 | 49 | 57.2 | -0.077 | -0.294 | -0.407 | +0.218 | 4.41 | 0.60 | -18.44 | -0.782 | -14.42 | 49/0 | 49 |
| live_both | control | 1 | 140 | 163.4 | +0.011 | -0.168 | -0.273 | +0.179 | 12.60 | 0.41 | -30.52 | -0.769 | -23.46 | 87/53 | 140 |
| live_both_nohtf | reference | 1 | 203 | 236.9 | +0.102 | -0.071 | -0.202 | +0.172 | 18.26 | 0.14 | -28.21 | -0.508 | -14.33 | 113/90 | 203 |
| live_long | direction | 1 | 87 | 101.5 | +0.054 | -0.152 | -0.282 | +0.206 | 7.83 | 0.62 | -19.17 | -0.691 | -13.25 | 87/0 | 87 |
| live_long_nohtf | reference | 1 | 146 | 170.4 | +0.047 | -0.145 | -0.259 | +0.192 | 13.14 | 0.90 | -32.96 | -0.643 | -21.19 | 146/0 | 146 |

`2023H2`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 1 | 35 | 69.5 | -0.473 | -0.671 | -0.767 | +0.199 | 3.14 | 1.26 | -23.87 | -0.984 | -23.50 | 35/0 | 35 |
| live_both | control | 1 | 88 | 174.7 | -0.006 | -0.219 | -0.309 | +0.213 | 7.92 | 0.99 | -19.32 | -0.997 | -19.27 | 59/29 | 88 |
| live_both_nohtf | reference | 1 | 130 | 258.1 | -0.005 | -0.184 | -0.284 | +0.179 | 11.69 | -0.48 | -26.52 | -0.903 | -23.94 | 78/52 | 130 |
| live_long | direction | 1 | 59 | 117.1 | -0.085 | -0.277 | -0.383 | +0.192 | 5.31 | 1.13 | -16.40 | -0.997 | -16.35 | 59/0 | 59 |
| live_long_nohtf | reference | 1 | 98 | 194.6 | -0.045 | -0.251 | -0.355 | +0.206 | 8.81 | 1.33 | -24.63 | -0.998 | -24.59 | 98/0 | 98 |

`2024`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 1 | 61 | 60.9 | +0.486 | +0.256 | +0.165 | +0.229 | 5.50 | 3.40 | -10.65 | 1.469 | 15.64 | 61/0 | 61 |
| live_both | control | 1 | 162 | 161.7 | +0.152 | -0.028 | -0.151 | +0.179 | 14.60 | 2.50 | -17.47 | -0.258 | -4.50 | 113/49 | 162 |
| live_both_nohtf | reference | 1 | 247 | 246.5 | +0.135 | -0.071 | -0.163 | +0.206 | 22.24 | -0.20 | -25.59 | -0.690 | -17.66 | 124/123 | 247 |
| live_long | direction | 1 | 113 | 112.8 | +0.312 | +0.142 | +0.011 | +0.171 | 10.18 | 2.94 | -9.71 | 1.649 | 16.01 | 113/0 | 113 |
| live_long_nohtf | reference | 1 | 176 | 175.7 | +0.219 | +0.037 | -0.075 | +0.182 | 15.85 | 3.51 | -11.72 | 0.554 | 6.49 | 176/0 | 176 |

`2025H1`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 1 | 33 | 75.3 | +0.255 | -0.007 | -0.092 | +0.262 | 2.97 | 0.61 | -12.74 | -0.017 | -0.22 | 33/0 | 33 |
| live_both | control | 1 | 73 | 166.7 | -0.068 | -0.306 | -0.428 | +0.238 | 6.56 | 0.28 | -22.58 | -0.988 | -22.32 | 45/28 | 73 |
| live_both_nohtf | reference | 1 | 112 | 255.7 | +0.258 | +0.081 | -0.012 | +0.178 | 10.07 | 0.09 | -10.63 | 0.852 | 9.06 | 65/47 | 112 |
| live_long | direction | 1 | 45 | 102.7 | -0.384 | -0.634 | -0.762 | +0.250 | 4.04 | 0.29 | -28.54 | -1.000 | -28.54 | 45/0 | 45 |
| live_long_nohtf | reference | 1 | 81 | 184.9 | -0.077 | -0.265 | -0.383 | +0.188 | 7.28 | 0.39 | -21.43 | -1.000 | -21.43 | 81/0 | 81 |

`stitched`

| arm | family | datasets | trades (positions) | trades/yr | gross %/trade | net %/trade | stress %/trade | cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % | positions L/S | clusters |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bar_1h | bar | 1 | 80 | 56.4 | +0.120 | -0.092 | -0.223 | +0.212 | 7.20 | 1.39 | -17.28 | -0.425 | -7.35 | 80/0 | 80 |
| live_both | control | 1 | 232 | 163.6 | +0.081 | -0.094 | -0.176 | +0.175 | 20.89 | 0.97 | -35.66 | -0.611 | -21.78 | 148/84 | 232 |
| live_both_nohtf | reference | 1 | 335 | 236.2 | +0.149 | -0.021 | -0.128 | +0.170 | 30.15 | 0.17 | -28.79 | -0.239 | -6.88 | 184/151 | 335 |
| live_long | direction | 1 | 148 | 104.4 | +0.155 | -0.036 | -0.123 | +0.191 | 13.32 | 1.36 | -20.76 | -0.260 | -5.39 | 148/0 | 148 |
| live_long_nohtf | reference | 1 | 245 | 172.8 | +0.129 | -0.060 | -0.148 | +0.189 | 22.05 | 1.94 | -37.62 | -0.391 | -14.70 | 245/0 | 245 |

## Statistics and verdict checks per judged arm

BH family size 36 (12 in-family arms per frame, three frames); p_decision is the larger of the plain and the cluster sign-flip p-values.

| frame | arm | noise baseline verdict | trades | clusters | gross %/trade | net %/trade | stress %/trade | p plain | p cluster | BH pass | venue | failed checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| spot | bar_4h | distinguishable_positive | 246 | 132 | +0.851 | +0.475 | +0.375 | 0.1080 | 0.1428 | no | - | bh_pass |
| spot | combo | distinguishable_positive | 272 | 98 | +0.572 | +0.126 | +0.026 | 0.3493 | 0.3709 | no | - | bh_pass |
| spot | gate_cm_up | distinguishable_positive | 638 | 242 | +0.400 | +0.117 | +0.017 | 0.2446 | 0.2897 | no | - | bh_pass |
| spot | p_13_34_89 | distinguishable_positive | 478 | 141 | +0.864 | +0.480 | +0.379 | 0.1087 | 0.2092 | no | - | bh_pass |
| paper | bar_1h | distinguishable_positive | 1099 | 528 | +0.048 | -0.179 | -0.272 | 0.9983 | 0.9904 | no | fail | bh_pass, net_positive_base, net_positive_stress, venue_not_fail |
| paper | gate_cm_up | distinguishable_positive | 1389 | 518 | +0.053 | -0.164 | -0.273 | 0.9988 | 0.9935 | no | pass | bh_pass, net_positive_base, net_positive_stress |
| live | bar_1h | indistinguishable_from_zero | 0 | 0 | - | - | - | 1.0000 | 1.0000 | no | inconclusive | bh_pass, half_the_datasets_remain, min_clusters, min_trades, net_positive_base, net_positive_stress |
| live | live_long | indistinguishable_from_zero | 0 | 0 | - | - | - | 1.0000 | 1.0000 | no | inconclusive | bh_pass, half_the_datasets_remain, min_clusters, min_trades, net_positive_base, net_positive_stress |

## Venue and bridge windows (perps frames)

### paper

venue `1h common base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 5 | 128 | -0.322 | -1.34 | -0.528 | -17.23 | 53 |
| control | 5 | 128 | -0.322 | -1.34 | -0.528 | -17.23 | 53 |
| gate_cm_up | 5 | 50 | -0.619 | -1.72 | -0.624 | -19.45 | 27 |

venue `1h common stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 5 | 128 | -0.453 | -2.02 | -0.651 | -18.31 | 55 |
| control | 5 | 128 | -0.453 | -2.02 | -0.651 | -18.31 | 55 |
| gate_cm_up | 5 | 50 | -0.710 | -2.00 | -0.684 | -20.06 | 27 |

venue `1h tail base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 5 | 291 | -0.377 | -1.44 | -0.582 | -45.77 | 103 |
| control | 5 | 291 | -0.377 | -1.44 | -0.582 | -45.77 | 103 |
| gate_cm_up | 5 | 85 | -0.629 | -1.48 | -0.558 | -32.90 | 57 |

venue `1h tail stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 5 | 289 | -0.502 | -2.14 | -0.725 | -47.55 | 107 |
| control | 5 | 289 | -0.502 | -2.14 | -0.725 | -47.55 | 107 |
| gate_cm_up | 5 | 87 | -0.735 | -1.89 | -0.641 | -33.99 | 59 |

venue `30m tail base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| control | 5 | 303 | +0.004 | 0.21 | 0.240 | -19.48 | 85 |
| gate_cm_up | 5 | 84 | +0.093 | 0.74 | 0.796 | -15.98 | 44 |

venue `30m tail stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| control | 5 | 302 | -0.102 | -0.79 | -0.229 | -21.67 | 87 |
| gate_cm_up | 5 | 82 | +0.014 | 0.33 | 0.508 | -16.60 | 42 |

bridge `1h common base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 5 | 132 | -0.162 | -0.77 | -0.262 | -16.85 | 57 |
| control | 5 | 132 | -0.162 | -0.77 | -0.262 | -16.85 | 57 |
| gate_cm_up | 5 | 56 | -0.260 | -1.02 | -0.394 | -17.00 | 34 |

bridge `1h common stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 5 | 133 | -0.273 | -1.33 | -0.459 | -18.08 | 58 |
| control | 5 | 133 | -0.273 | -1.33 | -0.459 | -18.08 | 58 |
| gate_cm_up | 5 | 56 | -0.352 | -1.30 | -0.466 | -17.53 | 34 |

bridge `1h tail base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 5 | 316 | -0.246 | -1.16 | -0.551 | -37.61 | 119 |
| control | 5 | 316 | -0.246 | -1.16 | -0.551 | -37.61 | 119 |
| gate_cm_up | 5 | 103 | -0.475 | -1.54 | -0.617 | -33.12 | 72 |

bridge `1h tail stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 5 | 310 | -0.345 | -1.68 | -0.681 | -40.09 | 119 |
| control | 5 | 310 | -0.345 | -1.68 | -0.681 | -40.09 | 119 |
| gate_cm_up | 5 | 103 | -0.564 | -1.83 | -0.669 | -34.46 | 72 |

bridge `30m tail base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| control | 5 | 308 | -0.019 | -0.24 | -0.091 | -17.47 | 93 |
| gate_cm_up | 5 | 98 | +0.120 | 0.50 | 0.503 | -9.70 | 46 |

bridge `30m tail stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| control | 5 | 314 | -0.166 | -1.75 | -0.548 | -19.73 | 96 |
| gate_cm_up | 5 | 101 | -0.037 | -0.41 | -0.099 | -10.86 | 47 |

- venue verdict for `bar_1h` against `control`: **fail** (loses to control on mean Sharpe and mean ddadj at cost x1)
- venue verdict for `gate_cm_up` against `control`: **pass** (beats control on mean Sharpe and mean ddadj at cost x1 and x2)

### live

venue `1h common base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 1 | 15 | -0.151 | -0.43 | -0.198 | -11.43 | 15 |

venue `1h common stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 1 | 17 | -0.389 | -1.44 | -0.475 | -13.93 | 17 |

venue `1h tail base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 1 | 28 | -0.566 | -1.90 | -0.908 | -17.45 | 28 |

venue `1h tail stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 1 | 30 | -0.690 | -2.58 | -0.949 | -21.78 | 30 |

venue `30m tail base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| live_both | 1 | 32 | +0.071 | 0.61 | 0.318 | -7.11 | 32 |
| live_both_nohtf | 1 | 54 | +0.350 | 3.18 | 3.240 | -5.83 | 54 |
| live_long | 1 | 23 | +0.023 | 0.21 | 0.078 | -6.76 | 23 |
| live_long_nohtf | 1 | 37 | +0.236 | 1.86 | 1.803 | -4.83 | 37 |

venue `30m tail stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| live_both | 1 | 32 | +0.056 | 0.49 | 0.280 | -6.36 | 32 |
| live_both_nohtf | 1 | 56 | +0.211 | 2.05 | 1.887 | -6.27 | 56 |
| live_long | 1 | 23 | +0.038 | 0.30 | 0.141 | -6.23 | 23 |
| live_long_nohtf | 1 | 38 | +0.171 | 1.39 | 0.898 | -7.24 | 38 |

bridge `1h common base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 1 | 16 | -0.150 | -0.49 | -0.213 | -11.29 | 16 |

bridge `1h common stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 1 | 17 | -0.312 | -1.19 | -0.388 | -13.65 | 17 |

bridge `1h tail base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 1 | 31 | -0.479 | -1.88 | -0.892 | -16.65 | 31 |

bridge `1h tail stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| bar_1h | 1 | 32 | -0.600 | -2.47 | -0.930 | -20.64 | 32 |

bridge `30m tail base`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| live_both | 1 | 36 | +0.059 | 0.58 | 0.319 | -6.61 | 36 |
| live_both_nohtf | 1 | 60 | +0.108 | 1.20 | 0.784 | -8.25 | 60 |
| live_long | 1 | 25 | +0.068 | 0.52 | 0.272 | -6.26 | 25 |
| live_long_nohtf | 1 | 40 | +0.191 | 1.64 | 0.995 | -7.67 | 40 |

bridge `30m tail stress`

| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |
|---|---|---|---|---|---|---|---|
| live_both | 1 | 37 | -0.053 | -0.38 | -0.244 | -8.08 | 37 |
| live_both_nohtf | 1 | 62 | -0.001 | 0.10 | -0.009 | -9.62 | 62 |
| live_long | 1 | 26 | -0.041 | -0.20 | -0.140 | -7.59 | 26 |
| live_long_nohtf | 1 | 41 | +0.090 | 0.85 | 0.402 | -9.23 | 41 |

- venue verdict for `bar_1h` against `live_long`: **inconclusive** (venue coverage incomplete or missing legs)
- venue verdict for `live_long` against `live_both`: **inconclusive** (venue coverage incomplete or missing legs)

## Exclusions

- spot: none
- paper HYPE 1h: coverage: is: warm-up prefix lacks 3011 consecutive bars (2926 missing, first 2025-02-04 13:00:00); stitched: warm-up prefix lacks 3011 consecutive bars (2926 missing, first 2025-02-04 13:00:00)
- paper HYPE 30m: coverage: is: warm-up prefix lacks 3011 consecutive bars (2841 missing, first 2025-04-08 06:30:00); stitched: warm-up prefix lacks 3011 consecutive bars (2841 missing, first 2025-04-08 06:30:00)
- live ETH 1h: failing proxy htf fidelity row htf 2024; failing proxy htf fidelity row htf 2025H1
- live ETH 30m: failing proxy htf fidelity row htf 2024; failing proxy htf fidelity row htf 2025H1

## Parity, smoke and fidelity

- Frozen higher-timeframe input against the existing `--htf-filter` path (ETH/USDT 30m `is` with its 3011-bar warm-up, 12851 bars): trend identical yes, filtered signal identical yes, bucket closes identical yes, fetch calls 0.
- The same `htf` leg scored with the derived trend and with the existing-path trend injected: identical yes (74 trades), fetch calls 0.
- Label cache parity (BTC/USDT 1h `is`): identical yes for control, gate_p21_up_clean.
- Smoke on synthetic candles: every arm ran yes, `live_both` opened a short yes, entry ATR is Wilder yes (10 entries), network calls 0; live sizing `{"budget_source": "strategy_cash", "exchange_leverage": 20.0, "margin_mode": "isolated", "margin_per_trade_usd": 50.0, "pool_available_margin": null, "pool_evidence": "verified", "provenance": {"source": "load_strategy_config"}, "sizing_leverage": 1.0}`.
- Fidelity: 84 rows, 80 pass; engine label check 76 rows, 19398 sampled bars, 0 rows with a mismatch.

| frame | group | dataset | window | kind | regime window | n | agreement | passes |
|---|---|---|---|---|---|---|---|---|
| live | proxy | ETH 1h | 2023H2 | composite | composite_medium | 4081 | 1.0000 | yes |
| live | proxy | ETH 1h | 2023H2 | htf | htf | 1104 | 0.9728 | yes |
| live | proxy | ETH 1h | 2024 | composite | composite_medium | 8449 | 1.0000 | yes |
| live | proxy | ETH 1h | 2024 | htf | htf | 2196 | 0.9485 | no |
| live | proxy | ETH 1h | 2025H1 | composite | composite_medium | 3505 | 1.0000 | yes |
| live | proxy | ETH 1h | 2025H1 | htf | htf | 960 | 0.9469 | no |
| live | proxy | ETH 1h | oos | composite | composite_medium | 7168 | 1.0000 | yes |
| live | proxy | ETH 1h | oos | htf | htf | 1875 | 0.9563 | yes |
| live | proxy | ETH 30m | 2023H2 | composite | composite_medium | 8497 | 1.0000 | yes |
| live | proxy | ETH 30m | 2023H2 | htf | htf | 1104 | 0.9728 | yes |
| live | proxy | ETH 30m | 2024 | composite | composite_medium | 17233 | 1.0000 | yes |
| live | proxy | ETH 30m | 2024 | htf | htf | 2196 | 0.9485 | no |
| live | proxy | ETH 30m | 2025H1 | composite | composite_medium | 7345 | 1.0000 | yes |
| live | proxy | ETH 30m | 2025H1 | htf | htf | 960 | 0.9469 | no |
| live | proxy | ETH 30m | oos | composite | composite_medium | 14671 | 1.0000 | yes |
| live | proxy | ETH 30m | oos | htf | htf | 1875 | 0.9563 | yes |
| live | venue | ETH 1h | tail | composite | composite_medium | 4249 | 1.0000 | yes |
| live | venue | ETH 30m | tail | composite | composite_medium | 4225 | 1.0000 | yes |
| paper | proxy | BNB 1h | 2023H2 | composite | composite_medium | 4067 | 1.0000 | yes |
| paper | proxy | BNB 1h | 2024 | composite | composite_medium | 8435 | 1.0000 | yes |
| paper | proxy | BNB 1h | 2025H1 | composite | composite_medium | 3491 | 1.0000 | yes |
| paper | proxy | BNB 1h | oos | composite | composite_medium | 7154 | 1.0000 | yes |
| paper | proxy | BTC 1h | 2023H2 | composite | composite_medium | 4067 | 1.0000 | yes |
| paper | proxy | BTC 1h | 2024 | composite | composite_medium | 8435 | 1.0000 | yes |
| paper | proxy | BTC 1h | 2025H1 | composite | composite_medium | 3491 | 1.0000 | yes |
| paper | proxy | BTC 1h | oos | composite | composite_medium | 7154 | 1.0000 | yes |
| paper | proxy | ETH 1h | 2023H2 | composite | composite_medium | 4067 | 1.0000 | yes |
| paper | proxy | ETH 1h | 2024 | composite | composite_medium | 8435 | 1.0000 | yes |
| paper | proxy | ETH 1h | 2025H1 | composite | composite_medium | 3491 | 1.0000 | yes |
| paper | proxy | ETH 1h | oos | composite | composite_medium | 7154 | 1.0000 | yes |
| paper | proxy | SOL 1h | 2023H2 | composite | composite_medium | 4067 | 1.0000 | yes |
| paper | proxy | SOL 1h | 2024 | composite | composite_medium | 8435 | 1.0000 | yes |
| paper | proxy | SOL 1h | 2025H1 | composite | composite_medium | 3491 | 1.0000 | yes |
| paper | proxy | SOL 1h | oos | composite | composite_medium | 7154 | 1.0000 | yes |
| paper | proxy | BNB 30m | 2023H2 | composite | composite_medium | 8483 | 1.0000 | yes |
| paper | proxy | BNB 30m | 2024 | composite | composite_medium | 17219 | 1.0000 | yes |
| paper | proxy | BNB 30m | 2025H1 | composite | composite_medium | 7331 | 1.0000 | yes |
| paper | proxy | BNB 30m | oos | composite | composite_medium | 14657 | 1.0000 | yes |
| paper | proxy | BTC 30m | 2023H2 | composite | composite_medium | 8483 | 1.0000 | yes |
| paper | proxy | BTC 30m | 2024 | composite | composite_medium | 17219 | 1.0000 | yes |
| paper | proxy | BTC 30m | 2025H1 | composite | composite_medium | 7331 | 1.0000 | yes |
| paper | proxy | BTC 30m | oos | composite | composite_medium | 14657 | 1.0000 | yes |
| paper | proxy | ETH 30m | 2023H2 | composite | composite_medium | 8483 | 1.0000 | yes |
| paper | proxy | ETH 30m | 2024 | composite | composite_medium | 17219 | 1.0000 | yes |
| paper | proxy | ETH 30m | 2025H1 | composite | composite_medium | 7331 | 1.0000 | yes |
| paper | proxy | ETH 30m | oos | composite | composite_medium | 14657 | 1.0000 | yes |
| paper | proxy | SOL 30m | 2023H2 | composite | composite_medium | 8483 | 1.0000 | yes |
| paper | proxy | SOL 30m | 2024 | composite | composite_medium | 17219 | 1.0000 | yes |
| paper | proxy | SOL 30m | 2025H1 | composite | composite_medium | 7331 | 1.0000 | yes |
| paper | proxy | SOL 30m | oos | composite | composite_medium | 14657 | 1.0000 | yes |
| paper | venue | BNB 1h | tail | composite | composite_medium | 4235 | 1.0000 | yes |
| paper | venue | BTC 1h | tail | composite | composite_medium | 4235 | 1.0000 | yes |
| paper | venue | ETH 1h | tail | composite | composite_medium | 4235 | 1.0000 | yes |
| paper | venue | HYPE 1h | tail | composite | composite_medium | 4235 | 1.0000 | yes |
| paper | venue | SOL 1h | tail | composite | composite_medium | 4235 | 1.0000 | yes |
| paper | venue | BNB 30m | tail | composite | composite_medium | 4211 | 1.0000 | yes |
| paper | venue | BTC 30m | tail | composite | composite_medium | 4211 | 1.0000 | yes |
| paper | venue | ETH 30m | tail | composite | composite_medium | 4211 | 1.0000 | yes |
| paper | venue | HYPE 30m | tail | composite | composite_medium | 4211 | 1.0000 | yes |
| paper | venue | SOL 30m | tail | composite | composite_medium | 4211 | 1.0000 | yes |
| spot | proxy | BTC 1h | 2023H2 | composite | composite_medium | 4067 | 1.0000 | yes |
| spot | proxy | BTC 1h | 2024 | composite | composite_medium | 8435 | 1.0000 | yes |
| spot | proxy | BTC 1h | 2025H1 | composite | composite_medium | 3491 | 1.0000 | yes |
| spot | proxy | BTC 1h | oos | composite | composite_medium | 7154 | 1.0000 | yes |
| spot | proxy | ETH 1h | 2023H2 | composite | composite_medium | 4067 | 1.0000 | yes |
| spot | proxy | ETH 1h | 2024 | composite | composite_medium | 8435 | 1.0000 | yes |
| spot | proxy | ETH 1h | 2025H1 | composite | composite_medium | 3491 | 1.0000 | yes |
| spot | proxy | ETH 1h | oos | composite | composite_medium | 7154 | 1.0000 | yes |
| spot | proxy | SOL 1h | 2023H2 | composite | composite_medium | 4067 | 1.0000 | yes |
| spot | proxy | SOL 1h | 2024 | composite | composite_medium | 8435 | 1.0000 | yes |
| spot | proxy | SOL 1h | 2025H1 | composite | composite_medium | 3491 | 1.0000 | yes |
| spot | proxy | SOL 1h | oos | composite | composite_medium | 7154 | 1.0000 | yes |
| spot | proxy | BTC 4h | 2023H2 | composite | composite_medium | 755 | 1.0000 | yes |
| spot | proxy | BTC 4h | 2024 | composite | composite_medium | 1847 | 1.0000 | yes |
| spot | proxy | BTC 4h | 2025H1 | composite | composite_medium | 611 | 1.0000 | yes |
| spot | proxy | BTC 4h | oos | composite | composite_medium | 1527 | 1.0000 | yes |
| spot | proxy | ETH 4h | 2023H2 | composite | composite_medium | 755 | 1.0000 | yes |
| spot | proxy | ETH 4h | 2024 | composite | composite_medium | 1847 | 1.0000 | yes |
| spot | proxy | ETH 4h | 2025H1 | composite | composite_medium | 611 | 1.0000 | yes |
| spot | proxy | ETH 4h | oos | composite | composite_medium | 1527 | 1.0000 | yes |
| spot | proxy | SOL 4h | 2023H2 | composite | composite_medium | 755 | 1.0000 | yes |
| spot | proxy | SOL 4h | 2024 | composite | composite_medium | 1847 | 1.0000 | yes |
| spot | proxy | SOL 4h | 2025H1 | composite | composite_medium | 611 | 1.0000 | yes |
| spot | proxy | SOL 4h | oos | composite | composite_medium | 1527 | 1.0000 | yes |

## Coverage

- live proxy ETH 1h: included (warm-up prefix and funding complete on is, oos, stitched)
- live proxy ETH 30m: included (warm-up prefix and funding complete on is, oos, stitched)
- live venue ETH 30m: included (complete candles, warm-up and funding on every window)
- live venue ETH 1h: included (complete candles, warm-up and funding on every window)
- paper proxy BNB 1h: included (warm-up prefix and funding complete on is, oos, stitched)
- paper proxy BNB 30m: included (warm-up prefix and funding complete on is, oos, stitched)
- paper proxy BTC 1h: included (warm-up prefix and funding complete on is, oos, stitched)
- paper proxy BTC 30m: included (warm-up prefix and funding complete on is, oos, stitched)
- paper proxy ETH 1h: included (warm-up prefix and funding complete on is, oos, stitched)
- paper proxy ETH 30m: included (warm-up prefix and funding complete on is, oos, stitched)
- paper proxy HYPE 1h: excluded (is: warm-up prefix lacks 3011 consecutive bars (2926 missing, first 2025-02-04 13:00:00); stitched: warm-up prefix lacks 3011 consecutive bars (2926 missing, first 2025-02-04 13:00:00))
- paper proxy HYPE 30m: excluded (is: warm-up prefix lacks 3011 consecutive bars (2841 missing, first 2025-04-08 06:30:00); stitched: warm-up prefix lacks 3011 consecutive bars (2841 missing, first 2025-04-08 06:30:00))
- paper proxy SOL 1h: included (warm-up prefix and funding complete on is, oos, stitched)
- paper proxy SOL 30m: included (warm-up prefix and funding complete on is, oos, stitched)
- paper venue BNB 30m: included (complete candles, warm-up and funding on every window)
- paper venue BNB 1h: included (complete candles, warm-up and funding on every window)
- paper venue BTC 30m: included (complete candles, warm-up and funding on every window)
- paper venue BTC 1h: included (complete candles, warm-up and funding on every window)
- paper venue ETH 30m: included (complete candles, warm-up and funding on every window)
- paper venue ETH 1h: included (complete candles, warm-up and funding on every window)
- paper venue HYPE 30m: included (complete candles, warm-up and funding on every window)
- paper venue HYPE 1h: included (complete candles, warm-up and funding on every window)
- paper venue SOL 30m: included (complete candles, warm-up and funding on every window)
- paper venue SOL 1h: included (complete candles, warm-up and funding on every window)
- spot proxy BTC 1h: included (warm-up prefix and funding complete on is, oos, stitched)
- spot proxy BTC 4h: included (warm-up prefix and funding complete on is, oos, stitched)
- spot proxy ETH 1h: included (warm-up prefix and funding complete on is, oos, stitched)
- spot proxy ETH 4h: included (warm-up prefix and funding complete on is, oos, stitched)
- spot proxy SOL 1h: included (warm-up prefix and funding complete on is, oos, stitched)
- spot proxy SOL 4h: included (warm-up prefix and funding complete on is, oos, stitched)

## Run time

`time-leg` measured 0.02918 s per labelled window-bar on BTC 1h `is` (load average 3.8, 4.5, 4.5 on 10 CPUs) and projected 19.1 process-hours for the label stages.

`judge` starts: 1; finished runs: 1.

## Prior reads

- issue 1282 and the fee rescreen (issue 1315) read is, oos, 2023, 2024 and 2025H1 of the six spot datasets for the registry-default tema_cross baseline (1h and 4h)
- breakout_2014 read every proxy window (is, oos, 2023H2, 2024, 2025H1, stitched) and the venue tail and common windows of the reused files with breakout
- no filter arm, no 30m leg and no deployed-shape leg of tema_cross was read before this study

## Disclosures

- closed-bar decisions are opt-in live and the deployed entries do not set them
- the paper regime_directional_policy is uncertified, so live runs it off; the arms run it off
- use_defaults resolves to the system ratchet ladders
- the venue windows lie inside oos
- live circuit-breaker and drawdown limits are not modelled
- the live higher-timeframe check fetches the last 60 buckets and also sees the bucket that closes at the decision bar, one bucket earlier than the shifted frozen series
- on the registry-close spot shape the higher-timeframe filter also suppresses the exit signal while the trend is up, as live does for that shape
- the live shape runs isolated margin at leverage 20 with a fixed margin per trade; the backtest uses 1000 USD of strategy cash
