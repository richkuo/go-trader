# go-trader — Crypto Trading Bot

A Go + Python hybrid trading system. Go scheduler (single binary, ~8MB RAM) orchestrates 24 trading strategies across spot and options markets by spawning short-lived Python scripts every 5 minutes. Features theta harvesting for BTC options and Discord notifications.

## Architecture

```
Go scheduler (always running, ~8MB idle)
  ↓ every 10 min, sequentially runs:
    python3 scripts/check_strategy.py <strategy> <symbol> <timeframe>   → JSON signal
    python3 scripts/check_options.py <strategy> <underlying> <positions> → JSON signal + actions
  ↓ processes signals
    Executes paper trades, manages risk
  ↓ saves state
    scheduler/state.json (survives restarts)
```

**Why this design:** Python gets the quant libraries (pandas, numpy, scipy, CCXT). Go gets memory efficiency. 22 strategies cost ~220MB peak for 30 seconds, then ~8MB idle. Down from 1.6GB with persistent Python processes.

```
trading-bot/
├── go-trader                  # Go binary (built from scheduler/)
├── scheduler/                 # Go scheduler
│   ├── main.go                # Entry point, main loop, graceful shutdown
│   ├── config.go              # Config types and loading
│   ├── config.json            # Strategy configuration (22 strategies)
│   ├── state.go               # State persistence (save/load JSON)
│   ├── state.json             # Runtime state (positions, portfolios)
│   ├── executor.go            # Python script runner, JSON parsing
│   ├── portfolio.go           # Spot position tracking, paper trades
│   ├── options.go             # Options position tracking, Greeks, scoring
│   ├── risk.go                # Drawdown, circuit breakers, loss limits
│   ├── logger.go              # Stdout-only logging
│   ├── server.go              # HTTP status endpoint (:8099)
│   ├── discord.go             # Discord notifications (cycle summaries to #trading)
│   └── go.mod
├── scripts/                   # Run-and-exit check scripts
│   ├── check_strategy.py      # Spot strategy checker
│   ├── check_options.py       # Options checker with portfolio-aware scoring
│   └── check_price.py         # Quick multi-symbol price fetcher
├── strategies/                # Spot strategy logic
│   ├── strategies.py          # 11 trading strategies
│   ├── indicators.py          # Technical indicator primitives (SMA, EMA, etc.)
│   └── alerts.py              # Alert system
├── options/                   # Options trading logic
│   ├── options_adapter.py     # Deribit adapter, Black-Scholes, Greeks
│   ├── options_strategies.py  # 4 options strategies
│   └── options_risk.py        # Options-specific risk management
├── core/                      # Shared infrastructure
│   ├── exchange_adapter.py    # Binance US spot adapter via CCXT
│   ├── data_fetcher.py        # OHLCV data fetching
│   ├── risk_manager.py        # Spot risk management rules
│   └── storage.py             # SQLite storage layer
├── backtest/                  # Backtesting tools
│   ├── backtester.py          # Event-driven backtesting engine
│   ├── backtest_options.py    # Options strategy backtester
│   ├── backtest_theta.py      # Theta harvesting comparison backtester
│   ├── THETA_HARVEST_RESULTS.md # Theta harvest backtest results & analysis
│   ├── optimizer.py           # Walk-forward optimization
│   ├── reporter.py            # Performance reporting
│   └── run_backtest.py        # Main backtesting entry point
├── archive/                   # Old/unused files
└── README.md
```

## Strategies (22 active)

### Spot (14 strategies via Binance/CCXT)

| Strategy | Tokens | Timeframe | Description |
|----------|--------|-----------|-------------|
| `momentum` | BTC, SOL, ETH | 1h | Rate of change breakouts |
| `volume_weighted` | BTC, SOL, ETH | 1h | Trend + volume confirmation |
| `rsi` | BTC, SOL, ETH | 1h | Buy oversold, sell overbought |
| `macd` | BTC, ETH | 1h | MACD/signal line crossovers |
| `pairs_spread` | BTC, ETH, SOL | 1d | Spread z-score stat arb |

### Options (8 strategies via Deribit testnet)

| Strategy | Tokens | Description |
|----------|--------|-------------|
| `momentum_options` | BTC, ETH | Buy calls on bullish momentum, puts on bearish |
| `vol_mean_reversion` | BTC, ETH | High IV → sell strangles, Low IV → buy straddles |
| `protective_puts` | BTC, ETH | OTM puts to hedge spot holdings |
| `covered_calls` | BTC, ETH | Sell OTM calls for income |

### Theta Harvesting (BTC Options)

Instead of holding sold options to expiry, the scheduler automatically buys back positions once a target profit % has been captured. This locks in gains and frees capital for new trades.

**Backtested settings (see `backtest/THETA_HARVEST_RESULTS.md`):**

| Asset | Profit Target | Stop Loss | Min DTE Close | Result |
|-------|--------------|-----------|---------------|--------|
| **BTC** | 70% | 200% | 2 days | Sharpe 2.14, DD 37.8% (vs 1.77/61.8% no harvest) |
| **ETH** | Disabled | — | — | No harvest wins (Sharpe 2.56, DD 17.4%) |

BTC benefits from theta harvesting due to higher volatility — locking in 70% and recycling capital beats holding. ETH premiums are too thin; early exits lose edge.

### Discord Notifications

The scheduler posts cycle summaries to a Discord channel after each run via `scheduler/discord.go`. Shows:
- Current prices (BTC, ETH, SOL)
- Main portfolio ($1K bots) with open positions
- $200 bots (now $1K) with open positions  
- Trade alerts with 🚨 when trades execute

### Portfolio-Aware Options Scoring

New options trades are scored against existing positions before execution:
- **Strike distance** — rejects overlapping strikes (<5% apart), rewards diversification
- **Expiry spread** — rewards different expiration dates
- **Greek balancing** — rewards delta-neutral, penalizes concentration
- **Premium efficiency** — bonus for better premium collection
- Min score **0.3** to execute, hard cap **2 positions per strategy**

## Quick Start

```bash
# Install Python dependencies
pip3 install numpy pandas ccxt scipy

# Install Go (if not installed)
curl -sL https://go.dev/dl/go1.23.6.linux-amd64.tar.gz | tar -C /usr/local -xzf -

# Build the Go scheduler
cd scheduler && go build -o ../go-trader . && cd ..

# Run one cycle (test)
./go-trader --config scheduler/config.json --once

# Run continuously
./go-trader --config scheduler/config.json

# Check status
curl localhost:8099/status
```

### systemd Service

```bash
# Install as service
sudo cp go-trader.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable go-trader
sudo systemctl start go-trader

# Check status
sudo systemctl status go-trader
```

### Manual Commands

```bash
# Run individual strategy checks
python3 scripts/check_strategy.py momentum BTC/USDT 1h
python3 scripts/check_options.py vol_mean_reversion BTC '[]'
python3 scripts/check_price.py BTC/USDT SOL/USDT ETH/USDT

# Backtest a strategy
python3 backtest/run_backtest.py --mode compare --symbol BTC/USDT
```

## Configuration

Edit `scheduler/config.json`:

```json
{
  "interval_seconds": 600,
  "state_file": "scheduler/state.json",
  "strategies": [
    {
      "id": "momentum-btc",
      "type": "spot",
      "script": "check_strategy.py",
      "args": ["momentum", "BTC/USDT", "1h"],
      "capital": 1000,
      "max_drawdown_pct": 60
    }
  ]
}
```

Add/remove strategies by editing this file and restarting the service.

## Risk Management

**Spot:**
- Max 20% per position, $5,000 hard cap
- Max 80% total exposure
- Daily loss limit: -5% stops trading
- Drawdown kill switch (configurable per strategy)
- Circuit breaker: consecutive losses → cooldown

**Options:**
- Portfolio Greeks tracking (delta, gamma, theta, vega limits)
- Max 2 positions per strategy
- Portfolio-aware scoring prevents stacking identical trades
- Premium at risk limits
- Margin estimation for short positions

## Dependencies

```
# Python
numpy pandas ccxt scipy

# Go
go 1.23+ (no external dependencies)
```

## Regeneration Prompt

To rebuild this entire system from scratch, give an AI this prompt:

> Build a Go + Python hybrid trading bot called "go-trader".
>
> **Go scheduler** (single always-running binary, ~8MB RAM):
> - Reads a JSON config listing N strategies (each with: id, type spot/options, script path, args, capital, risk params)
> - Every 10 minutes, sequentially spawns each strategy's Python script, reads JSON output from stdout, processes the signal
> - Manages all state in memory: portfolios per strategy (cash + positions), trade history, risk state (drawdown kill switch, circuit breakers, daily loss limits, consecutive loss tracking)
> - For spot: tracks positions by symbol, simulates market fills at current price, calculates portfolio value
> - For options: tracks positions with premium, Greeks (delta/gamma/theta/vega), expiry dates, auto-expires worthless OTM options
> - Passes existing option positions as JSON to Python scripts so they can do portfolio-aware trade scoring
> - Saves/loads state to a human-readable JSON file for restart recovery
> - Prints cycle summary to stdout (strategies checked, trades executed, total value, elapsed time)
> - HTTP status endpoint (localhost:8099/status) returning JSON of all portfolios
> - Graceful shutdown on SIGINT/SIGTERM — saves state before exit
> - `--once` flag to run a single cycle and exit (for testing)
> - `--config` flag to specify config file path
>
> **Python check scripts** in `scripts/` (stateless, run-and-exit, ~5 seconds each):
> - `scripts/check_strategy.py <strategy> <symbol> <timeframe>` — fetches OHLCV via CCXT (Binance), runs technical analysis from `strategies/` and data fetching from `core/`, outputs JSON: `{strategy, symbol, timeframe, signal: 1/-1/0, price, indicators, timestamp}`
> - `scripts/check_options.py <strategy> <underlying> <positions_json>` — fetches spot price via CCXT, evaluates options strategy (momentum options, vol mean reversion, protective puts, covered calls), scores proposed trades against existing positions (strike distance, expiry spread, Greek concentration, premium efficiency), filters by min score threshold, outputs JSON: `{strategy, underlying, signal, spot_price, actions: [{action, option_type, strike, expiry, dte, premium, premium_usd, greeks, score, score_reason}], iv_rank, timestamp}`
> - `scripts/check_price.py <symbols...>` — fetches current prices, outputs JSON map: `{"BTC/USDT": 67000.00, "ETH/USDT": 1940.00}`
>
> **Spot strategies** (11 in `strategies/strategies.py`): SMA crossover, EMA crossover, RSI, Bollinger bands, MACD, mean reversion, momentum (ROC), volume weighted, triple EMA, RSI+MACD combo, pairs spread. Each takes a pandas DataFrame with OHLCV, returns it with a signal column (1=buy, -1=sell, 0=hold). Indicators in `strategies/indicators.py`.
>
> **Options strategies** (4 in `options/`): Momentum options (ROC signals → buy ATM calls/puts 30-45 DTE), volatility mean reversion (high IV rank >75% → sell strangles, low IV <25% → buy straddles), protective puts (buy 12% OTM puts 45 DTE), covered calls (sell 12% OTM calls 21 DTE). Adapter with Black-Scholes/Greeks in `options/options_adapter.py`.
>
> **Options scoring system**: Before executing a new options trade, score it against existing positions. Factors: strike distance bonus (>10% apart = +0.4, <5% = -0.3), expiry spread bonus (different date = +0.3), Greek balancing (delta toward neutral = +0.2, skewing = -0.3), premium efficiency. Min score 0.3 to execute. Hard cap 2 positions per strategy.
>
> **Directory structure**: `scheduler/` (Go binary + config), `scripts/` (run-and-exit check scripts), `strategies/` (spot strategies + indicators), `options/` (options adapter + strategies + risk), `core/` (exchange adapter, data fetcher, risk manager, storage), `backtest/` (backtesting tools), `archive/` (old/unused files).
>
> **Tech stack**: Go 1.23+ for scheduler (no external deps), Python 3 with numpy, pandas, ccxt, scipy. CCXT connects to Binance for spot data, Deribit testnet for options. Deploy as systemd service with Restart=always.
>
> **Config format**: JSON file with interval_seconds, state_file, and array of strategy objects. Each strategy has: id, type (spot/options), script path (e.g. `scripts/check_strategy.py`), args array, capital, max_drawdown_pct.
