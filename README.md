# Crypto Trading Bot — Phases 1-5

A modular cryptocurrency trading bot with backtesting, paper trading, live trading, ML signals, and portfolio optimization. Built for Binance US spot markets.

## Architecture

```
trading-bot/
├── strategies.py          # 11 configurable trading strategies
├── indicators.py          # Technical indicator primitives (SMA, EMA, RSI, BB)
├── backtester.py          # Event-driven backtesting engine with metrics
├── optimizer.py           # Walk-forward optimization framework
├── reporter.py            # Text-based performance reporting
├── exchange_adapter.py    # Binance US adapter (paper + live via ccxt)
├── risk_manager.py        # Risk management rules engine
├── live_trader.py         # Live/paper trading engine with CLI
├── alerts.py              # Alert system (stdout → Discord ready)
├── ml_models.py           # XGBoost ML signal models
├── portfolio_optimizer.py # Multi-asset optimization & analytics
├── data_fetcher.py        # Historical + real-time OHLCV from Binance
├── storage.py             # SQLite storage layer
├── run_backtest.py        # Main backtesting entry point
└── models/                # Saved ML models
```

## Strategies (11 implemented)

| Strategy | Type | Description |
|----------|------|-------------|
| `sma_crossover` | Trend | Buy when fast SMA crosses above slow SMA |
| `ema_crossover` | Trend | EMA crossover (faster response) |
| `rsi` | Oscillator | Buy at oversold, sell at overbought |
| `bollinger_bands` | Mean Reversion | Trade band touches |
| `macd` | Momentum | MACD line / signal line crossovers |
| `mean_reversion` | Statistical | Z-score based mean reversion |
| `momentum` | Momentum | Rate of change breakouts |
| `volume_weighted` | Volume | Trend + volume confirmation |
| `triple_ema` | Trend | 3-EMA alignment (short/mid/long) |
| `rsi_macd_combo` | Multi-factor | Dual confirmation signals |
| `pairs_spread` | Stat Arb | Spread z-score trading |

## Quick Start

```bash
# Install dependencies
pip3 install numpy pandas ccxt scikit-learn xgboost scipy

# Run all strategies on BTC/USDT
python3 run_backtest.py --mode compare --symbol BTC/USDT

# Multi-asset comparison
python3 run_backtest.py --mode multi --symbols BTC/USDT ETH/USDT SOL/USDT BNB/USDT

# Walk-forward optimization
python3 run_backtest.py --mode optimize --strategy macd --symbol BTC/USDT

# Paper trading (real-time)
python3 live_trader.py --strategy macd --symbols BTC/USDT ETH/USDT --capital 10000

# Paper trading with custom settings
python3 live_trader.py --strategy bollinger_bands --symbols BTC/USDT --interval 300 --max-drawdown 10
```

## Risk Management

Built-in safety features:
- **Position sizing**: Max 20% per position, $5,000 hard cap
- **Exposure limits**: Max 80% total, 30% single asset
- **Daily loss limit**: -5% stops all trading
- **Max drawdown**: -15% kill switch
- **Circuit breaker**: 5 consecutive losses → 60min cooldown
- **Paper mode default**: Live requires explicit `--live` flag + API keys

## ML Models

XGBoost-based signal prediction:
- 23 features (returns, volatility, RSI, MACD, Bollinger, ATR, volume)
- Lightweight: ~50MB RAM, works on 2GB boxes
- Time-series aware train/test split (no look-ahead)
- Model persistence (pickle)

```python
from ml_models import MLSignalModel, train_and_backtest_ml
from data_fetcher import load_cached_data

df = load_cached_data("BTC/USDT", "1d")
result = train_and_backtest_ml(df, symbol="BTC/USDT")
```

## Portfolio Optimization

- Mean-variance (Markowitz) via Monte Carlo
- Strategy correlation analysis
- Performance attribution

```python
from portfolio_optimizer import mean_variance_optimize, format_portfolio_report
opt = mean_variance_optimize(returns_df)
print(format_portfolio_report(opt))
```

## Live Trading

```bash
# Paper mode (default, safe)
python3 live_trader.py --strategy macd --symbols BTC/USDT ETH/USDT

# ⚠️ Live mode (real money!)
python3 live_trader.py --strategy macd --symbols BTC/USDT --live --api-key KEY --api-secret SECRET
```

Safety guards in live mode:
- 5-second countdown before starting
- All risk management rules enforced
- Circuit breakers active
- Graceful shutdown on Ctrl+C
- Daily PnL reporting

## Data Storage

SQLite database (`trading_bot.db`):
- `ohlcv`: Cached price candles (fetch once, backtest many times)
- `backtest_results`: Strategy performance history

## Exchange

Uses **Binance US** (`binanceus`) via ccxt. Regular Binance is geo-blocked.

## Arbitrage Opportunity Tracker

Track arbitrage opportunities between Polymarket prediction markets and Binance spot prices:

```bash
# Test system connectivity
python3 test_arb_system.py

# Start arbitrage tracking
python3 arb_tracker.py

# Analyze opportunities
python3 arb_analyzer.py
```

See [ARBITRAGE.md](ARBITRAGE.md) for detailed documentation on the arbitrage tracking system.

## Dependencies

```
numpy pandas ccxt scikit-learn xgboost scipy requests
```
