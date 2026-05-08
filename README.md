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

**Quick flow for a new server:** tell OpenClaw or Hermes `install https://github.com/richkuo/go-trader and init`.

### AI Agent Setup (Recommended)

The fastest way to get running. Give your AI agent the [Agent Setup Guide](SKILL.md) — it's fully self-contained with the repo URL, step-by-step instructions, and exact prompts. The agent will clone the repo, install dependencies, walk you through configuration (Discord channels, strategy selection, risk settings), build the binary, and start the service.

For non-Claude agents (Codex, Gemini, etc.), see [AGENTS.md](AGENTS.md) for the equivalent project context and PR conventions.

**Raw link for agents:** `https://raw.githubusercontent.com/richkuo/go-trader/main/SKILL.md`

Using [OpenClaw](https://openclaw.ai) or [Hermes](https://hermes-agent.nousresearch.com/)? Just say:

> "Set up go-trader"

### Interactive Setup (go-trader init)

After building the binary, run the interactive config wizard — the easiest way to generate a config without manual JSON editing:

```bash
./go-trader init
```

The wizard walks you through asset selection, strategy types (spot/options/perps), platform selection, capital and risk settings, and Discord configuration, then writes a ready-to-use `scheduler/config.json`. Defaults to a minimal BTC spot starter — say yes to everything for a full multi-platform setup. Risk settings (warn threshold, portfolio kill-switch) are only prompted when live trading is selected.

For scripted/automated deployments (e.g. from OpenClaw, Hermes, or CI), use `--json` to generate a config non-interactively:

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

Full spot strategy suite on Hyperliquid perpetual futures. Strategies are auto-discovered at `go-trader init` time and include long-only, bidirectional, and short-focused entries: `momentum`, `sma_crossover`, `ema_crossover`, `rsi`, `bollinger_bands`, `macd`, `mean_reversion`, `volume_weighted`, `triple_ema`, `tema_cross`, `rsi_macd_combo`, `triple_ema_bidir`, `session_breakout`, `donchian_breakout`, `chart_pattern`, `liquidity_sweeps`, `bear_pullback_st`, `vwap_rejection_st`.

Direction is per-strategy via `direction: "long" | "short" | "both"` (#658). `long` (default) opens longs only; `short` opens shorts only and never flips into long; `both` opens shorts from flat and flips long↔short on reversals. Bidirectional strategies (`triple_ema_bidir`, `donchian_breakout`, `chart_pattern`, `liquidity_sweeps`) run best with `direction: "both"`; short-focused strategies (`bear_pullback_st`, `vwap_rejection_st`) require `direction: "short"` or `"both"`. The legacy `allow_shorts` boolean is migrated automatically (`false`→`"long"`, `true`→`"both"`).

Live mode requires `HYPERLIQUID_SECRET_KEY` env var. Paper mode simulates trades without a key.

Multiple HL perps strategies (and `type: "manual"` peers) can share a coin on the same wallet (#491, #619). They land on a single on-chain position; per-strategy SQLite bookkeeping keeps the legs separated. `LoadConfig` enforces that peer strategies on the same coin share `margin_mode` and exchange `leverage`, and rejects multiple trailing-stop owners (cancel/replace each cycle would race). Fixed-distance stops are allowed across peers; reduce-only TPs and SLs are sized per strategy and placed on-chain as N-tier ladders (#604, #615). Sub-account isolation is the only path for fully independent direction/leverage/margin per strategy.

### Futures (1h interval, ES/NQ/MES/MNQ/CL/GC)

TopStep futures support `momentum`, `mean_reversion`, `rsi`, `macd`, `breakout`, `session_breakout`, `tema_cross`, and `tema_cross_bd` (bidirectional triple-EMA crossover).

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
  "config_version": 14,
  "interval_seconds": 3600,
  "db_file": "scheduler/state.db",
  "log_dir": "logs",
  "auto_update": "daily",
  "status_port": 8099,
  "risk_free_rate": 0.04,
  "default_stop_loss_atr_mult": 1.0,
  "portfolio_risk": {
    "max_drawdown_pct": 25,
    "max_notional_usd": 0,
    "warn_threshold_pct": 60
  },
  "regime": {
    "enabled": false,
    "period": 14,
    "adx_threshold": 20
  },
  "discord": {
    "enabled": true,
    "token": "",
    "owner_id": "",
    "channels": { "spot": "CHANNEL_ID", "options": "CHANNEL_ID", "hyperliquid": "CHANNEL_ID", "topstep": "CHANNEL_ID", "robinhood": "CHANNEL_ID", "okx": "CHANNEL_ID", "luno": "CHANNEL_ID" },
    "trade_alert_channels": { "hyperliquid": "TRADE_CHANNEL_ID" }
  },
  "platforms": {
    "hyperliquid": { "risk": { "max_drawdown_pct": 50 } }
  },
  "strategies": [ ... ]
}
```

`config_version` is bumped automatically by `go-trader init` and migrated on startup. Recent migrations: v9 added `stop_loss_pct`/`stop_loss_margin_pct`/`margin_mode` for HL perps; v11 added the `regime` block; v13 reshaped `open_strategy`/`close_strategies` into co-located `{name, params}` refs (#642); v14 replaced `allow_shorts` with the `direction` enum (#658).

### Portfolio Risk

| Field | Description | Default |
|-------|-------------|---------|
| `portfolio_risk.max_drawdown_pct` | Kill switch — halt all trading if portfolio drops this % from peak | 25 |
| `portfolio_risk.max_notional_usd` | Hard cap on total notional exposure (0 = disabled) | 0 |
| `portfolio_risk.warn_threshold_pct` | Emit a Discord/Telegram warning when drawdown reaches this % of `max_drawdown_pct` (repeats every cycle while in band) | 60 |
| `risk_free_rate` | Annualized risk-free rate used in Sharpe-ratio calculations (e.g. `0.04` for 4%); `null`/omitted → default rate | 0.04 |
| `status_port` | HTTP status server port; auto-falls-back up to 5 ports on collision. Override via `--status-port` CLI flag. | 8099 |
| `default_stop_loss_atr_mult` | Top-level fallback ATR multiplier used to arm fixed-ATR stops on HL perps strategies that omit all five `stop_loss_*` / `trailing_stop_*` fields (#605/#606). Set to `0` to opt every such strategy out fleet-wide. | 1.0 |

### Regime Detection

Optional ADX+DI 3-state market regime gate (`trending_up` / `trending_down` / `ranging`). Computed once per check from the same OHLCV the strategy uses, then forwarded to entry/exit evaluators. Per-strategy `allowed_regimes` blocks new entries when the current regime isn't in the list (closes always pass).

```json
{
  "regime": { "enabled": true, "period": 14, "adx_threshold": 20 },
  "strategies": [
    { "id": "hl-momentum-btc", "allowed_regimes": ["trending_up", "trending_down"], ... }
  ]
}
```

| Field | Description | Default |
|-------|-------------|---------|
| `regime.enabled` | Compute regime label each cycle and persist on the trade row | false |
| `regime.period` | ADX/DI lookback in bars | 14 |
| `regime.adx_threshold` | ADX value below which the regime is labeled `ranging` | 20 |
| `<strategy>.allowed_regimes` | Optional whitelist of regime labels under which new entries may open | (no gate) |

`regime.enabled` toggles require restart; `allowed_regimes` is SIGHUP-reloadable. Options strategies don't currently emit a regime label.

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
| `discord.trade_alert_channels` | Optional override map (same key scheme) routing trade-fill alerts to a different channel from summaries (#573). A `stratType` key (e.g. `"perps"`) reroutes that type across all platforms; falls back to `channels` when unset. SIGHUP-reloadable. |
| `telegram.trade_alert_channels` | Same override for Telegram |
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
| `type` | `"spot"`, `"options"`, `"perps"`, `"futures"`, or `"manual"` (HL perps tracking strategy for hand-placed positions, #571) | Required |
| `platform` | `"binanceus"`, `"deribit"`, `"ibkr"`, `"hyperliquid"`, `"topstep"`, `"robinhood"`, `"okx"`, or `"luno"` | Required |
| `script` | Python script path (relative) — auto-filled for `type: "manual"` | Required |
| `args` | Arguments passed to script | Required |
| `capital` | Starting capital in USD | 1000 |
| `max_drawdown_pct` | Circuit breaker threshold — peak-relative for spot/options/futures; margin-relative for perps (#292) | Spot: 5%, Options: 10%, Perps: 5% |
| `interval_seconds` | Check interval (0 = use global) | 0 |
| `htf_filter` | Enable higher-timeframe trend filter | false |
| `open_strategy` | Co-located ref `{"name": "...", "params": {...}}` overriding the entry strategy (v13+ shape). Bare strings are migrated automatically (#642). Falls back to `args[0]` if omitted. | null |
| `close_strategies` | Ordered list of co-located refs `[{"name": "...", "params": {...}}, ...]`; the one with the largest `close_fraction` per cycle wins (max-wins). Each ref carries its own params, so per-close knobs no longer leak into the open strategy (#642). | null |
| `leverage` | Perps only — exchange leverage used for margin drawdown and HL `update_leverage` (#497). If `sizing_leverage` is omitted, this also controls order sizing for backwards compatibility. | 1 |
| `sizing_leverage` | Perps only — position-sizing multiplier used for `cash * sizing_leverage` order budgets (#497). Set lower than exchange `leverage` to run high exchange leverage without oversized orders. | `leverage` |
| `margin_per_trade_usd` | HL perps only — fixed per-trade margin override; notional becomes `min(margin_per_trade_usd, cash) × leverage`, replacing the legacy 0.95 buffer (#520). | omitted |
| `stop_loss_pct` | HL perps only — reduce-only stop-loss trigger as a % of entry price. Omit to fall back to the next field in priority order. Explicit `0` opts out. | omitted |
| `stop_loss_margin_pct` | HL perps only — leverage-aware alternative; price % derives as `stop_loss_margin_pct / leverage` (#490/#497). Mutually exclusive with the other four stop-loss fields when positive. | omitted |
| `stop_loss_atr_mult` | HL perps only — fixed ATR-based stop placed at `avg_cost ± mult * entry_atr` once `entry_atr` is known (#563). Never updated after arming. Mutually exclusive with the other four stop fields when positive. | omitted |
| `trailing_stop_pct` | HL perps only — synthetic trailing stop distance (% from high-water mark). Cancel/replace debounced by `trailing_stop_min_move_pct` (#501/#502). Mutually exclusive with the other four stop fields. Capped at 50%. | omitted |
| `trailing_stop_atr_mult` | HL perps only — ATR-distance trailing stop frozen at open (`mult * entry_atr / avg_cost`, #507). Mutually exclusive with the other four stop fields. | omitted |
| `trailing_stop_min_move_pct` | HL trailing stop only — minimum trigger-price move (%) before issuing a cancel/replace. Reduces churn against HL's 1000-OID account cap. | 0.5 |
| `margin_mode` | HL perps only — `"isolated"` or `"cross"`; applied via `update_leverage` from flat (#486) | `isolated` |
| `direction` | Perps only — `"long"`, `"short"`, or `"both"` (#658). Replaces the legacy `allow_shorts` boolean, which is migrated automatically. Long-only strategies should keep `"long"`; bidirectional or short-focused strategies opt in. | `"long"` |
| `allowed_regimes` | Optional whitelist of regime labels (`trending_up` / `trending_down` / `ranging`) under which entries may open. Closes always run. Requires top-level `regime.enabled`. | (no gate) |
| `theta_harvest` | Early exit config for sold options | null |

When all five HL perps stop-loss / trailing-stop fields are omitted, the scheduler arms a fixed ATR stop at `default_stop_loss_atr_mult * entry_atr` (default `1.0`, top-level config, #605/#606). Set the top-level field to `0` to opt every such strategy out fleet-wide. Same-coin peers may carry independent fixed-distance stops, but at most one peer may run a trailing stop (cancel/replace would race).

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

## Manual Trading on Hyperliquid

For positions you want to open by hand on Hyperliquid (or via TradingView alerts) but still have the scheduler track P&L, place stops/TPs, and surface them in Discord summaries, declare a `type: "manual"` strategy in `config.json` and use the `manual-open` / `manual-close` CLI subcommands. The scheduler auto-fills `script`, `args`, `interval_seconds`, and a default `stop_loss_atr_mult: 1.0` + `tiered_tp_atr_live` close strategy.

```bash
# Open a long with reduce-only SL + TP placed inline (#633)
./go-trader manual-open --strategy hl-manual-btc --side long --notional 500 --atr 250

# Or record a manual fill from outside the scheduler
./go-trader manual-open --strategy hl-manual-btc --side short --size 0.05 --record-only --fill-price 64500

# Close (full or partial)
./go-trader manual-close --strategy hl-manual-btc
./go-trader manual-close --strategy hl-manual-btc --fraction 0.5
```

Sizing is mutually exclusive: `--size` (coin units) / `--notional` (USD) / `--margin` (USD margin). When `--atr` is omitted, the scheduler arms a leverage-aware fallback ATR (`0.1 * fill_price / leverage`). On manual-open the SL and N-tier TPs are placed inline so the position is never naked between fill and the next cycle; if the queue insert fails after a successful fill, the scheduler auto-flattens and cancels the protective orders (#635).

---

## Backfilling Hyperliquid Fees

`exchange_fee = 0` rows recorded before the real-fee fix (#587) can be rewritten from Hyperliquid `userFills`:

```bash
./go-trader backfill hl-fees --strategy hl-btc-momentum               # dry-run, single strategy
./go-trader backfill hl-fees --all                                     # dry-run, all HL strategies
./go-trader backfill hl-fees --strategy hl-btc-momentum --apply        # apply changes
./go-trader backfill hl-fees --all --apply --reset-cash                # also replay strategies.cash
```

The command refuses `--apply` while another `go-trader` process is alive on the same DB to avoid concurrent writes (#591).

---

## Build & Deploy

The canonical update path is `scripts/update.sh` — an atomic `git pull --ff-only` → `uv sync` → `go build` (with version stamp) → optional `systemctl restart go-trader` flow that aborts on the first failure and is shared between operators and the auto-update DM flow (#648). Running `go build` against stale Python (or vice versa) will trip the startup compatibility probe and refuse to start (#645/#646), so prefer the script over hand-rolled rebuilds.

```bash
sudo bash scripts/update.sh --restart           # update + restart service
bash scripts/update.sh                          # update without restart
```

| Change | Action |
|--------|--------|
| Go or Python source | `sudo bash scripts/update.sh --restart` |
| Config changes (hot-reloadable subset) | `systemctl kill -s HUP go-trader` — applies capital/drawdown/intervals/params/risk knobs/channels/`allowed_regimes` in place; rejects shape changes (strategy add/remove, type/platform, leverage/`direction` with open positions) |
| Config changes (roster, script/args/type/platform, `regime` block) | `systemctl restart go-trader` |
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
- **Per-trade Hyperliquid stop-loss** — every HL perps strategy gets an exchange-side reduce-only trigger; pick one of five mutually-exclusive fields when positive: `stop_loss_pct` (price %), `stop_loss_margin_pct` (margin-aware, price-% = `stop_loss_margin_pct / leverage`, #490), `stop_loss_atr_mult` (fixed ATR distance armed at open, #563), `trailing_stop_pct` (high-water trailing %, #501), or `trailing_stop_atr_mult` (ATR-distance trailing frozen at open, #507). When all five are omitted, the scheduler arms a fixed ATR stop at top-level `default_stop_loss_atr_mult` (default `1.0`, #605). Set the top-level field to `0` to opt out fleet-wide; set any per-strategy field to explicit `0` to opt that strategy out. Trailing stops debounce cancel/replace via `trailing_stop_min_move_pct` to stay under HL's 1000-OID account cap.
- **On-chain N-tier TP/SL ladders** — HL perps strategies running `tiered_tp_atr` or `tiered_tp_atr_live` close evaluators place reduce-only TP orders on-chain at the configured ATR multiples (default `[{1×, 0.5}, {2×, 1.0}]`); the final tier auto-flattens any per-tier rounding dust (#604/#615/#629). On full close, all open SL+TP OIDs are cancelled in one shot. Same-coin peers each carry their own per-strategy SL/TP sized to their virtual quantity (#604).
- **Market regime gate** — when `regime.enabled: true`, per-strategy `allowed_regimes` blocks new entries when the current ADX+DI label (`trending_up` / `trending_down` / `ranging`) isn't in the list; close legs always run (#539–#559).
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
