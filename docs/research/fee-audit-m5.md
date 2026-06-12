# Fee-aware selectivity audit (#999 M5)

Registry-wide trade-count x fee-drag screen. Each strategy leg is run twice on the eval_windows.py harness — once with the audit fee model, once with commission and slippage zeroed — to isolate fee drag and apply the salvage test (does a positive *gross* edge exist under the churn?).

## Reproduce

```
uv run --no-sync python backtest/fee_audit.py --registry both --markdown docs/research/fee-audit-m5.md
```

- Generated: 2026-06-12
- Registries: both
- Windows: is (2025-06-10 → 2026-01-01), oos (2026-01-01 → latest)
- Datasets: BTC/USDT 1h, BTC/USDT 4h, ETH/USDT 1h, ETH/USDT 4h, SOL/USDT 1h, SOL/USDT 4h
- Capital: 1000.0
- Fee model: binanceus platform taker fee + 5 bps slippage (net); commission=0 and slippage=0 (gross). Fee drag = mean per-leg (gross - net) return.

Returns are mean per-leg total-return %; trades are summed across all scored legs; trades/yr is annualized over the summed calendar span. **Verdicts:** `deprecate` (gross <= 0, no edge to salvage), `graduate_m1` (gross > 0, net <= 0 — raise selectivity), `healthy` (net > 0), `no_trades` (never fired).

| rank | strategy | reg | trades | trades/yr | gross %/leg | net %/leg | fee drag (pp) | drag/trade (pp) | net Sharpe | verdict |
|-----:|----------|-----|-------:|----------:|------------:|----------:|--------------:|----------------:|-----------:|---------|
| 1 | vwap_reversion | spot | 1467 | 249.7 | -6.21 | -34.59 | 28.39 | 0.0194 | -1.90 | `deprecate` |
| 2 | parabolic_sar | spot | 1301 | 221.5 | -8.22 | -33.26 | 25.04 | 0.0192 | -1.88 | `deprecate` |
| 3 | macd | spot | 1231 | 209.5 | -8.05 | -30.98 | 22.93 | 0.0186 | -1.83 | `deprecate` |
| 4 | triple_ema_bidir | futures | 782 | 133.1 | -8.77 | -25.07 | 16.30 | 0.0208 | -1.46 | `deprecate` |
| 5 | tema_cross_bd | futures | 793 | 135.0 | -5.97 | -22.19 | 16.22 | 0.0205 | -1.07 | `deprecate` |
| 6 | heikin_ashi_ema | spot | 787 | 134.0 | -13.16 | -28.08 | 14.92 | 0.0190 | -1.56 | `deprecate` |
| 7 | stoch_rsi | spot | 652 | 111.0 | -12.01 | -24.96 | 12.95 | 0.0199 | -1.42 | `deprecate` |
| 8 | regime_adaptive | spot | 495 | 84.3 | 3.96 | -8.46 | 12.43 | 0.0251 | -0.94 | `graduate_m1` |
| 9 | ema_crossover | spot | 554 | 94.3 | -10.48 | -22.10 | 11.62 | 0.0210 | -1.40 | `deprecate` |
| 10 | consolidation_range | futures | 610 | 103.8 | -21.82 | -32.93 | 11.11 | 0.0182 | -1.89 | `deprecate` |
| 11 | triple_ema | spot | 507 | 86.3 | -6.12 | -16.98 | 10.87 | 0.0214 | -1.08 | `deprecate` |
| 12 | tema_cross | spot | 444 | 75.6 | 0.59 | -9.71 | 10.30 | 0.0232 | -0.97 | `graduate_m1` |
| 13 | supertrend | spot | 466 | 79.3 | -9.76 | -19.90 | 10.14 | 0.0218 | -1.20 | `deprecate` |
| 14 | mean_reversion | spot | 487 | 82.9 | -17.58 | -26.87 | 9.30 | 0.0191 | -1.51 | `deprecate` |
| 15 | sma_crossover | spot | 402 | 63.6 | -7.35 | -16.64 | 9.29 | 0.0231 | -0.95 | `deprecate` |
| 16 | atr_breakout | spot | 382 | 65.0 | -3.62 | -12.36 | 8.74 | 0.0229 | -1.02 | `deprecate` |
| 17 | donchian_breakout | spot | 354 | 60.3 | -2.01 | -10.53 | 8.52 | 0.0241 | -0.59 | `deprecate` |
| 18 | session_breakout | futures | 364 | 62.0 | -6.23 | -14.36 | 8.13 | 0.0223 | -0.97 | `deprecate` |
| 19 | bollinger_bands | spot | 338 | 57.5 | -11.50 | -18.98 | 7.48 | 0.0221 | -1.07 | `deprecate` |
| 20 | pairs_spread | spot | 367 | 62.5 | -18.37 | -25.23 | 6.86 | 0.0187 | -1.48 | `deprecate` |
| 21 | breakout | futures | 260 | 44.3 | 5.28 | -1.54 | 6.82 | 0.0262 | -0.10 | `graduate_m1` |
| 22 | order_blocks | spot | 288 | 49.0 | -1.63 | -8.37 | 6.74 | 0.0234 | -0.57 | `deprecate` |
| 23 | volume_weighted | spot | 275 | 46.8 | -6.92 | -12.69 | 5.78 | 0.0210 | -0.70 | `deprecate` |
| 24 | chart_pattern | spot | 184 | 31.3 | -3.71 | -8.15 | 4.45 | 0.0242 | -0.49 | `deprecate` |
| 25 | adx_trend | spot | 199 | 33.9 | -19.38 | -23.44 | 4.06 | 0.0204 | -1.21 | `deprecate` |
| 26 | ichimoku_cloud | spot | 171 | 29.1 | -1.67 | -5.21 | 3.54 | 0.0207 | -0.59 | `deprecate` |
| 27 | squeeze_momentum | spot | 131 | 22.3 | -0.29 | -3.69 | 3.40 | 0.0260 | -0.10 | `deprecate` |
| 28 | rsi_macd_combo | spot | 175 | 29.8 | -19.87 | -23.22 | 3.35 | 0.0192 | -1.11 | `deprecate` |
| 29 | funding_skew | futures | 124 | 21.1 | -9.33 | -12.05 | 2.72 | 0.0220 | -0.82 | `deprecate` |
| 30 | liquidity_sweeps | spot | 126 | 21.4 | -16.94 | -19.63 | 2.69 | 0.0213 | -1.07 | `deprecate` |
| 31 | momentum | spot | 127 | 21.6 | -9.30 | -11.95 | 2.65 | 0.0209 | -0.80 | `deprecate` |
| 32 | rsi | spot | 116 | 19.7 | -19.75 | -21.95 | 2.20 | 0.0190 | -1.17 | `deprecate` |
| 33 | vol_momentum | spot | 86 | 14.6 | -0.43 | -2.56 | 2.13 | 0.0248 | -0.57 | `deprecate` |
| 34 | amd_ifvg | spot | 85 | 14.5 | -8.50 | -10.18 | 1.68 | 0.0198 | -0.55 | `deprecate` |
| 35 | mtf_confluence | spot | 63 | 10.7 | -1.14 | -2.58 | 1.45 | 0.0230 | -0.23 | `deprecate` |
| 36 | momentum_pro | spot | 51 | 8.7 | -4.16 | -5.35 | 1.19 | 0.0233 | -0.60 | `deprecate` |
| 37 | regime_adaptive_htf | spot | 37 | 6.3 | 0.27 | -0.66 | 0.94 | 0.0253 | -0.05 | `graduate_m1` |
| 38 | sweep_squeeze_combo | spot | 25 | 4.3 | -6.65 | -7.18 | 0.52 | 0.0209 | -0.52 | `deprecate` |
| 39 | mean_reversion_pro | spot | 18 | 3.1 | -2.73 | -3.13 | 0.40 | 0.0221 | -0.04 | `deprecate` |
| 40 | range_scalper | spot | 11 | 1.9 | -8.29 | -8.47 | 0.17 | 0.0157 | -0.47 | `deprecate` |
| 41 | bear_pullback_st | futures | 0 | 0.0 | 0.00 | 0.00 | 0.00 | — | 0.00 | `no_trades` |
| 42 | delta_neutral_funding | futures | 0 | 0.0 | 0.00 | 0.00 | 0.00 | — | 0.00 | `no_trades` |
| 43 | vwap_rejection_st | futures | 0 | 0.0 | 0.00 | 0.00 | 0.00 | — | 0.00 | `no_trades` |

## Deprecation list (gross edge <= 0 — fee filter cannot save)

- **vwap_reversion** (spot): gross -6.21%, net -34.59%, 1467 trades (249.7/yr)
- **parabolic_sar** (spot): gross -8.22%, net -33.26%, 1301 trades (221.5/yr)
- **macd** (spot): gross -8.05%, net -30.98%, 1231 trades (209.5/yr)
- **triple_ema_bidir** (futures): gross -8.77%, net -25.07%, 782 trades (133.1/yr)
- **tema_cross_bd** (futures): gross -5.97%, net -22.19%, 793 trades (135.0/yr)
- **heikin_ashi_ema** (spot): gross -13.16%, net -28.08%, 787 trades (134.0/yr)
- **stoch_rsi** (spot): gross -12.01%, net -24.96%, 652 trades (111.0/yr)
- **ema_crossover** (spot): gross -10.48%, net -22.10%, 554 trades (94.3/yr)
- **consolidation_range** (futures): gross -21.82%, net -32.93%, 610 trades (103.8/yr)
- **triple_ema** (spot): gross -6.12%, net -16.98%, 507 trades (86.3/yr)
- **supertrend** (spot): gross -9.76%, net -19.90%, 466 trades (79.3/yr)
- **mean_reversion** (spot): gross -17.58%, net -26.87%, 487 trades (82.9/yr)
- **sma_crossover** (spot): gross -7.35%, net -16.64%, 402 trades (63.6/yr)
- **atr_breakout** (spot): gross -3.62%, net -12.36%, 382 trades (65.0/yr)
- **donchian_breakout** (spot): gross -2.01%, net -10.53%, 354 trades (60.3/yr)
- **session_breakout** (futures): gross -6.23%, net -14.36%, 364 trades (62.0/yr)
- **bollinger_bands** (spot): gross -11.50%, net -18.98%, 338 trades (57.5/yr)
- **pairs_spread** (spot): gross -18.37%, net -25.23%, 367 trades (62.5/yr)
- **order_blocks** (spot): gross -1.63%, net -8.37%, 288 trades (49.0/yr)
- **volume_weighted** (spot): gross -6.92%, net -12.69%, 275 trades (46.8/yr)
- **chart_pattern** (spot): gross -3.71%, net -8.15%, 184 trades (31.3/yr)
- **adx_trend** (spot): gross -19.38%, net -23.44%, 199 trades (33.9/yr)
- **ichimoku_cloud** (spot): gross -1.67%, net -5.21%, 171 trades (29.1/yr)
- **squeeze_momentum** (spot): gross -0.29%, net -3.69%, 131 trades (22.3/yr)
- **rsi_macd_combo** (spot): gross -19.87%, net -23.22%, 175 trades (29.8/yr)
- **funding_skew** (futures): gross -9.33%, net -12.05%, 124 trades (21.1/yr)
- **liquidity_sweeps** (spot): gross -16.94%, net -19.63%, 126 trades (21.4/yr)
- **momentum** (spot): gross -9.30%, net -11.95%, 127 trades (21.6/yr)
- **rsi** (spot): gross -19.75%, net -21.95%, 116 trades (19.7/yr)
- **vol_momentum** (spot): gross -0.43%, net -2.56%, 86 trades (14.6/yr)
- **amd_ifvg** (spot): gross -8.50%, net -10.18%, 85 trades (14.5/yr)
- **mtf_confluence** (spot): gross -1.14%, net -2.58%, 63 trades (10.7/yr)
- **momentum_pro** (spot): gross -4.16%, net -5.35%, 51 trades (8.7/yr)
- **sweep_squeeze_combo** (spot): gross -6.65%, net -7.18%, 25 trades (4.3/yr)
- **mean_reversion_pro** (spot): gross -2.73%, net -3.13%, 18 trades (3.1/yr)
- **range_scalper** (spot): gross -8.29%, net -8.47%, 11 trades (1.9/yr)

## M1 graduations (gross > 0, net <= 0 — mechanism: raise selectivity)

- **regime_adaptive** (spot): gross 3.96%, net -8.46%, fee drag 12.43pp over 495 trades (84.3/yr) — raise selectivity
- **tema_cross** (spot): gross 0.59%, net -9.71%, fee drag 10.30pp over 444 trades (75.6/yr) — raise selectivity
- **breakout** (futures): gross 5.28%, net -1.54%, fee drag 6.82pp over 260 trades (44.3/yr) — raise selectivity
- **regime_adaptive_htf** (spot): gross 0.27%, net -0.66%, fee drag 0.94pp over 37 trades (6.3/yr) — raise selectivity
