# go-trader — Crypto Trading Bot

A Go + Python hybrid trading system. Single Go binary (~8MB RAM) orchestrates 30+ paper trading strategies across spot and options markets by spawning short-lived Python scripts.

## Quick Start

```bash
# 1. Clone
git clone https://github.com/richkuo/go-trader.git
cd go-trader

# 2. Install Python dependencies (creates .venv from lockfile)
curl -LsSf https://astral.sh/uv/install.sh | sh  # install uv if needed
uv sync

# 3. Copy example configs
cp scheduler/config.example.json scheduler/config.json
cp scheduler/state.example.json scheduler/state.json

# 4. Configure (see Configuration section below)
# Edit scheduler/config.json with your Discord channels, strategies, etc.

# 5. Build Go scheduler
cd scheduler && /usr/local/go/bin/go build -o ../go-trader . && cd ..

# 6. Test one cycle
./go-trader --config scheduler/config.json --once

# 7. Run continuously
./go-trader --config scheduler/config.json
```

## Installation as systemd Service

```bash
# Set your Discord bot token as an environment variable (NOT in config.json)
export DISCORD_BOT_TOKEN="your-bot-token-here"

# Install service
sudo cp go-trader.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable go-trader
sudo systemctl start go-trader

# Verify
systemctl is-active go-trader
curl -s localhost:8099/status | python3 -m json.tool
```

### Setting the Discord Token in systemd

The bot token should be stored as an environment variable, not in config.json:

```bash
# Option 1: Edit the service file directly
sudo systemctl edit go-trader --full
# Add under [Service]: Environment="DISCORD_BOT_TOKEN=your-token"

# Option 2: Use a drop-in override
sudo systemctl edit go-trader
# Add:
# [Service]
# Environment="DISCORD_BOT_TOKEN=your-token"

sudo systemctl restart go-trader
```

If a token is found in both config.json and the environment variable, the env var takes priority and a warning is logged.

## Configuration

`scheduler/config.json` controls everything:

```json
{
  "interval_seconds": 300,
  "state_file": "scheduler/state.json",
  "discord": {
    "enabled": true,
    "token": "",
    "channels": {
      "spot": "CHANNEL_ID_FOR_SPOT_ALERTS",
      "options": "CHANNEL_ID_FOR_OPTIONS_ALERTS"
    },
    "spot_summary_freq": "hourly",
    "options_summary_freq": "per_check"
  },
  "strategies": [
    {
      "id": "momentum-btc",
      "type": "spot",
      "script": "scripts/check_strategy.py",
      "args": ["momentum", "BTC/USDT", "1h"],
      "capital": 1000,
      "max_drawdown_pct": 60,
      "interval_seconds": 300
    }
  ]
}
```

### Discord Setup

The bot posts trading summaries and alerts to Discord:

| Setting | Description |
|---------|-------------|
| `discord.enabled` | Enable/disable Discord notifications |
| `discord.token` | Leave blank — use `DISCORD_BOT_TOKEN` env var instead |
| `discord.channels.spot` | Channel ID for spot summaries (📈 hourly + immediate trade alerts) |
| `discord.channels.options` | Channel ID for options summaries (🎯 per-check, split by Deribit/IBKR) |
| `discord.spot_summary_freq` | `"hourly"` (default) or `"per_check"` (every 5 min) |
| `discord.options_summary_freq` | `"per_check"` (default, every 20 min) or `"hourly"` |

**To get channel/server IDs:** Enable Developer Mode in Discord (Settings → Advanced), then right-click a channel or server → Copy ID.

**If using with OpenClaw:** Add the channels to OpenClaw's Discord guild allowlist so the shared bot can post there:
```bash
openclaw config set "channels.discord.guilds.<GUILD_ID>.channels.<CHANNEL_ID>.requireMention" false
```

### Strategy Configuration

Each strategy entry requires:

| Field | Description |
|-------|-------------|
| `id` | Unique identifier (e.g., `momentum-btc`, `deribit-vol-eth`) |
| `type` | `"spot"` or `"options"` |
| `script` | Python script path (relative to project root) |
| `args` | Arguments passed to the script |
| `capital` | Starting capital in USD (default: $1,000) |
| `max_drawdown_pct` | Circuit breaker threshold (spot: 60%, options: 20%) |
| `interval_seconds` | Per-strategy check interval (0 = use global) |
| `theta_harvest` | (Optional) Early exit config for sold options |

### Theta Harvesting (Options)

```json
{
  "theta_harvest": {
    "enabled": true,
    "profit_target_pct": 60,
    "stop_loss_pct": 200,
    "min_dte_close": 3
  }
}
```

Closes sold options early when profit target is hit (e.g., captured 60% of premium), loss exceeds stop (e.g., 2× premium), or expiry is too close (avoid gamma risk).

## Architecture

```
Go scheduler (always running, ~8MB idle)
  ↓ spot: every 5min | options: every 20min
    python3 scripts/check_strategy.py <strategy> <symbol> <timeframe> [symbol_b] → JSON
    python3 scripts/check_options.py <strategy> <underlying> <positions_json>    → JSON
    python3 scripts/check_options_ibkr.py <strategy> <underlying> <positions>    → JSON
  ↓ processes signals, executes paper trades, manages risk
  ↓ marks options to market via Deribit REST API (live prices every cycle)
  ↓ saves state → scheduler/state.json (atomic writes, survives restarts)
  ↓ HTTP status → localhost:8099/status (live prices + real-time P&L)
  ↓ Discord → spot channel (hourly) + options channel (per check)
```

**Why this design:** Python gets the quant libraries (pandas, numpy, scipy, CCXT). Go gets memory efficiency. 30+ strategies cost ~220MB peak for ~30 seconds, then ~8MB idle. Down from 1.6GB with persistent Python processes.

## Strategies

### Spot (14 strategies, 5min interval, $1K each)

| Strategy | Tokens | Timeframe | Description |
|----------|--------|-----------|-------------|
| `momentum` | BTC, ETH, SOL | 1h | Rate of change breakouts |
| `rsi` | BTC, ETH, SOL | 1h | Buy oversold, sell overbought |
| `macd` | BTC, ETH | 1h | MACD/signal line crossovers |
| `volume_weighted` | BTC, ETH, SOL | 1h | Trend + volume confirmation |
| `pairs_spread` | BTC/ETH, BTC/SOL, ETH/SOL | 1d | Spread z-score stat arb |

### Options — Deribit + IBKR/CME (16 strategies, 20min interval, $1K each)

Same 6 strategies run on both exchanges for head-to-head comparison:

| Strategy | Description |
|----------|-------------|
| `vol_mean_reversion` | High IV → sell strangles, Low IV → buy straddles |
| `momentum_options` | ROC breakout → buy directional options |
| `protective_puts` | Buy 12% OTM puts, 45 DTE |
| `covered_calls` | Sell 12% OTM calls, 21 DTE |
| `wheel` | Sell 6% OTM puts, 37 DTE, ~2% premium |
| `butterfly` | Buy ITM, Sell 2× ATM, Buy OTM (±5% wings), 30 DTE |

### Portfolio-Aware Options Scoring

New options trades are scored against existing positions:
- Strike distance — rejects overlapping strikes (<5% apart), rewards diversification
- Expiry spread — rewards different expiration dates
- Greek balancing — rewards delta-neutral, penalizes concentration
- Max 4 positions per strategy, min score 0.3 to execute

## Live Option Pricing

Options positions are marked to market with live Deribit prices every cycle:

- `scheduler/deribit.go` fetches live mark prices via Deribit REST API
- Smart fallback maps paper trading expiries to nearest real Deribit expiry (7-day tolerance)
- IBKR positions use Deribit prices as proxy (same underlying/strikes)
- `scripts/deribit_utils.py` fetches real expiries and strikes for new trades

## Build & Deploy

```bash
# Build (only needed when scheduler/*.go files change)
cd scheduler && /usr/local/go/bin/go build -o ../go-trader . && cd ..

# Restart service
sudo systemctl restart go-trader

# Python script changes take effect on next cycle (no rebuild needed)
# Config changes: just restart the service (no rebuild)
# Service file changes: daemon-reload then restart
sudo systemctl daemon-reload && sudo systemctl restart go-trader
```

## Monitoring

```bash
# Service status
systemctl status go-trader

# Live status (prices + P&L)
curl -s localhost:8099/status | python3 -m json.tool

# Health check
curl -s localhost:8099/health

# Recent logs
journalctl -u go-trader -n 50

# Manual strategy check
uv run python scripts/check_strategy.py momentum BTC/USDT 1h
uv run python scripts/check_options.py vol_mean_reversion BTC '[]'
uv run python scripts/check_price.py BTC/USDT ETH/USDT SOL/USDT
```

## Risk Management

- **Per-strategy circuit breakers** — pause trading when max drawdown exceeded (24h cooldown)
- **Consecutive loss tracking** — 5 losses in a row triggers 1h pause
- **Spot:** max 95% capital per position
- **Options:** max 4 positions per strategy, portfolio-aware scoring, Greek balancing
- **Theta harvesting** — configurable early exit on sold options (profit target + stop loss)

## File Structure

```
go-trader/
├── scheduler/              # Go scheduler source
│   ├── main.go             # Main loop, strategy orchestration
│   ├── config.go           # Config parsing + validation
│   ├── executor.go         # Python subprocess runner
│   ├── state.go            # State persistence (atomic writes)
│   ├── portfolio.go        # Spot position tracking
│   ├── options.go          # Options position tracking, Greeks
│   ├── risk.go             # Drawdown, circuit breakers
│   ├── risk_test.go        # Risk management tests
│   ├── deribit.go          # Deribit REST API for live pricing
│   ├── discord.go          # Discord notifications
│   ├── server.go           # HTTP status endpoint
│   ├── logger.go           # Logging
│   ├── fees.go             # Trading fee calculations
│   ├── config.json         # Strategy configuration (gitignored)
│   ├── config.example.json # Example config template
│   ├── state.json          # Runtime state (gitignored)
│   └── state.example.json  # Example state template
├── scripts/                # Stateless Python check scripts
│   ├── check_strategy.py   # Spot strategy checker (Binance via CCXT)
│   ├── check_options.py    # Deribit options checker
│   ├── check_options_ibkr.py # IBKR/CME options checker
│   ├── check_price.py      # Multi-symbol price fetcher
│   └── deribit_utils.py    # Deribit expiry/strike lookup
├── strategies/             # Spot strategy logic
│   ├── strategies.py       # Trading strategies
│   └── indicators.py       # Technical indicators
├── options/                # Options trading logic
│   ├── options_adapter.py  # Deribit adapter, Black-Scholes
│   ├── ibkr_adapter.py     # IBKR/CME adapter
│   ├── options_strategies.py # Strategy definitions
│   └── options_risk.py     # Options risk management
├── core/                   # Shared utilities
│   ├── data_fetcher.py     # OHLCV data fetching
│   └── storage.py          # Local DB config
├── backtest/               # Backtesting tools
├── archive/                # Archived/unused code
├── CLAUDE.md               # AI agent project context
├── SKILL.md                # OpenClaw setup skill
├── ISSUES.md               # Known issues tracker
├── go-trader.service       # systemd unit file
└── pyproject.toml          # Python dependencies (managed by uv)
```

## Trading Fees & Slippage

| Market | Fee | Slippage |
|--------|-----|----------|
| Binance Spot | 0.1% taker | ±0.05% random |
| Deribit Options | 0.03% of premium | — |
| IBKR/CME Options | $0.25/contract | — |

## Dependencies

**Python** (managed by [uv](https://github.com/astral-sh/uv)):
- ccxt, pandas, numpy, scipy
- Install: `uv sync` (creates .venv from lockfile)

**Go** (1.23+): Standard library only, no external deps.

**System:** systemd for service management.

## Troubleshooting

| Problem | Solution |
|---------|----------|
| No Discord messages | Check `DISCORD_BOT_TOKEN` env var is set, channel IDs are correct, bot has Send Messages permission |
| Service won't start | `journalctl -u go-trader -n 50` |
| Stale prices | Check exchange API connectivity, look for `[WARN] Price fetch failed` in logs |
| Strategy not trading | Check circuit breaker status in `/status`, verify strategy params |
| Reset all positions | `cp scheduler/state.example.json scheduler/state.json && systemctl restart go-trader` |

## Regeneration Prompt

To rebuild this entire system from scratch, give an AI this prompt:

> Build a Go + Python hybrid trading bot called "go-trader".
>
> **Go scheduler** (single always-running binary, ~8MB RAM):
> - Reads a JSON config listing N strategies, each with: id, type (spot/options), script path, args, capital, risk params, and per-strategy `interval_seconds`
> - Main loop ticks at the shortest strategy interval (currently 300s). Each tick, only runs strategies whose individual interval has elapsed since last run
> - Sequentially spawns each due strategy's Python script via `.venv/bin/python3` (isolated uv environment), reads JSON output from stdout, processes the signal
> - Manages all state in memory: portfolios per strategy (cash + positions), trade history, risk state (drawdown kill switch, circuit breakers, daily loss limits, consecutive loss tracking)
> - For spot: tracks positions by symbol, simulates market fills at current price with slippage (±0.05%), applies trading fees (0.1% Binance taker), calculates portfolio value
> - For options: tracks positions with premium, Greeks (delta/gamma/theta/vega), expiry dates, auto-expires worthless OTM options; applies exchange-specific fees (Deribit 0.03%, IBKR $0.25/contract)
> - **Live option pricing** via Deribit REST API (`scheduler/deribit.go`):
>   - Fetches live mark prices from Deribit ticker endpoint every cycle
>   - Updates `CurrentValueUSD` for ALL option positions (Deribit + IBKR) with real market data
>   - IBKR positions use Deribit as pricing proxy (same underlying/strikes)
>   - Smart expiry mapping: tries exact instrument match first, falls back to nearest real expiry within 7-day tolerance
> - Passes existing option positions as JSON to Python scripts for portfolio-aware trade scoring
> - Saves/loads state to a human-readable JSON file (atomic write via tmp + rename)
> - On startup, initializes new strategies from config and auto-prunes removed strategies from state
> - HTTP status endpoint (localhost:8099/status) with live prices and real-time P&L per strategy
> - Discord notifications: separate channels for spot (hourly summaries) and options (per-check summaries split by Deribit/IBKR). Bot token read from `DISCORD_BOT_TOKEN` env var. Trade alerts posted immediately.
> - Graceful shutdown on SIGINT/SIGTERM — saves state before exit
> - `--once` flag for single cycle testing, `--config` flag for config path
> - Theta harvesting: configurable early exit on sold options (profit target %, stop loss %, min DTE)
> - Config validation: checks script paths exist, strategy IDs unique, capital > 0, drawdown in range
>
> **Python check scripts** in `scripts/` (stateless, run-and-exit):
> - `check_strategy.py <strategy> <symbol> <timeframe> [symbol_b]` — OHLCV via CCXT, technical analysis, JSON output
> - `check_options.py <strategy> <underlying> <positions_json>` — Deribit options with real expiry/strike lookup via `deribit_utils.py`
> - `check_options_ibkr.py <strategy> <underlying> <positions_json>` — IBKR/CME options with CME Micro contract specs
> - `check_price.py <symbols...>` — current prices as JSON
> - `deribit_utils.py` — Deribit REST API helpers for real expiry/strike discovery
>
> **Strategies:** 14 spot (momentum, RSI, MACD, volume weighted, pairs across BTC/ETH/SOL), 16 options (vol mean reversion, momentum, protective puts, covered calls, wheel, butterfly on BTC/ETH via both Deribit and IBKR).
>
> **Tech:** Go 1.23+ (stdlib only), Python 3.12+ with uv (ccxt, pandas, numpy). systemd service. No file logging (stdout only).
