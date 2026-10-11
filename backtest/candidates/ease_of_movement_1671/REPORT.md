# Ease of Movement breakout (#1671): report

Verdict: **FAIL**. Mean net return not above zero at x1 and x2.
Registry label: `edge_status="no_edge"`, `edge_source="study_fail"`.

This report is rendered by `run_study.py run` from [`results.json`](results.json). The frozen specification is [`README.md`](README.md); the frozen inputs and their sha256 are in [`study_manifest.json`](study_manifest.json).

## Provenance

- Source commit at run time: `f262c410b62a506125b3d622b9bdce25c6db94fa` (the run may precede the final commit; the per-file source sha256 values in `results.json` pin the code that ran).
- Candles and base volume: Binance US spot BTC/USDT, ETH/USDT and SOL/USDT at 1h and 4h, used as proxies for Hyperliquid perpetual prices and volume.
- Funding: Hyperliquid hourly funding charged through `shared_tools/funding_fetcher.py`; the replay runs with network fetches disabled, so a missing hour is recorded as incomplete.
- Fees: Hyperliquid taker 0.045% per fill. Slippage: the engine default 0.05% per fill (x1), with 0.10% (x2) and 0.20% (x4) stress runs of the selected candidate.
- Not modeled on this path: the venue lot grid, the $10 venue minimum, spread separately from slippage, and market impact. Fixed slippage does not prove that a low-volume bar was executable.

## Selection (training window `is`)

Selected `P28_B60` (eom_period=28, breakout_window=60); fallback to seed: False.

| Variant | Eligible | Mean Sharpe | Mean DD-adj | Mean return % | Positions |
|---|---|---|---|---|---|
| P7_B10 | True | -2.803 | -0.800 | -40.13 | 874 |
| P7_B20 | True | -1.943 | -0.725 | -28.56 | 672 |
| P7_B40 | True | -2.115 | -0.801 | -25.25 | 489 |
| P7_B60 | True | -2.169 | -0.800 | -24.59 | 395 |
| P14_B10 | True | -3.197 | -0.958 | -45.02 | 802 |
| P14_B20 | True | -2.442 | -0.903 | -34.30 | 643 |
| P14_B40 | True | -2.117 | -0.843 | -24.88 | 448 |
| P14_B60 | True | -2.011 | -0.811 | -22.79 | 368 |
| P21_B10 | True | -2.652 | -0.780 | -35.43 | 767 |
| P21_B20 | True | -1.881 | -0.695 | -25.88 | 618 |
| P21_B40 | True | -1.825 | -0.719 | -20.32 | 430 |
| P21_B60 | True | -1.883 | -0.745 | -20.30 | 349 |
| P28_B10 | True | -2.656 | -0.882 | -37.26 | 724 |
| P28_B20 | True | -1.874 | -0.830 | -26.31 | 569 |
| P28_B40 | True | -1.829 | -0.802 | -21.12 | 422 |
| P28_B60 | True | -1.638 | -0.761 | -18.74 | 331 |

## Scored windows (`2024`, `oos`)

Means are over the 12 scored dataset-window legs. Positions are independent opening events.

| Arm | Mean return % | Mean Sharpe | Mean DD-adj | Positions (L/S) | Fees $ | Turnover | Top-5 share of gains | Funding-incomplete legs |
|---|---|---|---|---|---|---|---|---|
| candidate_P28_B60 | -26.29 | -1.568 | -0.710 | 1114 (594/520) | 803.95 | 1786.14 | 0.077 | 0 |
| candidate_P28_B60_slip0.001 | -31.55 | -2.073 | -0.760 | 1136 (604/532) | 766.48 | 1702.90 | 0.085 | 0 |
| candidate_P28_B60_slip0.002 | -41.92 | -3.224 | -0.845 | 1190 (637/553) | 721.30 | 1602.54 | 0.100 | 0 |
| donchian_breakout_B60 | -31.29 | -1.879 | -0.784 | 1299 (687/612) | 893.14 | 1984.31 | 0.069 | 0 |
| chaikin_money_flow_breakout_B60 | -27.62 | -1.702 | -0.780 | 1092 (608/484) | 787.28 | 1749.12 | 0.078 | 0 |
| volume_weighted | -17.51 | -1.069 | -0.473 | 1114 (529/585) | 842.58 | 1871.97 | 0.062 | 0 |
| candidate_P14_B10 | -45.65 | -2.291 | -0.826 | 2371 (1238/1133) | 1438.60 | 3196.16 | 0.047 | 0 |
| candidate_P14_B20 | -41.55 | -2.174 | -0.848 | 1949 (1025/924) | 1271.36 | 2824.61 | 0.053 | 0 |
| candidate_P14_B40 | -30.26 | -1.683 | -0.742 | 1418 (738/680) | 1003.58 | 2229.68 | 0.060 | 0 |
| candidate_P14_B60 | -29.39 | -1.745 | -0.763 | 1148 (613/535) | 812.19 | 1804.46 | 0.075 | 0 |
| candidate_P21_B10 | -43.48 | -2.263 | -0.801 | 2258 (1201/1057) | 1377.49 | 3060.40 | 0.050 | 0 |
| candidate_P21_B20 | -40.25 | -2.185 | -0.809 | 1904 (1013/891) | 1224.77 | 2721.09 | 0.056 | 0 |
| candidate_P21_B40 | -27.98 | -1.570 | -0.670 | 1391 (727/664) | 983.45 | 2184.95 | 0.062 | 0 |
| candidate_P21_B60 | -28.13 | -1.709 | -0.724 | 1131 (610/521) | 803.61 | 1785.40 | 0.077 | 0 |
| candidate_P28_B10 | -40.68 | -2.043 | -0.793 | 2152 (1141/1011) | 1374.74 | 3054.29 | 0.051 | 0 |
| candidate_P28_B20 | -37.79 | -2.024 | -0.818 | 1786 (948/838) | 1209.93 | 2688.12 | 0.057 | 0 |
| candidate_P28_B40 | -26.61 | -1.451 | -0.664 | 1361 (708/653) | 979.16 | 2175.43 | 0.062 | 0 |
| candidate_P7_B10 | -44.15 | -2.040 | -0.781 | 2510 (1300/1210) | 1532.00 | 3403.69 | 0.044 | 0 |
| candidate_P7_B20 | -41.40 | -2.124 | -0.812 | 2013 (1035/978) | 1279.92 | 2843.62 | 0.053 | 0 |
| candidate_P7_B40 | -30.75 | -1.730 | -0.723 | 1469 (754/715) | 1016.84 | 2259.14 | 0.060 | 0 |
| candidate_P7_B60 | -30.32 | -1.888 | -0.772 | 1195 (631/564) | 828.20 | 1840.03 | 0.074 | 0 |
| chaikin_money_flow_breakout_B20 | -37.75 | -2.036 | -0.813 | 1755 (949/806) | 1159.93 | 2577.04 | 0.052 | 0 |
| donchian_breakout_B20 | -42.77 | -2.170 | -0.832 | 2190 (1138/1052) | 1396.27 | 3102.13 | 0.048 | 0 |

## Per window (selected candidate and matched comparators)

| Arm | Window | Mean return % | Mean Sharpe | Positions | Max concurrent | Funding-incomplete legs |
|---|---|---|---|---|---|---|
| candidate_P28_B60 | 2024 | -37.78 | -1.825 | 725 | 6 | 0 |
| candidate_P28_B60 | oos | -14.81 | -1.311 | 389 | 6 | 0 |
| candidate_P28_B60 | 2023 | -28.57 | -1.516 | 632 | 6 | 6 |
| candidate_P28_B60 | 2025H1 | -14.60 | -1.365 | 322 | 6 | 0 |
| donchian_breakout_B60 | 2024 | -42.16 | -2.037 | 850 | 6 | 0 |
| donchian_breakout_B60 | oos | -20.43 | -1.722 | 449 | 6 | 0 |
| donchian_breakout_B60 | 2023 | -32.40 | -1.697 | 723 | 6 | 6 |
| donchian_breakout_B60 | 2025H1 | -17.56 | -1.402 | 415 | 6 | 0 |
| chaikin_money_flow_breakout_B60 | 2024 | -38.14 | -1.849 | 728 | 6 | 0 |
| chaikin_money_flow_breakout_B60 | oos | -17.10 | -1.555 | 364 | 6 | 0 |
| chaikin_money_flow_breakout_B60 | 2023 | -25.25 | -1.368 | 611 | 6 | 6 |
| chaikin_money_flow_breakout_B60 | 2025H1 | -17.32 | -1.402 | 352 | 6 | 0 |
| volume_weighted | 2024 | -23.36 | -1.037 | 700 | 6 | 0 |
| volume_weighted | oos | -11.67 | -1.101 | 414 | 6 | 0 |
| volume_weighted | 2023 | -27.88 | -1.730 | 781 | 6 | 6 |
| volume_weighted | 2025H1 | -17.99 | -1.454 | 350 | 6 | 0 |

`2023` starts before the Hyperliquid funding history (first record 2023-05-12), so a position held before then has unpriced hours; `2025H1` overlaps the training window by 21 days. Neither enters the verdict.

## Verdict checks

1. coverage: ok `{"incomplete_arms": []}`
2. independent_positions: ok `{"minimum": 30, "positions": 1114}`
3. positive_net_return_x1_and_x2: not met `{"x1_mean_return_pct": -26.2942, "x2_mean_return_pct": -31.55}`
4. beats_each_comparator: not met `{"beats": {"chaikin_money_flow_breakout": true, "donchian_breakout": true, "volume_weighted": false}}`

## Volume and liquidity (selected candidate)

| Window and dataset | Bars | Volume p10 | Volume p50 | Zero-volume bars | Invalid bars after warm-up | Entries | Entries on a bottom-10% volume signal bar |
|---|---|---|---|---|---|---|---|
| 2024|BTC/USDT 1h | 8784 | 0.1393 | 1.0289 | 0 | 0 | 191 | 0 |
| 2024|BTC/USDT 4h | 2196 | 1.1918 | 5.3844 | 0 | 0 | 63 | 0 |
| 2024|ETH/USDT 1h | 8784 | 1.1102 | 7.8620 | 0 | 0 | 171 | 0 |
| 2024|ETH/USDT 4h | 2196 | 9.8840 | 38.9453 | 0 | 0 | 49 | 0 |
| 2024|SOL/USDT 1h | 8784 | 29.4288 | 212.6420 | 0 | 0 | 199 | 2 |
| 2024|SOL/USDT 4h | 2196 | 270.6845 | 1159.0820 | 0 | 0 | 52 | 0 |
| oos|BTC/USDT 1h | 5088 | 0.0740 | 0.5194 | 0 | 0 | 112 | 1 |
| oos|BTC/USDT 4h | 1272 | 0.6823 | 3.4373 | 0 | 0 | 29 | 0 |
| oos|ETH/USDT 1h | 5088 | 1.3296 | 10.3125 | 0 | 0 | 102 | 0 |
| oos|ETH/USDT 4h | 1272 | 12.1027 | 51.3698 | 0 | 0 | 23 | 0 |
| oos|SOL/USDT 1h | 5088 | 9.4153 | 66.2240 | 2 | 56 | 101 | 1 |
| oos|SOL/USDT 4h | 1272 | 101.7333 | 384.2585 | 0 | 0 | 22 | 0 |
| 2023|BTC/USDT 1h | 8753 | 2.1297 | 11.9779 | 0 | 61 | 170 | 1 |
| 2023|BTC/USDT 4h | 2189 | 12.7287 | 49.9124 | 0 | 61 | 42 | 0 |
| 2023|ETH/USDT 1h | 8753 | 3.8369 | 45.4749 | 0 | 61 | 159 | 1 |
| 2023|ETH/USDT 4h | 2189 | 27.2878 | 182.5138 | 0 | 61 | 45 | 0 |
| 2023|SOL/USDT 1h | 8753 | 46.8780 | 450.4500 | 23 | 615 | 166 | 1 |
| 2023|SOL/USDT 4h | 2189 | 431.2880 | 2413.4100 | 0 | 61 | 50 | 0 |
| 2025H1|BTC/USDT 1h | 4344 | 0.0361 | 0.2446 | 0 | 0 | 92 | 2 |
| 2025H1|BTC/USDT 4h | 1086 | 0.3580 | 1.4779 | 0 | 0 | 25 | 0 |
| 2025H1|ETH/USDT 1h | 4344 | 0.4827 | 4.7759 | 0 | 0 | 76 | 0 |
| 2025H1|ETH/USDT 4h | 1086 | 6.4163 | 31.6406 | 0 | 0 | 15 | 0 |
| 2025H1|SOL/USDT 1h | 4344 | 7.9806 | 66.9185 | 1 | 28 | 90 | 0 |
| 2025H1|SOL/USDT 4h | 1086 | 91.3685 | 423.5050 | 0 | 0 | 24 | 1 |

## Errors

0 leg error(s).

## Limits

- One fixed close ladder and one stop multiplier; other exits were not studied.
- Every leg starts cold at its window start, so early bars of each window cannot enter.
- A positive result on this proxy path could not pass by rule; it would need a venue-data study with lot and minimum rules.
