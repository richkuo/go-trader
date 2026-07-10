# Fee-aware selectivity audit (#999 M5)

Registry-wide trade-count x fee-drag screen. Each strategy leg is run twice on the eval_windows.py harness — once with the audit fee model, once with commission and slippage zeroed — to isolate fee drag and apply the salvage test (does a positive *gross* edge exist under the churn?).

## Reproduce

```
uv run --no-sync python backtest/fee_audit.py --registry both --strategies adx_trend,amd_ifvg,atr_breakout,bollinger_bands,ema_crossover,heikin_ashi_ema,ichimoku_cloud,macd,mean_reversion,momentum,mtf_confluence,order_blocks,pairs_spread,parabolic_sar,range_scalper,rsi,rsi_macd_combo,sma_crossover,squeeze_momentum,stoch_rsi,supertrend,sweep_squeeze_combo,triple_ema,volume_weighted,vol_momentum,vwap_reversion,tema_cross,regime_adaptive,triple_ema_bidir,tema_cross_bd,funding_skew,consolidation_range --markdown docs/research/1315-fee-rescreen-m5.md
```

- Generated: 2026-07-10
- Registries: both
- Windows: is (2025-06-10 → 2026-01-01), oos (2026-01-01 → latest)
- Datasets: BTC/USDT 1h, BTC/USDT 4h, ETH/USDT 1h, ETH/USDT 4h, SOL/USDT 1h, SOL/USDT 4h
- Direction: long
- Capital: 1000.0
- Fee model: hyperliquid platform taker fee + 5 bps slippage (net); commission=0 and slippage=0 (gross). Fee drag = mean per-leg (gross - net) return.

Returns are mean per-leg total-return %; trades are summed across all scored legs; trades/yr is annualized over the summed calendar span. **Verdicts:** `deprecate` (gross <= 0, no edge to salvage), `graduate_m1` (gross > 0, net <= 0 — raise selectivity), `healthy` (net > 0), `unscreened_short` (emitted short entries the long/flat harness drops — long leg alone can't justify deprecate/no_trades), `no_trades` (never fired). A † flags a row whose short half was unmeasured (verdict reflects the long leg only).

| rank | strategy | reg | trades | trades/yr | gross %/leg | net %/leg | fee drag (pp) | drag/trade (pp) | net Sharpe | verdict |
|-----:|----------|-----|-------:|----------:|------------:|----------:|--------------:|----------------:|-----------:|---------|
| 1 | vwap_reversion | spot | 1496 | 251.5 | -6.54 | -26.05 | 19.51 | 0.1565 | -1.29 | `deprecate` |
| 2 | parabolic_sar | spot | 1325 | 222.8 | -8.80 | -26.06 | 17.26 | 0.1563 | -1.33 | `deprecate` |
| 3 | macd | spot | 1258 | 211.5 | -10.14 | -25.60 | 15.47 | 0.1475 | -1.40 | `deprecate` |
| 4 | tema_cross_bd † | futures | 807 | 135.7 | -6.08 | -17.11 | 11.03 | 0.1640 | -0.73 | `unscreened_short` |
| 5 | triple_ema_bidir † | futures | 801 | 134.7 | -9.50 | -20.49 | 10.99 | 0.1646 | -1.13 | `unscreened_short` |
| 6 | heikin_ashi_ema | spot | 805 | 135.3 | -13.84 | -23.91 | 10.07 | 0.1501 | -1.24 | `deprecate` |
| 7 | regime_adaptive † | futures | 747 | 125.6 | -12.13 | -21.90 | 9.77 | 0.1570 | -1.02 | `unscreened_short` |
| 8 | stoch_rsi | spot | 662 | 111.3 | -13.30 | -21.76 | 8.46 | 0.1534 | -1.17 | `deprecate` |
| 9 | regime_adaptive | spot | 505 | 84.9 | 3.92 | -4.33 | 8.24 | 0.1959 | -0.56 | `graduate_m1` |
| 10 | ema_crossover | spot | 564 | 94.8 | -11.12 | -18.78 | 7.67 | 0.1631 | -1.16 | `deprecate` |
| 11 | consolidation_range † | futures | 625 | 105.1 | -22.86 | -30.25 | 7.38 | 0.1418 | -1.67 | `unscreened_short` |
| 12 | triple_ema | spot | 516 | 86.8 | -6.71 | -13.87 | 7.16 | 0.1666 | -0.83 | `deprecate` |
| 13 | tema_cross | spot | 454 | 76.3 | -0.08 | -6.88 | 6.80 | 0.1798 | -0.68 | `deprecate` |
| 14 | supertrend | spot | 476 | 80.0 | -10.31 | -17.00 | 6.69 | 0.1686 | -1.00 | `deprecate` |
| 15 | mean_reversion | spot | 496 | 83.4 | -18.19 | -24.30 | 6.11 | 0.1478 | -1.30 | `deprecate` |
| 16 | atr_breakout | spot | 389 | 65.4 | -4.08 | -9.83 | 5.74 | 0.1771 | -0.85 | `deprecate` |
| 17 | sma_crossover | spot | 364 | 61.2 | -7.38 | -12.23 | 4.84 | 0.1597 | -0.72 | `deprecate` |
| 18 | bollinger_bands | spot | 344 | 57.8 | -12.90 | -17.72 | 4.82 | 0.1680 | -0.99 | `deprecate` |
| 19 | pairs_spread | spot | 375 | 63.0 | -19.14 | -23.61 | 4.48 | 0.1434 | -1.32 | `deprecate` |
| 20 | order_blocks | spot | 294 | 49.4 | -2.75 | -7.10 | 4.35 | 0.1776 | -0.48 | `deprecate` |
| 21 | momentum | futures | 288 | 48.4 | -8.52 | -12.70 | 4.18 | 0.1744 | -1.04 | `deprecate` |
| 22 | volume_weighted | spot | 282 | 47.4 | -8.53 | -12.22 | 3.69 | 0.1569 | -0.66 | `deprecate` |
| 23 | adx_trend | spot | 203 | 34.1 | -19.35 | -22.01 | 2.66 | 0.1571 | -1.11 | `deprecate` |
| 24 | ichimoku_cloud | spot | 174 | 29.3 | -2.17 | -4.46 | 2.30 | 0.1584 | -0.53 | `deprecate` |
| 25 | squeeze_momentum | spot | 135 | 22.7 | -0.57 | -2.81 | 2.24 | 0.1994 | -0.06 | `deprecate` |
| 26 | vol_momentum † | futures | 149 | 25.1 | -2.13 | -4.37 | 2.23 | 0.1799 | -0.11 | `unscreened_short` |
| 27 | rsi_macd_combo | spot | 178 | 29.9 | -21.71 | -23.84 | 2.13 | 0.1433 | -1.13 | `deprecate` |
| 28 | funding_skew † | futures | 147 | 24.7 | -9.47 | -11.58 | 2.11 | 0.1720 | -0.81 | `unscreened_short` |
| 29 | momentum | spot | 128 | 21.5 | -9.33 | -11.03 | 1.70 | 0.1589 | -0.72 | `deprecate` |
| 30 | amd_ifvg | spot | 111 | 18.7 | -4.25 | -5.77 | 1.52 | 0.1642 | -0.15 | `deprecate` |
| 31 | rsi | spot | 117 | 19.7 | -21.00 | -22.39 | 1.39 | 0.1423 | -1.18 | `deprecate` |
| 32 | vol_momentum | spot | 87 | 14.6 | -0.52 | -1.89 | 1.37 | 0.1894 | -0.44 | `deprecate` |
| 33 | mtf_confluence † | futures | 77 | 12.9 | -4.55 | -5.65 | 1.10 | 0.1713 | -0.53 | `unscreened_short` |
| 34 | mtf_confluence | spot | 63 | 10.6 | -1.18 | -2.10 | 0.92 | 0.1756 | -0.20 | `deprecate` |
| 35 | sweep_squeeze_combo | spot | 26 | 4.4 | -8.21 | -8.54 | 0.33 | 0.1542 | -0.61 | `deprecate` |
| 36 | range_scalper | spot | 11 | 1.8 | -8.35 | -8.46 | 0.11 | 0.1191 | -0.46 | `deprecate` |

## Deprecation list (gross edge <= 0 — fee filter cannot save)

- **vwap_reversion** (spot): gross -6.54%, net -26.05%, 1496 trades (251.5/yr)
- **parabolic_sar** (spot): gross -8.80%, net -26.06%, 1325 trades (222.8/yr)
- **macd** (spot): gross -10.14%, net -25.60%, 1258 trades (211.5/yr)
- **heikin_ashi_ema** (spot): gross -13.84%, net -23.91%, 805 trades (135.3/yr)
- **stoch_rsi** (spot): gross -13.30%, net -21.76%, 662 trades (111.3/yr)
- **ema_crossover** (spot): gross -11.12%, net -18.78%, 564 trades (94.8/yr)
- **triple_ema** (spot): gross -6.71%, net -13.87%, 516 trades (86.8/yr)
- **tema_cross** (spot): gross -0.08%, net -6.88%, 454 trades (76.3/yr)
- **supertrend** (spot): gross -10.31%, net -17.00%, 476 trades (80.0/yr)
- **mean_reversion** (spot): gross -18.19%, net -24.30%, 496 trades (83.4/yr)
- **atr_breakout** (spot): gross -4.08%, net -9.83%, 389 trades (65.4/yr)
- **sma_crossover** (spot): gross -7.38%, net -12.23%, 364 trades (61.2/yr)
- **bollinger_bands** (spot): gross -12.90%, net -17.72%, 344 trades (57.8/yr)
- **pairs_spread** (spot): gross -19.14%, net -23.61%, 375 trades (63.0/yr)
- **order_blocks** (spot): gross -2.75%, net -7.10%, 294 trades (49.4/yr)
- **momentum** (futures): gross -8.52%, net -12.70%, 288 trades (48.4/yr)
- **volume_weighted** (spot): gross -8.53%, net -12.22%, 282 trades (47.4/yr)
- **adx_trend** (spot): gross -19.35%, net -22.01%, 203 trades (34.1/yr)
- **ichimoku_cloud** (spot): gross -2.17%, net -4.46%, 174 trades (29.3/yr)
- **squeeze_momentum** (spot): gross -0.57%, net -2.81%, 135 trades (22.7/yr)
- **rsi_macd_combo** (spot): gross -21.71%, net -23.84%, 178 trades (29.9/yr)
- **momentum** (spot): gross -9.33%, net -11.03%, 128 trades (21.5/yr)
- **amd_ifvg** (spot): gross -4.25%, net -5.77%, 111 trades (18.7/yr)
- **rsi** (spot): gross -21.00%, net -22.39%, 117 trades (19.7/yr)
- **vol_momentum** (spot): gross -0.52%, net -1.89%, 87 trades (14.6/yr)
- **mtf_confluence** (spot): gross -1.18%, net -2.10%, 63 trades (10.6/yr)
- **sweep_squeeze_combo** (spot): gross -8.21%, net -8.54%, 26 trades (4.4/yr)
- **range_scalper** (spot): gross -8.35%, net -8.46%, 11 trades (1.8/yr)

## M1 graduations (gross > 0, net <= 0 — mechanism: raise selectivity)

- **regime_adaptive** (spot): gross 3.92%, net -4.33%, fee drag 8.24pp over 505 trades (84.9/yr) — raise selectivity

## Unscreened short legs (long/flat harness drops short entries — verdict withheld)

- **tema_cross_bd** (futures): short-capable (bidirectional / allow_short); the long/flat harness measured only its long leg (gross -6.08%, net -17.11% over 807 long trades). Re-screen via the open/close engine (models both sides) before any deprecate/graduate call.
- **triple_ema_bidir** (futures): short-capable (bidirectional / allow_short); the long/flat harness measured only its long leg (gross -9.50%, net -20.49% over 801 long trades). Re-screen via the open/close engine (models both sides) before any deprecate/graduate call.
- **regime_adaptive** (futures): short-capable (bidirectional / allow_short); the long/flat harness measured only its long leg (gross -12.13%, net -21.90% over 747 long trades). Re-screen via the open/close engine (models both sides) before any deprecate/graduate call.
- **consolidation_range** (futures): short-capable (bidirectional / allow_short); the long/flat harness measured only its long leg (gross -22.86%, net -30.25% over 625 long trades). Re-screen via the open/close engine (models both sides) before any deprecate/graduate call.
- **vol_momentum** (futures): short-capable (bidirectional / allow_short); the long/flat harness measured only its long leg (gross -2.13%, net -4.37% over 149 long trades). Re-screen via the open/close engine (models both sides) before any deprecate/graduate call.
- **funding_skew** (futures): short-capable (bidirectional / allow_short); the long/flat harness measured only its long leg (gross -9.47%, net -11.58% over 147 long trades). Re-screen via the open/close engine (models both sides) before any deprecate/graduate call.
- **mtf_confluence** (futures): short-capable (bidirectional / allow_short); the long/flat harness measured only its long leg (gross -4.55%, net -5.65% over 77 long trades). Re-screen via the open/close engine (models both sides) before any deprecate/graduate call.

## Verdict comparison vs the binanceus-model baseline (#1315)

Context: this run re-screens the quarantine roster after #1315 switched the
audit fee model from binanceus (0.1% taker/side + 5 bps slippage, ~0.3%
round-trip) to hyperliquid base tier (0.045% taker/side + 5 bps slippage,
~0.19% round-trip). Baseline verdicts are the `docs/research/fee-audit-m5.md`
table (generated 2026-06-12, binanceus model). Only the *fee* axis changed:
the OHLCV data source is unchanged from the baseline (binanceus cached
candles — `eval_windows._load_data` loads under the default `exchange_id`,
independent of the audit fee model). Two caveats when comparing:

- The `oos` window ends at the latest cached bar, so this run carries ~1 month
  more data than the baseline. **Gross** legs are fee-free — any gross change
  between the two reports is data drift, not the fee model.
- Six names (`tema_cross`, `regime_adaptive`, `triple_ema_bidir`,
  `tema_cross_bd`, `funding_skew`, `consolidation_range`) are quarantined only
  by the #1282 adjudication, in flight on PR #1314 at generation time; they are
  included here so the corrected-model evidence is on record either way.

Result: **no strategy's verdict improves under the corrected (cheaper) fee
model.** Fee drag per row drops by roughly a third (e.g. vwap_reversion 28.39
→ 19.51 pp), but every roster member's net stays negative — at these churn
rates the fee model was not the deciding margin, the missing gross edge was.

| strategy | reg | binanceus verdict (2026-06-12) | hyperliquid verdict (this run) | flip? |
|----------|-----|-------------------------------|--------------------------------|-------|
| vwap_reversion | spot | `deprecate` | `deprecate` | no |
| parabolic_sar | spot | `deprecate` | `deprecate` | no |
| macd | spot | `deprecate` | `deprecate` | no |
| triple_ema_bidir † | futures | `unscreened_short` | `unscreened_short` | no |
| tema_cross_bd † | futures | `unscreened_short` | `unscreened_short` | no |
| heikin_ashi_ema | spot | `deprecate` | `deprecate` | no |
| regime_adaptive † | futures | `unscreened_short` | `unscreened_short` | no |
| stoch_rsi | spot | `deprecate` | `deprecate` | no |
| regime_adaptive | spot | `graduate_m1` | `graduate_m1` | no |
| ema_crossover | spot | `deprecate` | `deprecate` | no |
| consolidation_range † | futures | `unscreened_short` | `unscreened_short` | no |
| triple_ema | spot | `deprecate` | `deprecate` | no |
| tema_cross | spot | `graduate_m1` | `deprecate` | **yes — worse, data drift** (gross +0.59 → -0.08%/leg as the oos window extended 444 → 454 trades; gross legs are fee-free, so the fee model can't have moved this) |
| supertrend | spot | `deprecate` | `deprecate` | no |
| mean_reversion | spot | `deprecate` | `deprecate` | no |
| atr_breakout | spot | `deprecate` | `deprecate` | no |
| sma_crossover | spot | `deprecate` | `deprecate` | no |
| bollinger_bands | spot | `deprecate` | `deprecate` | no |
| pairs_spread | spot | `deprecate` | `deprecate` | no |
| order_blocks | spot | `deprecate` | `deprecate` | no |
| momentum | futures | `deprecate` | `deprecate` | no |
| volume_weighted | spot | `deprecate` | `deprecate` | no |
| adx_trend | spot | `deprecate` | `deprecate` | no |
| ichimoku_cloud | spot | `deprecate` | `deprecate` | no |
| squeeze_momentum | spot | `deprecate` | `deprecate` | no |
| vol_momentum † | futures | `unscreened_short` | `unscreened_short` | no |
| rsi_macd_combo | spot | `deprecate` | `deprecate` | no |
| funding_skew † | futures | `unscreened_short` | `unscreened_short` | no |
| momentum | spot | `deprecate` | `deprecate` | no |
| amd_ifvg | spot | `deprecate` | `deprecate` | no |
| rsi | spot | `deprecate` | `deprecate` | no |
| vol_momentum | spot | `deprecate` | `deprecate` | no |
| mtf_confluence † | futures | `unscreened_short` | `unscreened_short` | no |
| mtf_confluence | spot | `deprecate` | `deprecate` | no |
| sweep_squeeze_combo | spot | `deprecate` | `deprecate` | no |
| range_scalper | spot | `deprecate` | `deprecate` | no |

**Conclusion: no roster change.** The #1282 deprecations of `tema_cross` and
`regime_adaptive` (spot longs with real gross edges) also stand — their M5
nets remain negative under hyperliquid fees (tema_cross -6.88%/leg,
regime_adaptive -4.33%/leg on the protocol windows), and the #1282 verdicts
additionally rested on M1 protocol failures vs the incumbent bar, which the
fee change moves on both sides. `regime_adaptive` spot stays `graduate_m1` on
the M5 screen exactly as it was pre-change; its quarantine (PR #1314) came
from the deeper M1/noise protocol, not this screen.

---
Created with LLM: Fable 5 | high | Harness: Claude Code
