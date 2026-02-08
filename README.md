# Crypto Trading Bot — Phase 1

A modular cryptocurrency trading bot system with backtesting capabilities. Currently focused on Binance spot markets using public API data (no keys required).

## Architecture

```
trading-bot/
├── data_fetcher.py   # Historical & real-time OHLCV from Binance via ccxt
├── indicators.py     # Technical indicators (SMA crossover, RSI, Bollinger Bands)
├── backtester.py     # Backtesting engine with performance metrics
├── storage.py        # SQLite storage for price data & backtest results
├── run_backtest.py   # Main entry point — run & compare strategies
└── README.md
```

## Features

### Data Layer
- Fetches OHLCV candles from Binance (public API, no keys needed)
- Automatic pagination for full history downloads
- SQLite caching — fetch once, backtest many times
- Rate limit handling and retry logic

### Indicators
- **SMA Crossover**: Configurable fast/slow period moving average crossover
- **RSI**: Relative Strength Index with overbought/oversold signals
- **Bollinger Bands**: Mean reversion signals at band touches

### Backtesting Engine
- Event-driven simulation with realistic commission (0.1%) and slippage (0.05%)
- Full equity curve tracking
- Comprehensive metrics:
  - Total & annualized returns
  - Sharpe ratio, Sortino ratio
  - Max drawdown, Calmar ratio
  - Win rate, profit factor
  - Per-trade analytics
- Results saved to SQLite for comparison

## Quick Start

```bash
# Install dependencies
pip3 install numpy pandas ccxt

# Run all strategies on BTC/USDT daily (fetches data automatically)
python3 run_backtest.py

# Run a specific strategy
python3 run_backtest.py -s sma_crossover --symbol BTC/USDT --timeframe 1d --since 2022-01-01

# Available strategies: sma_crossover, rsi, bollinger_bands, all
python3 run_backtest.py -s rsi --capital 5000
```

## Configuration

Default parameters (editable in `run_backtest.py`):

| Strategy | Parameter | Default |
|----------|-----------|---------|
| SMA Crossover | fast_period | 20 |
| SMA Crossover | slow_period | 50 |
| RSI | period | 14 |
| RSI | overbought | 70 |
| RSI | oversold | 30 |
| Bollinger Bands | period | 20 |
| Bollinger Bands | num_std | 2.0 |

## Backtest Assumptions

- **Starting capital**: $1,000
- **Commission**: 0.1% per trade (Binance spot fee)
- **Slippage**: 0.05% per trade
- **Position sizing**: 100% of capital per trade (no partial positions)
- **Long only**: No short selling in Phase 1

## Data Storage

All data is stored in `trading_bot.db` (SQLite):
- `ohlcv` table: Cached price candles
- `backtest_results` table: Strategy performance history

## Roadmap

- **Phase 2**: More strategies, walk-forward optimization, paper trading
- **Phase 3**: Exchange integration, order management
- **Phase 4**: Live trading with risk management
- **Phase 5**: ML models, multi-exchange arbitrage

See `TRADING_BOT_PLAN.md` in the parent directory for the full roadmap.
