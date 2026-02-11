# go-trader — Crypto Trading Bot

A Go + Python hybrid trading system. Go scheduler (single binary, ~8MB RAM) orchestrates 22 trading strategies across spot and options markets by spawning short-lived Python scripts every 10 minutes.

## Architecture

```
Go scheduler (always running, ~8MB idle)
  ↓ every 10 min, sequentially runs:
    python3 check_strategy.py <strategy> <symbol> <timeframe>   → JSON signal
    python3 check_options.py <strategy> <underlying> <positions> → JSON signal + actions
  ↓ processes signals
    Executes paper trades, manages risk, logs everything
  ↓ saves state
    scheduler/state.json (survives restarts)
```

**Why this design:** Python gets the quant libraries (pandas, numpy, scipy, CCXT). Go gets memory efficiency. 22 strategies cost ~220MB peak for 30 seconds, then ~8MB idle. Down from 1.6GB with persistent Python processes.

```
trading-bot/
├── go-trader                  # Go binary (built from scheduler/)
├── scheduler/
│   ├── main.go                # Entry point, main loop, graceful shutdown
│   ├── config.go              # Config types and loading
│   ├── config.json            # Strategy configuration (22 strategies)
│   ├── state.go               # State persistence (save/load JSON)
│   ├── state.json             # Runtime state (positions, portfolios)
│   ├── executor.go            # Python script runner, JSON parsing
│   ├── portfolio.go           # Spot position tracking, paper trades
│   ├── options.go             # Options position tracking, Greeks, scoring
│   ├── risk.go                # Drawdown, circuit breakers, loss limits
│   ├── logger.go              # Per-strategy + combined logging
│   ├── server.go              # HTTP status endpoint (:8099)
│   └── go.mod
├── check_strategy.py          # Spot strategy checker (stateless, run-and-exit)
├── check_options.py           # Options strategy checker with portfolio-aware scoring
├── check_price.py             # Quick multi-symbol price fetcher
├── strategies.py              # 11 spot trading strategies
├── indicators.py              # Technical indicator primitives
├── options_adapter.py         # Deribit options adapter, Black-Scholes, Greeks
├── options_strategies.py      # 4 options strategies
├── options_risk.py            # Options-specific risk management
├── exchange_adapter.py        # Binance US spot adapter via CCXT
├── risk_manager.py            # Spot risk management rules
├── data_fetcher.py            # OHLCV data fetching
├── backtester.py              # Backtesting engine
├── live_trader.py             # Legacy persistent paper trader
└── logs/                      # Per-strategy log files
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
python3 check_strategy.py momentum BTC/USDT 1h
python3 check_options.py vol_mean_reversion BTC '[]'
python3 check_price.py BTC/USDT SOL/USDT ETH/USDT

# Backtest a strategy
python3 run_backtest.py --mode compare --symbol BTC/USDT

# Legacy paper trader (persistent Python — not recommended)
python3 live_trader.py --strategy macd --symbols BTC/USDT --capital 1000
```

## Configuration

Edit `scheduler/config.json`:

```json
{
  "interval_seconds": 600,
  "log_dir": "logs",
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
> - Logs per-strategy to individual files + prints combined cycle summary (strategies checked, trades executed, total value, elapsed time)
> - HTTP status endpoint (localhost:8099/status) returning JSON of all portfolios
> - Graceful shutdown on SIGINT/SIGTERM — saves state before exit
> - `--once` flag to run a single cycle and exit (for testing)
> - `--config` flag to specify config file path
>
> **Python check scripts** (stateless, run-and-exit, ~5 seconds each):
> - `check_strategy.py <strategy> <symbol> <timeframe>` — fetches OHLCV via CCXT (Binance), runs technical analysis (SMA, EMA, RSI, MACD, Bollinger, momentum ROC, volume weighted, pairs spread), outputs JSON: `{strategy, symbol, timeframe, signal: 1/-1/0, price, indicators, timestamp}`
> - `check_options.py <strategy> <underlying> <positions_json>` — fetches spot price via CCXT, evaluates options strategy (momentum options, vol mean reversion, protective puts, covered calls), scores proposed trades against existing positions (strike distance, expiry spread, Greek concentration, premium efficiency), filters by min score threshold, outputs JSON: `{strategy, underlying, signal, spot_price, actions: [{action, option_type, strike, expiry, dte, premium, premium_usd, greeks, score, score_reason}], iv_rank, timestamp}`
> - `check_price.py <symbols...>` — fetches current prices, outputs JSON map: `{"BTC/USDT": 67000.00, "ETH/USDT": 1940.00}`
>
> **Spot strategies** (11 in strategies.py): SMA crossover, EMA crossover, RSI, Bollinger bands, MACD, mean reversion, momentum (ROC), volume weighted, triple EMA, RSI+MACD combo, pairs spread. Each takes a pandas DataFrame with OHLCV, returns it with a signal column (1=buy, -1=sell, 0=hold).
>
> **Options strategies** (4): Momentum options (ROC signals → buy ATM calls/puts 30-45 DTE), volatility mean reversion (high IV rank >75% → sell strangles, low IV <25% → buy straddles), protective puts (buy 12% OTM puts 45 DTE), covered calls (sell 12% OTM calls 21 DTE).
>
> **Options scoring system**: Before executing a new options trade, score it against existing positions. Factors: strike distance bonus (>10% apart = +0.4, <5% = -0.3), expiry spread bonus (different date = +0.3), Greek balancing (delta toward neutral = +0.2, skewing = -0.3), premium efficiency. Min score 0.3 to execute. Hard cap 2 positions per strategy.
>
> **Tech stack**: Go 1.23+ for scheduler (no external deps), Python 3 with numpy, pandas, ccxt, scipy. CCXT connects to Binance for spot data, Deribit testnet for options. Deploy as systemd service with Restart=always.
>
> **Config format**: JSON file with interval_seconds, log_dir, state_file, and array of strategy objects. Each strategy has: id, type (spot/options), script, args array, capital, max_drawdown_pct.
