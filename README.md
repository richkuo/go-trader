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
  ↓ marks options to market via Deribit REST API (live prices every cycle)
  ↓ saves state → scheduler/state.json (survives restarts)
  ↓ HTTP status → localhost:8099/status (live prices + real-time P&L)
```

**Why this design:** Python gets the quant libraries (pandas, numpy, scipy, CCXT). Go gets memory efficiency. 30 strategies cost ~220MB peak for ~30 seconds, then ~8MB idle. Down from 1.6GB with persistent Python processes.

## Live Option Pricing

**Options positions are marked to market with live Deribit prices every cycle:**

- **Deribit REST API** integration in `scheduler/deribit.go` fetches live mark prices
- **Smart fallback** — maps fictional paper trading expiries to nearest real Deribit expiry
- **Real-time P&L** — `CurrentValueUSD` updates based on live market data (not static entry values)
- **IBKR positions** use Deribit prices as proxy (same underlying/strikes)

**Python scripts** use `scripts/deribit_utils.py` to fetch real Deribit expiries and strikes for new trades:
- `fetch_available_expiries(underlying, min_dte, max_dte)` — returns list of real Deribit expiries
- `find_closest_expiry(underlying, target_dte)` — maps target DTE to closest real expiry
- `find_closest_strike(underlying, expiry, option_type, target_strike)` — finds nearest available strike

This ensures new paper trades use real option contracts that exist on Deribit, and existing positions are valued at current market prices.

## Strategies (34 active)

### Spot (14 strategies, 5min interval, $1K each)

| Strategy | Tokens | Timeframe | Description |
|----------|--------|-----------|-------------|
| `momentum` | BTC, ETH, SOL | 1h | Rate of change breakouts |
| `rsi` | BTC, ETH, SOL | 1h | Buy oversold, sell overbought |
| `macd` | BTC, ETH | 1h | MACD/signal line crossovers |
| `volume_weighted` | BTC, ETH, SOL | 1h | Trend + volume confirmation |
| `pairs_spread` | BTC, ETH, SOL | 1d | Spread z-score stat arb |

### Options — Deribit vs IBKR/CME (10+10 strategies, 20min interval, $1K each)

Same 5 strategies run on both exchanges for head-to-head comparison:

| Strategy | Deribit IDs | IBKR IDs | Description |
|----------|------------|----------|-------------|
| `vol_mean_reversion` | deribit-vol-btc/eth | ibkr-vol-btc/eth | High IV → sell strangles, Low IV → buy straddles |
| `momentum_options` | deribit-momentum-btc/eth | ibkr-momentum-btc/eth | ROC breakout → buy directional options |
| `protective_puts` | deribit-puts-btc/eth | ibkr-puts-btc/eth | Buy 12% OTM puts, 45 DTE |
| `covered_calls` | deribit-calls-btc/eth | ibkr-calls-btc/eth | Sell 12% OTM calls, 21 DTE |
| `wheel` | deribit-wheel-btc/eth | ibkr-wheel-btc/eth | Sell 6% OTM puts, 37 DTE, ~2% premium |

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
│   ├── deribit.go               # Deribit REST API client for live option pricing
│   ├── risk.go                  # Drawdown, circuit breakers
│   ├── logger.go                # Stdout-only (no file logging)
│   ├── server.go                # HTTP status with live prices + P&L
│   ├── discord.go               # Discord trade notifications
│   └── go.mod
├── scripts/                     # Stateless check scripts
│   ├── check_strategy.py        # Spot strategy checker (Binance via CCXT)
│   ├── check_options.py         # Deribit options checker
│   ├── check_options_ibkr.py    # IBKR/CME options checker
│   ├── check_price.py           # Multi-symbol price fetcher
│   └── deribit_utils.py         # Deribit expiry/strike lookup utilities
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
  ],
  "discord": {
    "enabled": true,
    "channels": {
      "spot": "1234567890",
      "options": "0987654321"
    }
  }
}
```

**Discord configuration:**
- `discord.enabled`: Enable/disable Discord notifications
- `discord.channels.spot`: Channel ID for spot trading summaries (hourly)
- `discord.channels.options`: Channel ID for options trading summaries (every 20min)
- Discord bot token is read from `DISCORD_BOT_TOKEN` environment variable

On restart, the scheduler:
- Initializes new strategies from config
- **Auto-prunes** strategies in state that are no longer in config
- Preserves existing positions for strategies still in config

## Trading Fees & Slippage

**Spot trades (Binance US simulation):**
- Taker fee: **0.1%** of trade value
- Slippage: **±0.05%** random (5 basis points)

**Options trades:**
- **Deribit:** 0.03% of premium
- **IBKR/CME:** $0.25 per contract (CME Micro fee)
- Slippage: not currently simulated for options (fixed premium from scripts)

All fees are deducted from cash on execution. Slippage is applied randomly to simulate market impact.

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
pip3 install numpy pandas ccxt scipy ib_insync requests

# Go
go 1.23+ (no external dependencies, uses standard library only)

# System
systemd (for service management)
```

## Regeneration Prompt

To rebuild this entire system from scratch, give an AI this prompt:

> Build a Go + Python hybrid trading bot called "go-trader".
>
> **Go scheduler** (single always-running binary, ~8MB RAM):
> - Reads a JSON config listing N strategies, each with: id, type (spot/options), script path, args, capital, risk params, and per-strategy `interval_seconds`
> - Main loop ticks at the shortest strategy interval (currently 300s). Each tick, only runs strategies whose individual interval has elapsed since last run
> - Sequentially spawns each due strategy's Python script, reads JSON output from stdout, processes the signal
> - Manages all state in memory: portfolios per strategy (cash + positions), trade history, risk state (drawdown kill switch, circuit breakers, daily loss limits, consecutive loss tracking)
> - For spot: tracks positions by symbol, simulates market fills at current price with slippage (±0.05%), applies trading fees (0.1% Binance taker), calculates portfolio value
> - For options: tracks positions with premium, Greeks (delta/gamma/theta/vega), expiry dates, auto-expires worthless OTM options; applies exchange-specific fees (Deribit 0.03%, IBKR $0.25/contract)
> - **Live option pricing** via Deribit REST API (`scheduler/deribit.go`): 
>   - Fetches live mark prices from Deribit ticker endpoint every cycle
>   - Updates `CurrentValueUSD` for ALL option positions (Deribit + IBKR) with real market data (not static entry values)
>   - IBKR positions use Deribit as pricing proxy (same underlying/strikes)
>   - `NewDeribitPricer()` creates HTTP client, `GetOptionPrice(underlying, expiry, strike, optionType)` returns live mark price
>   - `MarkOptionPositions(positions)` updates entire portfolio in one pass
> - **Smart expiry mapping** in `deribit.go` for legacy positions with fictional expiries:
>   - Tries exact instrument match first (e.g. `BTC-13MAR26-75000-C`)
>   - Falls back to `findNearestExpiry()` which searches Deribit's full option chain for nearest real expiry with same strike
>   - Logs warning with details (original expiry → mapped expiry, days difference) when fallback used
>   - Handles expired options gracefully (returns $0 mark price)
> - Passes existing option positions as JSON to Python scripts so they can do portfolio-aware trade scoring
> - Saves/loads state to a human-readable JSON file for restart recovery
> - On startup, initializes new strategies from config and **auto-prunes** strategies in state that are no longer in config
> - Prints cycle summary to stdout only (no file logging)
> - HTTP status endpoint (localhost:8099/status) that **fetches live prices** from exchange and returns JSON with real-time portfolio_value, pnl, and pnl_pct per strategy
> - Graceful shutdown on SIGINT/SIGTERM — saves state before exit
> - `--once` flag to run a single cycle and exit (for testing)
> - `--config` flag to specify config file path
> - **Discord cycle summary format**: Two separate reports sent to different channels - **Spot Summary** (hourly) and **Options Summary** (every 20min). Config specifies separate channel IDs for each report type in `discord.channels.spot` and `discord.channels.options`. Each shows starting → current balance for relevant categories only. Spot report shows 📈 Spot category. Options report shows 🎯 Deribit and 🏦 IBKR categories. Each bot displays: asset label, strategy name, P&L %, trade count, and last 3 trades. Format: `• ASSET strategy_name (+X.X%) — N trades` followed by `- BUY/SELL symbol @ $price (timestamp)`. Messages auto-truncate at 2000 characters (Discord limit).
>
> **Python check scripts** in `scripts/` (stateless, run-and-exit, ~5 seconds each):
> - `scripts/check_strategy.py <strategy> <symbol> <timeframe>` — fetches OHLCV via CCXT (Binance US), runs technical analysis, outputs JSON: `{strategy, symbol, timeframe, signal: 1/-1/0, price, indicators, timestamp}`
> - `scripts/check_options.py <strategy> <underlying> <positions_json>` — Deribit-style options. Fetches spot price via CCXT, evaluates options strategy, scores proposed trades against existing positions, outputs JSON with actions. **CRITICAL:** Uses `deribit_utils.py` to fetch real Deribit expiries and strikes for ALL new trades (never generates fictional expiries). Helpers: `get_real_expiry(underlying, target_dte)` returns closest real expiry, `get_real_strike(underlying, expiry, option_type, target_strike)` returns closest available strike
> - `scripts/check_options_ibkr.py <strategy> <underlying> <positions_json>` — IBKR/CME-style options. Same strategies as Deribit but uses CME Micro contract specs (BTC=0.1x multiplier, ETH=0.5x), CME strike intervals ($1000 for BTC, $50 for ETH), and Black-Scholes for premium estimation
> - `scripts/check_price.py <symbols...>` — fetches current prices, outputs JSON map
> - `scripts/deribit_utils.py` — **Required utility** for fetching real Deribit option chains via REST API (public endpoints, no auth). Core functions: `fetch_available_expiries(underlying, min_dte, max_dte)` returns list of ISO expiry strings, `find_closest_expiry(underlying, target_dte)` maps target DTE to nearest real expiry, `fetch_available_strikes(underlying, expiry)` gets available strikes for given expiry, `find_closest_strike(underlying, expiry, option_type, target_strike)` finds nearest strike. All strategies in `check_options.py` must call these helpers instead of calculating synthetic expiries
>
> **34 strategies in 3 groups:**
> - **14 spot** (5min interval, $1K each): momentum, rsi, macd, volume_weighted, pairs_spread across BTC/ETH/SOL via Binance US CCXT
> - **10 Deribit options** (20min interval, $1K each): vol_mean_reversion, momentum_options, protective_puts, covered_calls, wheel on BTC/ETH with 1x multiplier
> - **10 IBKR/CME options** (20min interval, $1K each): same 5 strategies on BTC/ETH but with CME Micro contract multipliers (0.1x BTC, 0.5x ETH) for head-to-head comparison
>
> **Spot strategies** (11 in `strategies/strategies.py`): SMA crossover, EMA crossover, RSI, Bollinger bands, MACD, mean reversion, momentum (ROC), volume weighted, triple EMA, RSI+MACD combo, pairs spread. Each takes a pandas DataFrame with OHLCV, returns it with a signal column (1=buy, -1=sell, 0=hold).
>
> **Options strategies** (5, implemented in both `check_options.py` and `check_options_ibkr.py`): Momentum options (ROC signals → buy ATM calls/puts 37 DTE), volatility mean reversion (IV rank >75% → sell strangles, <25% → buy straddles, 30 DTE), protective puts (buy 12% OTM puts 45 DTE), covered calls (sell 12% OTM calls 21 DTE), wheel (sell 6% OTM puts 37 DTE for ~2% premium). Black-Scholes pricing and Greeks in `options/options_adapter.py` (Deribit) and `options/ibkr_adapter.py` (IBKR/CME).
>
> **Options scoring system**: Before executing a new options trade, score it against existing positions. Factors: strike distance bonus (>10% apart = +0.4, <5% = -0.3), expiry spread bonus (different date = +0.3), Greek balancing (delta toward neutral = +0.2, skewing = -0.3), premium efficiency. Min score 0.3 to execute. Hard cap **4 positions per strategy**.
>
> **Directory structure**: `scheduler/` (Go source + config + state + deribit.go for live pricing), `scripts/` (stateless check scripts + deribit_utils.py for expiry/strike lookups), `strategies/` (spot strategies + indicators), `options/` (Deribit adapter, IBKR adapter, strategies, risk), `core/` (exchange adapter, data fetcher, risk manager), `backtest/` (backtesting tools incl. options backtester with Black-Scholes).
>
> **Tech stack**: Go 1.23+ for scheduler (standard library only, no external deps), Python 3 with numpy, pandas, ccxt, scipy, ib_insync, requests. CCXT connects to Binance US for spot data. Deribit REST API (public endpoints, no auth) for live option pricing and expiry/strike lookups. Deploy as systemd service with Restart=always, stdout/stderr to /dev/null (no file logging).
>
> **Config format**: JSON with interval_seconds (global default), state_file, and strategies array. Each strategy: id, type (spot/options), script, args, capital, max_drawdown_pct, interval_seconds (per-strategy override).
>
> **Status endpoint**: GET localhost:8099/status returns JSON with cycle_count, live prices (fetched from exchange), and per-strategy: id, type, cash, initial_capital, positions, option_positions, trade_count, portfolio_value, pnl, pnl_pct, risk_state.
