# Bitcoin lead-lag entries: frozen study report (#1640)

Verdict: **FAIL** (the primary study misses a pass condition). Registry: `bitcoin_lead_lag` stays `no_edge` and `production_refused`; edge_source `study_fail`.

Every value below comes from `results.json`, which `run_study.py run` writes and `run_study.py --render-only` renders. The protocol is `candidate_spec.json` (sha256 `41a2b54b5e4c83e89c0920fa9b2cbdc213d73ab4733ba39bb8e31a88cac23360`). Research only: nothing here changes live defaults, deployed configuration or admission.

## Reproduce

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/bitcoin_lead_lag_1640_manifest_1h.json
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/bitcoin_lead_lag_1640_manifest_4h.json
uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py --protocol-hash
uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py run --workers 4
uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py --render-only
```

## Inputs

- `primary_1h`: `backtest/candidates/bitcoin_lead_lag_1640_manifest_1h.json` (proxy: Binance US spot BTC/ETH/SOL-USDT 1h candles (ccxt fetch_ohlcv, closed bars) as a Hyperliquid perpetual price proxy; Hyperliquid perpetual hourly funding; Hyperliquid venue fees, per-coin half spread from one stored L2 snapshot, slippage, szDecimals lots and the $10 minimum); interval 1h, warm-up 600 bars, windows train [2024-09-01, 2025-09-01), test [2025-09-01, 2026-09-01); costs {"maker_fee_pct": 0.00015, "min_notional_margin": 0.03, "min_notional_usd": 10.0, "slippage_bps": 5.0, "taker_fee_pct": 0.00045}.
  - BTC 1h: candles `d107c60f72363372d03273ba6439ca42a0d867a9089c5de171e9306af3ba2d17`, funding `520fd979d5594b4c2fb950cd52aa327749a124d9971a59ec3419752dd24a27f8`, szDecimals 5, half spread 0.06 bps
  - ETH 1h: candles `59d08735aecb22b63bab8189c7c32b11101e80da42b2ec724f39e308548b7913`, funding `4b7694d8fc26cb7245ffa882cbdcb2d571d17f3a9f1abd6832fb0deed4985b41`, szDecimals 4, half spread 0.19 bps
  - SOL 1h: candles `e4e5c15ffcce696253d78dc3b3c3448725e8cf3a0114a9fbc4b1df5f2a0efeec`, funding `b29de7625f7484f0971336e31ba52f5b042127e4eef4d8ee1e4554994383cd2b`, szDecimals 2, half spread 0.42 bps
- `secondary_4h`: `backtest/candidates/bitcoin_lead_lag_1640_manifest_4h.json` (venue: Hyperliquid mainnet perpetual candles from the public info API (candleSnapshot); closed bars only); interval 4h, warm-up 300 bars, windows train [2024-09-01, 2025-09-01), test [2025-09-01, 2026-09-01); costs {"maker_fee_pct": 0.00015, "min_notional_margin": 0.03, "min_notional_usd": 10.0, "slippage_bps": 5.0, "taker_fee_pct": 0.00045}.
  - BTC 4h: candles `89f84288d462b830ceba8108ea9d69db4d3dac417c730dc9cdd13271c10fd0f8`, funding `b961391682a15a619fe131abedcc638fd2c311af7cd30a67cf4af32754cabc48`, szDecimals 5, half spread 0.06 bps
  - ETH 4h: candles `a17523ecbb30f7b594aa80002b3c5ad80b73a7bdd8122d5512683585a8eec3ae`, funding `1b541abb8801a444cac4e23ebe31d71cbd7dc3b7d185456830daca1c09c6ea66`, szDecimals 4, half spread 0.19 bps
  - SOL 4h: candles `5ca4fa74940f38f180a85eb81ccaa96bb10f08988d1351c280abb2ce175fdd4c`, funding `a498781bca4cf017f7f772f5a0c00c0853979bacfd147303394f63a63da5af40`, szDecimals 2, half spread 0.42 bps

Source sha256 at run time:

- `backtest/backtester.py`: `5eb522bcc62094c25ae0b1c644f2e2fb301c8a65c09f5f29d823cd7876edad02`
- `backtest/candidates/bitcoin_lead_lag_1640/candidate_spec.json`: `1cfe342e934a70c5a7671c739bc5563cd19515af3e87f56520e7671a14719ffb`
- `backtest/candidates/bitcoin_lead_lag_1640/run_study.py`: `34cd8ae006b16e193dd64ca030ba16a02490dfae7ed99205c988471caca06bf6`
- `backtest/candidates/bitcoin_lead_lag_1640_manifest_1h.json`: `11670caa616ecafb04638882a0a25e98c7a65418fef74597240ab8995b76ca10`
- `backtest/candidates/bitcoin_lead_lag_1640_manifest_4h.json`: `a57ae56e2b9eb449b8d9c71294a08bea2360d35ee0c7c3976d603580d21cc150`
- `backtest/eval_windows.py`: `14a220ce02a5751e577be82564feb313eb1055c7869b63ee76cd3d935db73081`
- `backtest/lead_lag_history.py`: `a31da5eb098147c318c5ffe0bfc5f02746d47eab2e9b8299f0d67061bf9f4537`
- `backtest/offline_manifest.py`: `35c36f89c6c375e20d8488e55e6145e4a0cb396ab1000506ae96df44c451a337`
- `shared_strategies/open/bitcoin_lead_lag.py`: `d071348e7f03a6a23d01e8e47ddd9ab182f90f4fc70d33c6c5ee1a483b0656c1`
- `shared_strategies/open/registry.py`: `e22aac03ee4e09a3f4f59821cc84c6b643767c0c02a3594d2ee14c686f29adcb`
- `shared_tools/closed_bar.py`: `9df5675bcb3ca29eb51803509744e077b8205a80a73e6ead58aafac277daf3d9`
- `shared_tools/lead_lag_inputs.py`: `78ca309e1d34bdc2a6ab83f5b6c1746a1c67ba54206b728ba4bf99401bc14c3d`
- `shared_tools/market_payload.py`: `f5c3fbc436a55fcbc67f1d66324a64982475e9f234a228c5373e7a8277e7112f`
- `shared_tools/strategy_composition.py`: `a589469d1e0e62ae60d3947b5f30a806d73296dcb54fdbed303618c3bdca6bd7`

Limitations: the 1h study prices a Binance US spot proxy for the Hyperliquid perpetual; the spread comes from one stored L2 snapshot; slippage is a fixed 5 bps per side; lot flooring and the $10 minimum come from the manifest execution spec. Funding is the stored Hyperliquid hourly series charged as a cost. Fills happen at the next bar open; latency rows delay the fill by whole bars. The windows were used by earlier studies, so this is retrospective evidence.

## primary_1h

Study verdict: **fail**.

- miss: base-cost mean net return -9.125% is not above 0
- miss: stress-cost mean net return -13.65% is not above 0

Coverage gate: complete; largest support 498 bars.

| dataset window | expected | present | missing | warm-up missing | funding hours missing |
|---|---|---|---|---|---|
| BTC 1h test | 8760 | 8751 | 9 | 0 | 0 |
| BTC 1h train | 8760 | 8760 | 0 | 0 | 0 |
| ETH 1h test | 8760 | 8751 | 9 | 0 | 0 |
| ETH 1h train | 8760 | 8760 | 0 | 0 | 0 |
| SOL 1h test | 8760 | 8751 | 9 | 0 | 0 |
| SOL 1h train | 8760 | 8760 | 0 | 0 | 0 |

### Selections (train)

| arm | selected params | mean Sharpe | mean net % | positions | eligible grid points |
|---|---|---|---|---|---|
| candidate | {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -0.084 | -2.08 | 65 | 90 of 144 |
| follower_momentum | {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -2.662 | -62.00 | 531 | 36 of 36 |
| btc_trend_filter | {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -2.119 | -52.67 | 433 | 36 of 36 |

### Held-out (test)

| arm | cost | mean net % | mean Sharpe | worst DD % | positions | long/short | clusters | turnover (ETH, SOL) | funding USD (ETH, SOL) |
|---|---|---|---|---|---|---|---|---|---|
| candidate | base | -9.12 | -0.540 | -19.00 | 125 | 55/70 | 90 | 98.96, 145.76 | 0.08, -1.32 |
| candidate | stress x2 | -13.65 | -0.881 | -21.99 | 126 | 55/71 | 91 | 95.05, 144.44 | 0.42, -1.29 |
| follower_momentum | base | -69.95 | -3.808 | -72.18 | 550 | 251/299 | 349 | 369.18, 342.42 | -0.33, -3.92 |
| follower_momentum | stress x2 | -75.78 | -4.417 | -77.13 | 552 | 252/300 | 349 | 325.28, 310.59 | -0.53, -3.13 |
| btc_trend_filter | base | -60.72 | -3.187 | -62.11 | 477 | 199/278 | 310 | 333.30, 335.30 | -0.40, -4.69 |
| btc_trend_filter | stress x2 | -65.53 | -3.570 | -66.16 | 478 | 199/279 | 311 | 315.12, 317.47 | -0.52, -4.07 |

Per follower (candidate, base cost):

- ETH: net -11.22%, Sharpe -0.659, max DD -19.00%, positions 52 (22 long, 30 short), fees $38.03, funding $0.08, funding coverage complete True, leader rows matched 9351 of 9351
- SOL: net -7.03%, Sharpe -0.422, max DD -17.69%, positions 73 (33 long, 40 short), fees $54.85, funding $-1.32, funding coverage complete True, leader rows matched 9351 of 9351

Concentration: {"largest_gain_usd": 23.542233, "largest_loss_usd": -25.070241, "net_pnl_usd": -182.574348, "positions": 125, "top5_share_of_gains": 0.156247, "win_rate": 0.472}

Latency decay (candidate, base cost):

| extra bars | mean net % | mean Sharpe | positions |
|---|---|---|---|
| 0 | -9.12 | -0.540 | 125 |
| 1 | -18.93 | -1.306 | 116 |
| 2 | -21.98 | -1.540 | 116 |

Parameter stability: 0 of 7 one-step train neighbors have a positive mean train return (mean -14.37%); the train/test Spearman rank correlation of mean net return over the 144 candidate grid points is 0.741; 16.0% of grid points are net positive on the held-out window.

Decision parity on the held-out window: clean.
- ETH: 231 sampled rows (55 entries), 230 sealed-payload composer checks, tail mismatches 0, sealed mismatches 0, duplicate event ids 0
- SOL: 249 sampled rows (74 entries), 248 sealed-payload composer checks, tail mismatches 0, sealed mismatches 0, duplicate event ids 0

M1 incumbent comparison (report only): candidate mean Sharpe -0.540 vs incumbent median bar -0.292; mean ddadj -0.494 vs -0.334; M1 verdict `fail`.

### Every candidate grid point on the held-out window (base cost)

| params | mean net % | mean Sharpe | positions | worst DD % |
|---|---|---|---|---|
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -37.82 | -2.188 | 233 | -45.09 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -34.65 | -2.169 | 194 | -41.14 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -24.75 | -1.956 | 90 | -33.47 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -19.41 | -1.608 | 80 | -26.19 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -26.61 | -1.711 | 165 | -33.85 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -21.73 | -1.452 | 133 | -28.02 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -16.73 | -1.623 | 56 | -22.47 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -12.16 | -1.174 | 51 | -16.81 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -14.72 | -1.200 | 103 | -28.01 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -11.84 | -1.033 | 83 | -23.94 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -7.79 | -1.100 | 35 | -11.10 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -5.63 | -0.852 | 32 | -9.77 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -19.48 | -1.062 | 191 | -28.41 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -21.62 | -1.300 | 152 | -29.88 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 0.15 | 0.043 | 10 | -4.98 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -1.14 | -0.248 | 7 | -4.52 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -9.12 | -0.540 | 125 | -19.00 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -12.65 | -0.853 | 93 | -22.50 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 0.37 | 0.094 | 8 | -3.18 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -0.46 | -0.095 | 6 | -3.19 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -5.04 | -0.358 | 80 | -16.80 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -8.71 | -0.744 | 60 | -16.18 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | -0.23 | -0.068 | 5 | -2.15 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -0.71 | -0.245 | 4 | -2.15 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -50.48 | -2.994 | 263 | -51.70 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -48.01 | -2.984 | 228 | -51.99 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -19.09 | -1.472 | 94 | -30.96 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -18.87 | -1.552 | 84 | -28.21 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -32.19 | -2.037 | 166 | -40.30 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -28.45 | -1.827 | 154 | -39.05 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -14.14 | -1.370 | 57 | -17.38 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -12.55 | -1.236 | 57 | -17.37 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -19.54 | -1.329 | 111 | -30.11 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -18.46 | -1.275 | 104 | -30.04 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -4.69 | -0.651 | 29 | -11.58 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -4.70 | -0.643 | 29 | -11.58 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -40.02 | -2.244 | 286 | -43.97 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -37.54 | -2.110 | 253 | -42.84 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | -1.33 | -0.309 | 16 | -8.20 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -2.46 | -0.427 | 15 | -10.44 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -17.52 | -1.010 | 182 | -21.96 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -14.83 | -0.867 | 158 | -20.52 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 1.42 | 0.152 | 10 | -4.55 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | 1.42 | 0.152 | 10 | -4.55 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -4.55 | -0.244 | 115 | -16.09 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -4.60 | -0.242 | 106 | -16.14 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 2.40 | 0.342 | 9 | -4.55 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | 2.40 | 0.342 | 9 | -4.55 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -29.57 | -2.066 | 160 | -37.34 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -32.06 | -2.386 | 156 | -39.88 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -2.10 | -0.298 | 20 | -7.91 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -2.10 | -0.298 | 20 | -7.91 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -16.77 | -1.240 | 102 | -29.27 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -18.66 | -1.515 | 98 | -32.12 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 0.26 | 0.033 | 9 | -7.91 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 0.26 | 0.033 | 9 | -7.91 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -9.34 | -0.817 | 61 | -20.09 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -11.23 | -1.141 | 60 | -23.63 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -0.20 | -0.033 | 6 | -7.71 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -0.20 | -0.033 | 6 | -7.71 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -22.77 | -1.470 | 156 | -31.87 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -22.05 | -1.439 | 153 | -34.54 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 0.82 | 0.081 | 25 | -6.83 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -0.93 | -0.090 | 23 | -8.47 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -14.93 | -1.075 | 104 | -21.94 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -16.02 | -1.179 | 101 | -23.27 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 2.67 | 0.505 | 16 | -5.14 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | 0.99 | 0.333 | 14 | -6.90 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -6.80 | -0.516 | 69 | -16.68 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -9.78 | -0.840 | 66 | -16.12 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 3.56 | 0.686 | 10 | -4.85 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | 1.79 | 0.499 | 10 | -4.77 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -38.11 | -1.954 | 251 | -41.46 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -43.57 | -2.740 | 221 | -49.21 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -22.11 | -1.550 | 105 | -28.57 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -20.66 | -1.718 | 95 | -27.72 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -27.32 | -1.604 | 170 | -32.75 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -26.74 | -1.742 | 143 | -32.97 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -11.52 | -1.080 | 67 | -21.09 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -9.72 | -0.980 | 60 | -20.95 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -16.91 | -1.161 | 113 | -25.11 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -18.97 | -1.460 | 99 | -27.22 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -4.01 | -0.423 | 44 | -12.63 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -4.33 | -0.482 | 41 | -12.59 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -28.88 | -1.922 | 211 | -33.27 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -29.66 | -2.288 | 158 | -34.54 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | -7.75 | -1.183 | 43 | -16.07 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -6.95 | -1.183 | 32 | -12.26 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -11.44 | -0.838 | 132 | -21.34 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -14.18 | -1.182 | 102 | -24.33 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | -1.91 | -0.868 | 24 | -5.88 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -1.40 | -0.736 | 20 | -4.35 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -2.38 | -0.163 | 88 | -13.31 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -8.24 | -0.815 | 67 | -17.07 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 1.37 | -0.214 | 14 | -3.03 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | 0.42 | -0.317 | 13 | -2.84 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -44.20 | -2.478 | 260 | -44.99 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -41.88 | -2.297 | 237 | -43.02 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -18.43 | -1.409 | 90 | -25.37 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -17.68 | -1.355 | 84 | -22.73 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -26.75 | -1.567 | 171 | -30.24 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -25.68 | -1.501 | 152 | -29.48 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -9.34 | -0.752 | 60 | -15.18 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -10.64 | -0.889 | 54 | -16.95 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -8.70 | -0.555 | 110 | -17.70 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -8.56 | -0.541 | 99 | -19.06 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -0.73 | -0.032 | 33 | -11.02 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -1.85 | -0.147 | 31 | -12.02 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -23.41 | -1.075 | 253 | -39.86 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -22.86 | -1.050 | 236 | -38.41 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | -7.59 | -0.772 | 65 | -15.19 |
| {"horizon_bars": 4, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -6.12 | -0.650 | 62 | -15.29 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -8.37 | -0.342 | 172 | -22.95 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -7.11 | -0.280 | 154 | -24.53 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | -1.49 | -0.120 | 45 | -10.21 |
| {"horizon_bars": 4, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -1.61 | -0.199 | 43 | -10.28 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | 2.02 | 0.204 | 113 | -16.15 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | 1.17 | 0.160 | 105 | -17.64 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 2.60 | 0.337 | 30 | -6.75 |
| {"horizon_bars": 4, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | 1.90 | 0.299 | 29 | -6.75 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -27.59 | -2.022 | 155 | -31.35 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -24.14 | -1.777 | 146 | -27.60 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -2.84 | -0.762 | 29 | -10.24 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -1.68 | -0.462 | 28 | -9.77 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -20.52 | -1.639 | 105 | -28.46 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -14.90 | -1.086 | 102 | -27.05 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -0.32 | -0.313 | 14 | -6.75 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -0.32 | -0.313 | 14 | -6.75 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -13.82 | -1.214 | 71 | -22.46 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -10.17 | -0.792 | 67 | -22.72 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 1.46 | 0.063 | 5 | -2.88 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 1.46 | 0.063 | 5 | -2.88 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -15.72 | -0.881 | 173 | -28.07 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -13.53 | -0.788 | 164 | -25.99 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | -2.65 | -0.338 | 27 | -13.57 |
| {"horizon_bars": 8, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -3.29 | -0.691 | 26 | -13.57 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -10.14 | -0.643 | 120 | -21.26 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -9.48 | -0.620 | 112 | -21.79 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | -0.94 | -0.161 | 19 | -8.53 |
| {"horizon_bars": 8, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | -1.59 | -0.465 | 18 | -8.53 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 480} | -4.97 | -0.453 | 75 | -18.16 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 480} | -3.90 | -0.387 | 67 | -16.60 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 480} | 0.83 | 0.083 | 10 | -6.75 |
| {"horizon_bars": 8, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 480} | 0.18 | -0.228 | 9 | -6.75 |

## secondary_4h

Study verdict: **inconclusive**.

- inconclusive: 13 held-out position clusters, below 30
- miss: base-cost mean net return -0.62% is not above 0
- miss: stress-cost mean net return -5.9% is not above 0

Coverage gate: complete; largest support 248 bars.

| dataset window | expected | present | missing | warm-up missing | funding hours missing |
|---|---|---|---|---|---|
| BTC 4h test | 2190 | 2190 | 0 | 0 | 0 |
| BTC 4h train | 2190 | 2190 | 0 | 0 | 0 |
| ETH 4h test | 2190 | 2190 | 0 | 0 | 0 |
| ETH 4h train | 2190 | 2190 | 0 | 0 | 0 |
| SOL 4h test | 2190 | 2190 | 0 | 0 | 0 |
| SOL 4h train | 2190 | 2190 | 0 | 0 | 0 |

### Selections (train)

| arm | selected params | mean Sharpe | mean net % | positions | eligible grid points |
|---|---|---|---|---|---|
| candidate | {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 0.504 | 6.64 | 37 | 110 of 144 |
| follower_momentum | {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -2.111 | -52.30 | 120 | 36 of 36 |
| btc_trend_filter | {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -1.909 | -45.91 | 108 | 36 of 36 |

### Held-out (test)

| arm | cost | mean net % | mean Sharpe | worst DD % | positions | long/short | clusters | turnover (ETH, SOL) | funding USD (ETH, SOL) |
|---|---|---|---|---|---|---|---|---|---|
| candidate | base | -0.62 | -0.053 | -8.76 | 21 | 8/13 | 13 | 26.30, 15.56 | -0.50, -1.61 |
| candidate | stress x2 | -5.90 | -0.501 | -11.74 | 21 | 8/13 | 13 | 24.05, 15.55 | -1.25, -1.61 |
| follower_momentum | base | -32.53 | -1.020 | -52.38 | 144 | 61/83 | 88 | 136.44, 107.27 | -0.76, -5.90 |
| follower_momentum | stress x2 | -34.61 | -1.113 | -53.40 | 143 | 60/83 | 87 | 135.24, 104.97 | -0.50, -5.98 |
| btc_trend_filter | base | -29.43 | -0.893 | -45.26 | 138 | 58/80 | 88 | 129.36, 106.13 | -1.10, -6.35 |
| btc_trend_filter | stress x2 | -31.59 | -0.982 | -46.32 | 137 | 57/80 | 87 | 128.25, 103.72 | -0.83, -6.42 |

Per follower (candidate, base cost):

- ETH: net 5.19%, Sharpe 0.465, max DD -7.58%, positions 13 (6 long, 7 short), fees $9.74, funding $-0.50, funding coverage complete True, leader rows matched 2490 of 2490
- SOL: net -6.43%, Sharpe -0.572, max DD -8.76%, positions 8 (2 long, 6 short), fees $6.15, funding $-1.61, funding coverage complete True, leader rows matched 2490 of 2490

Concentration: {"largest_gain_usd": 48.317875, "largest_loss_usd": -32.779636, "net_pnl_usd": -12.381551, "positions": 21, "top5_share_of_gains": 0.630264, "win_rate": 0.47619}

Latency decay (candidate, base cost):

| extra bars | mean net % | mean Sharpe | positions |
|---|---|---|---|
| 0 | -0.62 | -0.053 | 21 |
| 1 | 3.45 | 0.403 | 23 |
| 2 | -1.39 | -0.080 | 21 |

Parameter stability: 2 of 7 one-step train neighbors have a positive mean train return (mean -11.73%); the train/test Spearman rank correlation of mean net return over the 144 candidate grid points is 0.288; 11.8% of grid points are net positive on the held-out window.

Decision parity on the held-out window: clean.
- ETH: 60 sampled rows (16 entries), 60 sealed-payload composer checks, tail mismatches 0, sealed mismatches 0, duplicate event ids 0
- SOL: 55 sampled rows (11 entries), 55 sealed-payload composer checks, tail mismatches 0, sealed mismatches 0, duplicate event ids 0

M1 incumbent comparison (report only): candidate mean Sharpe -0.053 vs incumbent median bar -0.109; mean ddadj -0.024 vs -0.175; M1 verdict `pass`.

### Every candidate grid point on the held-out window (base cost)

| params | mean net % | mean Sharpe | positions | worst DD % |
|---|---|---|---|---|
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -34.07 | -1.262 | 107 | -47.91 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -32.88 | -1.377 | 86 | -48.85 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -28.08 | -1.279 | 67 | -38.68 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -28.18 | -1.412 | 57 | -41.16 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -20.57 | -0.745 | 72 | -34.64 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -22.77 | -0.936 | 64 | -38.95 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -16.37 | -0.739 | 38 | -27.19 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -18.92 | -0.946 | 36 | -31.36 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -14.64 | -0.607 | 49 | -31.70 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -13.23 | -0.552 | 46 | -30.20 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -12.04 | -0.645 | 25 | -25.38 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -11.89 | -0.683 | 25 | -25.38 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -42.26 | -1.695 | 143 | -51.18 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -34.37 | -1.538 | 101 | -45.27 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -35.24 | -1.689 | 75 | -40.72 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -31.15 | -1.602 | 63 | -38.70 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -34.55 | -1.770 | 91 | -42.48 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -31.55 | -1.722 | 68 | -39.68 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -25.34 | -1.530 | 45 | -32.52 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -24.96 | -1.538 | 41 | -32.52 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -16.03 | -0.929 | 47 | -26.74 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -16.20 | -0.988 | 40 | -27.14 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -10.44 | -0.766 | 25 | -15.65 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -13.31 | -0.972 | 24 | -18.29 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -21.18 | -0.943 | 72 | -41.91 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -19.96 | -0.941 | 63 | -36.47 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -11.00 | -0.573 | 36 | -26.37 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -14.06 | -0.825 | 32 | -26.37 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -10.80 | -0.482 | 38 | -25.18 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -8.86 | -0.413 | 33 | -20.96 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -4.38 | -0.209 | 16 | -15.47 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -4.86 | -0.262 | 15 | -15.47 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -8.64 | -0.497 | 24 | -21.23 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -9.18 | -0.534 | 23 | -20.96 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -4.28 | -0.297 | 11 | -15.48 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -4.28 | -0.297 | 11 | -15.48 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -38.48 | -2.145 | 91 | -53.42 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -29.18 | -1.681 | 74 | -42.10 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -23.93 | -1.542 | 35 | -29.99 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -22.03 | -1.413 | 33 | -27.41 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -21.98 | -1.405 | 54 | -31.82 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -17.09 | -1.231 | 43 | -24.63 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -11.67 | -0.874 | 22 | -18.80 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -11.67 | -0.874 | 22 | -18.80 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -10.62 | -0.944 | 26 | -22.55 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -11.09 | -1.187 | 20 | -19.31 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -7.17 | -0.732 | 10 | -11.99 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -7.17 | -0.732 | 10 | -11.99 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -31.72 | -1.395 | 84 | -45.16 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -30.36 | -1.367 | 75 | -41.49 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -25.87 | -1.294 | 46 | -37.53 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -27.78 | -1.507 | 42 | -37.52 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -17.53 | -0.820 | 50 | -29.99 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -15.67 | -0.717 | 48 | -29.99 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -13.97 | -0.816 | 27 | -24.16 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -12.84 | -0.739 | 26 | -24.16 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -15.35 | -0.891 | 33 | -24.01 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -14.71 | -0.857 | 32 | -24.01 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -12.29 | -0.803 | 20 | -21.80 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -12.34 | -0.818 | 20 | -21.80 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -25.25 | -1.324 | 100 | -41.73 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -22.93 | -1.139 | 92 | -37.13 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -17.61 | -1.222 | 27 | -26.84 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -9.79 | -0.549 | 24 | -26.84 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -10.27 | -0.486 | 62 | -24.57 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -11.49 | -0.560 | 58 | -25.96 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -2.42 | -0.075 | 17 | -16.68 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -2.42 | -0.075 | 17 | -16.68 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -1.16 | -0.029 | 31 | -12.05 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -3.02 | -0.150 | 29 | -12.19 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 4.34 | 0.442 | 7 | -6.32 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 1, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 4.34 | 0.442 | 7 | -6.32 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -7.53 | -0.887 | 21 | -13.41 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -1.02 | -0.358 | 8 | -8.53 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -6.53 | -1.464 | 6 | -8.95 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -3.33 | -1.000 | 3 | -6.18 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -3.22 | -0.353 | 17 | -11.46 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -0.45 | -0.251 | 7 | -7.41 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -5.94 | -1.332 | 5 | -8.95 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -2.75 | -0.868 | 2 | -5.77 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -3.90 | -0.655 | 9 | -7.38 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -1.18 | -0.284 | 3 | -5.77 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -2.77 | -0.757 | 2 | -5.77 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -1.47 | -0.368 | 1 | -5.77 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -13.49 | -1.042 | 25 | -26.85 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -11.84 | -1.060 | 17 | -22.27 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 1.22 | 0.102 | 10 | -11.27 |
| {"horizon_bars": 1, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -0.92 | -0.107 | 8 | -10.10 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -3.28 | -0.299 | 21 | -16.94 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -8.41 | -0.662 | 16 | -20.80 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 1.95 | 0.305 | 8 | -10.10 |
| {"horizon_bars": 1, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 1.95 | 0.305 | 8 | -10.10 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -2.54 | -0.201 | 13 | -16.28 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -5.91 | -0.496 | 11 | -17.59 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 1.16 | 0.290 | 5 | -10.10 |
| {"horizon_bars": 1, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 1.16 | 0.290 | 5 | -10.10 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -17.68 | -0.935 | 73 | -24.51 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -14.11 | -0.736 | 64 | -21.99 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -9.78 | -0.677 | 34 | -19.92 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -9.96 | -0.688 | 34 | -20.04 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -5.22 | -0.272 | 47 | -14.86 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -3.53 | -0.163 | 45 | -13.40 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -3.68 | -0.201 | 28 | -14.92 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -3.71 | -0.203 | 28 | -14.98 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -6.33 | -0.441 | 33 | -12.97 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -5.62 | -0.396 | 32 | -12.07 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -6.83 | -0.502 | 21 | -14.28 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -6.87 | -0.504 | 21 | -14.75 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -23.73 | -1.417 | 57 | -36.20 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -24.29 | -1.458 | 55 | -39.32 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | -3.24 | -0.257 | 21 | -8.57 |
| {"horizon_bars": 2, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | -0.62 | -0.053 | 21 | -8.76 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -12.07 | -0.676 | 43 | -31.54 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -12.29 | -0.722 | 39 | -29.92 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 3.06 | 0.267 | 17 | -6.42 |
| {"horizon_bars": 2, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 3.06 | 0.267 | 17 | -6.42 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -6.71 | -0.419 | 25 | -21.28 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -6.52 | -0.398 | 25 | -22.11 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 1.85 | 0.142 | 13 | -7.65 |
| {"horizon_bars": 2, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 1.85 | 0.142 | 13 | -7.65 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -25.07 | -0.991 | 93 | -30.83 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -23.32 | -0.884 | 87 | -29.91 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -10.62 | -0.655 | 40 | -17.17 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -9.37 | -0.586 | 38 | -17.17 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -12.68 | -0.557 | 65 | -18.92 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -13.30 | -0.566 | 64 | -21.94 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -6.96 | -0.460 | 29 | -13.50 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -5.96 | -0.386 | 29 | -13.50 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 120} | -18.46 | -1.024 | 50 | -25.69 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 120} | -20.14 | -1.065 | 50 | -24.84 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 120} | -11.55 | -1.255 | 18 | -15.92 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 120} | -10.51 | -1.123 | 18 | -15.74 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -1.92 | -0.089 | 73 | -19.22 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -4.20 | -0.210 | 65 | -20.85 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 1.98 | 0.230 | 21 | -6.78 |
| {"horizon_bars": 3, "impulse_z": 1.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 5.07 | 0.523 | 19 | -6.78 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -5.42 | -0.233 | 45 | -18.19 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -2.12 | -0.040 | 43 | -15.50 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 7.09 | 0.697 | 16 | -6.78 |
| {"horizon_bars": 3, "impulse_z": 2.0, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 7.09 | 0.697 | 16 | -6.78 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.0, "training_bars": 240} | -4.50 | -0.232 | 32 | -17.85 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.0, "training_bars": 240} | -3.14 | -0.129 | 31 | -16.84 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 0.0, "min_r2": 0.01, "training_bars": 240} | 5.59 | 0.571 | 10 | -5.05 |
| {"horizon_bars": 3, "impulse_z": 2.5, "lag_bars": 2, "min_edge_bps": 10.0, "min_r2": 0.01, "training_bars": 240} | 5.59 | 0.571 | 10 | -5.05 |

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code
