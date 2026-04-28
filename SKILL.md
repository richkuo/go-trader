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
- `summary_frequency` is a map keyed like channels. Values: `hourly`, `daily`, `every`, `per_check`, `always`, or Go durations such as `30m`, `2h`.
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
     params, stop_loss_pct, allow_shorts, theta_harvest.*
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

After edits to `scheduler/config.json`, restart:

```bash
sudo systemctl restart go-trader
```

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
| Stop loss | `stop_loss_pct` | Hyperliquid perps only, 0 disabled, max 50 |
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
.venv/bin/python3 shared_strategies/spot/strategies.py --list-json
.venv/bin/python3 shared_strategies/futures/strategies.py --list-json
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

---

## Add Or Change Strategies

Single source of truth: `shared_strategies/registry.py`.

Checklist for new spot/futures strategies:

1. Add the implementation and `@register_strategy(...)` entry in `shared_strategies/registry.py`.
2. Set `platforms=(...)` correctly; use variants for platform-specific defaults/descriptions.
3. Append the name to `PLATFORM_ORDER`.
4. Add short name and default strategy entries in `scheduler/init.go`.
5. Add a param grid to `DEFAULT_PARAM_RANGES` in `backtest/optimizer.py`.
6. Run registry and optimizer tests.

Do not edit `shared_strategies/spot/strategies.py` or `shared_strategies/futures/strategies.py` to add strategies; they are thin shims.

Before refactoring registry/shims, snapshot list output:

```bash
.venv/bin/python3 shared_strategies/spot/strategies.py --list-json > /tmp/spot.json
.venv/bin/python3 shared_strategies/futures/strategies.py --list-json > /tmp/futures.json
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
.venv/bin/python3 shared_strategies/test_registry_parity.py
```

If Go cache needs an explicit writable path:

```bash
env GOCACHE=/tmp/go-build-cache /opt/homebrew/bin/go -C scheduler test ./...
```

Go CI does not install `.venv`, so tests for subprocess-based live helpers should extract and test pure parsers or decision helpers rather than invoking Python.
