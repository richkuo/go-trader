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

Supported platforms: Binance US, Deribit, IBKR/CME, Hyperliquid, TopStep, Robinhood, OKX, Luno.

## Community

Join the Discord: [https://discord.gg/46d7Fa2dXz](https://discord.gg/46d7Fa2dXz)

---

## Getting Started

**Quick flow for a new server:** tell OpenClaw `install https://github.com/richkuo/go-trader and init`.

### AI Agent Setup (Recommended)

The fastest way to get running. Give your AI agent the [Agent Setup Guide](SKILL.md) — it's fully self-contained with the repo URL, step-by-step instructions, and exact prompts. The agent will clone the repo, install dependencies, walk you through configuration (Discord channels, strategy selection, risk settings), build the binary, and start the service.

For non-Claude agents (Codex, Gemini, etc.), see [AGENTS.md](AGENTS.md) for the equivalent project context and PR conventions.

**Raw link for agents:** `https://raw.githubusercontent.com/richkuo/go-trader/main/SKILL.md`

Using [OpenClaw](https://openclaw.ai)? Just say:

> "Set up go-trader"

### Interactive Setup (go-trader init)

After building the binary, run the interactive config wizard — the easiest way to generate a config without manual JSON editing:

```bash
./go-trader init
```

The wizard walks you through asset selection, strategy types (spot/options/perps), platform selection, capital and risk settings, and Discord configuration, then writes a ready-to-use `scheduler/config.json`. Defaults to a minimal BTC spot starter — say yes to everything for a full multi-platform setup. Risk settings (warn threshold, portfolio kill-switch) are only prompted when live trading is selected.

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

# 3. Build (requires Go 1.26.2)
VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
cd scheduler && go build -ldflags "-X main.Version=$VER" -o ../go-trader . && cd ..

# 4. Generate config
./go-trader init                                    # interactive wizard (recommended)
# — or —
./go-trader init --json '{"assets":["BTC"],...}'   # non-interactive (scripted)
# — or —
cp scheduler/config.example.json scheduler/config.json
# then edit scheduler/config.json manually

# 5. Test one cycle
./go-trader --config scheduler/config.json --once

# 6. Run as service (installs, reloads, enables, and starts — survives reboot)
export DISCORD_BOT_TOKEN="your-token"
sudo bash scripts/install-service.sh

# 7. Verify
curl -s localhost:8099/status | python3 -m json.tool
```

`scripts/install-service.sh` copies the unit into `/etc/systemd/system/`, runs `daemon-reload`, enables the service for boot, starts it, and pre-creates the `logs/` directory with the right ownership.

### Running multiple instances (paper / live / testing)

For ad hoc variants deployed alongside the main instance, use the templated unit at `systemd/go-trader@.service`. Each instance lives under `/opt/go-trader-<name>/` and is addressed as `go-trader@<name>.service`. Pre-populate the instance directory before installing the template:

```bash
# 1. Create the instance directory and copy in the binary + config
sudo mkdir -p /opt/go-trader-paper-testing/scheduler
sudo cp go-trader /opt/go-trader-paper-testing/
sudo cp scheduler/config.json /opt/go-trader-paper-testing/scheduler/
sudo chown -R go-trader:go-trader /opt/go-trader-paper-testing

# 2. Install the templated unit for this instance
sudo bash scripts/install-service.sh systemd/go-trader@.service paper-testing
# → installs the template, enables + starts go-trader@paper-testing.service
```

Or copy `go-trader.service` to a named variant, edit paths, and install:

```bash
sudo bash scripts/install-service.sh go-trader-paper-testing.service
```

Set `NO_START=1` to enable without starting immediately.

---

## Architecture

```
Go scheduler (always running, ~8MB idle)
  ↓ every cycle, runs short-lived Python check scripts
  ↓ receives JSON signals, executes paper/live trades, manages risk
  ↓ saves state to scheduler/state.db and serves localhost:8099/status
  ↓ posts Discord/Telegram summaries and alerts

Python adapters:
  binanceus, deribit, ibkr, hyperliquid, topstep, robinhood, okx, luno
```

Python gets the quant libraries (pandas, numpy, scipy, CCXT). Go gets memory efficiency. 50+ strategies cost ~220MB peak for ~30 seconds, then ~8MB idle.

---

## Strategies

### Spot (1h interval, BTC/ETH/SOL)

Includes `sma_crossover`, `ema_crossover`, `momentum`, `rsi`, `bollinger_bands`, `macd`, `mean_reversion`, `volume_weighted`, `triple_ema`, `rsi_macd_combo`, and `pairs_spread`.

### Options (4h interval, BTC/ETH)

Deribit and IBKR/CME run the same core set: `vol_mean_reversion`, `momentum_options`, `protective_puts`, and `covered_calls`.

New options trades are scored against existing positions for strike distance, expiry spread, and Greek balancing. Max 4 positions per strategy, min score 0.3 to execute.

### Perps (1h interval, any HL-listed asset)

Full spot strategy suite on Hyperliquid perpetual futures. Strategies are auto-discovered at `go-trader init` time: `momentum`, `sma_crossover`, `ema_crossover`, `rsi`, `bollinger_bands`, `macd`, `mean_reversion`, `volume_weighted`, `triple_ema`, `rsi_macd_combo`, `triple_ema_bidir`, `session_breakout`.

Most strategies are long-only; `triple_ema_bidir` is the first bidirectional strategy (long on bullish EMA stack, short on bearish) and runs with `allow_shorts: true` so the scheduler opens shorts from flat and flips long↔short on reversals. New bidirectional strategies opt in per-strategy via the same flag — existing long-only strategies keep their semantics.

Live mode requires `HYPERLIQUID_SECRET_KEY` env var. Paper mode simulates trades without a key.

Multiple HL perps strategies can share a coin on the same wallet (#491). They land on a single on-chain position; per-strategy SQLite bookkeeping keeps the legs separated. `LoadConfig` enforces that peer strategies on the same coin share `margin_mode` and `leverage`, and that at most one peer carries a non-zero stop-loss (reduce-only triggers race on the shared position). Importantly, omitted `stop_loss_pct` / `stop_loss_margin_pct` are treated as an explicit opt-out for same-coin peers — the `max_drawdown_pct` auto-derive only fires for a single strategy on its coin; set one explicit positive stop-loss owner if a shared-position trigger is desired (#494). Sub-account isolation is the only correct path for fully independent direction/leverage/margin per strategy.

### Futures (1h interval, ES/NQ/MES/MNQ/CL/GC)

TopStep futures support `momentum`, `mean_reversion`, `rsi`, `macd`, `breakout`, and `session_breakout`.

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
| Luno | Spot | BTC, ETH, etc. | South African crypto exchange |

---

## Configuration Reference

### `scheduler/config.json`

Use `./go-trader init` (interactive) or `./go-trader init --json '...'` (scripted) to generate this file. The full structure:

```json
{
  "config_version": 9,
  "interval_seconds": 3600,
  "db_file": "scheduler/state.db",
  "log_dir": "logs",
  "auto_update": "daily",
  "status_port": 8099,
  "risk_free_rate": 0.04,
  "portfolio_risk": {
    "max_drawdown_pct": 25,
    "max_notional_usd": 0,
    "warn_threshold_pct": 60
  },
  "discord": {
    "enabled": true,
    "token": "",
    "owner_id": "",
    "channels": { "spot": "CHANNEL_ID", "options": "CHANNEL_ID", "hyperliquid": "CHANNEL_ID", "topstep": "CHANNEL_ID", "robinhood": "CHANNEL_ID", "okx": "CHANNEL_ID", "luno": "CHANNEL_ID" }
  },
  "platforms": {
    "hyperliquid": { "risk": { "max_drawdown_pct": 50 } }
  },
  "strategies": [ ... ]
}
```

### Portfolio Risk

| Field | Description | Default |
|-------|-------------|---------|
| `portfolio_risk.max_drawdown_pct` | Kill switch — halt all trading if portfolio drops this % from peak | 25 |
| `portfolio_risk.max_notional_usd` | Hard cap on total notional exposure (0 = disabled) | 0 |
| `portfolio_risk.warn_threshold_pct` | Emit a Discord/Telegram warning when drawdown reaches this % of `max_drawdown_pct` (repeats every cycle while in band) | 60 |
| `risk_free_rate` | Annualized risk-free rate used in Sharpe-ratio calculations (e.g. `0.04` for 4%); `null`/omitted → default rate | 0.04 |
| `status_port` | HTTP status server port; auto-falls-back up to 5 ports on collision. Override via `--status-port` CLI flag. | 8099 |

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

### Summary Frequency

Control how often each channel posts a summary via the top-level `summary_frequency` map. Keys match the `discord.channels` keys (e.g. `"spot"`, `"hyperliquid"`). Trades always force an immediate post regardless of the configured cadence.

```json
{
  "summary_frequency": {
    "spot": "hourly",
    "options": "every",
    "hyperliquid": "every",
    "topstep": "30m"
  }
}
```

| Value | Behavior |
|-------|----------|
| `"every"` / `"per_check"` / `"always"` | Post every scheduler cycle |
| `"hourly"` | Post once per hour (wall-clock) |
| `"daily"` | Post once per day (wall-clock) |
| `"30m"`, `"2h"`, etc. | Post when this much wall-clock time has elapsed since the last post (Go duration syntax) |
| `""` (omitted) | Legacy default — options/perps/futures post every channel run; spot posts hourly |

Cadence is wall-clock based and survives restarts: per-channel last-post timestamps are persisted in SQLite (`app_state.last_summary_post`), so variable scheduler wake-ups and SIGHUP reloads no longer reset the throttle window (#474).

### Strategy Entry

| Field | Description | Default |
|-------|-------------|---------|
| `id` | Unique identifier (e.g., `momentum-btc`, `hl-momentum-btc`) | Required |
| `type` | `"spot"`, `"options"`, `"perps"`, or `"futures"` | Required |
| `platform` | `"binanceus"`, `"deribit"`, `"ibkr"`, `"hyperliquid"`, `"topstep"`, `"robinhood"`, or `"okx"` | Required |
| `script` | Python script path (relative) | Required |
| `args` | Arguments passed to script | Required |
| `capital` | Starting capital in USD | 1000 |
| `max_drawdown_pct` | Circuit breaker threshold — peak-relative for spot/options/futures; margin-relative for perps (#292) | Spot: 5%, Options: 10%, Perps: 5% |
| `interval_seconds` | Check interval (0 = use global) | 0 |
| `htf_filter` | Enable higher-timeframe trend filter | false |
| `params` | Custom strategy parameters (e.g. `{"multiplier": 2.0}`) | null |
| `open_strategy` | Override the entry strategy name (otherwise read from `args[0]`) | null |
| `close_strategies` | Ordered list of exit evaluators; the one with the largest `close_fraction` wins (#483) | null |
| `disable_implicit_close` | Suppress the legacy signal-reversal close when no `close_strategies` is configured | false |
| `stop_loss_pct` | HL perps only — reduce-only stop-loss trigger as a % of entry price. Omit to auto-derive from `max_drawdown_pct` (capped at 50%) when this strategy is the only HL perps strategy on its coin; same-coin peers must name one explicit positive owner (#484, #494). Explicit `0` opts out. | omitted (auto for sole owner) |
| `stop_loss_margin_pct` | HL perps only — leverage-aware alternative to `stop_loss_pct`; price % is derived as `stop_loss_margin_pct / leverage` so the trigger tracks margin loss as leverage changes (#490). Mutually exclusive with `stop_loss_pct` unless both are explicit `0`. Omit to auto-derive only when sole strategy on the coin; same-coin peers default to opt-out (#494). | omitted |
| `margin_mode` | HL perps only — `"isolated"` or `"cross"`; applied via `update_leverage` from flat (#486) | `isolated` |
| `allow_shorts` | Per-strategy opt-in for bidirectional perps (`triple_ema_bidir`, etc.) | false |
| `theta_harvest` | Early exit config for sold options | null |

### Custom Strategy Parameters

Override default strategy parameters per-strategy. Useful for tuning indicators to specific assets or timeframes:

```json
{
  "id": "ts-st-es",
  "type": "futures",
  "platform": "topstep",
  "script": "shared_scripts/check_topstep.py",
  "args": ["supertrend", "ES", "5m", "--mode=paper"],
  "capital": 5000,
  "max_drawdown_pct": 10,
  "interval_seconds": 300,
  "params": {"multiplier": 2.0, "atr_period": 10}
}
```

The `params` object is passed to `apply_strategy()` and merged with the strategy's built-in defaults. Any key in `params` overrides the corresponding default. For strategies that also receive runtime data (e.g. Hyperliquid/OKX funding rates), runtime values take priority over config params.

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
| Go code (`scheduler/*.go`) | `VER=$(git describe --tags --always --dirty); cd scheduler && go build -ldflags "-X main.Version=$VER" -o ../go-trader . && systemctl restart go-trader` |
| Python scripts | `systemctl restart go-trader` (or wait for next cycle) |
| Config changes (hot-reloadable subset) | `systemctl kill -s HUP go-trader` — applies capital/drawdown/intervals/params/risk knobs/channels in place; logs rejection if the change isn't reload-safe |
| Config changes (roster, script/args/type/platform, leverage with open positions) | `systemctl restart go-trader` |
| Service file changes | `systemctl daemon-reload && systemctl restart go-trader` |

---

## Monitoring

```bash
systemctl status go-trader              # service health
curl -s localhost:8099/status            # live prices + P&L (default port 8099; override with --status-port)
curl -s localhost:8099/health            # simple health check
journalctl -u go-trader -n 50           # recent logs
```

Discord strategy summaries include columns: `Init | Value | PnL | PnL% | DD | Wallet% | Tf | Int | #T | W/L` (compact widths; DD rendered as whole percent), a `Book Sharpe (realized, annualized)` footer line, and the go-trader version + PID in the summary title (CI builds stamp the version via `git describe --tags --always --dirty` ldflags so released binaries no longer report `dev`, #465). The `okx-options` and `robinhood-options` channel keys route OKX/Robinhood options summaries separately from their spot/perps channels. `#T` and `W/L` are derived from the lifetime trades table — close legs are grouped per position so partial closes collapse into one round trip (#471), and the in-memory `RiskState` counters have been removed so SQLite is the only source of truth (#472).

Open-position lines now show two extra fragments when applicable (#485): `SL: $<trigger_px> (<signed_pct>%)` whenever a Hyperliquid stop-loss trigger is in place (the percent is sign-flipped for shorts so it always reads as the loss if hit), and `<N>x ($<margin> margin)` for leveraged perps positions, where margin is `notional / leverage` rounded to whole dollars. Spot and 1× perps stay clean.

---

## Risk Management

- **Portfolio kill switch** — halt all trading if portfolio drawdown exceeds threshold (default: 25%); when it fires, the scheduler also submits real close orders on Hyperliquid, OKX perps, Robinhood crypto, and TopStep futures live positions and only clears virtual state after every platform confirms flat (#341, #345, #346, #347, #350)
- **Notional cap** — optional hard limit on total notional exposure
- **Correlation tracking** — per-asset directional exposure monitoring; warns when a single asset exceeds concentration threshold (default: 60%) or too many strategies share the same direction (default: 75%); opt-in via `correlation.enabled`
- **Per-strategy circuit breakers** — pause trading when max drawdown exceeded (24h cooldown); spot/options/futures measure drawdown peak-relative, perps measure it relative to deployed margin so leveraged margin wipes fire the breaker in time (#292). When a per-strategy CB fires on HL perps, OKX perps, Robinhood crypto, or TopStep futures the scheduler enqueues and drains a reduce-only on-chain close (#356, #360, #361, #362); OKX spot and Robinhood options have no safe auto-close primitive and surface an `operator-required` warning on every cycle until the operator flattens manually (#363).
- **Per-trade Hyperliquid stop-loss** — every single-strategy-per-coin HL perps strategy with `max_drawdown_pct` set automatically gets an exchange-side reduce-only trigger on each open (capped at 50%, #484). Override with `stop_loss_pct` (price-%) or `stop_loss_margin_pct` (margin-aware; price-% = `stop_loss_margin_pct / leverage`, #490); set either to explicit `0` to opt out. Same-coin peer groups skip the auto-derive and require one explicit stop-loss owner (#494). HL caps open trigger orders at 1000 per account (#481).
- **HL margin mode** — defaults to `isolated` so a single losing strategy can't drain margin from unrelated positions (#486). Override per-strategy with `margin_mode: "cross"`. Applied via `update_leverage` from flat only; HL rejects mode/leverage changes on an open position.
- **Consecutive loss tracking** — 5 losses in a row → 1h pause
- **Spot**: max 95% capital per position
- **Options**: max 4 positions per strategy, portfolio-aware scoring
- **Theta harvesting**: configurable early exit on sold options

---

## TradingView Export

Export recorded SQLite trades to a TradingView portfolio-import CSV (`Symbol,Side,Qty,Status,Fill Price,Commission,Closing Time`):

```bash
./go-trader export tradingview --strategy hl-btc-momentum --output tv-hl-btc.csv
./go-trader export tradingview --strategy hl-btc-momentum --strategy okx-eth-breakout --output tv-selected.csv
./go-trader export tradingview --all --output tv-all.csv
```

Built-in mappings cover known OKX and BinanceUS pairs. For platforms/symbols without a safe default, set per-symbol overrides in config:

```json
"tradingview_export": {
  "symbol_overrides": { "hl:BTC": "BYBIT:BTCUSDT" }
}
```

Circuit-breaker close trades are included; their direction is parsed from the trade's `Details` field ("Close long" → sell, "Close short" → buy).

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

Live `--execute` fills on Hyperliquid, OKX, Robinhood, and TopStep record the **exchange-reported** fee (and per-leg fees on multi-leg fills) plus the exchange order ID, so backfills and TradingView exports match the venue ledger instead of the calculated estimate (#453, #461).

---

## File Structure

```
go-trader/
├── scheduler/          # Go scheduler, config, state DB, HTTP status, risk, notifications
├── shared_scripts/     # Python entry points called by the scheduler
├── platforms/          # Exchange adapters
├── shared_tools/       # Shared Python utilities
├── shared_strategies/  # Strategy registry and strategy implementations
├── backtest/           # Backtesting and optimization tools
├── systemd/            # Template service units
├── scripts/            # Install/service helper scripts
├── SKILL.md            # AI agent setup guide
└── AGENTS.md           # Agent project context
```

---

## Dependencies

- **Python 3.12+** — managed by [uv](https://github.com/astral-sh/uv) (ccxt, pandas, numpy, scipy, hyperliquid-python-sdk)
- **Go 1.26.2** — [`github.com/bwmarrin/discordgo`](https://github.com/bwmarrin/discordgo) for WebSocket gateway (DM support)
- **systemd** — service management

---

## Troubleshooting

| Problem | Solution |
|---------|----------|
| No Discord messages | Check `DISCORD_BOT_TOKEN` env var, channel IDs, bot permissions |
| Service won't start | `journalctl -u go-trader -n 50` |
| Service didn't come back after reboot | Unit was installed but not enabled. Run `sudo bash scripts/install-service.sh` (or `systemctl enable <unit>`) — `systemctl start` alone does not survive reboot. |
| Strategy not trading | Check circuit breaker in `/status`, verify params |
| Reset positions | `rm scheduler/state.db && systemctl restart go-trader` |
| Hyperliquid live mode fails | Set `HYPERLIQUID_SECRET_KEY` env var; paper mode works without it |
| TopStep live mode fails | Set `TOPSTEP_API_KEY`, `TOPSTEP_API_SECRET`, `TOPSTEP_ACCOUNT_ID` env vars |
| Robinhood live mode fails | Set `ROBINHOOD_USERNAME`, `ROBINHOOD_PASSWORD`, `ROBINHOOD_TOTP_SECRET` env vars |
| OKX live mode fails | Set `OKX_API_KEY`, `OKX_API_SECRET`, `OKX_PASSPHRASE` env vars; use `OKX_SANDBOX=1` for demo |
| "state DB missing but live strategies configured" warning on startup | The update process likely wiped the repo directory instead of `git pull`ing in place. Restore `scheduler/state.db` from backup, or set `GO_TRADER_ALLOW_MISSING_STATE=1` for a genuine first-run deployment (#339). |

---

## Risk Disclaimer

This application is provided for informational and educational purposes only. It does not constitute financial advice, investment advice, or a recommendation to buy or sell any asset.

Trading involves substantial risk of loss. Past performance is not indicative of future results. You may lose some or all of your invested capital. Only trade with funds you can afford to lose.

This application is an automated tool and makes no guarantees regarding accuracy, profitability, or outcomes. Market conditions can change rapidly and the application may not react appropriately to all scenarios.

By using this application, you acknowledge that you are solely responsible for your own investment decisions and any resulting gains or losses. The creators, developers, and operators of this application accept no liability for any financial losses incurred through its use.

*This is not financial advice. Trade at your own risk.*
