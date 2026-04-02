# go-trader — Crypto Trading Bot

[![GitHub release](https://img.shields.io/github/v/release/richkuo/go-trader)](https://github.com/richkuo/go-trader/releases/latest)
[![Discord](https://img.shields.io/badge/Discord-Join-5865F2?logo=discord&logoColor=white)](https://discord.com/invite/44BykmWZsP)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A Go + Python hybrid trading system. A single Go binary (~8MB RAM) orchestrates 50+ paper trading strategies across spot, options, perpetual futures, and CME futures markets by spawning short-lived Python scripts.

**Spot markets** via Binance US (CCXT): SMA/EMA crossovers, momentum, RSI, Bollinger Bands, MACD, and pairs spread strategies on BTC, ETH, and SOL.

**Options markets** via Deribit + IBKR/CME: volatility mean reversion, momentum, protective puts, and covered calls on BTC and ETH — running head-to-head across both exchanges.

**Perpetual futures** via Hyperliquid: full spot strategy suite on any HL-listed asset, with paper and live trading support.

**CME futures** via TopStep: momentum, mean reversion, RSI, MACD, breakout on ES, NQ, MES, MNQ, CL, GC — paper mode uses Yahoo Finance, live mode via TopStepX API.

**Crypto** via Robinhood: spot crypto trading using the full strategy suite (SMA, EMA, RSI, MACD, etc.) — paper mode uses Yahoo Finance for OHLCV data, live mode places real orders via robin_stocks with TOTP MFA.

**Discord alerts**: Per-platform channels for spot, options, hyperliquid, topstep, robinhood, and okx summaries, with immediate trade notifications. When a new release is detected, the bot DMs you directly — reply **yes** and it upgrades, rebuilds, and restarts itself automatically.

Supported platforms: Binance US, Deribit, IBKR/CME, Hyperliquid, TopStep, Robinhood.

## Community

Join the Discord: [https://discord.gg/46d7Fa2dXz](https://discord.gg/46d7Fa2dXz)

---

## Getting Started

### AI Agent Setup (Recommended)

The fastest way to get running. Give your AI agent the [Agent Setup Guide](SKILL.md) — it's fully self-contained with the repo URL, step-by-step instructions, and exact prompts. The agent will clone the repo, install dependencies, walk you through configuration (Discord channels, strategy selection, risk settings), build the binary, and start the service.

**Raw link for agents:** `https://raw.githubusercontent.com/richkuo/go-trader/main/SKILL.md`

Using [OpenClaw](https://openclaw.ai)? Just say:

> "Set up go-trader"

### Interactive Setup (go-trader init)

After building the binary, run the interactive config wizard — the easiest way to generate a config without manual JSON editing:

```bash
./go-trader init
```

The wizard walks you through asset selection, strategy types (spot/options/perps), platform selection, capital and risk settings, and Discord configuration, then writes a ready-to-use `scheduler/config.json`.

For scripted/automated deployments (e.g. from OpenClaw or CI), use `--json` to generate a config non-interactively:

```bash
./go-trader init --json '{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10}' --output config.json
```

### Manual Setup

```bash
# 1. Clone
git clone https://github.com/richkuo/go-trader.git
cd go-trader

# 2. Install Python dependencies
curl -LsSf https://astral.sh/uv/install.sh | sh   # install uv if needed
uv sync                                             # creates .venv from lockfile

# 3. Build (requires Go 1.26.0)
cd scheduler && go build -o ../go-trader . && cd ..

# 4. Generate config
./go-trader init                                    # interactive wizard (recommended)
# — or —
./go-trader init --json '{"assets":["BTC"],...}'   # non-interactive (scripted)
# — or —
cp scheduler/config.example.json scheduler/config.json
# then edit scheduler/config.json manually

# 5. Test one cycle
./go-trader --config scheduler/config.json --once

# 6. Run as service
export DISCORD_BOT_TOKEN="your-token"
sudo cp go-trader.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now go-trader

# 7. Verify
curl -s localhost:8099/status | python3 -m json.tool
```

---

## Architecture

```
Go scheduler (always running, ~8MB idle)
  ↓ spot/perps: every 1h | options: every 4h
    .venv/bin/python3 shared_scripts/check_strategy.py    → JSON signal (spot)
    .venv/bin/python3 shared_scripts/check_options.py     → JSON signal (--platform=deribit|ibkr)
    .venv/bin/python3 shared_scripts/check_hyperliquid.py → JSON signal (perps)
    .venv/bin/python3 shared_scripts/check_topstep.py     → JSON signal (futures)
    .venv/bin/python3 shared_scripts/check_robinhood.py  → JSON signal (crypto)
    .venv/bin/python3 shared_scripts/check_okx.py         → JSON signal (spot/perps)
    .venv/bin/python3 shared_scripts/check_price.py       → live prices
  ↓ processes signals, executes paper trades, manages risk
  ↓ marks options to market via Deribit REST API (live prices every cycle)
  ↓ saves state → scheduler/state.json (atomic writes, survives restarts)
  ↓ HTTP status → localhost:8099/status
  ↓ Discord → per-platform channels (spot, options, hyperliquid, topstep, robinhood, okx)

Platform adapters (Python):
  platforms/binanceus/adapter.py   — spot (CCXT)
  platforms/deribit/adapter.py     — options (live quotes, real expiries/strikes)
  platforms/ibkr/adapter.py        — options (CME Micro, Black-Scholes pricing)
  platforms/hyperliquid/adapter.py — perps (paper + live, SDK)
  platforms/topstep/adapter.py     — futures (CME, paper via yfinance + live via TopStepX)
  platforms/robinhood/adapter.py   — crypto (paper via yfinance + live via robin_stocks)
  platforms/okx/adapter.py         — spot + perps + options (CCXT, paper + live)
```

Python gets the quant libraries (pandas, numpy, scipy, CCXT). Go gets memory efficiency. 50+ strategies cost ~220MB peak for ~30 seconds, then ~8MB idle.

---

## Strategies

### Spot (10 strategies, 1h interval, BTC/ETH/SOL)

| Strategy | Description |
|----------|-------------|
| `sma_crossover` | Simple moving average crossover |
| `ema_crossover` | Exponential moving average crossover |
| `momentum` | Rate of change breakouts |
| `rsi` | Buy oversold, sell overbought |
| `bollinger_bands` | Mean reversion at band extremes |
| `macd` | MACD/signal line crossovers |
| `mean_reversion` | Statistical mean reversion |
| `volume_weighted` | Trend + volume confirmation |
| `triple_ema` | Triple EMA crossover |
| `rsi_macd_combo` | RSI and MACD confluence |
| `pairs_spread` | BTC/ETH, BTC/SOL, ETH/SOL spread z-score stat arb (1d) |

### Options (4 strategies, 4h interval, BTC/ETH)

Same 4 strategies on both Deribit and IBKR/CME for comparison:

| Strategy | Description |
|----------|-------------|
| `vol_mean_reversion` | High IV → sell strangles, Low IV → buy straddles |
| `momentum_options` | ROC breakout → buy directional options |
| `protective_puts` | Buy 12% OTM puts, 45 DTE |
| `covered_calls` | Sell 12% OTM calls, 21 DTE |

New options trades are scored against existing positions for strike distance, expiry spread, and Greek balancing. Max 4 positions per strategy, min score 0.3 to execute.

### Perps (10 strategies, 1h interval, any HL-listed asset)

Full spot strategy suite on Hyperliquid perpetual futures. Strategies are auto-discovered at `go-trader init` time: `momentum`, `sma_crossover`, `ema_crossover`, `rsi`, `bollinger_bands`, `macd`, `mean_reversion`, `volume_weighted`, `triple_ema`, `rsi_macd_combo`.

Live mode requires `HYPERLIQUID_SECRET_KEY` env var. Paper mode simulates trades without a key.

### Futures (5 strategies, 1h interval, ES/NQ/MES/MNQ/CL/GC)

| Strategy | Description |
|----------|-------------|
| `momentum` | Rate of change breakouts |
| `mean_reversion` | Statistical mean reversion |
| `rsi` | Buy oversold, sell overbought |
| `macd` | MACD/signal line crossovers |
| `breakout` | Price breakout detection |

CME futures on TopStep. Live mode requires `TOPSTEP_API_KEY`, `TOPSTEP_API_SECRET`, `TOPSTEP_ACCOUNT_ID` env vars. Paper mode uses Yahoo Finance for price data.

### Robinhood Crypto (10 strategies, 1h interval)

Same spot strategy suite as Binance US, running on Robinhood crypto. Paper mode uses Yahoo Finance for OHLCV data (no credentials needed). Live mode requires `ROBINHOOD_USERNAME`, `ROBINHOOD_PASSWORD`, `ROBINHOOD_TOTP_SECRET` env vars.

### OKX (spot + perps + options, BTC/ETH/SOL)

Full spot and perpetual swap strategies on OKX. Options support via `check_options.py --platform=okx` for BTC/ETH options. Uses CCXT for all API interactions.

Paper mode uses public OKX API (no credentials). Live mode requires `OKX_API_KEY`, `OKX_API_SECRET`, `OKX_PASSPHRASE` env vars. Set `OKX_SANDBOX=1` for the OKX demo trading environment.

### Robinhood Stock Options (6 strategies, 4h interval)

US equity options on SPY, QQQ, AAPL, etc. using the same options strategies as Deribit/IBKR (covered_calls, protective_puts, momentum_options, vol_mean_reversion, wheel, butterfly). Paper mode uses Black-Scholes pricing. Live mode uses robin_stocks for real options chains and greeks.

---

## Platforms

| Platform | Type | Assets | Features |
|----------|------|--------|----------|
| Binance US | Spot | BTC, ETH, SOL | CCXT, paper trading |
| Deribit | Options | BTC, ETH | Live quotes, real expiries/strikes |
| IBKR/CME | Options | BTC, ETH | CME Micro contracts, Black-Scholes pricing |
| Hyperliquid | Perps | BTC, ETH, SOL | Paper + live trading via SDK |
| TopStep | Futures | ES, NQ, MES, MNQ, CL, GC | Paper (yfinance) + live trading via TopStepX API |
| Robinhood | Crypto | BTC, ETH, SOL, DOGE, etc. | Paper (yfinance) + live trading via robin_stocks |
| Robinhood | Options | SPY, QQQ, AAPL, MSFT, etc. | Paper (Black-Scholes) + live chains via robin_stocks |
| OKX | Spot + Perps + Options | BTC, ETH, SOL | CCXT, paper + live, MiCA/EU licensed |

---

## Configuration Reference

### `scheduler/config.json`

Use `./go-trader init` (interactive) or `./go-trader init --json '...'` (scripted) to generate this file. The full structure:

```json
{
  "config_version": 2,
  "interval_seconds": 3600,
  "state_file": "scheduler/state.json",
  "log_dir": "logs",
  "auto_update": "daily",
  "portfolio_risk": {
    "max_drawdown_pct": 25,
    "max_notional_usd": 0
  },
  "discord": {
    "enabled": true,
    "token": "",
    "owner_id": "",
    "channels": { "spot": "CHANNEL_ID", "options": "CHANNEL_ID", "hyperliquid": "CHANNEL_ID", "topstep": "CHANNEL_ID", "robinhood": "CHANNEL_ID", "okx": "CHANNEL_ID" }
  },
  "platforms": {
    "hyperliquid": {
      "state_file": "platforms/hyperliquid/state.json"
    },
    "topstep": {
      "state_file": "platforms/topstep/state.json"
    },
    "robinhood": {
      "state_file": "platforms/robinhood/state.json"
    },
    "okx": {
      "state_file": "platforms/okx/state.json"
    }
  },
  "strategies": [ ... ]
}
```

### Portfolio Risk

| Field | Description | Default |
|-------|-------------|---------|
| `portfolio_risk.max_drawdown_pct` | Kill switch — halt all trading if portfolio drops this % from peak | 25 |
| `portfolio_risk.max_notional_usd` | Hard cap on total notional exposure (0 = disabled) | 0 |

### Correlation Tracking

Monitor portfolio-level directional exposure across all strategies. Disabled by default — opt in by setting `correlation.enabled: true`.

```json
{
  "correlation": {
    "enabled": true,
    "max_concentration_pct": 60,
    "max_same_direction_pct": 75
  }
}
```

| Field | Description | Default |
|-------|-------------|---------|
| `correlation.enabled` | Enable correlation tracking and warnings | false |
| `correlation.max_concentration_pct` | Warn when one asset exceeds this % of portfolio gross exposure | 60 |
| `correlation.max_same_direction_pct` | Warn when more than this % of strategies on an asset share a direction | 75 |

When thresholds are exceeded, warnings are sent to all active Discord channels and DM'd to the owner (if configured). The correlation snapshot is also available via the `/status` endpoint.

### Auto-Update & DM Upgrades

Set `auto_update` in config to enable automatic update checks:

| Value | Behavior |
|-------|----------|
| `"off"` | No automatic checking (default) |
| `"daily"` | Check once per day |
| `"heartbeat"` | Check every scheduler cycle |

When an update is found, all active Discord channels receive a notification. If `discord.owner_id` is set, the bot also **DMs you directly**:

```
Update available: `b204163a` → `f8c2e91b`
Would you like me to upgrade automatically? (yes/no)
```

Reply **yes** → the bot runs `git pull`, rebuilds the binary, and restarts itself. Reply **no** or ignore → skipped.

After a restart following an upgrade, any new config fields introduced since your `config_version` are collected via DM (with a 10-minute reply window per field). Replies are written back to `config.json` atomically before the bot confirms completion.

To get your Discord user ID: right-click your username in Discord → **Copy User ID** (requires Developer Mode: Settings → Advanced).

### Discord Settings

| Field | Description |
|-------|-------------|
| `discord.enabled` | Enable/disable Discord notifications |
| `discord.token` | Leave blank — use `DISCORD_BOT_TOKEN` env var |
| `discord.owner_id` | Your Discord user ID — enables DM upgrade prompts and post-upgrade config migration. Use `DISCORD_OWNER_ID` env var. |
| `discord.channels` | Map of channel IDs keyed by platform/type — `"spot"`, `"options"`, `"hyperliquid"`, `"topstep"`, `"robinhood"`, `"okx"`, etc. Options post per-check; others post hourly + on trades. |
| `config_version` | Schema version (set automatically by `go-trader init`; migration runs on startup when behind current version) |

### Strategy Entry

| Field | Description | Default |
|-------|-------------|---------|
| `id` | Unique identifier (e.g., `momentum-btc`, `hl-momentum-btc`) | Required |
| `type` | `"spot"`, `"options"`, `"perps"`, or `"futures"` | Required |
| `platform` | `"binanceus"`, `"deribit"`, `"ibkr"`, `"hyperliquid"`, `"topstep"`, `"robinhood"`, or `"okx"` | Required |
| `script` | Python script path (relative) | Required |
| `args` | Arguments passed to script | Required |
| `capital` | Starting capital in USD | 1000 |
| `max_drawdown_pct` | Circuit breaker threshold (from peak) | Spot: 5%, Options: 10%, Perps: 5% |
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

- **Portfolio kill switch** — halt all trading if portfolio drawdown exceeds threshold (default: 25%)
- **Notional cap** — optional hard limit on total notional exposure
- **Correlation tracking** — per-asset directional exposure monitoring; warns when a single asset exceeds concentration threshold (default: 60%) or too many strategies share the same direction (default: 75%); opt-in via `correlation.enabled`
- **Per-strategy circuit breakers** — pause trading when max drawdown exceeded (24h cooldown)
- **Consecutive loss tracking** — 5 losses in a row → 1h pause
- **Spot**: max 95% capital per position
- **Options**: max 4 positions per strategy, portfolio-aware scoring
- **Theta harvesting**: configurable early exit on sold options

---

## Trading Fees

| Market | Fee | Slippage |
|--------|-----|----------|
| Binance US Spot | 0.1% taker | ±0.05% |
| Deribit Options | 0.03% of premium | — |
| IBKR/CME Options | $0.25/contract | — |
| Hyperliquid Perps | 0.035% taker | ±0.05% |
| TopStep Futures | Per-contract (configurable) | ±0.05% |
| Robinhood Crypto | No commission (spread embedded) | ±0.05% |
| Robinhood Options | $0.03/contract (regulatory fee) | — |

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
│   ├── discord.go          # Discord gateway (discordgo), SendMessage/SendDM/AskDM
│   ├── updater.go          # Update checker, DM upgrade flow, applyUpgrade/restartSelf
│   ├── correlation.go      # Per-asset directional exposure tracking
│   ├── config_migration.go # Config version registry, MigrateConfig, DM-based migration
│   ├── server.go           # HTTP status endpoint
│   ├── fees.go             # Trading fee calculations
│   ├── pricer.go           # OptionPricer interface
│   ├── ibkr_pricer.go      # IBKR Black-Scholes pricer
│   ├── init.go             # go-trader init wizard
│   ├── prompt.go           # Interactive prompt helpers
│   ├── logger.go           # Logging
│   ├── config.example.json # Config template
│   └── state.example.json  # State template
├── shared_scripts/         # Stateless Python entry-point scripts
│   ├── check_strategy.py   # Spot checker (Binance US via CCXT)
│   ├── check_options.py    # Options checker (--platform=deribit|ibkr)
│   ├── check_hyperliquid.py # Hyperliquid perps checker
│   ├── check_okx.py         # OKX spot/perps checker
│   ├── check_topstep.py    # TopStep futures checker
│   ├── check_robinhood.py  # Robinhood crypto checker
│   └── check_price.py      # Multi-symbol price fetcher
├── platforms/              # Platform-specific adapters
│   ├── binanceus/          # BinanceUS spot adapter
│   ├── deribit/            # Deribit options adapter
│   ├── ibkr/               # IBKR/CME options adapter
│   ├── hyperliquid/        # Hyperliquid perps adapter
│   ├── topstep/            # TopStep futures adapter
│   ├── robinhood/          # Robinhood crypto adapter
│   └── okx/                # OKX spot/perps/options adapter (CCXT)
├── shared_tools/           # Shared Python utilities (pricing, exchange_base, storage)
├── shared_strategies/      # Shared strategy logic (spot/, options/, futures/)
├── core/                   # Legacy data utilities (used by backtest)
├── strategies/             # Legacy spot strategy logic (used by backtest)
├── backtest/               # Backtesting tools
├── archive/                # Retired/unused modules
├── SKILL.md                # AI agent setup guide
├── CLAUDE.md               # AI agent project context
├── ISSUES.md               # Known issues tracker
└── go-trader.service       # systemd unit file
```

---

## Dependencies

- **Python 3.12+** — managed by [uv](https://github.com/astral-sh/uv) (ccxt, pandas, numpy, scipy, hyperliquid-python-sdk)
- **Go 1.26.0** — [`github.com/bwmarrin/discordgo`](https://github.com/bwmarrin/discordgo) for WebSocket gateway (DM support)
- **systemd** — service management

---

## Troubleshooting

| Problem | Solution |
|---------|----------|
| No Discord messages | Check `DISCORD_BOT_TOKEN` env var, channel IDs, bot permissions |
| Service won't start | `journalctl -u go-trader -n 50` |
| Strategy not trading | Check circuit breaker in `/status`, verify params |
| Reset positions | `cp scheduler/state.example.json scheduler/state.json && systemctl restart go-trader` |
| Hyperliquid live mode fails | Set `HYPERLIQUID_SECRET_KEY` env var; paper mode works without it |
| TopStep live mode fails | Set `TOPSTEP_API_KEY`, `TOPSTEP_API_SECRET`, `TOPSTEP_ACCOUNT_ID` env vars |
| Robinhood live mode fails | Set `ROBINHOOD_USERNAME`, `ROBINHOOD_PASSWORD`, `ROBINHOOD_TOTP_SECRET` env vars |
| OKX live mode fails | Set `OKX_API_KEY`, `OKX_API_SECRET`, `OKX_PASSPHRASE` env vars; use `OKX_SANDBOX=1` for demo |

---

## Risk Disclaimer

This application is provided for informational and educational purposes only. It does not constitute financial advice, investment advice, or a recommendation to buy or sell any asset.

Trading involves substantial risk of loss. Past performance is not indicative of future results. You may lose some or all of your invested capital. Only trade with funds you can afford to lose.

This application is an automated tool and makes no guarantees regarding accuracy, profitability, or outcomes. Market conditions can change rapidly and the application may not react appropriately to all scenarios.

By using this application, you acknowledge that you are solely responsible for your own investment decisions and any resulting gains or losses. The creators, developers, and operators of this application accept no liability for any financial losses incurred through its use.

*This is not financial advice. Trade at your own risk.*
