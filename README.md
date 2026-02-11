# go-trader — Crypto Trading Bot

A Go + Python hybrid trading system. Single Go binary (~8MB RAM) orchestrates 30 paper trading strategies across spot and options markets by spawning short-lived Python scripts.

## Architecture

```
Go scheduler (always running, ~8MB idle)
  ↓ spot: every 5min | options: every 20min
    python3 scripts/check_strategy.py <strategy> <symbol> <timeframe>        → JSON signal
    python3 scripts/check_options.py <strategy> <underlying> <positions>      → JSON signal (Deribit)
    python3 scripts/check_options_ibkr.py <strategy> <underlying> <positions> → JSON signal (IBKR/CME)
  ↓ processes signals, executes paper trades, manages risk
  ↓ saves state → scheduler/state.json (survives restarts)
  ↓ HTTP status → localhost:8099/status (live prices + portfolio values)
```

**Why this design:** Python gets the quant libraries (pandas, numpy, scipy, CCXT). Go gets memory efficiency. 30 strategies cost ~220MB peak for ~30 seconds, then ~8MB idle. Down from 1.6GB with persistent Python processes.

## Strategies (30 active)

### Spot (14 strategies, 5min interval, $1K each)

| Strategy | Tokens | Timeframe | Description |
|----------|--------|-----------|-------------|
| `momentum` | BTC, ETH, SOL | 1h | Rate of change breakouts |
| `rsi` | BTC, ETH, SOL | 1h | Buy oversold, sell overbought |
| `macd` | BTC, ETH | 1h | MACD/signal line crossovers |
| `volume_weighted` | BTC, ETH, SOL | 1h | Trend + volume confirmation |
| `pairs_spread` | BTC, ETH, SOL | 1d | Spread z-score stat arb |

### Options — Deribit vs IBKR/CME (8+8 strategies, 20min interval, $1K each)

Same 4 strategies run on both exchanges for head-to-head comparison:

| Strategy | Deribit IDs | IBKR IDs | Description |
|----------|------------|----------|-------------|
| `vol_mean_reversion` | deribit-vol-btc/eth | ibkr-vol-btc/eth | High IV → sell strangles, Low IV → buy straddles |
| `momentum_options` | deribit-momentum-btc/eth | ibkr-momentum-btc/eth | ROC breakout → buy directional options |
| `protective_puts` | deribit-puts-btc/eth | ibkr-puts-btc/eth | Buy 12% OTM puts, 45 DTE |
| `covered_calls` | deribit-calls-btc/eth | ibkr-calls-btc/eth | Sell 12% OTM calls, 21 DTE |

**Key differences:**
- **Deribit:** Direct crypto options, 1x multiplier, $100 strike intervals
- **IBKR/CME:** CME Micro futures options, BTC=0.1x multiplier, ETH=0.5x, $1000/$50 strike intervals

### Portfolio-Aware Options Scoring

New options trades are scored against existing positions:
- **Strike distance** — rejects overlapping strikes (<5% apart), rewards diversification
- **Expiry spread** — rewards different expiration dates
- **Greek balancing** — rewards delta-neutral, penalizes concentration
- Max **4 positions per strategy**, min score **0.3** to execute

## File Structure

```
trading-bot/
├── go-trader                    # Go binary
├── scheduler/                   # Go scheduler source
│   ├── main.go                  # Main loop, per-strategy intervals, auto-prune
│   ├── config.go                # Config types (supports per-strategy intervals)
│   ├── config.json              # 30 strategies configuration
│   ├── state.go                 # State persistence
│   ├── state.json               # Runtime state (positions, portfolios)
│   ├── executor.go              # Python script runner
│   ├── portfolio.go             # Spot position tracking
│   ├── options.go               # Options position tracking, Greeks
│   ├── risk.go                  # Drawdown, circuit breakers
│   ├── logger.go                # Stdout-only (no file logging)
│   ├── server.go                # HTTP status with live prices + P&L
│   ├── discord.go               # Discord trade notifications
│   └── go.mod
├── scripts/                     # Stateless check scripts
│   ├── check_strategy.py        # Spot strategy checker (Binance via CCXT)
│   ├── check_options.py         # Deribit options checker
│   ├── check_options_ibkr.py    # IBKR/CME options checker
│   └── check_price.py           # Multi-symbol price fetcher
├── strategies/                  # Spot strategy logic
│   ├── strategies.py            # 11 trading strategies
│   └── indicators.py            # Technical indicators (SMA, EMA, RSI, etc.)
├── options/                     # Options trading logic
│   ├── options_adapter.py       # Deribit adapter, Black-Scholes, Greeks
│   ├── ibkr_adapter.py          # IBKR/CME adapter, CME contract specs
│   ├── options_strategies.py    # Options strategy definitions
│   └── options_risk.py          # Options risk management
├── backtest/                    # Backtesting tools
│   ├── backtester.py            # Event-driven spot backtester
│   ├── backtest_options.py      # Options backtester (Black-Scholes)
│   ├── run_backtest.py          # Main backtest entry point
│   ├── optimizer.py             # Walk-forward optimization
│   └── reporter.py              # Performance reporting
├── core/                        # Shared infrastructure
│   ├── exchange_adapter.py      # Binance US adapter
│   ├── data_fetcher.py          # OHLCV data fetching
│   └── risk_manager.py          # Spot risk management
└── README.md
```

## Quick Start

```bash
# Install Python dependencies
pip3 install numpy pandas ccxt scipy ib_insync

# Install Go
curl -sL https://go.dev/dl/go1.23.6.linux-amd64.tar.gz | tar -C /usr/local -xzf -

# Build
cd scheduler && go build -o ../go-trader . && cd ..

# Test one cycle
./go-trader --config scheduler/config.json --once

# Run continuously
./go-trader --config scheduler/config.json

# Check status (live prices + P&L)
curl localhost:8099/status | python3 -m json.tool
```

### systemd Service

```bash
sudo cp go-trader.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable go-trader
sudo systemctl start go-trader
```

The service file sends stdout/stderr to /dev/null (no logging). State persists in `scheduler/state.json`.

### Manual Strategy Checks

```bash
# Spot
python3 scripts/check_strategy.py momentum BTC/USDT 1h

# Deribit options
python3 scripts/check_options.py vol_mean_reversion BTC '[]'

# IBKR/CME options
python3 scripts/check_options_ibkr.py vol_mean_reversion BTC '[]'

# Prices
python3 scripts/check_price.py BTC/USDT SOL/USDT ETH/USDT

# Backtest options
python3 backtest/backtest_options.py --underlying BTC --since 2023-01-01 --capital 1000 --max-positions 4
```

## Configuration

`scheduler/config.json` — each strategy has its own check interval:

```json
{
  "interval_seconds": 300,
  "state_file": "scheduler/state.json",
  "strategies": [
    {"id": "momentum-btc", "type": "spot", "script": "scripts/check_strategy.py",
     "args": ["momentum", "BTC/USDT", "1h"], "capital": 1000,
     "max_drawdown_pct": 60, "interval_seconds": 300},
    {"id": "deribit-vol-btc", "type": "options", "script": "scripts/check_options.py",
     "args": ["vol_mean_reversion", "BTC"], "capital": 1000,
     "max_drawdown_pct": 20, "interval_seconds": 1200}
  ]
}
```

On restart, the scheduler:
- Initializes new strategies from config
- **Auto-prunes** strategies in state that are no longer in config
- Preserves existing positions for strategies still in config

## Risk Management

**Spot:** Max 95% capital per position, drawdown kill switch (configurable), circuit breaker on consecutive losses.

**Options:** Max 4 positions per strategy, portfolio-aware scoring, 20% max drawdown, Greek concentration limits.

## System Reset Recovery

Everything needed to recover after a system reset:

1. `scheduler/state.json` — all positions, cash, trade history (committed to repo)
2. `scheduler/config.json` — all 30 strategy definitions (committed to repo)
3. `go-trader.service` — systemd unit file
4. Rebuild: `cd scheduler && go build -o ../go-trader .`
5. Restart: `systemctl start go-trader`

State file is the source of truth. Config defines what runs. Both are in the repo.

## Dependencies

```
# Python
pip3 install numpy pandas ccxt scipy ib_insync

# Go
go 1.23+ (no external dependencies, uses standard library only)

# System
systemd (for service management)
```
