# go-trader — Crypto Trading Bot

[![GitHub release](https://img.shields.io/github/v/release/richkuo/go-trader)](https://github.com/richkuo/go-trader/releases/latest)
[![Discord](https://img.shields.io/badge/Discord-Join-5865F2?logo=discord&logoColor=white)](https://discord.com/invite/44BykmWZsP)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A Go + Python hybrid trading system. A single Go binary (~8MB idle RAM) orchestrates 50+ strategies across spot, options, perpetual futures, and CME futures by spawning short-lived Python scripts. Both paper and live execution are supported per strategy.

Supported platforms: Binance US, Deribit, IBKR/CME, Hyperliquid, TopStep, Robinhood (crypto + stock options), OKX (spot + perps + options), Luno. Per-platform Discord/Telegram channels post hourly summaries plus immediate trade alerts. When a new release ships, the bot DMs the configured owner — reply **yes** and it pulls, rebuilds, and restarts itself.

Join the Discord: [https://discord.gg/46d7Fa2dXz](https://discord.gg/46d7Fa2dXz)

---

## Getting Started

**Quick flow for a new server:** tell OpenClaw or Hermes:

```
install https://github.com/richkuo/go-trader and init.
```

### AI Agent Setup (Recommended)

Give your AI agent [SKILL.md](SKILL.md) (raw: `https://raw.githubusercontent.com/richkuo/go-trader/main/SKILL.md`) — it clones the repo, installs deps, walks through configuration, builds the binary, and starts the service. For non-Claude agents see [AGENTS.md](AGENTS.md). Using [OpenClaw](https://openclaw.ai) or [Hermes](https://hermes-agent.nousresearch.com/)? Just say "Set up go-trader".

### Interactive Setup (go-trader init)

```bash
./go-trader init
```

Walks asset/strategy/platform/capital/risk/Discord choices and writes `scheduler/config.json`. Defaults to a minimal BTC spot starter; risk prompts appear only when live trading is selected. Scripted: `./go-trader init --json '{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10}' --output config.json`

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

Use `systemd/go-trader@.service`. Code under `/opt/go-trader-<name>/`; **runtime config outside the tree** at `/var/lib/go-trader/<name>/config.json` (#1056) so `rsync`/`git clean` cannot clobber live config. `StateDirectory=go-trader/%i` keeps that path writable under `ProtectSystem=strict`.

```bash
sudo mkdir -p /opt/go-trader-paper-testing/scheduler /var/lib/go-trader/paper-testing
sudo cp go-trader /opt/go-trader-paper-testing/
sudo cp scheduler/config.json /var/lib/go-trader/paper-testing/config.json
sudo ln -s /var/lib/go-trader/paper-testing/config.json /opt/go-trader-paper-testing/scheduler/config.json
sudo chown -R go-trader:go-trader /opt/go-trader-paper-testing /var/lib/go-trader/paper-testing
sudo bash scripts/install-service.sh systemd/go-trader@.service paper-testing
```

Existing in-tree deploy: **stop the service**, then `scripts/migrate-config-out-of-tree.sh --instance <name>` (refuses while daemon is live). `NO_START=1` enables without starting. Detail: [SKILL.md](SKILL.md) / [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

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

Python provides quant libraries (pandas, numpy, scipy, CCXT); Go provides memory efficiency. Peak ~220MB for ~30s during checks, then back to ~8MB idle.

---

## Strategies & Platforms

Strategies are auto-discovered from `shared_strategies/` at `go-trader init` time. Common picks: spot entries include `sma_crossover`, `ema_crossover`, `momentum`, `rsi`, `bollinger_bands`, `macd`, `mean_reversion`, `triple_ema`, `tema_cross`, `pairs_spread`, `chart_pattern`, `anchored_vwap`, `liquidity_sweeps`; futures/perps also include `triple_ema_bidir`, `bear_pullback_st`, `vwap_rejection_st`, `delta_neutral_funding`, `funding_skew`, `momentum_pro`, `mean_reversion_pro`, `consolidation_range`, `atr_band_revert`, `mtf_confluence`, `regime_adaptive`, `regime_adaptive_htf`. Options use `vol_mean_reversion`, `momentum_options`, `protective_puts`, `covered_calls` (plus `wheel` and `butterfly` on Robinhood); new trades are scored vs. existing positions (strike distance, expiry spread, Greek balance). Max 4 positions per options strategy; min score 0.3 to execute.

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

**Hyperliquid perps direction** — per-strategy `direction: "long" | "short" | "both"`. `long` (default) opens longs only; `short` opens shorts only; `both` flips on reversals. Bidirectional/short-focused strategies (`triple_ema_bidir`, `bear_pullback_st`, `vwap_rejection_st`, `donchian_breakout`, `chart_pattern`, `anchored_vwap`, `liquidity_sweeps`, `momentum_pro`, `mean_reversion_pro`, `consolidation_range`, `atr_band_revert`, `mtf_confluence`, `funding_skew`, `regime_adaptive`) require `"short"` or `"both"`. Legacy `allow_shorts` migrates automatically.

**Coin sharing on Hyperliquid** — multiple HL strategies (including `type: "manual"`) can share a coin/wallet with per-strategy SQLite bookkeeping over one on-chain position. Peers must share `margin_mode` + `leverage`; reduce-only SL/TP are sized per strategy. Sub-accounts are the only path to fully independent direction/leverage/margin.

---

## Configuration Reference

### `scheduler/config.json`

Generate via `./go-trader init` or `--json`. Skeleton:

```json
{
  "config_version": 16,
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

`config_version` migrates on startup (current **15**: single `close_strategy` ref, canonical close params). Older versions upgrade automatically.

### Portfolio Risk

| Field | Description | Default |
|-------|-------------|---------|
| `portfolio_risk.max_drawdown_pct` | Kill switch — halt all trading if portfolio drops this % from peak | 25 |
| `portfolio_risk.max_notional_usd` | Hard cap on total notional exposure (0 = disabled) | 0 |
| `portfolio_risk.warn_threshold_pct` | Warning when drawdown reaches this % of `max_drawdown_pct` | 60 |
| `risk_free_rate` | Annualized rate for Sharpe calculations | 0.04 |
| `status_port` | HTTP status port (+5 fallback on collision); override with `--status-port` | 8099 |
| `default_stop_loss_atr_mult` | Fleet-wide HL perps fallback when all five `stop_loss_*` / `trailing_stop_*` fields omitted; `0` opts out | 1.0 |

### Regime Detection

Optional ADX+DI 3-state gate (`trending_up` / `trending_down` / `ranging`) from the strategy's OHLCV. `allowed_regimes` blocks new entries when the current regime isn't whitelisted (closes always pass). `regime.enabled` and `regime.windows` require restart; `allowed_regimes` and per-strategy window selectors are SIGHUP-reloadable when flat. Options strategies don't emit a regime label yet.

```json
{
  "regime": { "enabled": true, "period": 14, "adx_threshold": 20 },
  "strategies": [{ "id": "hl-momentum-btc", "allowed_regimes": ["trending_up", "trending_down"] }]
}
```

`regime.period` defaults to 14; `regime.adx_threshold` to 20 (below → `ranging`).

**Multi-window regime (#792).** Optional `regime.windows` runs independent ADX classifiers per named horizon (value = ADX period in bars). Empty `windows` → legacy single-window from `regime.period`. Three per-strategy selectors (empty/`default` → `regime.period`):

| Selector | Consumer |
|---|---|
| `regime_gate_window` | Entry gate (`allowed_regimes`) |
| `regime_atr_window` | Regime-aware SL/TP multipliers (stamped at open) |
| `regime_directional_window` | `regime_directional_policy` resolver |

OHLCV fetch scales to the longest window. `go-trader inspect <id>` shows resolved selectors and stamped windows on open positions.

**Regime-aware ATR multipliers (HL perps).** With `regime.enabled`, swap scalar stop/TP fields for `*_regime` siblings (`stop_loss_atr_regime`, `trailing_stop_atr_regime`, `tiered_tp_atr_regime`, `tiered_tp_atr_live_regime`). `{"use_defaults": true}` expands a baseline table; explicit form requires all three ADX labels. Regime is frozen at open for stops; live TP regime refs re-resolve each tick.

### Correlation Tracking

Opt-in via `correlation.enabled: true`. Warns when a single asset exceeds `max_concentration_pct` (default 60) of gross exposure or `max_same_direction_pct` (default 75) of strategies on an asset share a direction.

### Auto-Update & DM Upgrades

`auto_update`: `"off"` (default), `"daily"`, or `"heartbeat"`. When an update is found, channels are notified; with `discord.owner_id` set, reply **yes** to a DM to run `scripts/update.sh` and restart. Post-upgrade, new config fields may be collected via DM (10-minute window per field). Discord user ID: right-click username → **Copy User ID** (Developer Mode: Settings → Advanced).

### Discord Settings

| Field | Description |
|---|---|
| `discord.enabled` | Toggle Discord notifications |
| `discord.token` | Leave blank — set `DISCORD_BOT_TOKEN` env var |
| `discord.owner_id` | Owner DM for upgrades + config migration (`DISCORD_OWNER_ID`) |
| `discord.channels` | Map keyed by `spot` / `options` / `<platform>` / `<platform>-paper` |
| `discord.trade_alert_channels` | Optional per-type trade-alert routing; SIGHUP-reloadable |
| `telegram.trade_alert_channels` | Same override for Telegram |

### Summary Frequency

Top-level `summary_frequency` map keyed by channel name. Trades always post immediately.

```json
{ "summary_frequency": { "spot": "hourly", "hyperliquid": "every", "topstep": "30m" } }
```

Values: `every` / `per_check` / `always`, `hourly`, `daily`, Go durations (`30m`, `2h`), or `""` (legacy defaults). Wall-clock based, persisted in SQLite.

### Strategy Entry

| Field | Description | Default |
|---|---|---|
| `id` | Unique identifier (e.g. `hl-momentum-btc`) | required |
| `type` | `spot` / `options` / `perps` / `futures` / `manual` | required |
| `platform` | `binanceus` / `deribit` / `ibkr` / `hyperliquid` / `topstep` / `robinhood` / `okx` / `luno` | required |
| `script`, `args` | Python entry-point + argv (auto-filled for `manual`) | required |
| `capital` | Starting capital in USD | 1000 |
| `max_drawdown_pct` | Per-strategy CB; peak-relative (spot/options/futures), margin-relative (perps) | spot 5, options 10, perps 5 |
| `circuit_breaker` | Set `false` to disable both CB arms; latched CB still drains | enabled |
| `interval_seconds` | Check interval (0 → global) | 0 |
| `htf_filter` | Higher-timeframe trend filter | false |
| `open_strategy` | Co-located ref `{name, params}` overriding entry; falls back to `args[0]` | null |
| `close_strategy` | Single `{name, params}` close evaluator ref | null |
| `leverage` | Perps — exchange leverage (also sizing if `sizing_leverage` omitted) | 1 |
| `sizing_leverage` | Perps — order sizing multiplier | `leverage` |
| `margin_per_trade_usd` | HL perps — notional = `min(margin_per_trade_usd, cash) × leverage` | omitted |
| `stop_loss_pct` / `stop_loss_margin_pct` / `stop_loss_atr_mult` / `trailing_stop_pct` / `trailing_stop_atr_mult` | HL perps — at most one positive value; all omitted → `default_stop_loss_atr_mult × entry_atr`; `0` opts out | omitted |
| `trailing_stop_min_move_pct` | HL trailing stop debounce (OID cap 1000) | 0.5 |
| `margin_mode` | HL perps — `isolated` / `cross`; from flat only | `isolated` |
| `direction` | Perps — `long` / `short` / `both` | `long` |
| `allowed_regimes` | Whitelist for new entries; requires `regime.enabled` | (no gate) |
| `regime_gate_window` / `regime_atr_window` / `regime_directional_window` | Multi-window selectors | legacy |
| `theta_harvest` | Early-exit config for sold options | null |

### Custom Strategy Parameters

Per-strategy `params` merges under built-in defaults (config wins; runtime data wins over config).

```json
{ "id": "ts-st-es", "type": "futures", "platform": "topstep",
  "script": "shared_scripts/check_topstep.py",
  "args": ["supertrend", "ES", "5m", "--mode=paper"],
  "params": {"multiplier": 2.0, "atr_period": 10} }
```

### Theta Harvesting (Options)

`profit_target_pct` (% premium captured), `stop_loss_pct` (% premium lost), `min_dte_close` (force-close inside N days).

```json
{ "theta_harvest": { "enabled": true, "profit_target_pct": 60, "stop_loss_pct": 200, "min_dte_close": 3 } }
```

---

## Manual Trading on Hyperliquid

Hand-placed positions (or TradingView alerts) tracked for P&L, stops/TPs, and Discord summaries — declare `type: "manual"` and use:

```bash
./go-trader manual-open  hl-manual-btc                                          # defaults: --side long --margin 50
./go-trader manual-open  hl-manual-btc --side long  --notional 500 --atr 250
./go-trader manual-open  hl-manual-btc --side short --size 0.05 --record-only --fill-price 64500
./go-trader manual-open  hl-manual-btc --limit-price 68000 --side long --margin 50
./go-trader manual-open  hl-manual-btc --limit-price 68000 --tif Gtc --expire-after 4h
./go-trader manual-cancel <limit-order-id>
./go-trader manual-update-sl hl-manual-btc --trigger 66000
./go-trader manual-cancel-sl hl-manual-btc
./go-trader manual-close hl-manual-btc [--qty 0.025]
./go-trader force-close hl-tcross-eth-live [--qty 0.025]                 # live HL perps strategy close
```

Sizing: mutually exclusive `--size` / `--notional` / `--margin` (default `--margin 50` when omitted). `--side` defaults to `long`. Omitting `--atr` auto-fetches ATR(14); leverage-aware fallback if fetch fails. SL + tiered TPs placed inline so the position is never naked.

**Close defaults (#1115/#1135):** with `regime.enabled` and a resolvable per-regime trail, manual defaults to `trailing_tp_ratchet_regime` (regime trail owns the SL); otherwise `tiered_tp_atr_live` + scalar **2.0×ATR** SL (#1121). Override via `close_strategy`, stop fields, or `user_defaults.manual` (hot-reloadable via SIGHUP). Fleet close ladders live under `user_defaults.close`; standalone `*_atr_regime` defaults live under `user_defaults.regime_atr`.

`manual-update-sl` / `manual-cancel-sl` queue daemon-side cancel-then-place edits — rejected when automated ATR/regime/trailing protection would re-pin next cycle. `force-close` is for live Hyperliquid `type=perps` strategy positions; it submits the reduce-only close and queues the fill for the scheduler to adopt into state/trades. `--dry-run` previews without exchange calls. Limit opens are post-only (ALO) by default or GTC with `--tif Gtc`; scheduler polls fills each cycle.

---

## Backfilling Hyperliquid Fees

```bash
./go-trader backfill hl-fees --strategy hl-btc-momentum               # dry-run
./go-trader backfill hl-fees --all --apply                            # apply (stop daemon first)
./go-trader backfill trade-ledger --all --apply                       # shared-wallet gross-PnL migration
```

`--apply` refuses while another `go-trader` process holds the same DB. Trade-ledger backfill is idempotent — run once after adopting the gross-PnL convention.

---

## Build & Deploy

Canonical path: `scripts/update.sh` — `git pull --ff-only` → `uv sync` → version-stamped `go build` → atomic binary swap → optional restart with `/health` verify and rollback on failure. Startup probe refuses Go/Python version mismatch — prefer the script over hand-rolled rebuilds.

```bash
sudo bash scripts/update.sh --restart                              # systemd (default)
bash scripts/update.sh --restart --restart-mode signal             # bare-process (pidfile + run.sh)
bash scripts/update.sh --rsync-from /path/to/staged-build --restart
bash scripts/update.sh --all --restart                             # batch all instances
```

| Change | Action |
|--------|--------|
| Go or Python source | `sudo bash scripts/update.sh --restart` |
| Config (hot-reloadable subset) | `systemctl kill -s HUP go-trader` |
| Config (roster, script/args/type/platform, `regime` block) | `systemctl restart go-trader` |
| Service file | `systemctl daemon-reload && systemctl restart go-trader` |

Restart modes, batch discovery, and graceful drain: [SKILL.md](SKILL.md) / [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

---

## Monitoring

```bash
systemctl status go-trader
curl -s localhost:8099/status            # live prices + P&L
curl -s localhost:8099/health
open http://localhost:8099/dashboard     # charts, trades, equity, regime badge, tuner, reports
journalctl -u go-trader -n 50
./go-trader inspect <strategy-id>        # resolved config + SL/TP provenance
./go-trader inspect --all --json
./go-trader agent-info                   # capabilities, schema, env vars, live state
```

Loopback-only status server (`localhost:<port>`). Dashboard includes candle charts, trade history, equity sparklines, strategy tuner, and `/reports`. Set `status_token` for mutating API calls from the browser. Prefer VPN or reverse proxy over binding `0.0.0.0`.

**Tailscale Serve** — publish HTTPS on the tailnet while go-trader stays on loopback:

```bash
tailscale serve --bg --https=8443 http://127.0.0.1:8099   # live (8099)
tailscale serve --bg --https=8444 http://127.0.0.1:8100   # paper instance (8100)
```

Open `https://<node>.tailnet.ts.net:8443/dashboard`. `status_token` still applies.

`inspect` is read-only against live deploys — shows which stop field won, resolved TP tiers, and direction provenance. Discord summaries: header shows aggregate initial capital; table columns `Value | PnL | PnL% | DD | Wallet% | Tf | Int | #T | W/L` plus Book Sharpe footer.

---

## Risk Management

- **Portfolio kill switch** — halts at `portfolio_risk.max_drawdown_pct` (default 25); submits real closes on HL / OKX perps / Robinhood crypto / TopStep.
- **Per-strategy circuit breakers** — max-drawdown or 5 consecutive losses (24h cooldown). HL/OKX perps, Robinhood crypto, TopStep auto-close; OKX spot and Robinhood options need manual flatten. Latched HL perps CB still permits trailing-SL management. `circuit_breaker: false` disables firing.
- **Hyperliquid stop-loss** — one positive field among five scalar stop types; omitted → `default_stop_loss_atr_mult × entry_atr` (1.0); `0` opts out.
- **On-chain N-tier TP/SL** — `tiered_tp_atr` / `tiered_tp_atr_live` (default tiers `[{1.5×, 0.4}, {3×, 0.8}, {5×, 1.0}]`).
- **Trailing-ratchet close** — `trailing_tp_ratchet` / `trailing_tp_ratchet_regime`: cleared tiers tighten a single trailing stop; no fixed on-chain TPs. HL perps + `manual`.
- **Regime gate**, **HL margin mode** (`isolated` default), correlation warnings (opt-in), options position limits, theta harvesting.

---

## TradingView Export

```bash
./go-trader export tradingview --strategy hl-btc-momentum --output tv-hl-btc.csv
./go-trader export tradingview --all --output tv-all.csv
```

Built-in mappings cover known OKX/BinanceUS pairs; add `tradingview_export.symbol_overrides` for the rest.

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

Live fills record exchange-reported fees and order IDs.

---

## Layout & Dependencies

`scheduler/` (Go) · `shared_scripts/` · `platforms/` · `shared_tools/`, `shared_strategies/` · `backtest/` · `systemd/`, `scripts/` · `SKILL.md`, `AGENTS.md`, `docs/ARCHITECTURE.md`.

Python 3.12+ via [uv](https://github.com/astral-sh/uv); Go 1.26.2; systemd.

---

## Troubleshooting

| Problem | Solution |
|---|---|
| No Discord messages | Check `DISCORD_BOT_TOKEN`, channel IDs, bot permissions |
| Service won't start | `journalctl -u go-trader -n 50` |
| Didn't come back after reboot | Re-run `sudo bash scripts/install-service.sh` |
| Strategy not trading | Circuit breaker in `/status`, verify params |
| Reset positions | `rm scheduler/state.db && systemctl restart go-trader` |
| Live mode fails | Set env vars from Platforms table |
| "state DB missing but live strategies configured" | Restore `scheduler/state.db` from backup, or `GO_TRADER_ALLOW_MISSING_STATE=1` for first-run |

---

## Risk Disclaimer

This software is provided for informational and educational purposes only and does not constitute financial advice. Trading involves substantial risk of loss; past performance is not indicative of future results. The authors make no guarantees regarding accuracy, profitability, or outcomes, and accept no liability for any losses incurred. You are solely responsible for your investment decisions — only trade with funds you can afford to lose.

*This is not financial advice. Trade at your own risk.*
