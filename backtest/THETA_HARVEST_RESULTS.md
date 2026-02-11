# Theta Harvesting Backtest Results

**Date:** 2026-02-11
**Data:** Binance US, 2023-01-01 to 2026-02-11 (1,048 days)
**Strategy:** Vol Mean Reversion (sell strangles on high IV, buy straddles on low IV)

## What is Theta Harvesting?

Instead of holding sold options to expiry, buy them back early once a target % of premium has been captured. This locks in profit and frees capital for new trades.

Parameters tested:
- **Profit Target %**: Close sold option when this % of premium has decayed (40-80%)
- **Stop Loss %**: Close if loss exceeds this % of collected premium (150-200%)
- **Min DTE Close**: Force-close positions within N days of expiry (2 days)

## BTC Results ($1,000 capital)

| Metric | No Harvest | 40% | 50% | 60% | **70%** ✅ | 80% |
|---|---|---|---|---|---|---|
| Return | +9,540% | +11,615% | +11,512% | +11,580% | **+11,590%** | +10,797% |
| Max Drawdown | 61.8% | 49.4% | 46.3% | 46.3% | **37.8%** | 100.6% |
| Sharpe Ratio | 1.77 | 1.93 | 1.91 | 1.91 | **2.14** | 1.40 |
| Win Rate | 77.3% | 86.1% | 85.7% | 85.3% | 84.8% | 82.8% |
| Trades | 22 | 36 | 35 | 34 | 33 | 29 |
| Early Closes | 0 | 21 | 20 | 19 | 18 | 14 |

**Winner: 70% profit target** — Best Sharpe (2.14), lowest drawdown (37.8%), strong returns.

## ETH Results ($1,000 capital)

| Metric | **No Harvest** ✅ | 40% | 50% | 60% | 70% | 80% |
|---|---|---|---|---|---|---|
| Return | **+773%** | +583% | +516% | +577% | +591% | +575% |
| Max Drawdown | **17.4%** | 18.9% | 21.9% | 19.2% | 23.2% | 23.0% |
| Sharpe Ratio | **2.56** | 2.34 | 2.05 | 2.16 | 2.35 | 2.33 |
| Win Rate | 78.6% | 81.4% | 78.0% | 80.0% | 78.9% | 77.8% |

**Winner: No harvest** — ETH premiums too thin; early exits lose edge without reducing risk.

## BTC Results ($500 capital)

| Metric | No Harvest | 40% | 50% | 60% | **70%** ✅ | 80% |
|---|---|---|---|---|---|---|
| Return | +19,081% | +22,125% | +21,918% | +22,055% | **+22,074%** | +21,390% |
| Max Drawdown | 76.0% | 127.7% | 74.2% | 74.2% | **55.8%** | 125.5% |
| Sharpe Ratio | 1.65 | 1.43 | 1.58 | 1.58 | **1.86** | 1.17 |

**70% wins at both capital levels.**

## Key Insights

1. **BTC benefits from theta harvesting** — high volatility means premiums decay unevenly; locking in 70% and recycling capital outperforms holding
2. **ETH does not benefit** — lower volatility, smaller premiums, early exits cost more than they save
3. **40% and 80% are dangerous** — 40% recycles too fast (bad re-entries), 80% holds too long (captures gamma risk)
4. **70% is the sweet spot for BTC** — patient enough to capture most premium, exits before expiry gamma risk
5. **Stop loss at 200% never triggered** in any backtest — market didn't move enough against OTM strangles

## Deployed Config

- **BTC options strategies**: 70% profit target, 200% stop loss, 2-day min DTE
- **ETH options strategies**: Theta harvest disabled (hold to expiry)
