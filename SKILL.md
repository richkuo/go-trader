# Agent Setup Guide — go-trader

Repository: `https://github.com/richkuo/go-trader.git`

This is the concise skill entry point for agents setting up, configuring, operating, or extending go-trader. For broader project context and PR conventions for Codex/Gemini-style agents, see [AGENTS.md](AGENTS.md).

Quick flow for a new server: tell OpenClaw `install https://github.com/richkuo/go-trader and init`.

---

## Core Rules

- Work from the repo root for git commands.
- Use `/opt/homebrew/bin/go` on macOS or `/usr/local/go/bin/go` on Linux if `go` is not on PATH.
- Use `.venv/bin/python3` for Python scripts; in git worktrees, use the main repo `.venv`.
- Install Python deps with `uv sync`.
- Scheduler config is `scheduler/config.json`; start from `scheduler/config.example.json` when generating manually.
- State is SQLite only: default `scheduler/state.db`.
- Never store secrets in config files. Put Discord and exchange credentials in systemd environment variables.
- Prefer `./go-trader init` for humans and `./go-trader init --json ... --output scheduler/config.json` for agents/scripts.
- When a user asks to export data to TradingView, ask which strategy IDs to export or whether to export all strategies before running the export.

---

## Prerequisites

Check and install missing tools with user approval:

```bash
python3 --version
uv --version 2>/dev/null || echo "NOT_INSTALLED"
go version 2>/dev/null || /usr/local/go/bin/go version 2>/dev/null || /opt/homebrew/bin/go version 2>/dev/null || echo "NOT_INSTALLED"
git --version
```

Requirements:

- Python 3.12+
- `uv`
- Go 1.26.2
- Git

Install helpers:

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh

# Linux Go install
curl -sL https://go.dev/dl/go1.26.2.linux-amd64.tar.gz | tar -C /usr/local -xzf -

# macOS Go install
brew install go@1.26
```

---

## Install

```bash
git clone https://github.com/richkuo/go-trader.git
cd go-trader
uv sync
```

If the repo already exists, ask whether to reconfigure, update, or fresh install before changing it.

Build the binary:

```bash
VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
/opt/homebrew/bin/go -C scheduler build -ldflags "-X main.Version=$VER" -o ../go-trader .
./go-trader --help
```

Use `/usr/local/go/bin/go` on Linux. The `Version` ldflag appears in Discord summary titles; without it the binary reports `dev`.

---

## Configure

Recommended human flow:

```bash
./go-trader init
```

Recommended scripted flow:

```bash
./go-trader init --json '{"assets":["BTC","ETH"],"enableSpot":true,"spotStrategies":["momentum","rsi"],"spotCapital":1000,"spotDrawdown":60}' --output scheduler/config.json
```

The wizard covers assets, strategy groups, paper/live mode, per-strategy capital, live risk settings, Discord channels, and auto-update mode. It prompts before overwriting `scheduler/config.json`.

Manual config rules:

- Strategy entries need `id`, `type`, `script`, `args`, `capital`, `max_drawdown_pct`, and `interval_seconds`.
- `StrategyConfig.Params` is a JSON object of parameter overrides; runtime params such as funding rates take priority.
- `discord.channels` and `telegram.channels` are maps keyed by platform/type: `spot`, `options`, `hyperliquid`, `topstep`, `robinhood`, `okx`, `luno`, plus optional paper keys such as `okx-paper`.
- `summary_frequency` is a map keyed like channels. Values: `hourly`, `daily`, `every`, `per_check`, `always`, or Go durations such as `30m`, `2h`. Cadence is wall-clock based and persisted in SQLite (`app_state.last_summary_post`), so restarts and SIGHUP reloads keep the throttle window intact.
- Trades always force an immediate summary post regardless of cadence.
- `discord.owner_id` can be set with `DISCORD_OWNER_ID`; this enables DM upgrade prompts and migration prompts.

Live-mode risk defaults prompted by init:

- Per-strategy spot drawdown: 5%
- Per-strategy options drawdown: 10%
- Portfolio kill-switch drawdown: 25%
- Portfolio warn threshold: 60% of kill-switch; warnings repeat every cycle while in band

---

## Secrets

Set secrets in systemd overrides or exported environment variables before installation:

| Variable | Description |
| --- | --- |
| `DISCORD_BOT_TOKEN` | Discord bot token |
| `DISCORD_OWNER_ID` | Discord user ID for DM upgrades/migrations |
| `STATUS_AUTH_TOKEN` | Optional bearer token for `/status` |
| `BINANCE_API_KEY`, `BINANCE_API_SECRET` | Binance live trading |
| `HYPERLIQUID_SECRET_KEY`, `HYPERLIQUID_ACCOUNT_ADDRESS` | Hyperliquid live perps |
| `TOPSTEP_API_KEY`, `TOPSTEP_API_SECRET`, `TOPSTEP_ACCOUNT_ID` | TopStep live futures |
| `ROBINHOOD_USERNAME`, `ROBINHOOD_PASSWORD`, `ROBINHOOD_TOTP_SECRET` | Robinhood live crypto/options |
| `OKX_API_KEY`, `OKX_API_SECRET`, `OKX_PASSPHRASE`, `OKX_SANDBOX` | OKX live/demo |
| `LUNO_API_KEY_ID`, `LUNO_API_KEY_SECRET` | Luno live |
| `GO_TRADER_ALLOW_MISSING_STATE` | Set `1` only for genuine first-run live deployments with no DB |

---

## Run And Install Service

Smoke test:

```bash
./go-trader --config scheduler/config.json --once
```

Install the systemd service with the bundled installer:

```bash
mkdir -p logs
export DISCORD_BOT_TOKEN="{token}"
sudo bash scripts/install-service.sh
```

The installer copies the unit, runs `daemon-reload`, enables the service, starts it, and pre-creates `logs/` so `ProtectSystem=strict` does not block first-run logging.

Templated multi-instance deployment:

```bash
sudo bash scripts/install-service.sh systemd/go-trader@.service paper-testing
```

Install without starting:

```bash
NO_START=1 sudo bash scripts/install-service.sh
```

Common service commands:

```bash
sudo systemctl start go-trader
sudo systemctl stop go-trader
sudo systemctl restart go-trader
sudo systemctl status go-trader
journalctl -u go-trader -n 100 --no-pager
```

---

## Auto-Update

Configure `auto_update` as:

| Mode | Behavior |
| --- | --- |
| `off` | No automatic checking |
| `daily` | Check once per day |
| `heartbeat` | Check every scheduler cycle |

When an update is found, the bot notifies active Discord channels. If `DISCORD_OWNER_ID` is set, it DMs the owner asking whether to upgrade. Replying yes within 30 minutes runs `git pull --ff-only`, rebuilds the binary, saves state, and restarts.

Manual update:

```bash
git pull --ff-only
VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
/opt/homebrew/bin/go -C scheduler build -ldflags "-X main.Version=$VER" -o ../go-trader .
sudo systemctl restart go-trader
```

Verify update logs:

```bash
journalctl -u go-trader -f | grep -i "\[update\]"
```

---

## Post-Update Agent Protocol

When an agent is invoked after the operator updates go-trader (manual `git pull`, auto-update restart, or the operator says "I just updated" / "what changed"), the agent must walk the operator through anything the new commits can affect on their existing config, strategies, and open positions — and prompt before applying any opt-in.

The binary's built-in `runConfigMigrationDM` only handles fields registered in `configFieldRegistry` (currently up through v3). Newer config-version bumps and opt-in features land silently unless an agent surfaces them. This section closes that gap.

### Trigger

Run this protocol when ANY of the following is true:
- The operator says "I updated", "I just pulled", "what's new", or asks about migration.
- `git log -1 --format=%cI` is newer than the running binary's version (`./go-trader --version` or `curl -s localhost:8099/health` → `version`).
- `git status` shows the working tree is clean and `git rev-list --count <running-version>..HEAD` > 0.

### Steps

1. **Identify the diff.** `git log --oneline <running-version>..HEAD -- scheduler/ shared_scripts/ shared_strategies/ platforms/`. If the running version is unknown, ask the operator which tag/SHA was last deployed, or fall back to the last 30 commits.
2. **Classify each commit** into one of:
   - **Auto-migration** — `CurrentConfigVersion` bumped; `MigrateConfig` rewrites the JSON on next start. Summarize the change; no prompt.
   - **Runtime default change** — behavior shifts on existing strategies without a config edit (e.g., HL stop-loss auto-derive, margin mode default). Prompt: confirm, or set explicit opt-out per strategy.
   - **New opt-in field** — feature is dormant until the operator adds a field (e.g., `trailing_stop_pct`, `open_strategy`, `close_strategies`). Prompt per affected strategy.
   - **Open-position constraint** — change requires flat positions to apply (e.g., `margin_mode`, exchange `leverage`). List affected strategies; warn the operator and skip until flat.
   - **Internal/no-op** — refactors, tests, docs. Mention briefly, no prompt.
3. **Read current state.** Load `scheduler/config.json` and query `scheduler/state.db` for open positions per strategy:
   ```sql
   SELECT strategy_id, symbol, quantity, side FROM positions WHERE quantity > 0;
   SELECT strategy_id, symbol, contracts, action FROM option_positions WHERE contracts > 0;
   ```
4. **Prompt per item.** For each runtime-default and opt-in change, list the affected strategies and ask the operator to choose. Default to no change if they decline. For runtime defaults, also offer to write the explicit opt-out value so the new default never silently kicks in later.
5. **Apply via SIGHUP-safe edits.** When changes are SIGHUP-compatible (see SKILL.md "Hot reload"), edit `scheduler/config.json` and run `kill -HUP $(pgrep go-trader)`. For changes that are NOT hot-reloadable (strategy roster, kill-switch identity, leverage/margin_mode with positions open, DB path) tell the operator a full restart is required and confirm before proceeding.
6. **Verify.** After SIGHUP, tail the log for `[reload]` lines and confirm the diff was accepted; on rejection, show the rejection reason and offer a full restart.

### Required prompt template

For each item, the agent must ask in this shape:

> Change: `<short description>` (PR #<N>)
> Affects: `<strategy IDs>` (and any open positions: `<symbol qty side>`)
> Default if you do nothing: `<what happens silently>`
> Options: 1) accept the new default, 2) opt out by setting `<field> = <value>`, 3) opt in to the new feature with `<field> = <value>` (requires flat? Y/N).
> Your choice?

Never apply runtime-default changes silently when the operator hasn't been shown the affected strategies. "Auto" means automatic JSON rewrite, not automatic behavior change.

### Reference: known categories

Use the commit message and PR number to classify. When in doubt, treat as runtime default and prompt.

| Category | Examples |
| --- | --- |
| Auto-migration | `config_version` bump, deprecated field removal, silent field copy (e.g. v10 `sizing_leverage` ← `leverage`); silent field drop without version bump (e.g. `disable_implicit_close` removed in #508 — if set in config it no-ops; any strategy that had it `true` with no `close_strategies` now uses the open strategy as implicit close instead) |
| Runtime default | HL stop-loss auto-derive (#493), HL margin mode default isolated (#486), peer normalization (#494) |
| Opt-in field | trailing stop (#502), open/close composition (#483), `stop_loss_margin_pct` (#490) |
| Open-position constraint | `margin_mode`, exchange `leverage`, kill-switch identity changes |

When this list looks stale relative to recent commits, regenerate it from `git log --oneline -50` before prompting.

---

## Status

Default status port is `8099`. Override with `--status-port <port>` or `status_port` in config. If the port is busy, the server tries the next ports for up to 5 attempts; check logs for `[server] Status endpoint at http://localhost:<port>/status`.

```bash
curl -s localhost:8099/status | python3 -m json.tool
curl -s localhost:8099/health
curl -s localhost:8099/history
```

If Discord is enabled, wait for the first cycle and verify messages in configured channels.

Report success to the user with mode, number of strategies, status URL, and log command.

---

## TradingView Export

Export recorded SQLite trades to a TradingView portfolio transaction-import CSV:

```bash
./go-trader export tradingview --strategy hl-btc-momentum --output tradingview-hl-btc-momentum.csv
./go-trader export tradingview --strategy hl-btc-momentum --strategy okx-eth-breakout --output tradingview-selected.csv
./go-trader export tradingview --all --output tradingview-all.csv
```

The CSV contains TradingView's import header: `Symbol,Side,Qty,Status,Fill Price,Commission,Closing Time`. Built-in mappings cover known OKX and BinanceUS crypto pairs. Add `tradingview_export.symbol_overrides` when a platform/symbol cannot be safely mapped:

```json
"tradingview_export": {
  "symbol_overrides": {
    "hl:BTC": "BYBIT:BTCUSDT"
  }
}
```

---

## `/go-trader` Command

When the user says `/go-trader`, "check bot status", "show strategy health", or "how are the bots doing", run:

```bash
curl -s localhost:8099/status | python3 -c "
import json, sys
d = json.load(sys.stdin)
prices = d.get('prices', {})
strats = d.get('strategies', {})
print(f'=== GO-TRADER (Cycle {d[\"cycle_count\"]}) ===')
for sym, p in sorted(prices.items()):
    print(f'  {sym}: \${p:,.2f}')
total_val = sum(s['portfolio_value'] for s in strats.values())
total_cap = sum(s['initial_capital'] for s in strats.values())
total_pnl = total_val - total_cap
pct = (total_pnl/total_cap)*100 if total_cap else 0
print(f'\nPortfolio: \${total_cap:,.0f} -> \${total_val:,.0f} ({total_pnl:+,.0f} / {pct:+.1f}%)')
print(f'Strategies: {len(strats)}')
cb_active = [(id,s) for id,s in strats.items() if s['risk_state'].get('circuit_breaker_until','').startswith('20')]
print(f'Circuit breakers active: {len(cb_active)}')
ranked = sorted(strats.items(), key=lambda x: x[1]['pnl_pct'], reverse=True)
print('\nTop 5:')
for id, s in ranked[:5]:
    print(f'  {id}: {s[\"pnl_pct\"]:+.1f}% (\${s[\"pnl\"]:+,.0f}) | {s[\"trade_count\"]} trades')
print('\nBottom 5:')
for id, s in ranked[-5:]:
    print(f'  {id}: {s[\"pnl_pct\"]:+.1f}% (\${s[\"pnl\"]:+,.0f}) | {s[\"trade_count\"]} trades')
dead = [id for id,s in strats.items() if s['trade_count'] == 0]
if dead:
    print(f'\nDead (0 trades): {len(dead)} - {dead}')
if cb_active:
    print('\nCircuit breaker details:')
    for id, s in cb_active:
        rs = s['risk_state']
        print(f'  {id}: dd={rs[\"current_drawdown_pct\"]:.1f}% / max={rs[\"max_drawdown_pct\"]:.0f}% | until {rs[\"circuit_breaker_until\"][:19]}')
"
```

Present output in readable prose. Highlight circuit breakers, dead strategies, large PnL changes, and missing status data.

---

## `/menu` Command

When the user says `/menu`, "show menu", "what can I configure", "what's available", or "help me get started", output this overview:

```text
=== GO-TRADER MENU ===

1. TRADING PLATFORMS
   Binance US spot; Deribit options; IBKR/CME options; Hyperliquid perps;
   TopStep futures; Robinhood crypto/options; OKX spot/perps/options; Luno;
   custom platforms via the integration checklist.

2. AVAILABLE STRATEGIES
   Spot: sma_crossover, ema_crossover, momentum, rsi, bollinger_bands, macd,
     mean_reversion, volume_weighted, triple_ema, rsi_macd_combo, pairs_spread
   Futures/perps: momentum, mean_reversion, rsi, macd, breakout,
     session_breakout, triple_ema_bidir, delta_neutral_funding
   Options: vol_mean_reversion, momentum_options, protective_puts,
     covered_calls, wheel, butterfly

3. ADJUSTABLE SETTINGS
   Global: interval_seconds, db_file, auto_update, status_port,
     max_drawdown_pct, portfolio_risk.warn_threshold_pct,
     notional_cap_usd, risk_free_rate, correlation.*, summary_frequency
   Per-strategy: capital, max_drawdown_pct, interval_seconds, htf_filter,
     params, leverage, sizing_leverage, stop_loss_pct, stop_loss_margin_pct,
     trailing_stop_pct, trailing_stop_min_move_pct, margin_mode, allow_shorts,
     open_strategy, close_strategies, theta_harvest.*
   Discord/Telegram: enabled, channels, dm_channels, owner_id
   Environment: Discord token, status token, exchange credentials

4. COMMANDS
   /menu
   /go-trader
   ./go-trader init
   ./go-trader init --json '{...}' --output scheduler/config.json
   sudo systemctl start|stop|restart|status go-trader
   journalctl -u go-trader -n 50 --no-pager
   curl -s localhost:8099/status | python3 -m json.tool

5. BACKTESTING
   .venv/bin/python3 backtest/run_backtest.py --strategy <n> --symbol BTC/USDT --timeframe 1h --mode single|compare|multi|optimize
   .venv/bin/python3 backtest/backtest_options.py --underlying BTC --since 90 --capital 10000
   .venv/bin/python3 backtest/backtest_theta.py --underlying BTC --since 90 --capital 10000
```

---

## Backtesting

Use `.venv/bin/python3` for all backtests.

```bash
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode single
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode compare
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --timeframe 1h --mode multi
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode optimize
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --since 90

.venv/bin/python3 backtest/backtest_options.py --underlying BTC --since 90 --capital 10000
.venv/bin/python3 backtest/backtest_options.py --underlying BTC --since 90 --capital 10000 --verbose
.venv/bin/python3 backtest/backtest_theta.py --underlying BTC --since 90 --capital 10000
```

---

## Reconfiguration

After edits to `scheduler/config.json`, either hot-reload (preferred when the change is in the supported subset) or restart:

```bash
sudo systemctl kill -s HUP go-trader   # hot reload (no state loss)
sudo systemctl restart go-trader       # full restart
```

Hot reload (`SIGHUP`) re-applies a safe subset: capital, drawdown, intervals, params, stop-loss (including trailing), sizing leverage, theta-harvest, portfolio risk knobs, summary cadence, correlation thresholds, auto-update mode, Discord/Telegram channel maps and tokens. It refuses if the strategy roster, script/args/type/platform, HTF filter, kill-switch identity, or DB path changed, and refuses per-strategy exchange `leverage` or HL `margin_mode` changes while positions are open. It also re-runs the HL perps peer-on-same-coin check (`margin_mode`/exchange `leverage` must agree; at most one peer with a non-zero stop owner across `stop_loss_pct` / `stop_loss_margin_pct` / `trailing_stop_pct`). Logs report the applied diff and any rejection reason; on rejection, fall back to a full restart. The status server reflects the new port immediately.

Common changes:

- Regenerate config: `./go-trader init`
- Script config generation: `./go-trader init --json '{...}' --output scheduler/config.json`
- Change channels: edit `discord.channels` / `telegram.channels`; update OpenClaw allowlist if needed
- Change token: `sudo systemctl edit go-trader`, add environment override, restart
- Add/remove strategies: edit the `strategies` array; removed strategies are pruned from state
- Adjust risk: edit strategy `max_drawdown_pct`, portfolio `max_drawdown_pct`, or `portfolio_risk.warn_threshold_pct`
- Enable theta harvesting: add `theta_harvest` block to options strategy entries
- Switch paper to live: change script args from `--mode=paper` to `--mode=live`, add `--execute` where required, and configure exchange credentials

Changing `capital` on an existing strategy does not reset cash/positions. To fully reset, remove `scheduler/state.db` or that strategy's DB rows and restart.

---

## Adjustable Settings

Global config keys:

| Setting | Key | Default |
| --- | --- | --- |
| Check interval | `interval_seconds` | 300 |
| State DB path | `db_file` | `scheduler/state.db` |
| Auto-update | `auto_update` | `off` |
| Status port | `status_port` | 8099 |
| Risk-free rate | `risk_free_rate` | 0.04 |
| Portfolio drawdown kill switch | `max_drawdown_pct` | 25 |
| Portfolio warn threshold | `portfolio_risk.warn_threshold_pct` | 60 |
| Correlation tracking | `correlation.*` | disabled |
| Summary cadence | `summary_frequency` | legacy defaults |

Per-strategy keys:

| Setting | Key | Notes |
| --- | --- | --- |
| Capital | `capital` | Starting capital reference |
| Max drawdown | `max_drawdown_pct` | Strategy circuit breaker |
| Interval | `interval_seconds` | 0 uses global; auto-accelerates in drawdown warn band |
| HTF filter | `htf_filter` | Skips counter-trend signals |
| Params | `params` | Strategy default overrides |
| Allow shorts | `allow_shorts` | Required for bidirectional perps strategies |
| Stop loss (price %) | `stop_loss_pct` | Hyperliquid perps only. Omit to auto-derive from `max_drawdown_pct` (capped at 50) when sole strategy on the coin. Same-coin peers skip auto-derive and need one explicit positive owner (#494). Explicit `0` opts out. |
| Stop loss (margin %) | `stop_loss_margin_pct` | Hyperliquid perps only, leverage-aware alternative to `stop_loss_pct`. Mutually exclusive unless both are explicit `0`. Same-coin peers default to opt-out (#494). |
| Trailing stop (%) | `trailing_stop_pct` | Hyperliquid perps only — distance from high-water mark; mutually exclusive with `stop_loss_pct` / `stop_loss_margin_pct` (#501/#502). Capped at 50%. Explicit `0` disables. |
| Trailing stop debounce | `trailing_stop_min_move_pct` | Minimum trigger move before cancel/replace. Defaults to 0.5%. |
| Exchange leverage | `leverage` | Perps only — exchange margin/risk leverage and HL `update_leverage` (#497). 1× by default. |
| Sizing leverage | `sizing_leverage` | Perps only — order-size multiplier (`cash * sizing_leverage * 0.95`). Defaults to `leverage`; set lower to run high exchange leverage with conservative position size (#497). |
| Margin mode | `margin_mode` | Hyperliquid perps only, `isolated` (default) or `cross`. Applied from flat. |
| Open strategy | `open_strategy` | Override entry strategy name (otherwise from `args[0]`) |
| Close strategies | `close_strategies` | Ordered list of exit evaluators; max `close_fraction` wins |
| Theta harvest | `theta_harvest.*` | Options early-exit controls |

Discord/Telegram keys:

- `enabled`
- `channels`: platform/type channel map
- `dm_channels`: per-platform DM-style trade alerts
- `owner_id`: prefer `DISCORD_OWNER_ID` env var for Discord

Correlation tracking:

- `correlation.enabled`
- `correlation.max_concentration_pct`, default 60
- `correlation.max_same_direction_pct`, default 75

When enabled, correlation warnings go to all active channels and owner DM, and the snapshot appears in `/status`.

---

## Strategy Reference

Use the registry as source of truth:

```bash
.venv/bin/python3 shared_strategies/open/spot/strategies.py --list-json
.venv/bin/python3 shared_strategies/open/futures/strategies.py --list-json
.venv/bin/python3 shared_strategies/options/strategies.py --list-json
```

Platform conventions:

| Platform | ID prefix | Type/script |
| --- | --- | --- |
| BinanceUS spot | none | `spot`, `shared_scripts/check_strategy.py` |
| Hyperliquid perps | `hl-` | `perps`, `shared_scripts/check_hyperliquid.py` |
| TopStep futures | `ts-` | `futures`, `shared_scripts/check_topstep.py` |
| Robinhood | `rh-` | `spot` via `check_robinhood.py`, options via `check_options.py --platform=robinhood` |
| OKX | `okx-` | `check_okx.py` for spot/perps, `check_options.py --platform=okx` for options |
| Deribit options | `deribit-` | `check_options.py --platform=deribit` |
| IBKR options | `ibkr-` | `check_options.py --platform=ibkr` |
| Luno | `luno-` | Luno adapter/scripts |

Common entries:

```json
{"id":"momentum-btc","type":"spot","script":"shared_scripts/check_strategy.py","args":["momentum","BTC/USDT","1h"],"capital":1000,"max_drawdown_pct":60,"interval_seconds":300}
{"id":"deribit-vol-btc","type":"options","script":"shared_scripts/check_options.py","args":["vol_mean_reversion","BTC","--platform=deribit"],"capital":1000,"max_drawdown_pct":40,"interval_seconds":1200}
{"id":"ibkr-vol-btc","type":"options","script":"shared_scripts/check_options.py","args":["vol_mean_reversion","BTC","--platform=ibkr"],"capital":1000,"max_drawdown_pct":40,"interval_seconds":1200}
{"id":"ts-momentum-es","type":"futures","platform":"topstep","script":"shared_scripts/check_topstep.py","args":["momentum","ES","1h","--mode=paper"],"capital":1000,"max_drawdown_pct":5,"interval_seconds":3600}
{"id":"rh-sma-btc","type":"spot","platform":"robinhood","script":"shared_scripts/check_robinhood.py","args":["sma_crossover","BTC","1h","--mode=paper"],"capital":500,"max_drawdown_pct":5,"interval_seconds":3600}
{"id":"rh-ccall-spy","type":"options","platform":"robinhood","script":"shared_scripts/check_options.py","args":["covered_calls","SPY","--platform=robinhood"],"capital":5000,"max_drawdown_pct":10,"interval_seconds":14400,"theta_harvest":{"enabled":true,"profit_target_pct":60,"stop_loss_pct":200,"min_dte_close":3}}
{"id":"okx-sma-btc","type":"spot","platform":"okx","script":"shared_scripts/check_okx.py","args":["sma_crossover","BTC","1h","--mode=paper","--inst-type=spot"],"capital":1000,"max_drawdown_pct":5,"interval_seconds":3600}
{"id":"okx-sma-btc-perp","type":"perps","platform":"okx","script":"shared_scripts/check_okx.py","args":["sma_crossover","BTC","1h","--mode=paper","--inst-type=swap"],"capital":1000,"max_drawdown_pct":5,"interval_seconds":3600}
{"id":"okx-mom-btc","type":"options","platform":"okx","script":"shared_scripts/check_options.py","args":["momentum_options","BTC","--platform=okx"],"capital":5000,"max_drawdown_pct":10,"interval_seconds":14400,"theta_harvest":{"enabled":true,"profit_target_pct":60,"stop_loss_pct":200,"min_dte_close":3}}
```

Short-name conventions:

- Options: `vol_mean_reversion -> vol`, `momentum_options -> momentum`, `protective_puts -> puts`, `covered_calls -> calls`, `wheel -> wheel`, `butterfly -> butterfly`
- TopStep: `ts-{strategy}-{symbol}`
- Robinhood: `rh-{strategy_short}-{asset_or_symbol}`
- OKX: `okx-{strategy_short}-{asset}` for spot/options, `okx-{strategy_short}-{asset}-perp` for perps
- `triple_ema_bidir` is futures/perps only and needs `"allow_shorts": true`
- `session_breakout` is futures/perps only; short name `sbo`
- Multiple HL perps strategies on the same coin share an on-chain position; peer strategies must agree on `margin_mode` and exchange `leverage` (`sizing_leverage` may differ per peer, #497), and at most one peer may carry a non-zero stop owner (`stop_loss_pct`, `stop_loss_margin_pct`, or `trailing_stop_pct`, #491/#501). `LoadConfig` normalizes omitted stop fields on same-coin peers to explicit `0`, so the auto-SL fallback only fires for sole-owner strategies — set one explicit positive owner if a shared-position trigger is desired (#494). Sub-account isolation is the only correct path for fully independent direction/leverage/margin per strategy.

---

## Add Or Change Strategies

Open strategy source of truth: `shared_strategies/open/registry.py`.
Close evaluator source of truth: `shared_strategies/close/registry.py`.

Checklist for new spot/futures strategies:

1. Add the implementation and `@register(...)` entry in `shared_strategies/open/registry.py`.
2. Set `platforms=(...)` correctly; use variants for platform-specific defaults/descriptions.
3. Append the name to `PLATFORM_ORDER`.
4. Add short name and default strategy entries in `scheduler/init.go`.
5. Add a param grid to `DEFAULT_PARAM_RANGES` in `backtest/optimizer.py`.
6. Run registry and optimizer tests.

For close evaluators, add an `evaluate(position, market, params)` implementation under `shared_strategies/close/` and register it in `shared_strategies/close/registry.py`.

Do not edit `shared_strategies/open/spot/strategies.py` or `shared_strategies/open/futures/strategies.py` to add strategies; they are thin shims.

Before refactoring registry/shims, snapshot list output:

```bash
.venv/bin/python3 shared_strategies/open/spot/strategies.py --list-json > /tmp/spot.json
.venv/bin/python3 shared_strategies/open/futures/strategies.py --list-json > /tmp/futures.json
```

After changes, diff the outputs unless intentionally changing discovery.

---

## Custom Platform Integration

Gather:

- Platform name and ID prefix
- Supported products: spot, perps, futures, options
- API docs URL or `ccxt`
- Credential environment variable names
- Fees
- Assets and strategies
- Paper/live requirements

Implementation checklist:

1. Add `platforms/<name>/__init__.py`.
2. Add `platforms/<name>/adapter.py` with exactly one class ending in `ExchangeAdapter`.
3. Implement public adapter methods; check scripts must not touch private attributes.
4. Add `shared_scripts/check_<name>.py` only if existing entry scripts do not fit.
5. Add ID prefix inference in `scheduler/config.go`.
6. Add fee dispatch in `scheduler/fees.go`.
7. Add executor wiring only if a new live execution path is required.
8. Add config examples.
9. Add init wizard / `generateConfig` support if users should select it.
10. Add tests or pure helper tests for Go logic.

Adapter references:

- Spot: `platforms/binanceus/adapter.py`
- Perps: `platforms/hyperliquid/adapter.py`
- Futures: `platforms/topstep/adapter.py`
- Options: `platforms/deribit/adapter.py`

Verification:

```bash
.venv/bin/python3 -m py_compile platforms/<name>/adapter.py
.venv/bin/python3 -m py_compile shared_scripts/check_<name>.py
/opt/homebrew/bin/go -C scheduler build .
./go-trader --config scheduler/config.json --once
```

---

## Operator-Required Circuit Breakers

Some live venues lack a safe automated close path for per-strategy circuit breakers:

| Platform | Type | Pending key |
| --- | --- | --- |
| OKX | spot | `okx_spot` |
| Robinhood | options | `robinhood_options` |

When triggered, the scheduler enqueues `operator_required: true` and emits a CRITICAL warning every cycle until intervention.

Detect via `/status`:

```bash
curl -s localhost:8099/status | .venv/bin/python3 -c "
import json, sys
d = json.load(sys.stdin)
for sid, s in d['strategies'].items():
    pc = s['risk_state'].get('pending_circuit_closes') or {}
    for platform, p in pc.items():
        if p.get('operator_required'):
            legs = ', '.join(f\"{x['symbol']} size={x['size']}\" for x in p['symbols'])
            print(f'{sid} [{platform}]: {legs}')
"
```

Response:

1. Open the venue UI.
2. Flatten the listed positions manually.
3. Confirm positions are gone via `/status`.
4. Let the scheduler clear the pending on the next natural circuit-breaker reset, or reset the portfolio kill switch via owner DM if trading must resume sooner.

Do not confuse this with the portfolio kill switch. Portfolio kill switch is portfolio-level and runs automated close paths where available. Operator-required warnings are per-strategy and affect only the strategy that breached drawdown.

Kill-switch auto-reset: once the portfolio kill switch confirms all platforms are flat (`OnChainConfirmedFlat=true`), the next cycle clears virtual state and resumes trading. The bot posts `Virtual state cleared. Kill switch auto-reset; trading will resume next cycle.`

When multiple HL strategies trade the same coin on a shared wallet, the on-chain close fill is split across strategies by their **virtual quantity at snapshot time** (taken under RLock before the close mutates state), not by capital weight (#469). Trade and ClosedPosition rows therefore reflect each strategy's actual share of the fill, and a misconfigured caller whose strategy isn't among the peers receives `0, 0` rather than the entire portfolio fill.

Portfolio drawdown warnings repeat every cycle while drawdown remains in the warn band (`portfolio_risk.warn_threshold_pct`, default 60% of kill-switch). Silence by resolving the drawdown or changing the threshold.

Drain/live-exec failure alerts: repeated `DRAIN FAILURE` or `EXEC FAILURE` alerts mean a CB drain or live order failed. Check:

```bash
journalctl -u go-trader -n 100 | grep "liveExec\|drain"
```

---

## Implementation Patterns

- `StrategyConfig.Platform` is inferred from ID prefix in `LoadConfig`; prefer `s.Platform` over ID-prefix checks.
- Platform prefixes: `hl-`, `ibkr-`, `deribit-`, `ts-`, `rh-`, `okx-`, `luno-`; default is BinanceUS.
- Scheduler subprocess scripts must print valid JSON to stdout even on error.
- Python scripts exit 1 on error; Go still parses stdout.
- Per-strategy loop uses fine-grained locks; audit lock balance in `scheduler/main.go` before changing lock flow.
- Live helpers must check skip conditions before spawning Python, then skip state updates when execution fails.
- Bidirectional perps must thread `AllowShorts` through signal execution and live order sizing.
- Sort map keys before formatting operator-facing or test-asserted output.
- Add notification behavior through `MultiNotifier`.
- Native Go mark fetchers expose base URLs as vars for httptest stubs.
- Lifetime trade stats (`#T` / `W/L`) come exclusively from the SQLite `trades` table, grouped per `(strategy_id, position_id)` so partial closes collapse into one round trip (#471, #472). New trade-recording paths must populate `Trade.PositionID` (or rely on `RecordTrade`'s lookup against `s.Positions` / `s.OptionPositions`).
- Summary cadence is wall-clock and per-channel; if you add a new code path that posts summaries, thread `lastSummaryPost map[string]time.Time` and pass it to `ShouldPostSummary(freq, continuous, hasTrades, lastPost, now)`.

Useful audits:

```bash
grep -n "mu\.\(R\)\?Lock\(\)\|mu\.\(R\)\?Unlock\(\)" scheduler/main.go
grep -n "liveExecFailed" scheduler/main.go
```

---

## Tests

Preferred commands:

```bash
/opt/homebrew/bin/go -C scheduler test ./...
.venv/bin/python3 -m pytest
.venv/bin/python3 shared_strategies/open/test_registry_parity.py
```

If Go cache needs an explicit writable path:

```bash
env GOCACHE=/tmp/go-build-cache /opt/homebrew/bin/go -C scheduler test ./...
```

Go CI does not install `.venv`, so tests for subprocess-based live helpers should extract and test pure parsers or decision helpers rather than invoking Python.
