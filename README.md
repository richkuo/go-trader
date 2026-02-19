# go-trader — Crypto Trading Bot

A Go + Python hybrid trading system. A single Go binary (~8MB RAM) orchestrates 30+ paper trading strategies across spot and options markets by spawning short-lived Python scripts.

**Spot markets** via Binance (CCXT): momentum, RSI, MACD, volume-weighted, and pairs spread strategies on BTC, ETH, and SOL.

**Options markets** via Deribit + IBKR/CME: volatility mean reversion, momentum, protective puts, covered calls, wheel, and butterfly strategies on BTC and ETH — running head-to-head across both exchanges.

**Discord alerts**: Separate channels for spot and options summaries, with immediate trade notifications.

---

## Getting Started

### AI Agent Setup (Recommended)

The fastest way to get running. Any AI coding agent can set this up by following the [Agent Setup Guide](SKILL.md).

Using [OpenClaw](https://openclaw.ai)? Just say:

> "Set up go-trader"

Your agent will clone the repo, install dependencies, walk you through configuration (Discord channels, strategy selection, risk settings), build the binary, and start the service.

### Manual Setup

```bash
# 1. Clone
git clone https://github.com/richkuo/go-trader.git
cd go-trader

# 2. Install Python dependencies
curl -LsSf https://astral.sh/uv/install.sh | sh   # install uv if needed
uv sync                                             # creates .venv from lockfile

# 3. Create config from template
cp scheduler/config.example.json scheduler/config.json

# 4. Edit scheduler/config.json
#    - Add Discord channel IDs (or set enabled: false)
#    - Adjust strategies, capital, risk settings as needed
#    - Leave discord.token blank (use env var instead)

# 5. Build
cd scheduler && go build -o ../go-trader . && cd ..

# 6. Test one cycle
./go-trader --config scheduler/config.json --once

# 7. Run as service
export DISCORD_BOT_TOKEN="your-token"
sudo cp go-trader.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now go-trader

# 8. Verify
curl -s localhost:8099/status | python3 -m json.tool
```

---

## Architecture

```
Go scheduler (always running, ~8MB idle)
  ↓ spot: every 5min | options: every 20min
    .venv/bin/python3 scripts/check_strategy.py → JSON signal
    .venv/bin/python3 scripts/check_options.py  → JSON signal (Deribit)
    .venv/bin/python3 scripts/check_options_ibkr.py → JSON signal (IBKR/CME)
  ↓ processes signals, executes paper trades, manages risk
  ↓ marks options to market via Deribit REST API (live prices every cycle)
  ↓ saves state → scheduler/state.json (atomic writes, survives restarts)
  ↓ HTTP status → localhost:8099/status
  ↓ Discord → spot channel + options channel
```

Python gets the quant libraries (pandas, numpy, scipy, CCXT). Go gets memory efficiency. 30+ strategies cost ~220MB peak for ~30 seconds, then ~8MB idle.

---

## Strategies

### Spot (14 strategies, 5min interval)

| Strategy | Assets | Timeframe | Description |
|----------|--------|-----------|-------------|
| `momentum` | BTC, ETH, SOL | 1h | Rate of change breakouts |
| `rsi` | BTC, ETH, SOL | 1h | Buy oversold, sell overbought |
| `macd` | BTC, ETH | 1h | MACD/signal line crossovers |
| `volume_weighted` | BTC, ETH, SOL | 1h | Trend + volume confirmation |
| `pairs_spread` | BTC/ETH, BTC/SOL, ETH/SOL | 1d | Spread z-score stat arb |

### Options (16 strategies, 20min interval)

Same 6 strategies on both Deribit and IBKR/CME for comparison:

| Strategy | Description |
|----------|-------------|
| `vol_mean_reversion` | High IV → sell strangles, Low IV → buy straddles |
| `momentum_options` | ROC breakout → buy directional options |
| `protective_puts` | Buy 12% OTM puts, 45 DTE |
| `covered_calls` | Sell 12% OTM calls, 21 DTE |
| `wheel` | Sell 6% OTM puts, 37 DTE |
| `butterfly` | Buy ITM, Sell 2× ATM, Buy OTM (±5% wings), 30 DTE |

New options trades are scored against existing positions for strike distance, expiry spread, and Greek balancing. Max 4 positions per strategy, min score 0.3 to execute.

---

## Configuration Reference

### `scheduler/config.json`

```json
{
  "interval_seconds": 300,
  "state_file": "scheduler/state.json",
  "discord": {
    "enabled": true,
    "token": "",
    "channels": { "spot": "CHANNEL_ID", "options": "CHANNEL_ID" },
    "spot_summary_freq": "hourly",
    "options_summary_freq": "per_check"
  },
  "strategies": [ ... ]
}
```

### Discord Settings

| Field | Description |
|-------|-------------|
| `discord.enabled` | Enable/disable Discord notifications |
| `discord.token` | Leave blank — use `DISCORD_BOT_TOKEN` env var |
| `discord.channels.spot` | Channel for spot summaries (📈 hourly + trade alerts) |
| `discord.channels.options` | Channel for options summaries (🎯 per-check, Deribit/IBKR split) |
| `discord.spot_summary_freq` | `"hourly"` (default) or `"per_check"` |
| `discord.options_summary_freq` | `"per_check"` (default) or `"hourly"` |

### Strategy Entry

| Field | Description | Default |
|-------|-------------|---------|
| `id` | Unique identifier (e.g., `momentum-btc`) | Required |
| `type` | `"spot"` or `"options"` | Required |
| `script` | Python script path (relative) | Required |
| `args` | Arguments passed to script | Required |
| `capital` | Starting capital in USD | 1000 |
| `max_drawdown_pct` | Circuit breaker threshold | Spot: 60%, Options: 20% |
| `interval_seconds` | Check interval (0 = use global) | 0 |
| `theta_harvest` | Early exit config for sold options | null |

### Theta Harvesting (Options)

Closes sold options early based on profit target, stop loss, or approaching expiry:

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

| Field | Description |
|-------|-------------|
| `profit_target_pct` | Close when this % of premium captured (e.g., 60 = take profit at 60%) |
| `stop_loss_pct` | Close if loss exceeds this % of premium (e.g., 200 = 2× premium) |
| `min_dte_close` | Force-close positions with fewer than N days to expiry |

---

## Build & Deploy

| Change | Action |
|--------|--------|
| Go code (`scheduler/*.go`) | `cd scheduler && go build -o ../go-trader . && systemctl restart go-trader` |
| Python scripts | `systemctl restart go-trader` (or wait for next cycle) |
| Config changes | `systemctl restart go-trader` |
| Service file changes | `systemctl daemon-reload && systemctl restart go-trader` |

Go path on this server: `/usr/local/go/bin/go`

---

## Monitoring

```bash
systemctl status go-trader              # service health
curl -s localhost:8099/status            # live prices + P&L
curl -s localhost:8099/health            # simple health check
journalctl -u go-trader -n 50           # recent logs
```

---

## Risk Management

- **Per-strategy circuit breakers** — pause trading when max drawdown exceeded (24h cooldown)
- **Consecutive loss tracking** — 5 losses in a row → 1h pause
- **Spot**: max 95% capital per position
- **Options**: max 4 positions per strategy, portfolio-aware scoring
- **Theta harvesting**: configurable early exit on sold options

---

## Trading Fees

| Market | Fee | Slippage |
|--------|-----|----------|
| Binance Spot | 0.1% taker | ±0.05% |
| Deribit Options | 0.03% of premium | — |
| IBKR/CME Options | $0.25/contract | — |

---

## File Structure

```
go-trader/
├── scheduler/              # Go scheduler source + config
│   ├── main.go             # Main loop, strategy orchestration
│   ├── config.go           # Config parsing + validation
│   ├── executor.go         # Python subprocess runner
│   ├── state.go            # State persistence (atomic writes)
│   ├── portfolio.go        # Spot position tracking
│   ├── options.go          # Options positions, Greeks, theta harvest
│   ├── risk.go             # Drawdown, circuit breakers
│   ├── deribit.go          # Deribit REST API for live pricing
│   ├── discord.go          # Discord notifications
│   ├── server.go           # HTTP status endpoint
│   ├── fees.go             # Trading fee calculations
│   ├── logger.go           # Logging
│   ├── config.example.json # Config template
│   └── state.example.json  # State template
├── scripts/                # Stateless Python check scripts
│   ├── check_strategy.py   # Spot checker (Binance via CCXT)
│   ├── check_options.py    # Deribit options checker
│   ├── check_options_ibkr.py # IBKR/CME options checker
│   ├── check_price.py      # Multi-symbol price fetcher
│   └── deribit_utils.py    # Deribit expiry/strike lookup
├── strategies/             # Spot strategy logic + indicators
├── options/                # Options adapters, strategies, risk
├── core/                   # Shared utilities (data fetcher, storage)
├── backtest/               # Backtesting tools
├── SKILL.md                # AI agent setup guide
├── CLAUDE.md               # AI agent project context
├── ISSUES.md               # Known issues tracker
└── go-trader.service       # systemd unit file
```

---

## Dependencies

- **Python 3.12+** — managed by [uv](https://github.com/astral-sh/uv) (ccxt, pandas, numpy, scipy)
- **Go 1.23+** — standard library only
- **systemd** — service management

---

## Troubleshooting

| Problem | Solution |
|---------|----------|
| No Discord messages | Check `DISCORD_BOT_TOKEN` env var, channel IDs, bot permissions |
| Service won't start | `journalctl -u go-trader -n 50` |
| Strategy not trading | Check circuit breaker in `/status`, verify params |
| Reset positions | `cp scheduler/state.example.json scheduler/state.json && systemctl restart go-trader` |
