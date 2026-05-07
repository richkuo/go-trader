# Agent Setup Guide — go-trader

Repository: `https://github.com/richkuo/go-trader.git`

Concise skill entry point for agents setting up, configuring, operating, or extending go-trader. For broader context and PR conventions, see [AGENTS.md](AGENTS.md).

Quick flow for a new server: tell OpenClaw `install https://github.com/richkuo/go-trader and init`.

---

## Core Rules

- Run git from repo root.
- Use `/opt/homebrew/bin/go` (macOS) or `/usr/local/go/bin/go` (Linux) if `go` is not on PATH.
- Use `.venv/bin/python3`; in worktrees, use the main repo's `.venv`.
- Install Python deps with `uv sync`.
- Scheduler config: `scheduler/config.json` (start from `scheduler/config.example.json` when generating manually).
- State is SQLite only: default `scheduler/state.db`.
- Never store secrets in config files — put Discord/exchange credentials in systemd environment variables.
- Prefer `./go-trader init` for humans, `./go-trader init --json ... --output scheduler/config.json` for agents/scripts.
- TradingView export: ask which strategy IDs (or all) before running.

---

## Prerequisites

```bash
python3 --version
uv --version 2>/dev/null || echo "NOT_INSTALLED"
go version 2>/dev/null || /usr/local/go/bin/go version 2>/dev/null || /opt/homebrew/bin/go version 2>/dev/null || echo "NOT_INSTALLED"
git --version
```

Requirements: Python 3.12+, `uv`, Go 1.26.2, Git.

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
# Linux
curl -sL https://go.dev/dl/go1.26.2.linux-amd64.tar.gz | tar -C /usr/local -xzf -
# macOS
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

Build:

```bash
VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
/opt/homebrew/bin/go -C scheduler build -ldflags "-X main.Version=$VER" -o ../go-trader .
./go-trader --help
```

The `Version` ldflag appears in Discord summary titles; without it the binary reports `dev`.

---

## Configure

Human flow:

```bash
./go-trader init
```

Scripted flow:

```bash
./go-trader init --json '{"assets":["BTC","ETH"],"enableSpot":true,"spotStrategies":["momentum","rsi"],"spotCapital":1000,"spotDrawdown":60}' --output scheduler/config.json
```

The wizard covers assets, strategy groups, paper/live mode, per-strategy capital, live risk settings, Discord channels, auto-update mode. Prompts before overwriting.

Manual config rules:

- Strategy entries need `id`, `type`, `script`, `args`, `capital`, `max_drawdown_pct`, `interval_seconds`.
- `open_strategy` and each entry in `close_strategies` are objects of shape `{"name": "<id>", "params": {...}}` (#640/#642). Per-evaluator params (e.g. `tiered_tp_atr`'s `tiers`) live on the matching close ref, not on the strategy. Pre-v13 configs with a flat `params` map and string-typed `open_strategy`/`close_strategies` are migrated automatically on next start (synchronous, no DM); flat keys split per close-strategy ownership and everything else stays on the open ref.
- `discord.channels` / `telegram.channels` keys: `spot`, `options`, `hyperliquid`, `topstep`, `robinhood`, `okx`, `luno`, plus optional paper keys (e.g., `okx-paper`).
- `summary_frequency`: same key scheme. Values: `hourly`, `daily`, `every`, `per_check`, `always`, or Go durations (`30m`, `2h`). Wall-clock cadence persisted in SQLite (`app_state.last_summary_post`); survives restart/SIGHUP.
- Trades always force an immediate summary post regardless of cadence.
- `discord.owner_id` from `DISCORD_OWNER_ID`; enables DM upgrade/migration prompts.

Live-mode risk defaults prompted by init:

- Per-strategy spot drawdown: 5%
- Per-strategy options drawdown: 10%
- Portfolio kill-switch drawdown: 25%
- Portfolio warn threshold: 60% of kill-switch (warnings repeat every cycle while in band)

---

## Secrets

Set in systemd overrides or exported env vars before installation:

| Variable | Description |
| --- | --- |
| `DISCORD_BOT_TOKEN` | Discord bot token |
| `DISCORD_OWNER_ID` | Discord user ID for DM upgrades/migrations |
| `STATUS_AUTH_TOKEN` | Optional bearer token for `/status` |
| `BINANCE_API_KEY`, `BINANCE_API_SECRET` | Binance live |
| `HYPERLIQUID_SECRET_KEY`, `HYPERLIQUID_ACCOUNT_ADDRESS` | Hyperliquid live |
| `TOPSTEP_API_KEY`, `TOPSTEP_API_SECRET`, `TOPSTEP_ACCOUNT_ID` | TopStep live |
| `ROBINHOOD_USERNAME`, `ROBINHOOD_PASSWORD`, `ROBINHOOD_TOTP_SECRET` | Robinhood live |
| `OKX_API_KEY`, `OKX_API_SECRET`, `OKX_PASSPHRASE`, `OKX_SANDBOX` | OKX live/demo |
| `LUNO_API_KEY_ID`, `LUNO_API_KEY_SECRET` | Luno live |
| `GO_TRADER_ALLOW_MISSING_STATE` | `1` only for genuine first-run live deployments |

---

## Run And Install Service

Smoke test:

```bash
./go-trader --config scheduler/config.json --once
```

Install systemd:

```bash
mkdir -p logs
export DISCORD_BOT_TOKEN="{token}"
sudo bash scripts/install-service.sh
```

The installer copies the unit, runs `daemon-reload`, enables, starts, and pre-creates `logs/` so `ProtectSystem=strict` doesn't block first-run logging.

Templated multi-instance: `sudo bash scripts/install-service.sh systemd/go-trader@.service paper-testing`. Without starting: `NO_START=1 sudo bash scripts/install-service.sh`.

```bash
sudo systemctl start|stop|restart|status go-trader
journalctl -u go-trader -n 100 --no-pager
```

---

## Auto-Update

`auto_update`: `off` | `daily` | `heartbeat`. When an update is found, the bot notifies active Discord channels. With `DISCORD_OWNER_ID` set, it DMs the owner; replying yes within 30 minutes runs `git pull --ff-only`, rebuilds, saves state, restarts.

Manual update:

```bash
git pull --ff-only
VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
/opt/homebrew/bin/go -C scheduler build -ldflags "-X main.Version=$VER" -o ../go-trader .
sudo systemctl restart go-trader
```

Verify: `journalctl -u go-trader -f | grep -i "\[update\]"`.

---

## Post-Update Agent Protocol

When invoked after an update (manual `git pull`, auto-update restart, "I just updated" / "what changed"), walk the operator through anything new commits affect on their existing config, strategies, and open positions — and prompt before applying any opt-in. The binary's `runConfigMigrationDM` only handles fields registered in `configFieldRegistry` (≤ v3); newer config-version bumps and opt-ins land silently unless an agent surfaces them.

### Trigger

Run when ANY of:
- Operator says "I updated", "I just pulled", "what's new", or asks about migration.
- `git log -1 --format=%cI` is newer than the running binary's version (`./go-trader --version` or `curl -s localhost:8099/health` → `version`).
- `git status` clean and `git rev-list --count <running-version>..HEAD` > 0.

### Steps

1. **Identify the diff.** `git log --oneline <running-version>..HEAD -- scheduler/ shared_scripts/ shared_strategies/ platforms/`. If running version unknown, ask the operator (or fall back to last 30 commits).
2. **Classify** each commit:
   - **Auto-migration** — `CurrentConfigVersion` bumped; `MigrateConfig` rewrites JSON on next start. Summarize, no prompt.
   - **Runtime default change** — behavior shifts on existing strategies without a config edit. Prompt: confirm, or set explicit opt-out.
   - **New opt-in field** — feature dormant until field added. Prompt per affected strategy.
   - **Open-position constraint** — needs flat positions to apply. List affected; warn and skip until flat.
   - **Internal/no-op** — refactors, tests, docs. Mention briefly.
3. **Read current state.** Load `scheduler/config.json` and query `scheduler/state.db`:
   ```sql
   SELECT strategy_id, symbol, quantity, side FROM positions WHERE quantity > 0;
   SELECT strategy_id, symbol, contracts, action FROM option_positions WHERE contracts > 0;
   ```
4. **Prompt per item.** Default to no change if declined. For runtime defaults, also offer to write the explicit opt-out value.
5. **Apply via SIGHUP-safe edits** when supported (see "Reconfiguration"); else require full restart.
6. **Verify.** Tail logs for `[reload]`; on rejection, show reason and offer restart.

### Required prompt template

> Change: `<short description>` (PR #<N>)
> Affects: `<strategy IDs>` (and any open positions: `<symbol qty side>`)
> Default if you do nothing: `<what happens silently>`
> Options: 1) accept the new default, 2) opt out by setting `<field> = <value>`, 3) opt in to the new feature with `<field> = <value>` (requires flat? Y/N).
> Your choice?

Never apply runtime-default changes silently when the operator hasn't been shown the affected strategies. "Auto" means automatic JSON rewrite, not automatic behavior change.

### Reference: known categories

When in doubt, treat as runtime default and prompt. Regenerate from `git log --oneline -50` when stale.

**Auto-migration**
- `config_version` bump, deprecated field removal, silent field copy (e.g. v10 `sizing_leverage` ← `leverage`)
- v11 no-op bump (#546)
- `disable_implicit_close` removed in #508 — `true` + no `close_strategies` now uses implicit open-strategy close
- **v12 → v13 co-located strategy refs (#640/#642)** — `open_strategy: "name"` rewritten to `{"name":..., "params":{...}}`; `close_strategies: ["a","b"]` rewritten to `[{"name":"a","params":{...}}, ...]`; flat `params` map split between the open ref and each close ref by ownership (`tiered_tp_atr.tiers`, `tp_at_pct.pct`, etc. routed to the matching close ref). `args[0]` falls back as the open name; `type=manual` defaults to `"hold"`. Migration runs synchronously inside `LoadConfig` and rewrites the JSON file. Pre-v13 backtests via `--config` are rejected with a hint to start the live binary once for migration.

**Runtime default**
- HL stop-loss auto-derive from `max_drawdown_pct` (#493); margin mode default `isolated` (#486)
- Peer normalization of omitted stop/trailing fields (#494/#507; superseded by #601 — peers now place per-strategy sized stops)
- Shared-coin CB drain clears pending **without** on-chain close when peers share the coin (#515) — operator must flatten manually
- ATR(14) auto-injected + MISSING ENTRY ATR notifier for `tiered_tp_atr` (#525)
- Paper trailing now books synthetic closes — previously silently ignored (#532)
- **Top-level `default_stop_loss_atr_mult` default 1.0 for all-five-omitted HL perps/manual (#562/#601/#605)** — applies to shared-coin peers since #601 sizes protection per strategy. Per-strategy `stop_loss_atr_mult: 0` opts out one strategy; top-level `default_stop_loss_atr_mult: 0` opts out fleet-wide
- **EntryATR backfill (#568)** — pre-stamping or UI-opened positions get ATR stamped on next cycle, silently arming any `tiered_tp_atr` / `trailing_stop_atr_mult` / `stop_loss_atr_mult` previously inert
- **HL shared-coin reconcile (#565)** — `reconcileHyperliquidAccountPositions` closes virtual peers when (a) on-chain qty ≈ 0 (full flat) or (b) sole SL owner's residual matches non-owner peers' qty (owner's trigger fired). Ambiguous gaps still gap-only
- **Detector 3 — TP partial fill (#617)** — same-side residual + exactly one strategy with a cleared on-chain TP tier matching the drift → reconciler books external partial close on that strategy and shrinks virtual qty so next protection-sync re-arms SL/TP from true residual; multiple TP candidates → leave gap visible
- **`type=manual` reconcile (#576)** — manual strategies in `isHLLiveReconcilable`; UI/SL/TP closes clear scheduler state automatically (no more ghosts)
- **`type=manual` skips `CheckRisk` (#574)** — exempt from CB DD math (capital=0, funded ad-hoc)
- **HL real exchange fee (#585–#590/#603)** — scheduler-placed and reconciler-booked trades query `userFills` for real fee instead of modeled 0.035% estimate. #603 hardens response shape, warns to stderr on malformed `closed_pnl`. Pre-existing rows have `exchange_fee=0` — run `go-trader backfill hl-fees --all --apply` to correct (#591/#602 widened lookup window)
- **HL peer cash on external close (#584)** — non-SL-owner peers closed by Detector 1 get mark-based realized PnL credited to `strategies.cash` (was $0)
- **HL coin-size fill fallback narrowed (#600)** — when OID lookup misses, `lookupHyperliquidFillByCoinSize` now anchors on the newest matching record's OID group instead of summing every same-size match in the 24h window — unrelated same-size closes no longer conflate fees/PnL
- **`#T` counts positions opened, not round-trips (#608)** — `LifetimeTradeStats.PositionsOpened` (sourced from `is_close=0`) replaces `RoundTrips`; opens contribute immediately. W/L still round-trip aggregated
- **HL on-chain reduce-only TP suppresses in-process tiered close evaluators (#604)** — for HL perps strategies that place per-strategy on-chain reduce-only TPs, `tiered_tp_atr` / `tiered_tp_atr_live` are auto-stripped from `close_strategies` to prevent races with the on-chain limit fill. SL re-arming queries `userFills` to detect filled-externally
- **Tiered-TP final-tier dust fix (#592/#593)** — sole-peer final-tier closes use `market_close(sz=None)` to flatten on-chain residual; shared-coin peers still use sized close to avoid zeroing peer exposure
- **`type=manual` coin-sharing with HL perps (#619/#620)** — blanket validation ban (#599) replaced with owner guards; `shouldCloseFullPosition` prevents flattening peer exposure on full-close; all TP OIDs cancelled via `extraCancelOIDs`; peers must share `leverage` and `margin_mode`
- **HL SL size capped at on-chain qty (#621/#622)** — `hlSLEffectiveQty` caps stop-loss placement at `min(virtualQty, onChainQty)` to prevent rejected oversized orders after a manual TP; reconciler SL-close uses actual `FilledQty` from `userFills` for PnL/cash (was: stale virtual qty)
- **Trade SL/TP stamp on arm + protection-sync (#624/#625/#631)** — `trades.stop_loss_trigger_px`/`entry_atr` now backfill the moment `StopLossTriggerPx` arms (paper + live) and again after `applyHyperliquidProtectionSync` (TradeHistory + SQLite), so trade alerts show the SL price even when the SL is placed by the protection sync rather than the execute path
- **HL TP tier residual eliminated (#629)** — non-final tiers pre-floored to lot precision; final tier absorbs the remainder via integer-lot subtraction (`floor_size(size) - sum(non-final floors)`) so per-tier truncation can't strand an uncovered residual. Virtual qty normalized via `adapter.round_size` first to absorb Go float drift; sub-lot result skips the tier block with `[INFO]` log
- **Manual-open SL+TP placed inline (#633)** — was: SL armed immediately, TP[n] reduce-only orders deferred to the next scheduler cycle (and both skipped entirely if `--atr` omitted). Now: `placeManualProtectionInline` runs `--sync-protection` immediately after the fill, returning TP OIDs that round-trip via `pending_manual_actions.tp_oids_json`. `--atr` is optional — fallback `0.1*fillPrice/leverage` arms SL@1×ATR + TPs when omitted; operator notified via owner DM if fallback can't be computed (no leverage / no fill price)
- **Manual-open queue-failure cleanup (#635)** — when `InsertPendingManualAction` fails after a successful on-chain fill (disk full, DB locked), `attemptManualOpenCleanup` flattens the position via reduce-only market close sized to `fillQty` and cancels SL + TP OIDs in the same call; sized so peer manual/perps positions on the same coin are preserved; skipped under `--record-only`. Cleanup failures notify loudly — operator must flatten manually if both queue insert and cleanup fail
- **Discord SL line shows ATR multiplier (#638) + ATR before SL ordering (#639)** — open trade DMs and per-position summary extras now read `ATR | SL [×Nmult] | TP[i] | leverage`; sole-owner fixed-ATR stops display the multiplier next to the SL price so operators can confirm the configured `stop_loss_atr_mult` from the alert without checking config

**Opt-in field**
- `trailing_stop_pct` (#502); `trailing_stop_atr_mult` (#507 — initial trigger deferred one cycle)
- Open/close composition (#483); `stop_loss_margin_pct` (#490); `margin_per_trade_usd` (#520)
- `tiered_tp_atr_live` (#527 — `atr_source` param, falls back to entry ATR on warm-up)
- Regime detection `regime.enabled` + `allowed_regimes` (#541/#546/#558 — `Trade.Regime` column added on first start)
- **`type: "manual"` strategy + `manual-open` / `manual-close` CLI (#569)** — operator-driven HL perps with auto-defaults SL@1×ATR + `tiered_tp_atr_live` (TP1@2× / TP2@3×); can now share a coin with HL perps or another `type=manual` (#619/#620 — blanket ban lifted; owner guards + `shouldCloseFullPosition` + `extraCancelOIDs` prevent cross-strategy mutation; peers must agree on `leverage` and `margin_mode`). SL + TP[n] orders now placed inline on `manual-open` (#633) so the position is never naked between fill and the next scheduler cycle; `--atr` is optional — fallback `0.1*fillPrice/leverage` is used when omitted (risks ~10% margin at 1× ATR)
- **`discord.trade_alert_channels` / `telegram.trade_alert_channels` (#572/#573)** — optional map to route trade-fill alerts to a separate channel; omit to keep current behavior (summaries + alerts on same `channels` entry)
- **N-tier HL TP via `params.tiers` (#615/issue #612)** — list of `{atr_multiple, close_fraction}` (cumulative); default `[{1×,0.5},{2×,1.0}]`; final tier coerced to 1.0 so on-chain TPs sum to full position; non-numeric values rejected per tier. `Position.TPOIDs` / `positions.tp_oids_json` SQLite column (legacy `tp1_oid` / `tp2_oid` retained for rollback to pre-#615 — only first two tiers survive a downgrade)

**Internal / no ops impact**
- Discord column truncation/aliases (#514); registry split into open+close (#511)
- `close_fraction` honored — existing `close_strategies` configs partial-close as specified (#521)
- Discord SL/TP1/TP2/ATR position lines (#528/#529/#561); partial-close DMs as `TRADE CLOSED` (#530/#531)
- Backtester close registry with `--close-strategy`/`--close-params` (#535)
- HL adapter `cancel_trigger_order` → `cancel_order_by_oid` with backward-compat alias (#604)
- `shared_tools/hl_user_fills.py` consolidates fee-lookup helpers shared by `check_hyperliquid.py` and `close_hyperliquid_position.py` (#603/#598)
- **Backtester API aligned with co-located refs (#641/#643)** — `Backtester(open_strategy={"name":..., "params":...}, close_strategies=[{"name":..., "params":...}])` mirrors the live `StrategyConfig` shape. `run_backtest.py --close-strategy` now accepts both bare names and JSON refs and is repeatable; **`--close-params` is removed** — fold params into the JSON ref. New `--config <path> --strategy <id>` flow imports a single strategy from a v13+ live config and uses its open + close refs verbatim (single-mode only; compare/multi/optimize rejected upfront).
- **Startup compatibility probe (#645/#646)** — the binary now invokes each unique configured check script with `--probe-only` after notifier init and before the trading loop. On any non-zero exit it logs the rejecting script + stderr, DMs the owner if Discord is configured, and `exit 1` so systemd surfaces the breakage. Catches asymmetric deploys (new binary against stale Python that doesn't accept `--strategy-refs` — the May 7 outage). Operator action: if startup now fails after a `git pull` / auto-update, the probe is reporting that `shared_scripts/` didn't actually update — re-pull and rebuild rather than bypassing.

**Open-position constraint**
- `margin_mode`, exchange `leverage`, kill-switch identity changes
- HL `trailing_stop_atr_mult` / `stop_loss_atr_mult` nil↔positive toggle blocked while open

---

## Status

Default port `8099`. Override with `--status-port <port>` or `status_port` in config. If busy, server tries next 5 ports; check logs for `[server] Status endpoint at http://localhost:<port>/status`.

```bash
curl -s localhost:8099/status | python3 -m json.tool
curl -s localhost:8099/health
curl -s localhost:8099/history
```

If Discord enabled, wait for the first cycle and verify messages in configured channels. Report success with mode, # strategies, status URL, log command.

---

## TradingView Export

Export SQLite trades to a TradingView portfolio transaction-import CSV:

```bash
./go-trader export tradingview --strategy hl-btc-momentum --output tradingview-hl-btc-momentum.csv
./go-trader export tradingview --strategy hl-btc-momentum --strategy okx-eth-breakout --output tradingview-selected.csv
./go-trader export tradingview --all --output tradingview-all.csv
```

CSV header: `Symbol,Side,Qty,Status,Fill Price,Commission,Closing Time`. Built-in mappings cover known OKX and BinanceUS crypto pairs. Add `tradingview_export.symbol_overrides` for unmapped:

```json
"tradingview_export": { "symbol_overrides": { "hl:BTC": "BYBIT:BTCUSDT" } }
```

---

## `/go-trader` Command

When the user says `/go-trader`, "check bot status", "show strategy health", or "how are the bots doing":

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

Present output in readable prose. Highlight CBs, dead strategies, large PnL changes, missing status data.

---

## `/menu` Command

When the user says `/menu`, "show menu", "what can I configure", "what's available", or "help me get started":

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
     notional_cap_usd, risk_free_rate, correlation.*, summary_frequency,
     regime.enabled, regime.period, regime.adx_threshold
   Per-strategy: capital, max_drawdown_pct, interval_seconds, htf_filter,
     params, leverage, sizing_leverage, margin_per_trade_usd, stop_loss_pct,
     stop_loss_margin_pct, trailing_stop_pct, trailing_stop_atr_mult,
     trailing_stop_min_move_pct, margin_mode, allow_shorts, open_strategy,
     close_strategies, allowed_regimes, theta_harvest.*
   Discord/Telegram: enabled, channels, trade_alert_channels, dm_channels, owner_id
   Environment: Discord token, status token, exchange credentials

4. COMMANDS
   /menu
   /go-trader
   ./go-trader init
   ./go-trader init --json '{...}' --output scheduler/config.json
   ./go-trader manual-open <strategy-id> --side long|short (--size N | --notional N | --margin N)
   ./go-trader manual-close <strategy-id> [--qty N]
   ./go-trader backfill hl-fees [--strategy <id>|--all] [--apply] [--reset-cash]
   sudo systemctl start|stop|restart|status go-trader
   journalctl -u go-trader -n 50 --no-pager
   curl -s localhost:8099/status | python3 -m json.tool

5. BACKTESTING
   .venv/bin/python3 backtest/run_backtest.py --strategy <n> --symbol BTC/USDT --timeframe 1h --mode single|compare|multi|optimize
   .venv/bin/python3 backtest/backtest_options.py --underlying BTC --since 90 --capital 10000
   .venv/bin/python3 backtest/backtest_theta.py --underlying BTC --since 90 --capital 10000
```

---

## Manual Trading (HL perps)

Use `type: "manual"` on Hyperliquid for hand-driven entries/exits with scheduler-tracked P/L, close evaluators (default SL@1×ATR + `tiered_tp_atr_live` TP1@2× / TP2@3×), and Discord trade DMs (#569).

Config skeleton (no `script` / `args` / `interval_seconds` — `LoadConfig` fills them):

```json
{"id":"hl-manual-btc","type":"manual","platform":"hyperliquid","symbol":"BTC","capital":1000,"leverage":3,"max_drawdown_pct":10}
```

Multiple `type=manual` strategies and HL perps strategies may share a coin (#619/#620). Owner guards prevent cross-strategy mutation; full-close uses `shouldCloseFullPosition` to avoid flattening a peer's position; all TP OIDs are cancelled on full close. Peers must share `leverage` and `margin_mode`; at most one trailing-stop owner per coin.

CLI:

```bash
# Open — pick exactly one of --size, --notional, --margin
./go-trader manual-open hl-manual-btc --side long --size 0.01
./go-trader manual-open hl-manual-btc --side long --notional 500
./go-trader manual-open hl-manual-btc --side short --margin 100   # margin × leverage = notional

# Optional: pass live ATR for accurate SL/TP distances; omit to use fallback
./go-trader manual-open hl-manual-btc --side long --size 0.01 --atr 850

# Close — full or partial
./go-trader manual-close hl-manual-btc            # full close
./go-trader manual-close hl-manual-btc --qty 0.005

# Record-only (operator placed on HL UI; scheduler tracks)
./go-trader manual-open  hl-manual-btc --side long --size 0.01 --record-only --fill-price 67800
./go-trader manual-close hl-manual-btc --qty 0.005 --record-only --fill-price 68250
```

Notes:

- `--record-only` skips the live HL order; pair with `--fill-price`. SL is **not** auto-armed in record-only mode — place the trigger on the UI manually.
- SL and TP[n] reduce-only orders are placed **inline** on open (#633). `--atr` is optional: when omitted, `0.1*fillPrice/leverage` is used as a fallback ATR (≈10% margin risked at 1× ATR SL). Pass `--atr` for accuracy when you have the live indicator value.
- Open blocked when portfolio kill switch active or strategy has pending CB close.
- Fills queued in `pending_manual_actions`, applied at top of next scheduler cycle (need `--once` if daemon idle). If the queue insert fails after a successful on-chain fill, the position is auto-flattened and SL/TP cancelled (#635); cleanup failures notify loudly — flatten manually.
- A 99% partial close is **not** silently collapsed into a full close — the queue carries explicit `is_full_close` intent from `--qty`.
- External closes (UI, SL, TP) detected by reconciler and cleared automatically (#576) — no ghosts.
- `type=manual` exempt from CB drawdown checks (#574).

---

## Backfill HL Fees

HL `exchange_fee` was $0 for trades placed before #587. Backfill:

```bash
# Dry run
./go-trader backfill hl-fees --all
./go-trader backfill hl-fees --strategy hl-btc-momentum

# Apply (stop daemon first)
sudo systemctl stop go-trader
./go-trader backfill hl-fees --all --apply
sudo systemctl start go-trader
```

Notes:

- `--apply` refuses when another `go-trader` process is alive.
- Close-leg `realized_pnl` adjusted by `(modeled_fee − real_fee)`.
- `strategies.cash` replayed from `initial_capital` using corrected fee/PnL stream.
- Cash replay divergence > $1 (likely SIGHUP capital top-up) is WARNING and blocks `--apply` unless `--reset-cash` passed.
- Paper-mode HL strategies skipped (no real OIDs). Manual strategies included.
- Skip reasons (`missing_oid`, `no_fill_match`, `already_real_fee`) reported per row.

---

## Backtesting

Use `.venv/bin/python3` for all backtests.

```bash
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode single
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode compare
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --timeframe 1h --mode multi
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode optimize
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --since 90

# Close-strategy registry (#535/#641) — repeatable; max close_fraction wins.
# --close-strategy accepts both bare names and JSON refs ({"name","params"}).
# --close-params is removed — fold params into the JSON ref.
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --close-strategy tp_at_pct \
  --close-strategy '{"name":"tiered_tp_atr","params":{"tiers":[{"atr_multiple":1,"close_fraction":0.5},{"atr_multiple":2,"close_fraction":1.0}]}}'

# Backtest a live strategy verbatim (single mode only) — pulls the strategy's
# open + close refs from the live config (#643). Pre-v13 configs are rejected.
.venv/bin/python3 backtest/run_backtest.py --config scheduler/config.json --strategy hl-btc-momentum \
  --symbol BTC/USDT --timeframe 1h --mode single

# Regime gate (#549) — blocks entries outside allowed regimes; closes always execute
.venv/bin/python3 backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --regime-enabled --regime-period 14 --regime-adx-threshold 20 --allowed-regimes trending_up trending_down

.venv/bin/python3 backtest/backtest_options.py --underlying BTC --since 90 --capital 10000
.venv/bin/python3 backtest/backtest_theta.py --underlying BTC --since 90 --capital 10000
```

---

## Reconfiguration

After edits to `scheduler/config.json`:

```bash
sudo systemctl kill -s HUP go-trader   # hot reload (no state loss)
sudo systemctl restart go-trader       # full restart
```

Hot reload (`SIGHUP`) re-applies a safe subset: capital, drawdown, intervals, params, stop-loss (incl. `%`/ATR-mult trailing), sizing leverage, theta-harvest, portfolio risk knobs, summary cadence, correlation thresholds, `allowed_regimes` per-strategy, auto-update mode, Discord/Telegram channels and tokens. Refuses if strategy roster, script/args/type/platform, HTF filter, kill-switch identity, or DB path changed; refuses per-strategy exchange `leverage` / HL `margin_mode` while positions open. Global `regime` block (enabled/period/adx_threshold) requires full restart (mirrors `correlation`). Re-runs HL peer-on-same-coin check (`margin_mode`/exchange `leverage` agreement; at most one trailing-stop owner). On rejection, fall back to restart. Status server reflects new port immediately.

Common changes:

- Regenerate config: `./go-trader init`
- Scripted: `./go-trader init --json '{...}' --output scheduler/config.json`
- Channels: edit `discord.channels` / `telegram.channels`; update OpenClaw allowlist if needed; use `trade_alert_channels` to send fills to a different channel than summaries
- Token: `sudo systemctl edit go-trader`, add env override, restart
- Add/remove strategies: edit `strategies` array; removed strategies pruned from state
- Risk: edit strategy `max_drawdown_pct`, portfolio `max_drawdown_pct`, `portfolio_risk.warn_threshold_pct`
- Theta harvesting: add `theta_harvest` block to options strategy entries
- Paper → live: change `--mode=paper` to `--mode=live`, add `--execute` where required, configure exchange credentials

Changing `capital` does not reset cash/positions. Full reset: remove `scheduler/state.db` (or that strategy's rows) and restart.

---

## Adjustable Settings

Global config:

| Setting | Key | Default |
| --- | --- | --- |
| Check interval | `interval_seconds` | 300 |
| State DB | `db_file` | `scheduler/state.db` |
| Auto-update | `auto_update` | `off` |
| Status port | `status_port` | 8099 |
| Risk-free rate | `risk_free_rate` | 0.04 |
| Portfolio kill switch | `max_drawdown_pct` | 25 |
| Portfolio warn threshold | `portfolio_risk.warn_threshold_pct` | 60 |
| Correlation tracking | `correlation.*` | disabled |
| Summary cadence | `summary_frequency` | legacy defaults |
| Regime detection | `regime.enabled`, `regime.period`, `regime.adx_threshold` | disabled; period=14, threshold=20 |

Per-strategy:

| Setting | Key | Notes |
| --- | --- | --- |
| Capital | `capital` | Starting capital reference |
| Max drawdown | `max_drawdown_pct` | Strategy CB |
| Interval | `interval_seconds` | 0 uses global; auto-accelerates in DD warn band |
| HTF filter | `htf_filter` | Skips counter-trend signals |
| Open strategy params | `open_strategy.params` | Per-open overrides; no longer a flat top-level `params` map (#640). Migrated from legacy on first start |
| Close strategy params | `close_strategies[i].params` | Per-close evaluator overrides (e.g. `tiered_tp_atr.tiers`); each ref carries its own params so they don't leak into the open strategy |
| Allow shorts | `allow_shorts` | Required for bidirectional perps |
| Stop loss (price %) | `stop_loss_pct` | HL perps. Sole-owner auto-derives from `max_drawdown_pct` (cap 50) when omitted; same-coin peers need one explicit positive owner. `0` opts out. |
| Stop loss (margin %) | `stop_loss_margin_pct` | HL perps — leverage-aware; mutually exclusive with the other four owners. `0` opts out. |
| Fixed ATR stop | `stop_loss_atr_mult` | HL perps — trigger `avg_cost ± mult × entry_atr`, armed once after open. Top-level `default_stop_loss_atr_mult` defaults to `1.0` and applies to every HL perps with all five stop fields omitted (incl. shared-coin peers since #601) (#562/#601/#605); per-strategy `0` or top-level `0` restores `max_drawdown_pct` fallback. |
| Trailing stop (%) | `trailing_stop_pct` | HL perps — distance from high-water mark; mutually exclusive when positive. Live + paper (#532). Capped at 50%; `0` disables. |
| Trailing stop (ATR×mult) | `trailing_stop_atr_mult` | HL perps — `mult × entry_atr / avg_cost` frozen at open; mutually exclusive when positive. Live + paper (#532). Arms cycle after open once ATR exists. |
| Trailing debounce | `trailing_stop_min_move_pct` | Min trigger move before cancel/replace. Default 0.5%. |
| Exchange leverage | `leverage` | Perps — exchange margin/risk leverage and HL `update_leverage` (#497). 1× default. |
| Sizing leverage | `sizing_leverage` | Perps — notional multiplier (`cash * sizing_leverage`); defaults to `leverage` (#497). |
| Margin per trade | `margin_per_trade_usd` | Perps (opt-in) — `notional = min(margin_per_trade_usd, cash) × leverage`. Overrides `sizing_leverage`. SIGHUP-aware (#520). |
| Margin mode | `margin_mode` | HL perps, `isolated` (default) or `cross`. Applied from flat. |
| Open strategy | `open_strategy` | Override entry strategy name (else `args[0]`) |
| Close strategies | `close_strategies` | Ordered list; max `close_fraction` wins |
| Regime gate | `allowed_regimes` | Labels allowing entries (`trending_up`, `trending_down`, `ranging`); empty = allow all; needs `regime.enabled=true`; not on type=options |
| Theta harvest | `theta_harvest.*` | Options early-exit |
| HL on-chain TP tiers | `close_strategies[i].params.tiers` (where ref is `tiered_tp_atr` or `tiered_tp_atr_live`) | HL perps only — list of `{atr_multiple, close_fraction}` (cumulative). Default `[{1×,0.5},{2×,1.0}]`; final tier coerced to 1.0 so on-chain TPs sum to full position; non-numeric rejected per tier. Configuring tiers auto-suppresses the in-process `tiered_tp_atr*` close evaluator (#604/#615). Pre-v13 configs with flat `params.tiers` are routed to the matching close ref by `closeStrategyOwnedKeys` on migration (#640). |

Discord/Telegram:

- `enabled`
- `channels`: platform/type map for summaries + trade alerts (fallback)
- `trade_alert_channels`: optional override for trade fills only; same key scheme; SIGHUP-reloadable (#572)
- `dm_channels`: per-platform DM-style trade alerts
- `owner_id`: prefer `DISCORD_OWNER_ID` env

Correlation:

- `correlation.enabled`, `correlation.max_concentration_pct` (60), `correlation.max_same_direction_pct` (75)
- Warnings → all active channels + owner DM; snapshot in `/status`.

Regime detection (global opt-in):

- `regime.enabled` — must be `true` for any per-strategy `allowed_regimes` to fire
- `regime.period` — ADX lookback (Wilder), default 14
- `regime.adx_threshold` — below = `ranging`, default 20.0
- Valid labels: `trending_up`, `trending_down`, `ranging`. `AllowedRegimes` SIGHUP-compatible; global `regime` block needs full restart. Not on type=options.

---

## Strategy Reference

Source of truth:

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
| Hyperliquid manual | `hl-` | `manual` (#569), no script/interval; `manual-open`/`manual-close`; auto-defaults SL@1×ATR + `tiered_tp_atr_live` (TP1@2× / TP2@3×); can share coin with HL perps peers (#619/#620) |
| TopStep futures | `ts-` | `futures`, `shared_scripts/check_topstep.py` |
| Robinhood | `rh-` | spot via `check_robinhood.py`, options via `check_options.py --platform=robinhood` |
| OKX | `okx-` | `check_okx.py` (spot/perps), `check_options.py --platform=okx` for options |
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

- Options: `vol_mean_reversion → vol`, `momentum_options → momentum`, `protective_puts → puts`, `covered_calls → calls`, `wheel → wheel`, `butterfly → butterfly`
- TopStep: `ts-{strategy}-{symbol}`
- Robinhood: `rh-{strategy_short}-{asset_or_symbol}`
- OKX: `okx-{strategy_short}-{asset}` for spot/options, `okx-{strategy_short}-{asset}-perp` for perps
- `triple_ema_bidir` is futures/perps only and needs `"allow_shorts": true`
- `session_breakout` is futures/perps only; short name `sbo`
- Multiple HL perps strategies on the same coin share an on-chain position; peers must agree on `margin_mode` and exchange `leverage` (`sizing_leverage` may differ). Since #601 each peer places its own per-strategy sized reduce-only protection, so multiple peers can own fixed ATR / margin / trailing stops simultaneously. `LoadConfig` defaults all-five-omitted peers to `default_stop_loss_atr_mult` (#562/#601/#605); set per-strategy `stop_loss_atr_mult: 0` (one) or top-level `default_stop_loss_atr_mult: 0` (fleet-wide) to opt out. **Per-strategy CB (#515):** drain skips on-chain close when peers share the coin — exchange leg stays open until another path flattens. Sub-account isolation is the only path for full per-strategy independence.

---

## Add Or Change Strategies

Open: `shared_strategies/open/registry.py`. Close: `shared_strategies/close/registry.py`.

New spot/futures strategy:

1. Add implementation + `@register(...)` in `shared_strategies/open/registry.py`.
2. Set `platforms=(...)` correctly; use variants for platform-specific defaults.
3. Append name to `PLATFORM_ORDER`.
4. Add short name + default entries in `scheduler/init.go`.
5. Add a param grid to `DEFAULT_PARAM_RANGES` in `backtest/optimizer.py`.
6. Run registry + optimizer tests.

For close evaluators, add an `evaluate(position, market, params)` impl under `shared_strategies/close/` and register in `close/registry.py`.

Do not edit `shared_strategies/open/{spot,futures}/strategies.py` to add strategies — they are thin shims.

Before refactoring registry/shims:

```bash
.venv/bin/python3 shared_strategies/open/spot/strategies.py --list-json > /tmp/spot.json
.venv/bin/python3 shared_strategies/open/futures/strategies.py --list-json > /tmp/futures.json
```

Diff afterwards unless intentionally changing discovery.

---

## Custom Platform Integration

Gather: platform name + ID prefix; products (spot/perps/futures/options); API docs URL or `ccxt`; credential env var names; fees; assets/strategies; paper/live requirements.

Implementation:

1. `platforms/<name>/__init__.py`
2. `platforms/<name>/adapter.py` — exactly one class ending in `ExchangeAdapter`
3. Implement public adapter methods only (no private attribute access from check scripts)
4. `shared_scripts/check_<name>.py` only if existing entry scripts don't fit
5. ID prefix inference in `scheduler/config.go`
6. Fee dispatch in `scheduler/fees.go`
7. Executor wiring only if a new live execution path is needed
8. Config examples
9. Init wizard / `generateConfig` if user-selectable
10. Tests / pure helper tests for Go logic

Adapter references: spot — `binanceus`; perps — `hyperliquid`; futures — `topstep`; options — `deribit`.

```bash
.venv/bin/python3 -m py_compile platforms/<name>/adapter.py
.venv/bin/python3 -m py_compile shared_scripts/check_<name>.py
/opt/homebrew/bin/go -C scheduler build .
./go-trader --config scheduler/config.json --once
```

---

## Operator-Required Circuit Breakers

Some venues lack a safe automated close path:

| Platform | Type | Pending key |
| --- | --- | --- |
| OKX | spot | `okx_spot` |
| Robinhood | options | `robinhood_options` |

Triggered → scheduler enqueues `operator_required: true` and emits a CRITICAL warning every cycle until intervention.

Detect:

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
2. Flatten the listed positions.
3. Confirm via `/status`.
4. Let the scheduler clear pending on the next CB reset, or reset the portfolio kill switch via owner DM if trading must resume sooner.

Not the same as the portfolio kill switch (portfolio-level, runs automated close paths where available). Operator-required is per-strategy and affects only the strategy that breached drawdown.

Kill-switch auto-reset: once all platforms confirmed flat (`OnChainConfirmedFlat=true`), the next cycle clears virtual state and resumes trading. The bot posts `Virtual state cleared. Kill switch auto-reset; trading will resume next cycle.`

Multi-strategy HL coins: kill-switch fills split by **virtual quantity at snapshot time** (#469). Per-strategy CB on shared HL coins (#515) does not submit a close — reconcile manually if expected to flatten. Reconciliation (#565/#617): if HL flattens to ~0, sole-SL trigger fires (residual matches non-owner peers' qty), or a single TP tier filled externally (Detector 3, #617), the next cycle closes affected virtual peers automatically; ambiguous gaps still gap-only.

Portfolio drawdown warnings repeat every cycle while in warn band (`portfolio_risk.warn_threshold_pct`, default 60%). Silence by resolving DD or changing threshold.

Drain/live-exec failure alerts:

```bash
journalctl -u go-trader -n 100 | grep "liveExec\|drain"
```

---

## Implementation Patterns

See CLAUDE.md "Key Patterns" for full coding constraints. Notes:

- New trade-recording paths must populate `Trade.PositionID` (or rely on `RecordTrade`'s lookup against `s.Positions`/`s.OptionPositions`) so partial closes collapse into one round trip.
- New summary-posting paths must thread `lastSummaryPost map[string]time.Time` and call `ShouldPostSummary(freq, continuous, hasTrades, lastPost, now)`.
- `FormatCategorySummary` row labels use `summaryStrategyLabel` (fixed width + alias substitution); assert exact text in tests.

Audits:

```bash
grep -n "mu\.\(R\)\?Lock\(\)\|mu\.\(R\)\?Unlock\(\)" scheduler/main.go
grep -n "liveExecFailed" scheduler/main.go
```

---

## Tests

```bash
/opt/homebrew/bin/go -C scheduler test ./...
.venv/bin/python3 -m pytest
.venv/bin/python3 shared_strategies/open/test_registry_parity.py
```

If Go cache needs an explicit writable path:

```bash
env GOCACHE=/tmp/go-build-cache /opt/homebrew/bin/go -C scheduler test ./...
```

Go CI does not install `.venv`, so tests for subprocess-based live helpers should extract pure parsers/decision helpers rather than invoking Python.
