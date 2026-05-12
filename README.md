# go-trader — Crypto Trading Bot

[![GitHub release](https://img.shields.io/github/v/release/richkuo/go-trader)](https://github.com/richkuo/go-trader/releases/latest)
[![Discord](https://img.shields.io/badge/Discord-Join-5865F2?logo=discord&logoColor=white)](https://discord.com/invite/44BykmWZsP)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A Go + Python hybrid trading system. A single Go binary (~8MB idle RAM) orchestrates 50+ strategies across spot, options, perpetual futures, and CME futures by spawning short-lived Python scripts. Both paper and live execution are supported per strategy.

Supported platforms: Binance US, Deribit, IBKR/CME, Hyperliquid, TopStep, Robinhood (crypto + stock options), OKX (spot + perps + options), Luno. Per-platform Discord/Telegram channels post hourly summaries plus immediate trade alerts. When a new release ships, the bot DMs the configured owner — reply **yes** and it pulls, rebuilds, and restarts itself.

Join the Discord: [https://discord.gg/46d7Fa2dXz](https://discord.gg/46d7Fa2dXz)

---

## Getting Started

**Quick flow for a new server:** tell OpenClaw or Hermes `install https://github.com/richkuo/go-trader and init`.

### AI Agent Setup (Recommended)

Give your AI agent [SKILL.md](SKILL.md) (raw: `https://raw.githubusercontent.com/richkuo/go-trader/main/SKILL.md`) — it clones the repo, installs deps, walks through configuration, builds the binary, and starts the service. For non-Claude agents see [AGENTS.md](AGENTS.md). Using [OpenClaw](https://openclaw.ai) or [Hermes](https://hermes-agent.nousresearch.com/)? Just say "Set up go-trader".

### Interactive Setup (go-trader init)

After building the binary, run the config wizard:

```bash
./go-trader init
```

It walks asset/strategy/platform/capital/risk/Discord choices and writes `scheduler/config.json`. Defaults to a minimal BTC spot starter; risk prompts (warn threshold, portfolio kill-switch) appear only when live trading is selected.

For scripted deployments, use `--json`:

```bash
./go-trader init --json '{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10}' --output config.json
```

### Manual Setup

```bash
git clone https://github.com/richkuo/go-trader.git && cd go-trader
curl -LsSf https://astral.sh/uv/install.sh | sh    # install uv if needed
uv sync                                             # Python deps from lockfile

VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
cd scheduler && go build -ldflags "-X main.Version=$VER" -o ../go-trader . && cd ..

./go-trader init                                    # or --json '{...}', or copy config.example.json
./go-trader --config scheduler/config.json --once   # smoke-test one cycle

export DISCORD_BOT_TOKEN="your-token"
sudo bash scripts/install-service.sh                # systemd install + enable + start
curl -s localhost:8099/status | python3 -m json.tool
```

### Running multiple instances

Use the templated unit `systemd/go-trader@.service`; each instance lives under `/opt/go-trader-<name>/`:

```bash
sudo mkdir -p /opt/go-trader-paper-testing/scheduler
sudo cp go-trader /opt/go-trader-paper-testing/
sudo cp scheduler/config.json /opt/go-trader-paper-testing/scheduler/
sudo chown -R go-trader:go-trader /opt/go-trader-paper-testing
sudo bash scripts/install-service.sh systemd/go-trader@.service paper-testing
```

Set `NO_START=1` to enable without starting.

---

## Architecture

```
Go scheduler (always running, ~8MB idle)
  ↓ each cycle, spawns short-lived Python check scripts
  ↓ receives JSON signals, executes paper/live trades, manages risk
  ↓ persists to scheduler/state.db, serves localhost:8099/status
  ↓ posts Discord/Telegram summaries and trade alerts

Python adapters: binanceus, deribit, ibkr, hyperliquid, topstep, robinhood, okx, luno
```

Python provides the quant libraries (pandas, numpy, scipy, CCXT); Go provides memory efficiency. 50+ strategies peak around ~220MB for ~30s, then back to ~8MB idle.

---

## Strategies & Platforms

Strategies are auto-discovered from `shared_strategies/` at `go-trader init` time. Common picks: spot/perps share entries like `sma_crossover`, `ema_crossover`, `momentum`, `rsi`, `bollinger_bands`, `macd`, `mean_reversion`, `triple_ema`, `tema_cross`, `pairs_spread`, `chart_pattern`, `liquidity_sweeps`, `donchian_breakout`, `session_breakout`. Options use `vol_mean_reversion`, `momentum_options`, `protective_puts`, `covered_calls` (plus `wheel` and `butterfly` on Robinhood); new trades are scored vs. existing positions (strike distance, expiry spread, Greek balance). Max 4 positions per options strategy; min score 0.3 to execute.

| Platform | Type | Assets | Live env vars | Paper data |
|---|---|---|---|---|
| Binance US | Spot | BTC, ETH, SOL | — | CCXT public |
| Deribit | Options | BTC, ETH | — | Live quotes |
| IBKR/CME | Options | BTC, ETH | IBKR creds | Black-Scholes |
| Hyperliquid | Perps | any HL-listed | `HYPERLIQUID_SECRET_KEY` | SDK public |
| TopStep | Futures | ES, NQ, MES, MNQ, CL, GC | `TOPSTEP_API_KEY` / `_SECRET` / `_ACCOUNT_ID` | yfinance |
| Robinhood | Crypto | BTC, ETH, SOL, DOGE, … | `ROBINHOOD_USERNAME` / `_PASSWORD` / `_TOTP_SECRET` | yfinance |
| Robinhood | Stock options | SPY, QQQ, AAPL, … | (same as above) | Black-Scholes |
| OKX | Spot + Perps + Options | BTC, ETH, SOL | `OKX_API_KEY` / `_SECRET` / `_PASSPHRASE` (`OKX_SANDBOX=1` for demo) | CCXT public |
| Luno | Spot | BTC, ETH, … | Luno creds | CCXT public |

**Hyperliquid perps direction** — per-strategy `direction: "long" | "short" | "both"`. `long` (default) opens longs only; `short` opens shorts only; `both` flips on reversals. Bidirectional/short-focused strategies (`triple_ema_bidir`, `bear_pullback_st`, `vwap_rejection_st`, `donchian_breakout`, `chart_pattern`, `liquidity_sweeps`) require `"short"` or `"both"`. Legacy `allow_shorts` migrates automatically.

**Coin sharing on Hyperliquid** — multiple HL strategies (including `type: "manual"`) can share a coin/wallet, with per-strategy SQLite bookkeeping over a single on-chain position. Peers must share `margin_mode` + `leverage`; at most one peer may run a trailing stop. Reduce-only SL and N-tier TPs are sized per strategy. Sub-accounts are the only path to fully independent direction/leverage/margin.

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

`config_version` is bumped automatically by `go-trader init` and migrated on startup. Recent migrations: v9 added HL perps stop-loss / margin-mode fields; v11 added the `regime` block; v13 reshaped `open_strategy`/`close_strategies` into co-located `{name, params}` refs; v14 replaced `allow_shorts` with the `direction` enum.

### Portfolio Risk

| Field | Description | Default |
|-------|-------------|---------|
| `portfolio_risk.max_drawdown_pct` | Kill switch — halt all trading if portfolio drops this % from peak | 25 |
| `portfolio_risk.max_notional_usd` | Hard cap on total notional exposure (0 = disabled) | 0 |
| `portfolio_risk.warn_threshold_pct` | Emit a Discord/Telegram warning when drawdown reaches this % of `max_drawdown_pct` (repeats every cycle while in band) | 60 |
| `risk_free_rate` | Annualized risk-free rate used in Sharpe-ratio calculations (e.g. `0.04` for 4%); `null`/omitted → default rate | 0.04 |
| `status_port` | HTTP status server port; auto-falls-back up to 5 ports on collision. Override via `--status-port` CLI flag. | 8099 |
| `default_stop_loss_atr_mult` | Top-level fallback ATR multiplier used to arm fixed-ATR stops on HL perps strategies that omit all five `stop_loss_*` / `trailing_stop_*` fields. Set to `0` to opt every such strategy out fleet-wide. | 1.0 |

### Regime Detection

Optional ADX+DI 3-state gate (`trending_up` / `trending_down` / `ranging`) computed from the strategy's own OHLCV. Per-strategy `allowed_regimes` blocks new entries when the current regime isn't whitelisted (closes always pass). `regime.enabled` requires a restart; `allowed_regimes` is SIGHUP-reloadable. Options strategies don't emit a regime label yet.

```json
{
  "regime": { "enabled": true, "period": 14, "adx_threshold": 20 },
  "strategies": [{ "id": "hl-momentum-btc", "allowed_regimes": ["trending_up", "trending_down"] }]
}
```

`regime.period` defaults to 14, `regime.adx_threshold` to 20 (below this → `ranging`).

### Correlation Tracking

Opt-in via `correlation.enabled: true`. Warns when a single asset exceeds `max_concentration_pct` (default 60) of portfolio gross exposure or `max_same_direction_pct` (default 75) of strategies on an asset share a direction. Warnings go to active Discord channels and the owner DM; snapshot also available at `/status`.

### Auto-Update & DM Upgrades

`auto_update`: `"off"` (default), `"daily"`, or `"heartbeat"` (every cycle). When an update is found, all active Discord channels are notified; if `discord.owner_id` is set, the bot also DMs you `Would you like me to upgrade automatically? (yes/no)`. Reply **yes** → it runs `scripts/update.sh` (git pull → uv sync → go build) and restarts itself.

After an upgrade, any new config fields introduced since your `config_version` are collected via DM (10-minute reply window per field) and written back to `config.json` atomically.

Discord user ID: right-click your username → **Copy User ID** (Developer Mode: Settings → Advanced).

### Discord Settings

| Field | Description |
|---|---|
| `discord.enabled` | Toggle Discord notifications |
| `discord.token` | Leave blank — set `DISCORD_BOT_TOKEN` env var |
| `discord.owner_id` | Discord user ID for DM upgrade prompts + post-upgrade config migration (env: `DISCORD_OWNER_ID`) |
| `discord.channels` | Map of channel IDs keyed by `spot` / `options` / `<platform>` / `<platform>-paper` |
| `discord.trade_alert_channels` | Optional override routing trade-fill alerts to a separate channel; `stratType` keys (e.g. `perps`) reroute that type across all platforms. SIGHUP-reloadable. |
| `telegram.trade_alert_channels` | Same override for Telegram |

### Summary Frequency

Top-level `summary_frequency` map keyed by channel name controls per-channel cadence. Trades always force an immediate post.

```json
{ "summary_frequency": { "spot": "hourly", "hyperliquid": "every", "topstep": "30m" } }
```

Values: `every` / `per_check` / `always` (every cycle), `hourly`, `daily`, Go durations like `30m` / `2h`, or `""` (legacy: options/perps/futures every cycle, spot hourly). Wall-clock based and persisted in SQLite, so restarts and SIGHUP reloads don't reset the throttle.

### Strategy Entry

| Field | Description | Default |
|---|---|---|
| `id` | Unique identifier (e.g. `hl-momentum-btc`) | required |
| `type` | `spot` / `options` / `perps` / `futures` / `manual` (HL hand-placed positions) | required |
| `platform` | `binanceus` / `deribit` / `ibkr` / `hyperliquid` / `topstep` / `robinhood` / `okx` / `luno` | required |
| `script`, `args` | Python entry-point + argv (auto-filled for `manual`) | required |
| `capital` | Starting capital in USD | 1000 |
| `max_drawdown_pct` | Per-strategy CB; peak-relative (spot/options/futures), margin-relative (perps) | spot 5, options 10, perps 5 |
| `interval_seconds` | Check interval (0 → global) | 0 |
| `htf_filter` | Higher-timeframe trend filter | false |
| `open_strategy` | Co-located ref `{name, params}` overriding the entry; falls back to `args[0]` | null |
| `close_strategies` | Ordered `[{name, params}, …]`; largest `close_fraction` per cycle wins | null |
| `leverage` | Perps — exchange leverage (also sizing if `sizing_leverage` omitted) | 1 |
| `sizing_leverage` | Perps — order sizing multiplier (`cash × sizing_leverage`); separate from exchange leverage | `leverage` |
| `margin_per_trade_usd` | HL perps — notional becomes `min(margin_per_trade_usd, cash) × leverage` | omitted |
| `stop_loss_pct` / `stop_loss_margin_pct` / `stop_loss_atr_mult` / `trailing_stop_pct` / `trailing_stop_atr_mult` | HL perps — pick at most one positive value. Margin variant divides by `leverage`; ATR variants arm at open from `entry_atr`; trailing stops cap at 50% and debounce via `trailing_stop_min_move_pct`. All five omitted → scheduler arms `default_stop_loss_atr_mult * entry_atr`; explicit `0` opts out. | omitted |
| `trailing_stop_min_move_pct` | HL trailing stop — minimum move before cancel/replace (HL caps OIDs at 1000) | 0.5 |
| `margin_mode` | HL perps — `isolated` / `cross`; applied from flat only | `isolated` |
| `direction` | Perps — `long` / `short` / `both`; legacy `allow_shorts` migrates automatically | `long` |
| `allowed_regimes` | Whitelist of regime labels for new entries (closes always run); requires `regime.enabled` | (no gate) |
| `theta_harvest` | Early-exit config for sold options | null |

### Custom Strategy Parameters

Per-strategy `params` is merged with the strategy's built-in defaults (config keys override). Runtime data (e.g. funding rates) wins over config params.

```json
{ "id": "ts-st-es", "type": "futures", "platform": "topstep",
  "script": "shared_scripts/check_topstep.py",
  "args": ["supertrend", "ES", "5m", "--mode=paper"],
  "params": {"multiplier": 2.0, "atr_period": 10} }
```

### Theta Harvesting (Options)

Closes sold options early. `profit_target_pct` (% of premium captured), `stop_loss_pct` (% of premium lost, e.g. `200` = 2× premium), `min_dte_close` (force-close inside N days to expiry).

```json
{ "theta_harvest": { "enabled": true, "profit_target_pct": 60, "stop_loss_pct": 200, "min_dte_close": 3 } }
```

---

## Manual Trading on Hyperliquid

For hand-placed positions (or TradingView alerts) tracked by the scheduler for P&L, stops/TPs, and Discord summaries, declare a `type: "manual"` strategy and use:

```bash
./go-trader manual-open  hl-manual-btc                                          # defaults: --side long --margin 50
./go-trader manual-open  hl-manual-btc --side long  --notional 500 --atr 250
./go-trader manual-open  hl-manual-btc --side short --size 0.05 --record-only --fill-price 64500
./go-trader manual-close hl-manual-btc [--qty 0.025]
```

Sizing flags are mutually exclusive (`--size` / `--notional` / `--margin`); when all three are omitted (and not `--record-only`), `--margin 50` is auto-applied. `--side` defaults to `long`. Omitting `--atr` auto-fetches ATR(14) from Hyperliquid OHLCV for the strategy's symbol+timeframe; a leverage-aware fallback (`0.1 * fill_price / leverage`) is used only if the fetch fails. SL + N-tier TPs are placed inline so the position is never naked; on queue-insert failure the scheduler auto-flattens and cancels the protective orders. `type=manual` strategies with no stop fields default to `stop_loss_atr_mult = 1.5×`. All four defaults (margin, SL multiplier, side, TP tiers) are overridable via an optional top-level `manual_defaults` config block (hot-reloadable via SIGHUP).

---

## Backfilling Hyperliquid Fees

Historical `exchange_fee = 0` rows can be rewritten from Hyperliquid `userFills`:

```bash
./go-trader backfill hl-fees --strategy hl-btc-momentum               # dry-run, single strategy
./go-trader backfill hl-fees --all                                     # dry-run, all HL strategies
./go-trader backfill hl-fees --strategy hl-btc-momentum --apply        # apply changes
./go-trader backfill hl-fees --all --apply --reset-cash                # also replay strategies.cash
```

`--apply` refuses to run while another `go-trader` process is alive on the same DB.

---

## Build & Deploy

The canonical update path is `scripts/update.sh` — `git pull --ff-only` → `uv sync` → `go build` (version-stamped) → atomic binary swap → optional `systemctl restart` with `/health`-and-PID verify and automatic rollback to the previous binary on failed restart. A startup compatibility probe refuses to launch on a Go/Python version mismatch, so prefer the script over hand-rolled rebuilds. `systemctl restart` drains gracefully (in-flight `--execute` / close orders complete; read-only checks cancel immediately) and exits within ~20s instead of hanging on systemd's SIGKILL.

```bash
sudo bash scripts/update.sh --restart   # update + restart service
bash scripts/update.sh                  # update without restart
```

| Change | Action |
|--------|--------|
| Go or Python source | `sudo bash scripts/update.sh --restart` |
| Config (hot-reloadable subset) | `systemctl kill -s HUP go-trader` — applies capital/drawdown/intervals/params/risk knobs/channels/`allowed_regimes` in place; rejects shape changes (strategy add/remove, type/platform, leverage/`direction` with open positions) |
| Config (roster, script/args/type/platform, `regime` block) | `systemctl restart go-trader` |
| Service file | `systemctl daemon-reload && systemctl restart go-trader` |

---

## Monitoring

```bash
systemctl status go-trader              # service health
curl -s localhost:8099/status            # live prices + P&L (default port 8099; override with --status-port)
curl -s localhost:8099/health            # simple health check
open http://localhost:8099/dashboard     # local dashboard with strategy charts and trade markers
journalctl -u go-trader -n 50           # recent logs
./go-trader inspect <strategy-id>        # effective post-migration config (resolved SL/TP + provenance)
./go-trader inspect --all --json         # all strategies, machine-readable
```

The dashboard is served by the same local/LAN status server and does not add browser-side authentication. Keep it bound behind your existing network controls; for remote access, put it behind an authenticated reverse proxy or VPN rather than exposing `8099` directly.

`inspect` is read-only and safe to run against a live deployment — it loads `scheduler/config.json`, applies migrations and defaults, and prints which `stop_loss_*` field won, the resolved tier list on the configured TP close ref, and explicit-vs-default markers from the raw JSON. Use it to diagnose why a strategy isn't behaving like the JSON suggests, or to verify a config edit before SIGHUP.

Discord strategy summaries show columns `Init | Value | PnL | PnL% | DD | Wallet% | Tf | Int | #T | W/L` plus a `Book Sharpe (realized, annualized)` footer and the go-trader version + PID in the title. `okx-options` and `robinhood-options` channel keys route options summaries separately from spot/perps. `#T`/`W/L` come from the SQLite trades table; partial closes collapse into one round trip per position.

Open-position lines append `SL: $<trigger_px> (<signed_pct>%)` when a Hyperliquid stop-loss trigger is set (percent sign-flipped for shorts so it always reads as the loss if hit), `<N>x ($<margin> margin)` for leveraged perps, and tier marks (`✓`) for filled TP rungs. Spot and 1× perps stay clean.

---

## Risk Management

- **Portfolio kill switch** — halts trading at `portfolio_risk.max_drawdown_pct` (default 25); submits real close orders on HL / OKX perps / Robinhood crypto / TopStep, clearing virtual state only after every platform confirms flat.
- **Per-strategy circuit breakers** — pause on max-drawdown breach (24h cooldown); peak-relative for spot/options/futures, margin-relative for perps. HL/OKX perps, Robinhood crypto, and TopStep CBs auto-close reduce-only; OKX spot and Robinhood options surface an `operator-required` warning every cycle until flattened by hand.
- **Hyperliquid stop-loss** — exchange-side reduce-only trigger via one of `stop_loss_pct` / `stop_loss_margin_pct` / `stop_loss_atr_mult` / `trailing_stop_pct` / `trailing_stop_atr_mult` (mutually exclusive when positive). All five omitted → fixed ATR stop at `default_stop_loss_atr_mult * entry_atr` (default `1.0`); explicit `0` opts out. Trailing stops debounce via `trailing_stop_min_move_pct` to stay under HL's 1000-OID cap.
- **On-chain N-tier TP/SL ladders** — `tiered_tp_atr` / `tiered_tp_atr_live` close evaluators place reduce-only TPs at configured ATR multiples (default `[{1×, 0.5}, {2×, 1.0}]`); final tier absorbs rounding dust. Full close cancels all SL+TP OIDs in one shot.
- **Regime gate** — per-strategy `allowed_regimes` blocks new entries outside the whitelist; closes always run.
- **HL margin mode** — defaults to `isolated`; override with `margin_mode: "cross"` (applied from flat only).
- **Misc** — notional cap (`portfolio_risk.max_notional_usd`); correlation warnings (opt-in); 5 consecutive losses → 1h pause; spot max 95% capital per position; options max 4 positions per strategy with portfolio-aware scoring; theta harvesting on sold options.

---

## TradingView Export

Export SQLite trades to a TradingView portfolio-import CSV. `--strategy` is repeatable; `--all` exports everything.

```bash
./go-trader export tradingview --strategy hl-btc-momentum --output tv-hl-btc.csv
./go-trader export tradingview --all --output tv-all.csv
```

Built-in mappings cover known OKX/BinanceUS pairs; add `tradingview_export.symbol_overrides: { "hl:BTC": "BYBIT:BTCUSDT" }` for the rest. CB close trades are included.

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

Live `--execute` fills on Hyperliquid / OKX / Robinhood / TopStep record the exchange-reported fee plus order ID, so backfills and TradingView exports match the venue ledger.

---

## Layout & Dependencies

`scheduler/` (Go: config, state DB, HTTP status, risk, notifications) · `shared_scripts/` (Python entry points) · `platforms/` (exchange adapters) · `shared_tools/`, `shared_strategies/` (registry + impls) · `backtest/` · `systemd/`, `scripts/` · `SKILL.md`, `AGENTS.md` (agent guides).

Python 3.12+ via [uv](https://github.com/astral-sh/uv) (ccxt, pandas, numpy, scipy, hyperliquid-python-sdk); Go 1.26.2 with [`bwmarrin/discordgo`](https://github.com/bwmarrin/discordgo); systemd.

---

## Troubleshooting

| Problem | Solution |
|---|---|
| No Discord messages | Check `DISCORD_BOT_TOKEN`, channel IDs, bot permissions |
| Service won't start | `journalctl -u go-trader -n 50` |
| Didn't come back after reboot | Unit installed but not enabled — re-run `sudo bash scripts/install-service.sh` |
| Strategy not trading | Check circuit breaker in `/status`, verify params |
| Reset positions | `rm scheduler/state.db && systemctl restart go-trader` |
| Live mode fails | Set the env vars listed in the Platforms table for that platform |
| "state DB missing but live strategies configured" | Update wiped the repo dir instead of `git pull`. Restore `scheduler/state.db` from backup, or set `GO_TRADER_ALLOW_MISSING_STATE=1` for a genuine first-run deployment. |

---

## Risk Disclaimer

This software is provided for informational and educational purposes only and does not constitute financial advice. Trading involves substantial risk of loss; past performance is not indicative of future results. The authors make no guarantees regarding accuracy, profitability, or outcomes, and accept no liability for any losses incurred. You are solely responsible for your investment decisions — only trade with funds you can afford to lose.

*This is not financial advice. Trade at your own risk.*
