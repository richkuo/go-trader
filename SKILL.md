# Agent Setup Guide — go-trader

Repository: `https://github.com/richkuo/go-trader.git`

Concise skill entry point for agents setting up, configuring, operating, or extending go-trader. For broader context and PR conventions, see [AGENTS.md](AGENTS.md).

Quick flow for a new server: tell OpenClaw `install https://github.com/richkuo/go-trader and init`.

---

## Core Rules

- Run git from repo root.
- Use `/opt/homebrew/bin/go` (macOS) or `/usr/local/go/bin/go` (Linux) if `go` is not on PATH.
- Use `uv run --no-sync python` for dev/backtest/manual CLI; Go subprocess calls (scheduler) use `.venv/bin/python3` directly — deterministic relative path after `uv sync`, no PATH config needed.
- **Production:** bundled systemd units use `ProtectSystem=strict`; no `PATH`/`UV_CACHE_DIR` env injection needed for the scheduler since it calls `.venv/bin/python3` directly (#752/#753 reverted #748).
- Install Python deps with `uv sync`.
- Scheduler config: `scheduler/config.json` (start from `scheduler/config.example.json` when generating manually).
- State is SQLite only: default `scheduler/state.db`.
- Never store secrets in config files — put Discord/exchange credentials in systemd environment variables.
- Prefer `./go-trader init` for humans, `./go-trader init --json ... --output scheduler/config.json` for agents/scripts.
- TradingView export: ask which strategy IDs (or all) before running.
- **CRITICAL: ALWAYS use `scripts/update.sh` to update go-trader. NEVER manually run git pull + go build.** `update.sh` is the single source of truth for `git pull --ff-only` + `uv sync` + `go build` atomically. Manual steps cause asymmetric deploys (#642).

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

> Rebuilding the binary alone is unsafe after #642. The Go binary and Python check scripts share an argv contract (`--strategy-refs`, `--probe-only`, etc.); a build without `git pull` + `uv sync` from the same SHA can produce an asymmetric deploy. Use `bash scripts/update.sh` for any update past the initial install — it does pull + sync + build atomically.

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
- `open_strategy` and `close_strategy` are objects of shape `{"name": "<id>", "params": {...}}` (#640/#642; the close collapsed from an array to a single ref in #842 — a legacy `close_strategies` array of length ≤1 is still read, len>1 is rejected). Per-evaluator params (e.g. `tiered_tp_atr`'s `tp_tiers`) live on the close ref, not on the strategy. Pre-v13 configs with a flat `params` map and string-typed `open_strategy`/`close_strategies` are migrated automatically on next start (synchronous, no DM); flat keys split per close-strategy ownership and everything else stays on the open ref.
- **#841 canonical close keys:** the tier list is `tp_tiers` and each tier is `{"atr_multiple"|"profit_pct": N, "close_fraction": 0..1, "sl_after"?: {...}}`. The legacy tier-list key `tiers` is rewritten on-disk by the v15 migration (`config_migration_v15.go`) and is NOT read at runtime; per-tier legacy `atr` / `multiple` / `fraction` aliases are still read at runtime. Write the canonical names.
- **#844 trailing_tp_ratchet / trailing_tp_ratchet_regime:** a trailing-ATR stop where each cleared TP tier tightens the trail and optionally scales out. The strategy declares a positive strategy-level `trailing_stop_atr_mult` (the initial loose trail — and the SL owner; no other stop fields allowed). The close ref's `tp_tiers` is a list (plain) or `{regime: [tiers]}` (regime form, frozen at open via `Position.Regime`, keys matched to the `regime_atr_window` classifier — 3-state adx or 7-state composite). Each tier is `{atr_multiple, close_fraction?, trailing_mult_after | tp_atr_fraction}`: `close_fraction` (default `0`, cumulative target) scales out, `0` = trail-only rung; the trail tightens to `trailing_mult_after` (absolute ATR mult) **or** `tp_atr_fraction × atr_multiple` (relative) — mutually exclusive — monotonically (never loosens; the first rung must be ≤ the initial trail). Places **no on-chain TP**: partial closes ride the close evaluator, the on-chain SL rides the trailing-stop walker. Tier triggers use **entry ATR**. **Scope: HL perps + `manual`.** Backtestable. Example: `{"trailing_stop_atr_mult": 3.0, "close_strategy": {"name": "trailing_tp_ratchet", "params": {"tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.0, "trailing_mult_after": 2.0}, {"atr_multiple": 3.0, "close_fraction": 0.3, "tp_atr_fraction": 0.33}]}}}`.
- **#841 unified per-regime close block** (`tiered_tp_atr_regime` / `tiered_tp_atr_live_regime`): instead of a tier-keyed list, give the close ref a top-level `trend_regime` where each label owns its own plan — its stop loss and tier ladder co-located, varying freely per regime:
  ```json
  {"name": "tiered_tp_atr_live_regime", "params": {"trend_regime": {
    "trending_up": {"stop_loss_atr": 1.5, "tp_tiers": [
      {"atr_multiple": 2.0, "close_fraction": 0.5, "sl_after": {"kind": "trail_from_here", "tp_atr_fraction": 0.5}},
      {"atr_multiple": 4.0, "close_fraction": 1.0}]},
    "ranging": {"stop_loss_atr": 0.8, "tp_tiers": [
      {"atr_multiple": 1.0, "close_fraction": 1.0}]}
  }}}
  ```
  All regime labels must be present (exhaustive, no fallback); tier counts may differ per regime; every value under a label is a plain scalar (the regime is resolved once at the top, so `sl_after` carries no `trend_regime` sub-block). The block **owns the stop loss** via per-regime `stop_loss_atr` — declaring any strategy-level stop field (`stop_loss_atr_mult`/`stop_loss_atr_regime`/`stop_loss_pct`/`stop_loss_margin_pct`/`trailing_stop_*`) alongside it is rejected at load. The whole block is hot-reload-gated as a unit (changing it while a position is open is rejected — flatten first).
- `discord.channels` / `telegram.channels` keys: `spot`, `options`, `hyperliquid`, `topstep`, `robinhood`, `okx`, `luno`, plus optional paper keys (e.g., `okx-paper`).
- `summary_frequency`: same key scheme. Values: `hourly`, `daily`, `every`, `per_check`, `always`, or Go durations (`30m`, `2h`). Wall-clock cadence persisted in SQLite (`app_state.last_summary_post`); survives restart/SIGHUP.
- **Cadence defaults:** `options`, `perps`, `futures`, and `manual` channel types post every channel run (continuous); `spot` posts hourly. Override per channel via `summary_frequency`. (#890 — `manual` added to the continuous-cadence group, matching perps behavior.)
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
| `GO_TRADER_CASHFLOW_JOURNAL_ALARM` | `0`/`off`/`false`/`no` forces the legacy trade-ledger drift basis for HL shared wallets (default on — exchange-sourced cash-flow journal) |

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

`auto_update`: `off` | `daily` | `heartbeat`. When an update is found, the bot notifies active Discord channels. With `DISCORD_OWNER_ID` set, it DMs the owner; replying yes within 30 minutes runs `scripts/update.sh` (atomic git pull + uv sync + go build), saves state, and restarts.

Manual update:

```bash
# Systemd deploy (default)
cd /path/to/go-trader && bash scripts/update.sh --restart

# Linux bare-process deploy (no systemd)
cd /path/to/go-trader && bash scripts/update.sh --restart --restart-mode signal

# Sync from a source clone without clobbering secrets/state/venv/binary (#791)
bash scripts/update.sh --rsync-from /path/to/source-clone --restart

# Batch-update all go-trader-* siblings at once (requires --restart)
bash scripts/update.sh --all --restart [--update-all-root <parent-dir>]
```

`scripts/update.sh` is the single source of truth for `git pull --ff-only` + `uv sync` + `go build` (all three steps gated under `set -euo pipefail`). External deploy automation (Ansible, image bake, etc.) should call this script rather than reproducing the steps inline — that's how asymmetric deploys land.

**`--rsync-from <src>` (#791):** replaces `git pull --ff-only` with an `rsync` from a source clone into the deployment directory. Preserves `.git/`, `scheduler/config.json` (the exclude protects the file *or* the #1056 transition symlink → `/var/lib/go-trader[/<instance>]/config.json` — the real config out of the tree is never even in rsync's scope), `state.db` and WAL sidecars, `.venv/`, and the live binary; safe to use when the deployment directory has local changes or was not cloned from origin. Before the systemd restart, warns on stderr when any required `EnvironmentFile=` declared in the unit is missing (optional entries prefixed with `-` are skipped silently).

**Signal mode** (`--restart-mode signal` / `RESTART_MODE=signal`): SIGTERMs the PID in `GO_TRADER_PIDFILE` (default `./go-trader.pid`), respawns via `GO_TRADER_RUN_SH` (default `./run.sh`), then polls `/health` + PID freshness — same verify/rollback flow as systemd mode. Generate a starter `run.sh` with `bash scripts/create-run-sh.sh`. Signal-mode env vars: `GO_TRADER_RUN_SH`, `GO_TRADER_PIDFILE`, `GO_TRADER_SIGNAL_LOG`. **Systemd→signal fallback (#786):** when `--restart-mode systemd` encounters a missing unit (systemctl exit 5), update.sh automatically retries via signal mode if `go-trader.pid` and an executable `run.sh` are present — no operator action needed for mixed-mode deployments.

**Batch mode** (`--all`): **#1055** auto-discovers deployments from the systemd `WorkingDirectory` of every loaded `go-trader`/`go-trader-*`/`go-trader@*` unit (layout-independent — siblings need not share a parent dir) and runs the full update flow in each sequentially. `--update-all-root <dir>` / `GO_TRADER_UPDATE_ALL_ROOT` pins the legacy `go-trader-*/` glob and skips discovery (also the automatic fallback when `systemctl` is absent or no units load). Skipped dirs (non-dir or missing `scheduler/config.json`) are logged on stderr; a batch that updates nothing now fails loudly instead of reporting success. Each child inherits `GO_TRADER_SERVICE` — set per-worktree env if systemd unit names differ across instances.

Verify: `journalctl -u go-trader -f | grep -i "\[update\]"` (systemd) or `tail -f ./go-trader-signal.log` (signal mode).

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
- **v13 → v14 direction enum (#658)** — `allow_shorts: false` rewritten to `direction: "long"`, `allow_shorts: true` rewritten to `direction: "both"`; legacy key dropped. Default for new strategies is `"long"`. Use `"short"` to run any bidirectional strategy as a dedicated bear-only instrument (e.g. `ichimoku_cloud` + `direction: "short"` + `allowed_regimes: ["trending_down"]`) without writing a new strategy module. Migration is silent — no operator prompt needed.

**Runtime default**
- **Single `close_strategy` per strategy (#842)** — the `close_strategies` array (which ran every entry each cycle with "max `close_fraction` wins") collapsed to one `close_strategy` ref; one profit-taking close owns the exit and risk backstops belong at the strategy level. Configs may keep writing the legacy `close_strategies` array — a length-1 array is read as the single close; a length>1 array is rejected at load with the strategy id (collapse it). On-disk migration + `config_version` bump landed in v15 (#841/#853). `close_strategy` is the canonical key; `close_strategies` still parses. Internals: `StrategyConfig.CloseStrategy *StrategyRef`; the Go↔Python `--strategy-refs` wire still carries a length-≤1 `"closes"` list; backtest/simulate read `close_strategy` with legacy fallback.
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
- **HL on-chain reduce-only TP suppresses in-process tiered close evaluators (#604)** — for HL **live** perps strategies that place per-strategy on-chain reduce-only TPs, `tiered_tp_atr` / `tiered_tp_atr_live` are auto-stripped from `close_strategies` to prevent races with the on-chain limit fill. **Paper perps are never suppressed** — paper has no on-chain TPs and relies on the in-process close evaluator for tier exits (#781). SL re-arming queries `userFills` to detect filled-externally
- **Tiered-TP final-tier dust fix (#592/#593)** — sole-peer final-tier closes use `market_close(sz=None)` to flatten on-chain residual; shared-coin peers still use sized close to avoid zeroing peer exposure
- **`type=manual` coin-sharing with HL perps (#619/#620)** — blanket validation ban (#599) replaced with owner guards; `shouldCloseFullPosition` prevents flattening peer exposure on full-close; all TP OIDs cancelled via `extraCancelOIDs`; peers must share `leverage` and `margin_mode`
- **HL SL size capped at on-chain qty (#621/#622)** — `hlSLEffectiveQty` caps stop-loss placement at `min(virtualQty, onChainQty)` to prevent rejected oversized orders after a manual TP; reconciler SL-close uses actual `FilledQty` from `userFills` for PnL/cash (was: stale virtual qty)
- **Trade SL/TP stamp on arm + protection-sync (#624/#625/#631)** — `trades.stop_loss_trigger_px`/`entry_atr` now backfill the moment `StopLossTriggerPx` arms (paper + live) and again after `applyHyperliquidProtectionSync` (TradeHistory + SQLite), so trade alerts show the SL price even when the SL is placed by the protection sync rather than the execute path
- **HL TP tier residual eliminated (#629)** — non-final tiers pre-floored to lot precision; final tier absorbs the remainder via integer-lot subtraction (`floor_size(size) - sum(non-final floors)`) so per-tier truncation can't strand an uncovered residual. Virtual qty normalized via `adapter.round_size` first to absorb Go float drift; sub-lot result skips the tier block with `[INFO]` log
- **Manual-open SL+TP placed inline (#633)** — was: SL armed immediately, TP[n] reduce-only orders deferred to the next scheduler cycle (and both skipped entirely if `--atr` omitted). Now: `placeManualProtectionInline` runs `--sync-protection` immediately after the fill, returning TP OIDs that round-trip via `pending_manual_actions.tp_oids_json`. `--atr` is optional — fallback `0.1*fillPrice/leverage` arms SL@1×ATR + TPs when omitted; operator notified via owner DM if fallback can't be computed (no leverage / no fill price)
- **Manual-open queue-failure cleanup (#635)** — when `InsertPendingManualAction` fails after a successful on-chain fill (disk full, DB locked), `attemptManualOpenCleanup` flattens the position via reduce-only market close sized to `fillQty` and cancels SL + TP OIDs in the same call; sized so peer manual/perps positions on the same coin are preserved; skipped under `--record-only`. Cleanup failures notify loudly — operator must flatten manually if both queue insert and cleanup fail
- **Discord SL line shows ATR multiplier (#638) + ATR before SL ordering (#639)** — open trade DMs and per-position summary extras now read `ATR | SL [×Nmult] | TP[i] | leverage`; sole-owner fixed-ATR stops display the multiplier next to the SL price so operators can confirm the configured `stop_loss_atr_mult` from the alert without checking config
- **Portfolio peak rebaselined after strategy prune (#650/#653)** — when strategies are removed from config and the service restarts, `rebaselinePortfolioPeakAfterPrune` resets `PortfolioRisk.PeakValue` to the sum of surviving strategies' `RiskState.PeakValue` (falling back to configured `Capital` for cold starts), floored at `computeInitialPortfolioPeak`. Was: stale pre-prune peak survived restart and the first risk-check cycle latched the kill switch immediately on a shrunken book. No action needed — fires automatically on any prune
- **#954 trade-ledger display PnL + gross convention** — trades rows now store PRE-FEE `realized_pnl` with `exchange_fee` always stamped (`pnl_gross=1`, `fee_source` = `userfills`|`modeled`); HL shared-wallet display values derive from `initial_capital + Σ ledger + owned uPnL` (the wallet balance becomes a pure drift alarm, baseline-anchored on first cycle); hourly funding is booked as `trade_type='funding'` rows split by virtual-qty share; deposits/withdrawals/transfers land in `wallet_transfers`; live flips apportion the single real fill fee across close+open legs (was: full real fee on close + modeled fee on open, overcharging cash); no-mark-price external closes book at AvgCost (zero gross PnL, modeled fee) instead of dropping the row. Prompt the operator to run `./go-trader backfill trade-ledger --all --apply` (daemon stopped) so history is migrated and the drift baseline is meaningful — without it, legacy rows replay under net semantics (correct but fee-lossy for paper opens).
- **Update.sh hardening (#647/#648)** — `scripts/update.sh` is now the single source of truth for `git pull --ff-only` + `uv sync` + `go build` (under `set -euo pipefail`); both the operator-DM snippet and `applyUpgrade` (auto-update DM path) call it. Fails fast with a friendly error if `uv` is missing on PATH; `applyUpgrade` timeout bumped 180s→300s for cold dep bumps; output tail-trimmed to 1500 chars for DM. External deploy automation (Ansible, image bake) should call this script too
- **Owner DM on HL TP/SL fill (#661/#663)** — was: reconciler-detected TP/SL fills only surfaced via the next summary post (up to 5 min later). Now: all three reconciler detectors emit an owner DM the same cycle. Default on; disable with top-level `notify_tp_sl_fills: false`. Filled TP tiers also marked `✓` in Discord/Telegram position summaries (#662/#664) — no toggle
- **Sole-owner TP partial-fill attribution (#670/#672)** — was: non-shared HL perps strategies silently absorbed TP partial fills (no Trade row, no realized PnL credited, no `#T`/W-L update, no owner DM); final-tier flatten was booked at SL trigger price when the auto-cancel hadn't propagated. Now: `tryBookSoleOwnerTPFill` mirrors shared-coin Detector 3 — books partial drift / final-tier flatten at the actual VWAP fill price (or configured TP price as fallback) and emits a TP{n} owner DM. **Full-close attribution requires ALL `TPOIDs[i]==0`** so a residual closed by SL/operator/CB after a prior partial TP defers to the legacy SL branch instead of mis-attributing to the already-booked tier. After recovery booking, `stampSoleOwnerRecoveryTierConsumed` clears that tier's OID and arms `TPArmedTiers` so `hlAttemptCloseFromTPFills` cannot re-book the same TP OID on a later vanish (#758/#760). Fires automatically — no operator action.
- **TP-fired close attributed to TP fills, not SL trigger (#673/#675)** — was: `syncHyperliquidAccountPositions` saw a state position vanish on-chain with `StopLossOID > 0` and booked the close at the SL trigger price, producing fictitious losses on dual-TP flattens (HL auto-cancels the resting SL once on-chain qty hits zero). Now: `hlAttemptCloseFromTPFills` queries `userFills` for the SL OID first; if SL has no fills but TP OIDs do, books each filled tier as a partial close at VWAP. Falls through to the legacy SL path on confirmed SL fills (preserves the #621 partial-qty adjustment). Fires automatically.
- **manual-open ATR auto-fetch (#689/#690)** — was: `manual-open --atr <X>` was effectively required because omitting it triggered the leverage-aware fallback `0.1*fillPrice/leverage` (≈10% margin at 1× ATR) on every open. Now: when `--atr` is omitted the binary calls `check_hyperliquid.py --fetch-atr` to fetch ATR(14) from HL OHLCV at the strategy's symbol+timeframe — same baseline strategy opens get via `ensure_atr_indicator`. 50%-of-fillPrice plausibility guard mirrors `stampEntryATRIfOpened`. `computeFallbackATR` is preserved as a last-resort when the fetch fails (network error, insufficient candles), behind a single combined notifier message. The startup probe also exercises `--fetch-atr` against `check_hyperliquid.py`, so a stale Python missing `run_fetch_atr` fails the probe instead of silently degrading. Fires automatically — no operator action.
- **manual-open operator-friendly defaults (#691/#692)** — was: `manual-open` required explicit `--side` and one of `--size`/`--notional`/`--margin`; `type=manual` strategies with no stop fields inherited the fleet-wide `default_stop_loss_atr_mult` (typically 1.0× ATR). Now: `--side` defaults to `long`; when no sizing flag is passed (and not `--record-only`), `--margin 50` is auto-applied so `./go-trader manual-open <id>` is a valid smoke command; `type=manual` strategies with all five HL stop fields omitted default `stop_loss_atr_mult` to **1.5× ATR** (`defaultManualStopLossATRMult`) — non-manual HL perps keep the tighter fleet default. Explicit `stop_loss_atr_mult`/other stop fields still win; fleet `default_stop_loss_atr_mult: 0` opts manual out too. Init wizard `manualLeverage` default bumped from 10 to 20 (matches `--json` fallback in `generateConfig`).
- **Unknown per-strategy config keys now rejected (#704/#707)** — was: typos like `take_profit_pct` or pre-v13 `params`/`open`/`close_strategy` at the strategy root silently no-op'd, so a missed migration could run undetected against the configured defaults. Now: `LoadConfig` walks the strategies array a second time post-migration and rejects any key absent from `StrategyConfig`'s json tags, with targeted hints (`take_profit_*` → "TP lives under close_strategies"; `stop_loss_*` → list of five valid mutually-exclusive owners; `close_strategy`/`params`/`open` → "pre-v13 shape — use co-located refs"). One-line `[config] <id>: type=X open=Y close=[..] sl=... tp=...` summary is logged after load. Operator action: if startup now fails with `[config] unknown key`, fix or remove the listed key — agents can run `./go-trader inspect <id>` to see the post-migration effective shape side-by-side with the raw JSON before editing.
- **Trade-alert DM `Source:` line on close legs (#707/#719)** — was: every close-leg trade alert read identically regardless of trigger, so operators had to grep `Trade.Details` to tell an exchange SL fill from a paper trailing close. Now: close legs append `Source: <exchange SL|exchange TP{n}|close-strategy exit|external (peer/manual UI)|circuit breaker|paper SL|paper trailing SL|trailing SL>` derived from the Details prefix. `recordPerpsStopLossClose` writes the prefix via `stopLossCloseDetailsPrefix(reason)` so paper-mode and immediate trailing-SL closes don't masquerade as exchange SL fires. Open legs skip the line. No operator action; surface in next trade DM.
- **TP-fill never-armed gate (#719)** — was: `findHighestClearedTier` treated any tier with `pos.TPOIDs[i]==0` as "fired", so a tier whose first protection-sync placement failed (transient HL reject — OID stayed 0) would be advanced over the watermark on the next close-evaluator partial close, triggering the `sl_after` SL bump for a TP that never filled. Now: `Position.TPArmedTiers []bool` (`tp_armed_tiers_json`) records every tier observed with a positive OID at least once; cleared detection requires `armed[i]==true`. Legacy rows backfill conservatively (`armed[i] = oid[i] > 0`). Fires automatically. No operator action.
- **HL TP/SL fill alert includes exchange OID (#706)** — owner DMs from reconciler-detected fills now read `<FillType> filled (oid=<id>) — <strategyID>` so operators can map a Discord alert directly to a Hyperliquid `userFills` entry. No toggle.
- **Shared-coin reconciler SL attribution hardened (#754/#756/#757)** — was: Detectors 1 and 2 booked `hl_sync_stop_loss` whenever the SL OID was set and `userFills` returned any fill for that coin/size, which could mis-attribute a TP fill as a stop-loss on strategies with coinciding sizes. Now: all four SL-attribution call sites (sole-owner vanish, Detector 1, Detector 2, `hlAttemptCloseFromTPFills`) require `hlReconcileSLFillConfirmed` — exact OID match + positive `FilledQty` from `userFills`; unconfirmed SL routes to `hl_sync_external` (mark-based PnL). Fires automatically — no operator action. TP fill owner DMs from `hlAttemptCloseFromTPFills` (the sole-owner vanish TP-path) were also missing; now emitted parity with shared-coin detectors (#754/#755).
- **HL `--sync-protection` reconcile fill hints (#759/#761)** — Go forwards JSON snapshots from the same-cycle reconciler prefetch via `--reconcile-fill-hints-json`; Python skips a duplicate `userFills` fetch only when the hint confirms `filled: true` (false/malformed/unrelated OID still runs the indexer). JSON marshal failures log to stderr. Fires automatically — no operator action.
- **HL all-TP-tiers dust reconcile (#777/#778)** — was: when every TP tier filled but a tiny same-direction residual remained on-chain (exchange rounding), the reconciler silently resynced virtual qty to on-chain qty with no Trade row, creating a phantom quantity gap and a gap in realized PnL bookkeeping. Now: reconciler detects `all tiers armed + all OIDs zero + same-side dust` and calls `hlAttemptCloseFromArmedTPClears`, which books each tier as a partial close at `userFills` VWAP; OIDs fall back to the open-trade snapshot when protection-sync has already zeroed `pos.TPOIDs`. Qty-drift auto-resync is suppressed in this state to avoid clobbering the fill lookup. Fires automatically — no operator action.
- **Paper HL perps now run `tiered_tp_atr*` close evaluators (#781/#782)** — was: `hyperliquidPlacesOnChainTPs` did not check live-vs-paper, so paper perps running `tiered_tp_atr` or `tiered_tp_atr_live` had those evaluators stripped by `filterCloseStrategiesForHLOnChainProtection` each cycle, silently disabling the in-process TP exits. Now: the gate requires `--mode=live`; paper mode is never suppressed. Fires automatically — no config change needed.
- **Default-window label resolved correctly (#797/#799)** — was: `regime.enabled=true` with no explicit `regime.windows` emitted a `"default"` key but `RegimePayload.Label` remapped empty/default selectors to `primaryRegimeWindowKey` (which returns `""` when no windows are configured), so `regime_directional_policy` silently fell back to the base config direction/invert_signal (ignoring the policy table) and `allowed_regimes` gate failed open (all regimes admitted). Now: `Label` falls back to `"default"` when `regimeMultiWindowEnabled` is false, matching the check-script emission. **No config change required.** Strategies using `regime_directional_policy` or `allowed_regimes` without explicit `regime.windows` now behave correctly — verify entries were/are gated as expected after update.
- **HL perps direction orphan auto-close (#822/#823)** — during hl-sync reconcile, if a sole-owner live HL perps position's side conflicts with the strategy's *current* effective direction (resolved from `regime_directional_policy` when configured, else base `direction`), the reconciler queues a reduce-only market close. Booked as `regime_direction_flip`. **Scope is broader than `regime_directional_policy`**: also fires for static-direction strategies (e.g. `direction=long` with a seeded short). `direction="both"` never triggers. Detection lags by one scheduler cycle (reconcile reads prior cycle's regime state). **No config change required** — fires automatically for any sole-owner HL live perps strategy where position side contradicts effective direction. Operators should review open positions after enabling `regime_directional_policy` or changing `direction` to ensure no unexpected auto-close fires.
- **`storage.py` lazy DB init (#824/#825)** — `shared_tools/storage.py` previously called `init_db()` at import time, opening a SQLite WAL file at import. Under systemd `ProtectSystem=strict` the deploy directory is read-only, so the `simulate_strategy.py --probe-only` startup probe (which imports `backtester → storage`) failed with "unable to open database file" and exited 78, keeping the service down. Fixed: schema initialization is now lazy — `get_connection()` ensures the schema once per path on first real use; import is side-effect free. **No operator action required** — runtime behavior is unchanged where the filesystem is writable; only probes and read-only contexts benefit.
- **Composite (7-state) regime classifier unit fix (#861)** — was: `map_composite_label` thresholds were per-bar fractions (`return_pct=0.05`/`range_pct=0.03`) but the metrics fed in were ATR-multiples (window-spanning numerator over single-bar `standard_atr`), so `big_move`/`wide` were near-always true and the three `ranging_*` states (`ranging_quiet`/`ranging_volatile`/`ranging_directional`) were unreachable — pure mean-reverting markets mislabeled as `trending_*`. Now: ATR-efficiency normalization via `_composite_efficiency_metrics` (shared by live `latest_regime_composite` + backtest `compute_regime_composite`): `return_eff=(close_end-close_start)/(atr*period)`, `range_eff=(high-low)/(atr*period)`, Kaufman `efficiency=|net move|/Σ|bar-to-bar move| ∈ [0,1]`; a clean trend is now `efficiency ≥ efficiency_th (default 0.5) and high_adx` (narrow range relative to net travel — clean is NOT `wide`, correcting the prior backwards rule). New `efficiency` threshold added to both Python `_DEFAULT_COMPOSITE_THRESHOLDS` and Go `RegimeCompositeThresholds` (`regime_window_spec.go`, validated `(0,1]`) so operators can tune it without tripping the config unknown-key guard. Fires automatically — no config change required. ⚠️ **Behavior change:** strategies using `composite` regime windows that previously always read as trending will now correctly surface `ranging_*` states, so `allowed_regimes`/`regime_directional_policy`/`*_atr_regime`/unified-per-regime closes gating on composite labels will gate differently — review `composite`-window strategies after update.
- **Default TP ladder retune (#870)** — the system default tier ladder for every `tiered_tp_atr*` strategy that omits explicit `tp_tiers` changed from the old `[{1×,0.5},{2×,1.0}]` to a patient 3-rung `[{1.5×,0.4},{3×,0.8},{5×,1.0}]`. ⚠️ **On-chain impact for HL live strategies on defaults**: existing reduce-only TP orders will be repositioned from 1×/2× to 1.5×/3×/5× on the next protection sync. Validate with a live smoke before deploying. Regime ATR-TP variants (`tiered_tp_atr_regime`/`_live_regime`) now also have per-quality-group defaults (clean 4-tier, choppy/scalar 3-tier, ranging 2-tier) — strategies using `use_defaults:true` will get regime-differentiated ladders. `trailing_tp_ratchet_regime` per-group ratchet defaults added (since split into composite ranging substates — see #1059 below); opt out by providing explicit `tp_tiers`.
- **Manual regime stamp (#872)** — was: `stampPositionRegimeIfOpened` ran on all 5 perps dispatches but missed manual positions, so regime-keyed closes (`trailing_tp_ratchet_regime`, `tiered_tp_atr_regime`, etc.) on `type=manual` strategies silently never armed and regime label was blank in operator display. Now: regime stamped on manual open, strategy-level regime synced for `/status` display, and post-TP SL adjustment deferred until after stamp. Fires automatically — no operator action; check manual positions after update to confirm regime label is populated.
- **ATR-trailing SL armed inline at open (#885)** — was: HL perps strategies using `trailing_stop_atr_mult` or `trailing_tp_ratchet` had a one-interval gap after open where the position was live with no stop-loss on-chain. Now: `armTrailingStopAtOpenNow` runs immediately after the open fill on both live and paper dispatches (same cycle). No config change required — fires automatically for all trailing-SL strategies.
- **Ratchet default final-tier trail tightened (#887)** — `defaultTrailingRatchetTiers()` final rung changed from `0.5×ATR` to `0.8×ATR` at 3×ATR profit. Strategies using `use_defaults:true` or omitting `tp_tiers` on `trailing_tp_ratchet*` will now trail 0.8× instead of 0.5× at the last tier (less aggressive trailing at maturity). Set explicit `tp_tiers` to keep the old 0.5× behavior. Python `DEFAULT_RATCHET_TIERS` updated to match. Also: `standard_atr` now rounds to whole numbers only when ATR ≥ 100 (protects sub-dollar assets from being zeroed). No operator action needed for either change beyond awareness.
- **Composite clean-trend opening trail default tightened to 2.0 (#940/#949)** — for `trailing_stop_atr_regime` / `*_atr_regime` blocks resolving via `{"use_defaults": true}` on a `composite` regime window, the `trending_up_clean` / `trending_down_clean` opening-trail default changed from `2.5×ATR` to `2.0×ATR` (now aligned with the choppy group). Go (`regimeATRDefaults.Trailing`) and Python (`REGIME_ATR_DEFAULTS_TRAILING`) tables updated together; ratchet rung ladders and TP tiers unchanged. ⚠️ **On-chain impact for HL live strategies on defaults**: the initial trailing stop for a clean-trend position is now tighter (2.0× instead of 2.5×) — existing positions' SL repositions on the next protection sync. Set explicit per-regime ATR values to keep the old 2.5×. ADX-label defaults are unchanged.
- **Backtest `--config` entry-transform parity + v15 gate (#942/#951)** — `run_backtest.py --config <path> --strategy <id>` now: (1) requires `config_version>=15` (was 13) and rejects older files with a migrate-then-retry message — a pre-v15 file's legacy close keys silently no-op'd against the canonical evaluators, diverging from live; (2) applies `direction` and `invert_signal` to the signal (mirrors live order: invert then direction-gate), so a bidirectional or inverted config now backtests the same side it trades live; (3) rejects loudly at load any config with `regime_window_divergence` (#907, was silently ignored), a short/both direction with no close evaluator (unmodelable), or a stray `invert_signal` on a non-perps/manual strategy (which the live daemon also refuses). ⚠️ **If a backtest you ran before this update used a pre-v15 file or a directional/inverted config, re-run it** — the prior numbers may not match live. Migrate stale configs by starting the live binary once. No live behavior change.
- **Persistent shared-coin SL-gap operator alerts (#971/#974)** — when an HL shared-coin position partially drops but the reconciler cannot confirm the residual by exact stop-loss/TP OID (user-fills miss or wrong OID), it fails closed (no owner guessed, no SL booked), leaving a phantom virtual position that keeps feeding drawdown/kill-switch math. Previously this was only visible via `/status` + stderr `[WARN]`. Now `HLReconcileGapTracker` surfaces it as an owner DM after the gap persists 3 consecutive reconcile cycles, then re-throttles (material residual change / every 10th cycle / hourly) with a one-shot recovery notice when it clears or the coin stops being shared. **Alerting only** — never books, guesses an owner, or mutates a position; a transient miss self-heals on a later cycle. Fires automatically. Operator action only when an alert arrives: inspect the coin, confirm the exact on-chain residual, and flatten/reconcile by hand if it is genuinely stuck.
- **Strategy-audit report page (#956/#975)** — the in-dashboard UI gains a **Reports** sidebar section with a `/reports/strategy-audit` page: full strategy ranking table (sortable, color-coded keep/watch/deprecate/bug verdicts), deprecation list, supertrend-bug callout, out-of-sample table, and candidate verdicts. Loopback-only and drain-aware like the rest of the dashboard; no external assets. Read-only — no config or behavior change. View at `http://localhost:<port>/reports`.
- **Perps force-close `trade_type` relabel (#1008)** — circuit-breaker / kill-switch force-closes on HL and OKX perps (and HL `type=manual`) are now labeled `trade_type='perps'` instead of `'futures'` (those positions carry `Multiplier=1` as a PnL-valuation value, not a contract multiplier). **Display-only** — `tradeLedgerDeltaSQL` never reads `trade_type`, so ledger sums and the #954 display value are unaffected; only Discord/leaderboard/audit labels change. The shared-wallet drift alert now shows BOTH the raw reconciliation diff (Σ member − balance) and the post-baseline drift, so a large raw diff with small post-baseline drift reads as "investigate" rather than a hard accounting bug. Fires automatically — no operator action. If you adopted a drift baseline that masked the old phantom-PnL, a one-shot `wallet_ledger_state.baseline_set=0` re-anchors it on the next reconcile.
- **Corrupt-position zero-PnL force-close + close-action flip fix (#1009/#1010)** — two direction-reversal corruption roots fixed. (1) Under `direction="both"`, a fractional/full **close** action no longer flip-sizes a `posQty+newSize` reversal (the sizer now requires `closeFraction==0` for a flip, matching the executor), and `closeQty` is capped at the held quantity — so a position can no longer be driven to a persisted **negative** quantity that spammed shared-wallet drift alerts. A full close (`closeFraction==1.0`) under `"both"` also suppresses arming a fresh reduce-only SL on the just-closed position (was: orphan OID / HL order-cap burn / could cut a live peer on a shared coin). (2) A force/flatten/kill-switch close on a structurally **corrupt** position (qty≤0 or avg-cost≤0) books a **zero-PnL** `*_corrupt` leg (cash untouched) instead of a phantom realized PnL that diverged from the closed_positions row. Fires automatically — no operator action; genuine flips and partial closes are unaffected.
- **Shared-wallet aggregate fill apportionment (#1030)** — when shared-coin reconciler Detector-1 closes all peers on a coin-level external flat, a single userFills row sized to the aggregate virtual qty is now split across peers by virtual-qty share (`splitHyperliquidFillLookupByQty`) so each Trade carries a real fee, VWAP price, and OID for later ledger true-up. `backfill trade-ledger` uses the same apportionment for aggregate OIDs. Fires automatically — no operator action.
- **Force-close `reconcile_adjustment` fee_source (#1042)** — circuit-breaker / kill-switch model-only close legs (including options force-close) now stamp `fee_source='reconcile_adjustment'` and `pnl_gross=1` with zero exchange fee so they are visibly model-only rather than lost-OID exchange fills. `backfill trade-ledger` skips these rows from userFills true-up but still replays their cash effect; shared-wallet ledger planning also skips them. Fires automatically — no operator action.
- **Discovery-hidden deprecated strategies (#1034/#1035/#1040/#1041/#1038)** — `amd_ifvg`, `range_scalper`, `session_breakout`, and `vol_momentum` are omitted from `go-trader init` / `--list-json` (`DISCOVERY_HIDDEN_STRATEGIES` in `open/registry.py`) after M1 held-out failures; explicit `args[0]` / config refs and backtests still resolve them. **No operator action** for existing configs. New deploys won't see these in the wizard — use an explicit strategy name if you intentionally keep one.
- **Trailing stop keeps ratcheting under a latched circuit breaker (#1046)** — was: a latched per-strategy circuit breaker `continue`d past the whole per-strategy body, so an open HL perps position's trailing stop-loss stopped ratcheting for the latch's duration (up to 24h) — biting shared-coin positions the CB leaves open (force-close skipped when the coin is shared). Now: a latched CB on an HL perps strategy with an open position falls through in manage-only mode (signal forced to 0) so the trailing-SL/TP ratchet and protection-sync still run; every entry/add/flip/close stays suppressed (no new position can open). First-fire cycle (force-close/reduce-only drain) is excluded; other venues unaffected. Fires automatically — no operator action.
- **`regime_directional_policy` is now evidence-gated, default-OFF (#1085/#1087, #1076/#1084)** — the per-regime `direction`/`invert_signal` override no longer fires unless the `(asset, timeframe, classifier)` cell is **certified** in the SSoT artifact `backtest/research/regime_directional_certifications.json` — **shipped empty**, so every configured policy currently resolves to the strategy's **base direction** in live AND backtest. The gate is per regime-state: a certified state whose configured side contradicts the certified sign also falls back to base; `direction:"both"` never contradicts. Open positions freeze the cert map at entry (`Position.DirectionCertifiedStatesAtOpen`, SQLite `direction_certified_states_json`); uncertified/legacy open positions resolve to base, so a from-flat migration auto-closes a sole-owner position whose side is now un-evidenced (#822 `regime_direction_flip`), and shared-coin conflicts escalate CRITICAL + owner-DM. Configuring the block also now logs a **non-breaking `[WARN]`** at load: the regime→direction premise tested empirically false (#1076 — regime separates forward *volatility*, not forward returns), so the surface is advisory, not removed (forced-disable would strand shared-coin shorts). **Operator action:** if you relied on a directional policy actively switching sides, it is now inert until you certify the cell; review open HL perps positions after update for unexpected `regime_direction_flip` auto-closes. `inspect` and config-load surface the cert verdict; `GO_TRADER_DIRECTIONAL_CERT_PATH` overrides the artifact path.
- **Ranging ratchet ladder split into composite substates (#1059/#1068)** — composite-regime `trailing_tp_ratchet*` strategies resolving defaults (`use_defaults:true` / omitted `tp_tiers`) now get substate-specific ladders instead of one ranging ladder: `ranging_quiet` unchanged; `ranging_volatile` widens triggers to `1.0/2.0/3.0×ATR`; `ranging_directional` scales out `25/50/75%` over `1.0/2.0/3.0×ATR` plus a 4th let-ride rung at `4.5×ATR` (trail tightens to `0.6×`). Bare-ADX "ranging" still maps to `ranging_quiet`. ⚠️ **On-chain impact:** affected HL live positions reposition their reduce-only SL on the next protection sync. Opt out with explicit `tp_tiers`. Fires automatically — review composite ratchet strategies after update.
- **HL kill-switch no-OID fill repair (#1092)** — kill-switch closes on HL positions the exchange has **already flattened** (and lowercase-prefixed coins like `kPEPE`) no longer drop the real fill: `recoverHyperliquidAlreadyFlatFills` fetches recent `userFills` and books realized PnL/fee instead of dropping the row; shared-coin fills split by virtual qty, ambiguous shared-coin falls back to mark + `reconcile_adjustment`. Fires automatically — no operator action.
- **Shared-wallet drift / SL-gap WARN log throttled (#1088/#1089)** — the per-cycle `[WARN]` for a stable shared-wallet drift or reconcile SL-gap is now gated (onset + material-change + hourly heartbeat); the `cycles%10` re-alert was removed, so a stable already-alerted condition re-alerts hourly only (was every 10 cycles) and the WARN log volume drops sharply. Worsening sub-threshold drift still logs per material move. Fires automatically — no operator action.
- **Batch update auto-discovers deployments (#1055/#1057)** — `scripts/update.sh --all` now reads deployment dirs from the systemd `WorkingDirectory` of loaded `go-trader`/`go-trader-*`/`go-trader@*` units (any layout), falling back to the `go-trader-*/` glob only when systemd is absent or no units load; `--update-all-root`/`GO_TRADER_UPDATE_ALL_ROOT` pins the legacy glob. A zero-update batch now fails loudly. Operator action: none for standard systemd deploys.
- **Backtester models composite 7-state regimes (#1058/#1061)** — `run_backtest.py --config` now threads `regime.windows` into the backtester (primary medium-first window, composite or ADX) instead of legacy single-lookback ADX; `--allowed-regimes` is validated against the primary window's classifier vocabulary; an ADX-windows `--config` backtest now sources the **medium** window rather than top-level `regime.period`. By-name `--regime-windows-spec-json` is rejected alongside `--config` and in non-single modes. **#1067:** `--config` open-strategy name falls back to `args[0]` when `open_strategy.name` is empty. ⚠️ If you ran a `--config` regime backtest before this update, re-run it — the numbers may shift.
- **Exchange-sourced cash-flow journal for shared-wallet drift (#1100/#1103/#1104/#1105/#1106)** — the HL shared-wallet **total-drift alarm** is now driven by an exchange-sourced cash-flow journal that reconstructs settled-cash equity from the exchange's own fills + funding + transfers (durable per-stream cursors + per-event dedup), instead of summing the bot's internal trade rows. This removes false drift caused by any mis-priced internal row (modeled fee when userFills missed, mark-priced force-close, kill-switch residual) — the TOTAL now comes from the exchange while the internal trade rows stay the source of truth for **per-strategy attribution + member display** only. The trade-ledger basis is retained as a **fail-closed fallback** (journal incomplete/unmapped event kind, or persistent feed outage), and both bases are logged every cycle for comparison. **OKX and TopStep journals run in SHADOW only** — they log the journal-vs-trade-ledger comparison each cycle but do **not** drive those alarms yet (later phases of #1100). **Opt-out:** set `GO_TRADER_CASHFLOW_JOURNAL_ALARM=0` (or `off`/`false`/`no`) to force the legacy trade-ledger basis for HL. New SQLite tables `cashflow_journal` / `cashflow_journal_state` (additive, no config-version bump). Fires automatically — no config change; review the per-cycle drift-basis log line after update to confirm the journal is `usable` rather than falling back.
- **Manual close defaults to the regime ratchet (#1115/#1117)** — was: every `type=manual` strategy with no explicit `close_strategy` got `tiered_tp_atr_live` + a scalar `1.5×ATR` SL. Now: when `regime.enabled` is true AND the active classifier's vocabulary (ADX-3 or composite-7) maps cleanly onto the default per-regime opening-trail baseline, the manual close defaults instead to `trailing_tp_ratchet_regime` — a synthesized `trailing_stop_atr_regime` `use_defaults` block becomes the SL owner (the scalar SL self-suppresses) so the trail tracks volatility per regime. Falls back to the old `tiered_tp_atr_live` + scalar path when regime is off, an explicit `close_strategy`/stop field is set, or the classifier has no mappable per-regime trail; the selection is INFO-logged (`resolveManualRatchetRegimeTrailBlock`). `manual-open` arms the initial per-regime trailing SL inline (1.5×ATR fallback if the live regime can't be read) so the position is never naked. Tune via `manual_defaults.trailing_stop_atr_regime`. ⚠️ **This changes the manual close DEFAULT for regime-enabled configs** — operators with open manual positions relying on the default should set `close_strategy` explicitly before upgrading for continuity across restart; the daemon also emits a one-shot owner DM (`manualCloseEvaluatorDriftedFromTPs`) when an open position opened under a tiered-TP close (resting on-chain TP OIDs) is now under the ratchet default after upgrade — non-destructive, never auto-cancels the on-chain TPs. Review manual strategies after update.
- **Owner DM on trailing-ratchet tier trigger (#1110/#1112)** — was: a `trailing_tp_ratchet` / `trailing_tp_ratchet_regime` tier clearing and tightening the trail surfaced only via the next summary post. Now: a proactive owner DM fires the instant a tier newly advances the watermark AND tightens the trail (cleared tier, ATR-multiple threshold/price, mark, entry/anchor + entry ATR, profit in ATR and USD, old→new trail mult, intended SL trigger, next tier, resolution regime). Idempotent on the existing `SLAdjustedTiersProcessed` watermark — a watermark-only advance with no tighten and an already-processed tier never alert. Wired at all three ratchet sites (perps Signal==0 management, manual live, post-trade deferred open). Default on; disable with top-level `notify_ratchet_triggers: false`. Fires automatically — no config change required.

**Opt-in field**
- `trailing_stop_pct` (#502); `trailing_stop_atr_mult` (#507 — initial trigger deferred one cycle)
- Open/close composition (#483); `stop_loss_margin_pct` (#490); `margin_per_trade_usd` (#520)
- `tiered_tp_atr_live` (#527 — `atr_source` param, falls back to entry ATR on warm-up)
- Regime detection `regime.enabled` + `allowed_regimes` (#541/#546/#558 — `Trade.Regime` column added on first start)
- **`type: "manual"` strategy + `manual-open` / `manual-close` CLI (#569)** — operator-driven HL perps with auto-defaults SL@1.5×ATR (#691/#692; was 1×ATR) + `tiered_tp_atr_live` (TP1@2× / TP2@3×) **— #1115: this scalar-SL + tiered-TP default now applies ONLY when regime is disabled (or an explicit `close_strategy`/stop field is set); with `regime.enabled` and a resolvable per-regime trail, the manual close defaults to `trailing_tp_ratchet_regime` (synthesized `trailing_stop_atr_regime` use_defaults block owns the SL, no scalar stop), tunable via `manual_defaults.trailing_stop_atr_regime`, selection INFO-logged**; can now share a coin with HL perps or another `type=manual` (#619/#620 — blanket ban lifted; owner guards + `shouldCloseFullPosition` + `extraCancelOIDs` prevent cross-strategy mutation; peers must agree on `leverage` and `margin_mode`). SL + TP[n] orders now placed inline on `manual-open` (#633) so the position is never naked between fill and the next scheduler cycle (**#1115:** the ratchet-regime path resolves the per-regime opening trail at CLI time and arms the initial trailing SL inline too — `1.5×ATR` fallback if the live regime can't be read). **`--atr` is optional and now auto-fetched (#689/#690):** when omitted, the binary calls `check_hyperliquid.py --fetch-atr` to compute ATR(14) from the strategy's symbol+timeframe (same baseline strategy opens see via `ensure_atr_indicator`); falls back to `0.1*fillPrice/leverage` only if the fetch fails (network error, insufficient candles). **`--side` defaults to `long` (#691/#692);** when no sizing flag is passed (and not `--record-only`), defaults to `--margin 50`
- **`discord.trade_alert_channels` / `telegram.trade_alert_channels` (#572/#573)** — optional map to route trade-fill alerts to a separate channel; omit to keep current behavior (summaries + alerts on same `channels` entry)
- **Top-level `manual_defaults` block (#696/#697)** — optional overrides for the four hardcoded `type=manual` / `manual-open` defaults: `margin_usd` (50), `stop_loss_atr_mult` (1.5), `side` (`"long"`, lowercase required), `tp_tiers` (`[{2×,0.5},{3×,1.0}]`). Resolution order at every site is CLI/strategy-param → `manual_defaults` → hardcoded constant, so omitting the block preserves existing behavior exactly. Hot-reloadable via SIGHUP. `manual_defaults.stop_loss_atr_mult: 0` is a manual-only opt-out that doesn't affect non-manual HL perps; the fleet-wide `default_stop_loss_atr_mult: 0` still wins over both. `tp_tiers: []` is rejected at validation — omit the key to inherit the default. No config-version bump.
- **Regime-aware ATR multipliers across stop/TP surfaces (#733/#735)** — four HL-perps-only call sites resolve ATR multiplier from the active trend regime: `stop_loss_atr_regime` and `trailing_stop_atr_regime` (strategy-level), and `tiered_tp_atr_regime` / `tiered_tp_atr_live_regime` (close-strategy refs). Shape: `{"trend_regime": {"trending_up": {"atr": …}, "trending_down": {"atr": …}, "ranging": {"atr": …}}}` (close-ref tiers add `close_fraction`), or `{"use_defaults": true}`. Mutually exclusive with the five scalar SL fields. Regime frozen at open via `pos.Regime`; `_live_regime` re-resolves each tick. Requires `regime.enabled=true`. SIGHUP blocks scalar↔regime flips AND shape changes while open. **Backtester parity added in #737/#747** — `Backtester(stop_loss_atr_regime=..., trailing_stop_atr_regime=...)` and regime TP close refs now work; `--config <path> --strategy <id>` loads regime fields verbatim. No default behavior change; opt in by switching scalar → `_regime` sibling.
- **Post-TP stop-loss adjustment `sl_after` (#708/#710/#712/#835/#836)** — strategy-level default and/or per-tier rule on a `tiered_tp_atr*` close ref that cancel+replaces the on-chain SL when a TP tier fills. Scalar modes: `"breakeven"` (SL → AvgCost), `{atr_mult: N}` (SL → AvgCost ± N·EntryATR, signed; 0=breakeven, negative=behind entry), `{trail_from_here: {atr_mult: M}}` (perps only — converts to trailing at M·EntryATR), and `{trail_from_here: {tp_atr_fraction: F}}` (trail distance = F × firing tier's `atr_multiple`). **Regime-aware shapes (#736/#742/#835):** `{kind:"atr_offset","trend_regime":{...}}`, `{kind:"trail_from_here","trail_from_here":{"trend_regime":{...}}}`, and `{trail_from_here:{tp_atr_fraction:{trend_regime:{label:F}}}}`; resolves from the ATR regime label at fire time, supports composite labels via `regime_atr_window`, and defers when label missing. Scalar keys rejected when regime block present. Backtester supports scalar modes including scalar `tp_atr_fraction`; regime-aware `sl_after` remains HL-live-only ("HL-live-only" error). Per-tier overrides shadow strategy-level default. Scope: HL perps + `type=manual` (manual rejects `trail_from_here`). Idempotent; highest cleared tier wins. Requires fixed SL (`stop_loss_atr_mult`, `stop_loss_atr_regime`, `stop_loss_pct`, or `stop_loss_margin_pct`). SIGHUP rejects scalar↔regime or shape changes while open. Add via `close_strategies[i].params.sl_after` and/or `tiers[j].sl_after`. Example:
  ```json
  {"name":"tiered_tp_atr","params":{
     "tiers":[{"atr_multiple":1,"close_fraction":0.5,"sl_after":"breakeven"},
              {"atr_multiple":2,"close_fraction":1.0,"sl_after":{"trail_from_here":{"atr_mult":1.0}}}]}}
  ```
- **`trailing_tp_ratchet` / `trailing_tp_ratchet_regime` close (#844/#870)** — a trailing-ATR-stop close where each cleared TP tier monotonically tightens the trail and optionally scales out `close_fraction`. **Scalar form** (`trailing_tp_ratchet`): `trailing_stop_atr_mult` is the SL owner + initial trail. **Regime form** (`trailing_tp_ratchet_regime`): `trailing_stop_atr_regime` is the SL owner (#870 change — scalar `trailing_stop_atr_mult` rejected; provides per-regime opening trail). The close ref's `tp_tiers` carries the ratchet rungs (plain list or `{regime: [tiers]}`, frozen at open); `use_defaults:true`/omitting `tp_tiers` resolves to system defaults. Places **no on-chain TP**. **Scope: HL perps + `type=manual`.** Backtestable. Opt in by setting a `trailing_tp_ratchet*` close ref + the appropriate SL owner field. No default behavior change for existing strategies with explicit `tp_tiers`.
- **N-tier HL TP via `params.tiers` (#615/issue #612)** — list of `{atr_multiple, close_fraction}` (cumulative); default `[{1×,0.5},{2×,1.0}]`; final tier coerced to 1.0 so on-chain TPs sum to full position; non-numeric values rejected per tier. `Position.TPOIDs` / `positions.tp_oids_json` SQLite column (legacy `tp1_oid` / `tp2_oid` retained for rollback to pre-#615 — only first two tiers survive a downgrade)
- **New open strategies (#957–#973)** — five new entries registered (`go-trader init` / `/add-strategy`); existing configs unaffected until an operator adds one. `mtf_confluence` (HTF trend gate over LTF pullback entries, #957), `vol_momentum` (volatility-targeted ATR-normalized momentum, #959 — **deprecated/hidden from discovery #1040**), `funding_skew` (funding-rate z-score crowding extremes — needs HL funding history; backtester attaches a per-bar `funding_rate` column, #960), `regime_adaptive` (composite-metric breakout/mean-reversion switch, #958), and `regime_adaptive_htf` (HTF-classified composite-regime fades, #973). The first four register **bidirectional** futures variants (`direction: "short"` or `"both"` on HL perps); `regime_adaptive_htf` ships **long/flat only**. All are spot+futures registry entries with optimizer param ranges.
- **`atr_band_revert` (`abr`) new strategy (#1069)** — ranging mean-reversion: fade ATR-scaled bands around an SMA (long below `mid − k·ATR`; short above `mid + k·ATR` on the bidirectional futures/perps variant). Entries only — the exit is config, not code, so pair it with a `tiered_tp_atr` close ref (~`k_entry/2` and `k_entry` ATR tiers) + a `stop_loss_atr_mult`. Defaults `{period:20, atr_period:14, k_entry:1.5}`; spot is long-only, futures/perps register `direction:"both"`. `go-trader init` ships it **pre-gated** to the composite ranging substates `allowed_regimes:["ranging_quiet","ranging_volatile"]` (excludes `ranging_directional`) on a composite "medium" window. Tunable baseline — backtest before live. Existing configs unaffected until you add it.
- **`regime.display_windows` (#1062)** — optional top-level `[]string` that filters **which** regime windows render in the Discord/cycle summary; display-only, never affects gating, calculation, or persisted state. Names must be `regime.windows` keys (validated at load, case/space-insensitive — an unknown name or `display_windows` without `regime.windows` is rejected). Empty/omitted shows every window (legacy). Hot-reloadable via SIGHUP. No config-version bump.
- **Per-strategy circuit-breaker opt-out `circuit_breaker` (#1048)** — new `circuit_breaker: false` on a strategy disables BOTH circuit-breaker firing arms (drawdown > `max_drawdown_pct` AND 5 consecutive losses), for live and paper alike. Nil/omitted → enabled (the safe default). The gate only suppresses NEW fires — an already-latched CB and any pending circuit close still drain, and the drawdown/peak display keeps updating. A disabled, threshold-breached strategy emits a one-shot WARNING (no halt, nothing closed) so the missing auto-protection is visible mid-cycle, not just at startup; `cb=off` also shows in the startup summary and `go-trader inspect`. Hot-reloadable via SIGHUP including while a position is open (toggle is not restart-required); re-enabling resumes next cycle and may fire immediately if already past a threshold. `type=manual` is unaffected (exempt from `CheckRisk`). No config-version bump. Opt out only for strategies you deliberately want trading without the auto-protective halt.
- **Regime-profile allocation `regime_profile_allocation` (#998)** — opt-in per-strategy block that runs **two validated parameter profiles of one open strategy** and slowly switches between them as a long-window market regime changes. Scope: **HL perps live + paper**, requires `regime.enabled=true`. Shape: `{"window": "<long-window key from regime.windows>", "profiles": {"<regime label>": "<profile name>", …}, "param_sets": {"<profile name>": {<open-strategy param overrides>}, …}, "confirm_bars": N, "initial_profile": "<name>"}`. Exactly two profiles; `profiles` must cover every classifier label of the named window; `confirm_bars` ≥ 1 (a WARN fires below 12 — switching faster than the signals are curve-fit-prone). The switch is **hysteretic** (a new regime must persist `confirm_bars` closed bars) and **flat-only** — while a position is open the active profile is frozen to the one it opened under. The active profile's params merge over the base open-strategy params before each signal check, so it shapes the entry itself. Persisted across restarts (`strategies.active_profile`); the pending-switch counter re-arms from zero on restart. Hot-reload: any shape change is blocked while a position is open and resets the switch state when flat. Active/pending profiles surface in `/status` and trade-alert DMs. Backtestable verbatim via `--config` (or `eval_windows.py --profile-allocation`). No config-version bump. Opt in by adding the block; omit to keep single-profile behavior.

**Internal / no ops impact**
- Discord column truncation/aliases (#514); registry split into open+close (#511)
- **Trade DM extras enriched (#665/#668)** — open-trade DMs now show `| OID: <id>` on live fills (paper omits), reorder extras to `ATR | SL | TP[i] | leverage`, and append `(<n>x)` ATR-multiplier suffix on SL + each TP using `%g` so fractional tiers (1.25×, 2.5×) render exactly as configured. Shared `tradeAlertExtras` helper means Discord and Telegram can never drift on these lines.
- **SL ATR mult + TP tiers persisted on trades (#669/#671)** — every Trade row now snapshots the SL arming method (`stop_loss_atr_mult` REAL, NULL when armed via pct/margin) and the full TP tier list (`tp_tiers_json` TEXT) at fill time, mirrored on Position. Closes the analytics gap where back-computing muls or reading current-config tiers couldn't reconstruct what older trades were placed against after a config edit. Schema migration is idempotent; pre-#671 rows have NULL/empty for these columns.
- **Open-trade snapshot refactor (#674/#677)** — open trades now record `entry_atr` / `stop_loss_oid` / `stop_loss_trigger_px` / `tp_oids_json` / `stop_loss_atr_mult` / `tp_tiers_json` in a single INSERT via `recordPositionOpen` (deferred-open execute variants stamp protection between fill and `RecordTrade`). New `trades.stop_loss_oid` (INT) / `tp_oids_json` (TEXT) columns; migration is idempotent. `stampOpenTradeFromPosition` remains as the fallback path for late-armed protection (paper SL transition, post-`applyHyperliquidProtectionSync`).
- `close_fraction` honored — existing `close_strategies` configs partial-close as specified (#521)
- Discord SL/TP[1..n]/ATR position lines (#528/#529/#561); partial-close DMs as `TRADE CLOSED` (#530/#531). TP labels and prices in position extras + Discord/Telegram trade DMs now read from configured `tiers` instead of hardcoded 1×/2× — operators with custom `tiered_tp_atr*` tiers (e.g. 2×/3× or 3+ tiers) see the actual rendered TPs match their on-chain orders (#660)
- Backtester close registry with `--close-strategy`/`--close-params` (#535)
- HL adapter `cancel_trigger_order` → `cancel_order_by_oid` with backward-compat alias (#604)
- `shared_tools/hl_user_fills.py` consolidates fee-lookup helpers shared by `check_hyperliquid.py` and `close_hyperliquid_position.py` (#603/#598)
- **Backtester API aligned with co-located refs (#641/#643)** — `Backtester(open_strategy={"name":..., "params":...}, close_strategies=[{"name":..., "params":...}])` mirrors the live `StrategyConfig` shape. `run_backtest.py --close-strategy` now accepts both bare names and JSON refs and is repeatable; **`--close-params` is removed** — fold params into the JSON ref. New `--config <path> --strategy <id>` flow imports a single strategy from a v13+ live config and uses its open + close refs verbatim (single-mode only; compare/multi/optimize rejected upfront).
- **Startup compatibility probe (#645/#646)** — the binary invokes each unique configured check script with `--probe-only` after notifier init and before the trading loop. On any non-zero exit it logs the rejecting script + stderr, DMs the owner if Discord is configured, and exits with code **78** (`ExitProbeFailure` / `EX_CONFIG`). **#821:** both `go-trader.service` and `systemd/go-trader@.service` set `RestartPreventExitStatus=78` so a probe failure keeps the service down rather than restarting every `RestartSec` and spamming Discord. Missing-script failures (`"can't open file"`) now produce a distinct error message pointing to a deploy-tree gap rather than an argv mismatch. Operator action: if startup fails after `git pull` / auto-update, re-pull and rebuild; run `sudo systemctl daemon-reload` once if you updated the service files to pick up `RestartPreventExitStatus=78`.
- **Graceful shutdown on SIGTERM (#681)** — was: `systemctl restart go-trader` regularly hung ~90s in `deactivating (stop-sigterm)` until systemd's default SIGKILL, because in-flight Python subprocesses ran out their full 30s `scriptTimeout` per slot. Now: two-phase drain — read-only subprocesses (`check_*.py` / fetch helpers) are cancelled immediately on SIGTERM; side-effecting subprocesses (`--execute` / `close_*.py` / `--sync-protection` / trigger updates) are waited on up to 15s before SIGKILL backstop, so on-chain orders aren't killed mid-call (which would leave on-chain state with no local Trade row). State save / notifier flush / DB close run after the drain. Unit file sets `TimeoutStopSec=20`. Operator action: after `git pull`, run `sudo systemctl daemon-reload` to pick up the new `TimeoutStopSec` (the binary change works without it, but the unit-file change does not). Verify: `journalctl -u go-trader -n 50 | grep '\[shutdown\]'` should show `draining` → `State saved` → `Complete` after the next restart. `/health` returns 503 with `{"status":"draining"}` during the drain — k8s/ELB-friendly.
- **HL `closedPnl` field renamed to `ClosedPnLGross` (#698/#699)** — forward-looking guardrail: `userFills.closedPnl` is **gross** of trading fees (the HL UI shows net). No production code currently mis-uses this — `bookPerpsPartialCloseWithFillFee` computes realized PnL locally from `AvgCost`/`FillPx`/`Qty` minus the real fee, and `backfill hl-fees` recomputes from stored pre-fee PnL. The rename + regression test (`portfolio_closedpnl_gross_test.go`) ensures any future refactor that wires the gross value into `Trade.RealizedPnL` fails loudly. No operator action; no behavior change.
- **Misplaced subcommand now errors (#700/#701)** — was: `./go-trader --config foo manual-open <id> ...` (global flags before the subcommand) silently fell through to `flag.Parse()`, consuming `--config foo` and dropping the rest, which booted a second scheduler/Discord daemon instead of running `manual-open`. Now: `validateDaemonInvocation` rejects any positional args remaining after `flag.Parse()` with exit code 2; if the leftover token matches a known subcommand (`init`, `manual-open`, `manual-close`, `backfill`, `export`, `probe`, `version`) the error names it and reminds the operator that subcommands must precede global flags. Correct order is `./go-trader <subcommand> [subcommand flags]` or `./go-trader [global flags]` for the daemon — never both with the subcommand last. No behavior change for valid invocations.
- **Update.sh refuses missing config (#702/#703)** — was: running `bash scripts/update.sh` from a bare source clone (where `scheduler/config.json` is gitignored and absent) would `git pull` + `uv sync` + build first, then fail in the probe phase with a confusing `read config: open scheduler/config.json: no such file or directory`. The mutated tree sometimes drove operators to rsync source over a deployment, clobbering live state. Now: an early `[update] phase: preflight` check verifies `scheduler/config.json` exists in the repo root (after `cd "$repo_root"` so subdirectory invocations report the repo root) and exits 1 with explicit guidance before any mutation. Operator action: run `update.sh` from a deployment directory; for bare clones, copy `scheduler/config.example.json` → `scheduler/config.json` first.
- **`./go-trader inspect` subcommand (#704/#707)** — new CLI: `./go-trader inspect <strategy-id> [--all] [--json]` prints the post-migration, post-default effective shape of a strategy: resolved open + close refs with their params, which SL field won `EffectiveStopLossPct` (with explicit-vs-default provenance read from raw JSON), tier list resolved from the configured TP close ref, direction provenance. When `regime_directional_policy` is set, shows `base_direction` + per-regime direction/invert table; otherwise legacy `direction:` label (#784). Loads optional state DB when present for per-open-position `effective_direction` lines (symbol-sorted). Use to diagnose why a strategy isn't behaving like the JSON suggests without grepping code. Agent-friendly: prefer this over re-reading `config_migration.go`.
- **manual-open accepts interspersed flags + resolves --margin/--notional via mark fetch (#711/#713)** — was: `./go-trader manual-open hl-manual-btc --side long --margin 50` silently failed because stdlib `flag.Parse` stops at the first positional, dropping all flags after the strategy ID. Separately, `--margin` / `--notional` resolved to `0` coin qty in the non-record-only path because no mark price was fetched before sizing. Now: the wrapper reorders args before parsing (both `subcmd <id> --flag` and `subcmd --flag <id>` work), and the binary fetches the current HL mid before `resolveManualSize` so notional/margin sizing yields a non-zero order. Dry-run sizing failures prefix the line with `[sizing failed]` to avoid misreading a `0.000000 ETH` plan as real. Fires automatically — no operator action.
- **Post-TP SL replace capped at on-chain qty (#714/#717)** — was: when an `sl_after` rule fired after a TP partial cleared, `runPostTPStopLossAdjustment` issued the cancel+replace at the *virtual* qty, so HL rejected the order with "size too big" once on-chain qty had shrunk. Now: the SL replace threads `hlOnChainAbsQty` and applies `hlSLEffectiveQty` before the subprocess call (matching every other SL placement site). Fires automatically.
- **Framework-injected `regime` kwarg now reaches wrapper-shaped strategies (#720/#721)** — was: every registered open strategy uses the `def *_strategy(df, **params): return *_core(df, **params)` wrapper shape, but `strip_unsupported_position_context` short-circuited on `VAR_KEYWORD`, so framework-injected `regime` (and other position-context kwargs) sailed through `**params` into thin cores that crash with `TypeError: *_core() got an unexpected keyword argument 'regime'`. Now: the VAR_KEYWORD short-circuit is dropped — `regime` and `POSITION_CONTEXT_PARAM_KEYS` are forwarded only when the wrapper names them explicitly. Affects all 10+ wrapper-shaped strategies on every check script that injects regime (HL/OKX/RH/TopStep/spot). Fires automatically.
- **Regime-aware ATR backtester parity (#737/#747)** — `Backtester` now accepts `stop_loss_atr_regime`/`trailing_stop_atr_regime` dicts and regime TP close refs (`tiered_tp_atr_regime`/`tiered_tp_atr_live_regime`); `--config <path> --strategy <id>` loads regime fields from the live config. Regression suite in `backtest/tests/test_regime_backtester_737.py`. No operator action.
- **Regime label on Discord summary / leaderboard price lines (#741/#746)** — when `regime.enabled`, current market regime appended to inline price lines in channel summaries and leaderboard header (one label per base asset). Fires automatically. No operator action.
- **`sl_after` regime-aware shapes (#736/#742)** — `atr_offset` and `trail_from_here` sl_after rules can now accept a `trend_regime` block (same wrapper as the other #733 surfaces) in place of scalar `atr_mult`. SIGHUP blocks shape change while open. Backtester rejects regime-aware `sl_after` at init with "HL-live-only" error — parity is a follow-up. Operators opt in by replacing scalar `atr_mult` with `{kind:"atr_offset","trend_regime":{...}}` or `{kind:"trail_from_here","trail_from_here":{"trend_regime":{...}}}`. No default behavior change.
- **TP tier re-placement gate (#749/#751)** — `pos.TPArmedTiers` forwarded to `check_hyperliquid.py` as `--tp-armed-tiers-json`; Python now skips re-placing a tier whose OID==0 because it already fired rather than was never placed. Previously a TP1 fill would cause re-placement of TP1 at the cumulative fraction of the reduced position size. Fires automatically. No operator action.
- **Inspect provenance for regime TP prices (#738/#750)** — `go-trader inspect <id>` now shows per-regime tier prices and provenance (`use_defaults` vs. explicit) for `stop_loss_atr_regime`/`trailing_stop_atr_regime`/`tiered_tp_atr_regime`. Discord position summaries use regime-stamped TP prices for open positions. No config change required.
- **Update.sh atomic swap + rollback (#683)** — was: `scripts/update.sh` overwrote `./go-trader` directly during build, so a killed/failed `go build` could leave the live binary corrupted; restart was fire-and-forget with no verification. Now: builds to `go-trader.new`, probes against just-synced Python, atomic-swaps with `.prev` retention, and on `--restart` polls `systemctl is-active` + `/health` until `version` matches AND `MainPID` differs from the pre-restart PID. On timeout/mismatch the script resets git tree to the pre-pull SHA, re-syncs uv if HEAD advanced, and restores the `.prev` binary automatically. `HEALTH_TIMEOUT` default is **60s** (was 15s) to accommodate multi-script startup probes. Operator action: none — the auto-update DM flow and manual `bash scripts/update.sh --restart` both pick up the hardening transparently. If a rollback fires, the journal shows `[update] rollback: ...` with the failing phase; investigate before the next update attempt.
- **Embedded strategy dashboard at `/dashboard` (#734)** — the status server now also serves an HTML+JS dashboard at `http://localhost:<status_port>/dashboard` with per-strategy candle charts and trade markers, plus JSON endpoints `/api/strategies` and `/api/strategies/<id>/(candles|trades|status)` for tooling. Candles fetched via `shared_scripts/fetch_candles.py`, cached 30s. The dashboard reads `status_token` if configured — the page prompts once and persists to browser local storage. Internally, `StatusServer` gained `strategiesMu` (separate from the global state `mu`) so SIGHUP `UpdateStrategies` no longer deadlocks against the reload lock; lock ordering when both held: `mu → strategiesMu`. **Operator action:** none for default deploys. If you've exposed the status port beyond localhost, gate `/dashboard` behind an authenticated reverse proxy or VPN rather than relying on `status_token` alone (the page is a thin client; the JSON endpoints are the real surface).
- **Dashboard equity sparklines (#813)** — strategy sidebar cards now show a mini equity curve. New endpoint `/api/strategies/<id>/equity` returns `{points:[{t,v}]}` (up to 500 closed positions, independent of `sharpeLookbackLimit`). No operator action.
- **Dashboard sortable all-strategies table (#814)** — new table view listing all strategies with PnL%, win rate, Sharpe. New endpoint `/api/strategies/overview` returns `{strategies:[UIStrategyOverview]}` with `pnl_pct`, `win_rate`, `sharpe`, `regime`, `direction`. No operator action.
- **Dashboard color-coded PnL and dark mode (#807/#804)** — status grid PnL/drawdown values color-coded; dark mode toggle with theme persisted in browser local storage. No operator action.
- **Dashboard regime badge + mobile sidebar (#809/#810)** — regime label pill in topbar; collapsible sidebar drawer on mobile. No operator action.
- **Dashboard trade history panel (#808)** — scrollable trade history panel below chart. `/api/strategies/<id>/trades` response now includes a `trades` key (same markers array, oldest-first copy) in addition to `markers`. No operator action.
- **Dashboard strategy tuner (#811)** — new parameter editor with live signal preview. GET `/api/strategies/<id>/config` → editable fields + current params; POST `/api/strategies/<id>/config` → writes patched config to disk (atomic, validated via `LoadConfigForProbe`); POST `/api/strategies/<id>/simulate` → runs `simulate_strategy.py` via stdin and returns `{live_markers, simulated_markers}`. **Requires `status_token`** to be configured (POST endpoints return 403 otherwise). **Requires `config_version >= 13`** (auto-migrated on any previous start). Both `strategy_tuner_schema.py` and `simulate_strategy.py` are probed at startup — a stale Python will fail the probe. `options` strategies show "not supported" in the preview. After applying via the UI, either SIGHUP (for params/risk fields) or restart (for indicator/script fields). No operator action needed beyond ensuring `status_token` is set if you want to use the Apply button.
- **Discord summary spills full positions list to a dedicated message on split (#728/#729)** — was: when `FormatCategorySummary` had to split across multiple Discord messages, positions were packed into msg 1 until the ~2000-char limit, then truncated with `… and N more`, dropping operators mid-list. Now: when the summary doesn't fit, msg 1 carries header + leaderboard top chunk + `Positions: N open` + trades; msg 2 carries the **full** positions list with no truncation; leaderboard continuation chunks splice in between. Trades section also peels into its own message when msg 1 + leaderboard + trades would breach 2000 chars. Single-fits case unchanged. Operator action: none.
- **Update.sh honors custom systemd unit name (#727)** — `scripts/update.sh` previously hardcoded `go-trader.service` in the restart/verify path. Now the unit name is parsed from `${UNIT:-go-trader.service}` (or auto-derived for templated `go-trader@.service` deploys via the `INSTANCE` env var). Operators running multi-instance deploys (`/opt/go-trader-<name>/` with `go-trader@<name>.service`) get correct verification after `--restart` instead of the script polling the wrong unit. Operator action: none for single-instance deploys; for templated deploys, set `INSTANCE` or `UNIT` env when invoking `update.sh`.
- **Update.sh Go resolution + ExecStart vs swap warning (#764/#765)** — was: `go` had to be on `PATH`, so Linux tarball installs (`/usr/local/go/bin/go`) often failed preflight. Now: after `command -v go`, the script tries `/opt/homebrew/bin/go` then `/usr/local/go/bin/go` (see `scripts/update.sh --help`). With `--restart`, before `systemctl restart` it warns when `systemctl show ExecStart`'s binary realpath does not match the repo-root `./go-trader` this run just swapped — the service may still be pointing at another path. Operator action: install `go` on PATH or ensure a fallback path exists; if you see the ExecStart warning, fix the unit file (`ExecStart=`) or deployment layout so the daemon runs the same binary `update.sh` builds, then restart again.
- **Backtester regime gate look-ahead fix + closed-bar contract (#730/#731)** — backtester `ensure_regime_columns` previously wrote bar N's regime to row N, but entries fill at bar N+1 (post-signal-shift); the entry gate at row N+1 was therefore reading a regime label that wouldn't be knowable until *after* the decision. Now `ensure_regime_columns` shifts the regime column by 1 post-injection so the entry gate reads bar N-1's regime, matching the live timing (regime computed at decision time, order fills next bar). New top-of-file docstring documents the look-ahead contract; new regression suite `backtest/tests/test_backtester_lookahead.py` pins signal-fills-at-K+1, intra-bar-jump capture at next open, prior-bar regime usage (positive + negative), forward-peek inflation (caller responsibility), and a mechanical shift(1) canary. Mid-series NaN regimes also block entries after `fillna` — matches live "no regime data, no entry". No operator action; backtest results for strategies with `allowed_regimes` will shift slightly vs. pre-fix.
- **Open-strategy look-ahead bias fixes — amd_ifvg / liquidity_sweeps / chart_patterns (#732/#740)** — three caller strategies were peeking forward at data not yet observable: `amd_ifvg` selected entry IFVGs by distance to the **day's final close**; `liquidity_sweeps` read swing classification before the centered confirmation window completed; `chart_patterns` started breakout search at `swing_bar+1` instead of `swing_bar+lookback+1`. All three now respect the closed-bar contract. Backtest deltas (BTC/USDT, single mode, default params): `amd_ifvg` 15m Return -79.02% → -57.04%, Sharpe -0.81 → -0.36, PF 0.607 → 0.880 — the day-final-close peek was a confounder, not edge. `liquidity_sweeps` 1h slightly worse (-52.40 → -55.14). `chart_pattern` 1h slightly worse (-1.94 → -4.25). Regression tests added for all three. No operator action; live behavior unaffected (live signal generation never had access to forward bars), but backtest comparisons against pre-fix runs will differ.
- **Revert uv subprocess wrapper (#752/#753)** — PR #748's `uv run` subprocess path broke servers where `uv` is not on systemd's restricted PATH (default `curl | sh` installs go to `~/.local/bin`, which systemd's default `PATH` omits). Reverted to `.venv/bin/python3` in `executor.go` and `version_probe.go`; `scheduler/python_cmd.go` (`newPythonCommand`/`GO_TRADER_UV`) deleted; service units no longer inject `PATH`/`UV_CACHE_DIR`; `scripts/install-service.sh` no longer pre-creates the uv cache dir. `runPythonWithTimeout` and `pythonScriptTimeoutError` retained (still used by `backfill_hl_fees.go`). No operator action; behavior identical to pre-#748.
- **Discord category-summary TP tiers show ATR multiples (#763)** — open-position lines in hourly/per-channel summaries append a `%g`-formatted `(Nx)` suffix on each TP tier (same convention as trade-alert extras), so summary TP lines match trade DMs. No operator action.
- **Update.sh signal-mode restart + batch update (#766/#767)** — `scripts/update.sh` now supports two restart modes. Default `--restart-mode systemd` unchanged. New `--restart-mode signal` (or `RESTART_MODE=signal`) for Linux bare-process deploys: SIGTERMs the PID from `GO_TRADER_PIDFILE` (default `./go-trader.pid`), respawns via `GO_TRADER_RUN_SH` (default `./run.sh`), then polls `/health` + PID freshness with same verify/rollback flow as systemd mode. Generate a starter `run.sh` via `bash scripts/create-run-sh.sh`. New `--all` flag (requires `--restart`) batch-updates all `go-trader-*/` directories under `GO_TRADER_UPDATE_ALL_ROOT` (default: parent of repo). New env: `GO_TRADER_RUN_SH`, `GO_TRADER_PIDFILE`, `GO_TRADER_SIGNAL_LOG`, `GO_TRADER_UPDATE_ALL_ROOT`. Operator action: none for existing systemd deploys. Signal-mode operators: create `run.sh` with `scripts/create-run-sh.sh`, set `RESTART_MODE=signal` or pass `--restart-mode signal`.

- **`./go-trader agent-info` subcommand (#1051)** — new CLI: dumps a self-describing JSON snapshot of the binary's surface so an LLM agent discovers capabilities from the binary itself rather than drift-prone hand docs — capabilities, reflection-derived config schema, env vars, state-DB schema, a read-only live-state snapshot (open positions incl. `option_positions`), and open/close strategy modules. `--bootstrap-md` writes `AGENTS.generated.md` (never `AGENTS.md`, which is a CLAUDE.md symlink); `--append-changelog` keeps a version-stamped history (capped at 50). Read-only by construction — loads a temp copy of the config (no in-place migration) and opens the state DB `mode=ro`, so it never mutates state or creates a stray `state.db`. `scripts/update.sh` refreshes the doc post-swap (best-effort, never fails a deploy); `AGENTS.generated.md` is gitignored (embeds a position snapshot). No operator action.
- **HL /info burst mitigation (#768/#769)** — HL adapter now caches `spot_meta`+`meta` to `/tmp/hl_meta.json` (60-min TTL) shared across all go-trader instances on the host; symbol-miss forces a fresh fetch. Go forwards `allMids` snapshot via `--mark-price` to skip a duplicate `get_spot_price` call, and `clearinghouseState` leverage/margin-mode via `--account-leverage`/`--account-margin-mode` to skip a per-cycle `get_position_leverage` call. 429 from `lookup_fill_fee_by_oid` returns `{}` immediately instead of retrying; modeled-fee fallback preserves bookkeeping. `executeProbeArgv` added to probe `check_hyperliquid.py` in execute mode at startup (asymmetric deploys fail fast). Fires automatically — no operator action.
- **Two-leg pairs backtester (#771)** — new `backtest/backtest_pairs.py` (`PairsBacktester`): standalone simulator for beta-hedged long/short pairs driven by rolling z-score of log spread. Research/analysis tool only — no live execution path. No operator action.
- **`invert_signal` for HL perps/manual (#774/#776)** — new `StrategyConfig.InvertSignal bool` field (`invert_signal`): flips BUY↔SELL on non-zero signals from `runHyperliquidCheck` before execution. Lets inverse-trend variants reuse the same open/close strategy refs without forking the Python module. HOLD (0) never flipped. Composes with `direction`: invert runs in the Go layer before direction interprets the resulting sign, so `direction="short"` + `invert_signal=true` is valid and distinct from plain `direction="short"` (opens short on raw-BUY vs. raw-SELL respectively). Rejected only on non-HL-perps/manual strategies. SIGHUP-blocked while positions are open. Default off; opt in by setting `invert_signal: true`.
- **Regime-aware directional policy `regime_directional_policy` (#779/#780)** — new `StrategyConfig.RegimeDirectionalPolicy` field: per-regime override for `direction` + `invert_signal` so a single HL perps strategy automatically switches long/short/inverse mode as the market regime changes, without operator SIGHUP or hot-edits. Shape:
  ```json
  "regime_directional_policy": {
    "trend_regime": {
      "trending_up":   { "direction": "long",  "invert_signal": false },
      "trending_down": { "direction": "short", "invert_signal": true },
      "ranging":       { "direction": "long",  "invert_signal": false }
    }
  }
  ```
  All three canonical labels (`trending_up`, `trending_down`, `ranging`) required — no undefined runtime fallback. **Resolver semantics:** when flat, resolves from current cycle's regime (fresh entry decision); when a position is open, resolves from `pos.Regime` stamped at open ("hold until natural exit" — the position runs under the policy it opened with until natural SL/TP/close-evaluator exit; new entries in the opposing direction never fire because `PerpsOrderSkipReason` gates on the resolved `Direction`). `base_direction`/`base_invert_signal` remain the static fallback when the block is absent or regime detection disabled. Requires `regime.enabled=true` at top level. HL perps only. SIGHUP: shape add/remove/mutate blocked while a position is open; change-when-flat applies on next cycle. `/status` surfaces `base_direction`, `base_invert_signal`, `effective_direction`, `effective_invert_signal`, `regime_directional_policy` (bool flag), `effective_policy_regime` per strategy. Backtest parity (#1025): `run_backtest.py --config` replays the same per-cycle resolver. Default off; opt in by adding the block. ⚠️ **As of #1085 the policy is evidence-gated default-OFF** — it resolves to base direction until the `(asset, timeframe, classifier)` cell is certified in `regime_directional_certifications.json` (shipped empty), and configuring it logs a non-breaking `[WARN]` (#1076: the regime→direction premise is a validated negative result). See the Post-Update runtime-default entry.
- **Perps direction validation honors regime policy (#783/#784)** — was: startup `ValidatePerpsDirectionConfig` compared open position side to base `direction` only, so `regime_directional_policy` strategies could false-alarm (e.g. short opened under `trending_down` while base `direction` is `long`). Now: validation uses stamped `pos.Regime` via `EffectiveDirectionForPosition` (same hold-on-transition as live); unstamped legacy positions skip the warning when any policy regime allows the side. `inspect` direction section aligned (#784). Fires automatically — no operator action unless you previously ignored a `[WARN] perps state-vs-config gap` that was a false positive.
- **Multi-window regime detection (#792/#793)** — new `regime.windows` map: run independent ADX classifiers per named horizon (value = ADX period in bars); empty = legacy single-lookback unchanged. Per-strategy `regime_gate_window`/`regime_atr_window`/`regime_directional_window` selectors route each consumer to a different horizon. `RegimePayload` from check scripts is now string (legacy) or JSON dict keyed by window name. `positions.regime_windows_json` SQLite column added (migration idempotent). OHLCV limit scales to cover the largest window. `regime.windows` requires restart; per-strategy `regime_*_window` SIGHUP when flat, blocked while open. No default behavior change — existing configs with empty `windows` are unaffected. Opt in by adding `regime.windows` and per-strategy selectors.
- **Default-window label resolved correctly (#797/#799)** — bug fix: `regime.enabled=true` with no explicit `regime.windows` caused the "default" window key to resolve to an empty label, silently disabling `regime_directional_policy` (base config used instead of policy entry) and failing open `allowed_regimes` gate (entries admitted in disallowed regimes). Fixed in `RegimePayload.Label`. **No config change required** — existing configs are unaffected and now behave correctly. Strategies using `regime_directional_policy` or `allowed_regimes` without explicit `regime.windows` should verify their entries are now properly gated.
- **update.sh `--rsync-from` + EnvironmentFile warning (#791)** — new `--rsync-from <src>` flag syncs code from a source clone without clobbering `.git/`, config, state DB, venv, or live binary; useful for deployment pipelines that stage builds separately. Before systemd restart, warns on stderr when a required `EnvironmentFile=` path declared in the unit file is missing. No behavior change to trading; no operator action for existing systemd deploys (missing-envfile warning is advisory, restart proceeds).
- **update.sh systemd→signal fallback (#786)** — when systemd mode cannot find the unit (systemctl exit 5), update.sh automatically retries via signal mode when `go-trader.pid` and an executable `run.sh` are present. No operator action for existing setups.
- **Probe skips live credential checks (#788)** — `LoadConfigForProbe` no longer requires `HYPERLIQUID_SECRET_KEY` and related env vars during the pre-swap probe step; `LoadConfig` at daemon startup still validates them. No behavior change to live trading; probe now succeeds on build machines without exchange credentials.
- **Signal script consecutive-failure alerts (#829)** — `ScriptFailureTracker` in `script_failure_alerts.go` sends an operator alert (all channels + owner DM) when a strategy's check script fails 3 consecutive cycles, then re-alerts every-10th / hourly / on new error signature. A recovery notice fires when the script succeeds after an alerted streak. Covers both hard crashes (timeout, OOM, import error) and soft JSON errors across all six check paths (spot, options, HL, topstep, robinhood, okx) plus the manual close evaluator. Fires automatically — no operator action; no config change.
- **Regime label in per-strategy status log (#826)** — Phase 6 status log line now includes `regime=<label>` (e.g. `trending_up`) or `regime=-` for strategies without regime detection. No operator action; observable in `journalctl -u go-trader` per cycle.
- **HL spotMeta sparse token index fix (#831)** — `_normalize_spot_meta` in `platforms/hyperliquid/adapter.py` rebuilds the `spot_meta["tokens"]` list into an index-aligned dense form before passing it to the SDK (`HLInfo.__init__`). Prevents `IndexError` crashes that killed adapter init for all strategies when HL's token index became sparse (e.g. index 479 at list position 459). Applied on both cache-hit and fresh-fetch paths, so a poisoned cache is also fixed transparently. No operator action.
- **Per-tier `sl_after` on regime closes now works (#836)** — was: a `tiers[j].sl_after` entry (e.g. `tp_atr_fraction`) under `tiered_tp_atr_regime` or `tiered_tp_atr_live_regime` failed config-load with an unknown-key error AND silently never armed at fire time (fire-path re-parses tier multiples through the same function). Fixed: `parseRegimeTPTiers` now strips `sl_after` from the subset passed to ATR-block parsing (same treatment as `close_fraction`). Also: `defaultHLProtectionTiers()` extracted as a single source of truth for the `[{1×,0.5},{2×,1.0}]` default ladder so `tp_atr_fraction` fire-tier multiples can't drift; `stop_loss_atr_regime` now satisfies the `sl_after` fixed-SL requirement. Fires automatically — no config change required; existing `sl_after` on regime closes now activates correctly.
- **HL OHLCV fetch dedup cache (#839)** — every HL strategy subprocess fetched its own candles from `/info`; ~20 strategies sharing a handful of asset+timeframe combos fired the same request dozens of times per cycle from one IP, tripping HL's 429 rate limit and cascading into script-failure alerts (#829). Fix: `get_ohlcv` in `platforms/hyperliquid/adapter.py` now caches transformed candles to `/tmp/hl_ohlcv_<symbol>_<interval>_<limit>.json` with an interval-aware TTL (`min(60s, half-bar)`, so 1m strategies never read >½-bar-stale data) — the first batch of a cycle fetches, peers read from cache. Covers signal, HTF-filter, and ATR-recompute fetch paths uniformly (all route through `get_ohlcv`). Empty/insufficient results are never cached (peers keep retrying live); writes are atomic (temp + `os.replace`), mirroring the meta cache. **Per-instance only:** `/tmp` is shared across an instance's subprocesses but isolated per systemd instance (`PrivateTmp=true`), so dedup is within an instance, not across the IP's instances — still flattens each instance's burst enough to clear the 429s. Enabled by default; disable with `GO_TRADER_HL_OHLCV_CACHE=0`. No config change, no Go/CLI change, no operator action.
- **Discord mutating slash commands (#868)** — five new owner-DM-only commands: `/go-trader-config show|set`, `/go-trader-add-strategy`, `/go-trader-remove-strategy`, `/go-trader-add-platform`, `/go-trader-paper-to-live`. All write through `writeValidatedConfigRoot` / `applyStrategyConfigPatch` serialized on `configWriteMu`. No config change required — commands appear automatically after update if the bot was already configured. Operator action: if using multi-instance or `GO_TRADER_SERVICE` env deployments, verify that `/go-trader-restart` and the mutating restart path use the correct unit name (now via `restartSelf()`, not hardcoded `go-trader` unit, since #893).
- **Discord command namespacing (#891)** — all commands now register under the `go-trader-` prefix (e.g. `/go-trader-status`, `/go-trader-restart`). Existing command entries in guild command lists will be replaced on next startup; first propagation can take up to ~1h. No config change required. **Operator action:** if you set up slash command shortcuts or permissions by name in Discord, re-configure them with the new prefixed names.
- **Manual strategies use continuous Discord cadence (#890)** — `type=manual` strategies now post to Discord every channel run (same as perps/futures), not hourly. No config change required. To restore hourly, add `"manual": "hourly"` to `summary_frequency` in config.
- **New strategies: momentum_pro, mean_reversion_pro (#895); range_scalper, consolidation_range (#896)** — four open strategies in the registry. `momentum_pro` and `mean_reversion_pro` are bidirectional (`direction: "both"` for HL perps). `consolidation_range` is bidirectional (range-edge mean-reversion). `range_scalper` is unidirectional and **deprecated/hidden from discovery (#1034)**. **Operator action:** none unless you want to add `momentum_pro`, `mean_reversion_pro`, or `consolidation_range`.

- **Global per-cycle regime store (#879)** — `regime_store.go` runs all regime subprocesses once per cycle (via `check_regime.py`) and serves `RegimePayload` / injection JSON to every strategy, eliminating per-strategy regime fetches. Failure policy: a failed bundle → empty `RegimePayload` → gate fails open, `regime=-`, no inline fallback; sustained failure → `scriptFailureTracker` alerts. `/api/regime` endpoint exposes window snapshots + `adx3`/`composite7` views. `--regime-payload-json` appended to signal probe argvs so asymmetric deploys fail at startup. Fires automatically — no operator action.
- **Enriched portfolio warning DMs (#904)** — `BuildPortfolioWarningMessage` expands single-line portfolio risk reason into a triage block: top-5 contributors (ID, P&L, dd%, position), trend direction, distance to kill switch, last 15m activity, recommendation. Fires automatically when the portfolio enters the warning band.
- **Enriched circuit-breaker DMs (#905)** — `formatPerStrategyCircuitBreakerBlock` builds a rich alert with trigger line, portfolio impact, perps context, position table, trade rows, and recommendation. Fires automatically on CB trigger.
- **Regime window divergence (#907)** — new per-strategy opt-in `regime_window_divergence` block. See Opt-in field below.
- **Shared-wallet total dedup (#915)** — `computeSubsetPortfolioValue` and `computeInitialPortfolioPeak` deduplicate same-account HL total-value reads across all participant strategies. Fires automatically for any shared-wallet HL setup; no config change.
- **Shared-wallet exchange-authoritative display (#918)** — `reconcileSharedWalletMemberValues` now splits real account balance (`value_i = w_i*(acctBal-U)+ownedUPnL_i`) and writes `StrategyState.SharedWalletValue` for operator surfaces. Risk/kill-switch math still uses `PortfolioValue` unchanged. `SharedWalletDriftTracker` alerts on >$0.01 drift confirmed 2 consecutive cycles, re-alerts at 10% ratio (#1088: stable-drift WARN log and re-alert now throttled — hourly heartbeat, not every 10 cycles). Fires automatically.
- **Risk-path manual dedup (#921/#924)** — same-account `type=manual` strategies are folded into the shared-wallet dedup for `computeSubsetPortfolioValue`, `computeInitialPortfolioPeak`, and `rebaselinePortfolioPeakAfterPrune`, preventing double-count of HL account balance. Fires automatically for any deployment mixing `type=manual` and shared-wallet HL perps on the same account.
- **`regime_window_divergence` opt-in (#907)** — new per-strategy `regime_window_divergence` block for HL perps live only. Shape: `{"short_window": "<name>", "medium_window": "<name>", "on_divergence": "<mode>"}`. Modes: `trust_short` / `trust_medium` / `alert_only`. When short and medium windows disagree (hard divergence = bullish+bearish; soft = one ranging), overrides local `sc.Direction` after `regime_directional_policy`. `ranging_directional` composite label supported via `return_eff`. `RegimeDivergenceState.CyclesActive` tracked in-memory; visible in `/status` + DMs + dashboard badge. SIGHUP-blocked while open. Requires non-empty `regime.windows` with both named windows. Default: off.

**Open-position constraint**
- `margin_mode`, exchange `leverage`, kill-switch identity changes
- HL `trailing_stop_atr_mult` / `stop_loss_atr_mult` nil↔positive toggle blocked while open
- `invert_signal` toggle blocked while open
- `regime_directional_policy` add/remove/shape change blocked while open (flatten first or restart after close)
- `regime_window_divergence` add/remove/shape change blocked while open (flatten first)

---

## Status

Default port `8099`. Override with `--status-port <port>` or `status_port` in config. If busy, server tries next 5 ports; check logs for `[server] Status endpoint at http://localhost:<port>/status`.

```bash
curl -s localhost:8099/status | python3 -m json.tool
curl -s localhost:8099/health
curl -s localhost:8099/history
open http://localhost:8099/dashboard   # embedded strategy charts + trade markers (#734)
```

Dashboard JSON endpoints: `/api/strategies`, `/api/strategies/overview`, `/api/strategies/<id>/(candles|trades|status|equity|config|simulate)`. Candles/equity cached 30s. `config` (GET) and `simulate`/`config` (POST) require `status_token` + same-origin header. If `status_token` is configured, the dashboard page prompts for it and stores it in browser local storage. Don't expose the status port publicly — gate behind reverse proxy or VPN.

**Remote access via Tailscale Serve (#744):** The status HTTP server listens on loopback only (`localhost:<port>` — same as `http://127.0.0.1:<port>`). Do not rebind go-trader to `0.0.0.0` for remote dashboard use; keep each instance on loopback and front it with [Tailscale Serve](https://tailscale.com/kb/1242/tailscale-serve) (or another authenticated proxy on the machine). Example for two instances: `tailscale serve --bg --https=8443 http://127.0.0.1:8099` and `tailscale serve --bg --https=8444 http://127.0.0.1:8100` → browse `https://<node>.tailnet.ts.net:8443/dashboard` and `:8444/dashboard`. Common multi-instance port map (tune to each `status_port` in config): live `8099`, paper-testing `8100`, paper-hl-btc `8101`, paper-hl-eth `8102`, paper-hl-bnb `8103`, paper-hl-sol `8104`. **OpenClaw** (or any other agent stack) may expose its own dashboard on different ports/routes — that UI is not go-trader’s `/dashboard`.

If Discord enabled, wait for the first cycle and verify messages in configured channels. Report success with mode, # strategies, status URL, log command.

---

## Discord Slash Commands (#212)

The bot registers global slash commands at startup (`scheduler/discord_commands.go`,
wired in `main.go` via `DiscordNotifier.RegisterSlashCommands`). Global registration
covers every guild the bot is in plus DMs; first-time command-shape changes can take up
to ~1h to propagate.

**Namespacing (#891):** every command is registered under the `go-trader-` prefix
(`commandPrefix`) so the bot's commands are unambiguous in shared guilds (e.g.
`/go-trader-status`, `/go-trader-restart`). The prefix is the wire name only —
`slashCommands()` builds each command as `commandPrefix+<id>` and `interactionCreate`
strips it back to the bare `<id>` before auth/dispatch, so `readOnlyCommandNames`,
`opsCommandNames`, and the dispatch `switch` all operate on the unprefixed IDs.
`commandPrefix` is the single source of truth; subcommand/option names are not prefixed.

**Setup:** the bot must be invited with the `applications.commands` OAuth scope (in
addition to `bot`) for the commands to appear. No code/config change — re-invite via the
Discord developer portal OAuth2 URL generator.

**Read-only** (usable in a guild OR a DM, by anyone):
`/go-trader-status`, `/go-trader-health`, `/go-trader-positions`, `/go-trader-pnl`,
`/go-trader-leaderboard [top]`, `/go-trader-circuit-breakers`, `/go-trader-dead-strategies`,
`/go-trader-correlation`. These read live in-process state via the
`StatusServer` (no HTTP round-trip). The four that fetch live marks (`/go-trader-status`,
`/go-trader-positions`, `/go-trader-pnl`, `/go-trader-leaderboard`) use a deferred ACK + follow-up so they don't blow
Discord's 3-second interaction deadline (`fetchLiveMarkPrices` spawns a Python subprocess +
venue HTTP); the rest answer inline. Replies are public in-channel by default; set
`discord.ephemeral_replies: true` in config to make read-only replies ephemeral
(visible only to the invoker).

**Ops** (owner-only AND DM-only; restricted via command `Contexts: [BotDM]` and re-checked
in the handler by `authorizeCommand`):
- `/go-trader-logs [n]` — last N `journalctl -u go-trader` lines. Owner-DM-only because daemon logs
  can carry wallet addresses / error payloads (sharper exposure than P&L/positions).
- `/go-trader-restart` — `systemctl restart go-trader` (ACKs, then this instance is replaced).
- `/go-trader-backtest <strategy> <symbol> [timeframe]` — runs `backtest/run_backtest.py --mode single`
  (5-min timeout via `runPythonWithTimeout` + `shutdownReadOnlyCtx`; holds one of 4
  `pythonSemaphore` slots while running); replies with a summary and attaches the full
  report as `backtest.txt`.
- `/go-trader-report-an-issue <title> <body> [label]` — files a GitHub issue against `discord.report_repo`
  (default `richkuo/go-trader`) via the REST API (`discord_report.go`: `buildIssueRequest`
  builds the payload + a "Filed via /go-trader-report-an-issue" footer; `createGitHubIssue` POSTs and returns
  the issue URL). Defers the ACK because the GitHub round-trip can exceed Discord's 3s
  deadline. Token resolves from `GO_TRADER_GITHUB_TOKEN`, then `GITHUB_TOKEN`, then
  `discord.report_github_token` (env preferred; keep the secret in `/opt/go-trader/.env`).
  Replies that reporting is not configured when no token is set.

**Mutating ops** (#868; owner-only AND DM-only):
- `/go-trader-config show` — displays the running config with secrets redacted (`redactConfigForDisplay`).
- `/go-trader-config set <key> <value>` — sets a top-level or per-strategy config field. Per-strategy keys (`strategies.<id>.<field>`) route through `applyStrategyConfigPatch` (tuner path, requires `config_version>=13`); top-level keys use `applyTopLevelConfigSet`. Writes atomic via `writeValidatedConfigRoot` (`configWriteMu` serializes with the dashboard tuner). Apply path: SIGHUP hot-reload when `applyHotReloadConfig` allows it; else `restartSelf()` (strategy add/remove, paper→live args). Reply states which path was taken.
- `/go-trader-add-strategy <name> <platform> <asset>` — generates a new HL-perps (always `--mode=paper`) or BinanceUS-spot strategy entry. `name` must be in `knownShortNames`. New strategy includes a comment noting the default SL and recommending configuration before `/paper-to-live`.
- `/go-trader-remove-strategy <id>` — removes a strategy from config after an out-of-band DM confirm. Requires `restartSelf()` (shape change).
- `/go-trader-add-platform <name>` — emits a setup checklist for the requested platform (secrets live in `/opt/go-trader/.env`, never the config file).
- `/go-trader-paper-to-live <strategy>` — flips a strategy from `--mode=paper` to `--mode=live` after an out-of-band DM confirm. Requires `restartSelf()`.

All config writes serialize on `ss.configWriteMu`; mutating commands use `restartSelf()` for deployment-agnostic restart (#893 — tries `systemctl restart` with `GO_TRADER_SERVICE` name, falls back to `syscall.Exec` for signal-mode deploys). Pure helpers (`redactConfigForDisplay`, `buildAddStrategyEntry`, `flipStrategyToLive`, `applyTopLevelConfigSet`, `buildTunerOverride`, `classifyConfigSetKey`) are unit-tested in `discord_mutating_commands_test.go` without a gateway.

Auth lives in `authorizeCommand`; command set in `slashCommands()`; pure response builders
(`format*Response`) are unit-tested in `discord_commands_test.go`. Registration failure is
non-fatal (logged + owner DM).

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
     triple_ema_bidir, delta_neutral_funding
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
     trailing_stop_min_move_pct, margin_mode, direction, open_strategy,
     close_strategies, allowed_regimes, theta_harvest.*
   Discord/Telegram: enabled, channels, trade_alert_channels, dm_channels, owner_id
   Environment: Discord token, status token, exchange credentials

4. COMMANDS
   /menu
   /go-trader
   ./go-trader init
   ./go-trader init --json '{...}' --output scheduler/config.json
   ./go-trader manual-open <strategy-id> [--side long|short] [--size N | --notional N | --margin N]
   ./go-trader manual-open <strategy-id> --limit-price N [--tif Alo|Gtc] [--expire-after N]
   ./go-trader manual-cancel <limit-order-id>
   ./go-trader manual-close <strategy-id> [--qty N]
   ./go-trader manual-update-sl <strategy-id> --trigger N [--symbol Y] [--dry-run]
   ./go-trader manual-cancel-sl <strategy-id> [--symbol Y] [--dry-run]
   ./go-trader backfill hl-fees [--strategy <id>|--all] [--apply] [--reset-cash]
   ./go-trader backfill trade-ledger [--strategy <id>|--all] [--apply] [--reset-cash]
   ./go-trader inspect <strategy-id> [--all] [--json]
   ./go-trader agent-info [--bootstrap-md] [--append-changelog]
   sudo systemctl start|stop|restart|status go-trader
   journalctl -u go-trader -n 50 --no-pager
   curl -s localhost:8099/status | python3 -m json.tool

5. BACKTESTING
   uv run --no-sync python backtest/run_backtest.py --strategy <n> --symbol BTC/USDT --timeframe 1h --mode single|compare|multi|optimize
   uv run --no-sync python backtest/backtest_options.py --underlying BTC --since 90 --capital 10000
   uv run --no-sync python backtest/backtest_theta.py --underlying BTC --since 90 --capital 10000
```

---

## Manual Trading (HL perps)

Use `type: "manual"` on Hyperliquid for hand-driven entries/exits with scheduler-tracked P/L, close evaluators (default SL@1.5×ATR + `tiered_tp_atr_live` TP1@2× / TP2@3×), and Discord trade DMs (#569).

Config skeleton (no `script` / `args` / `interval_seconds` — `LoadConfig` fills them):

```json
{"id":"hl-manual-btc","type":"manual","platform":"hyperliquid","symbol":"BTC","capital":1000,"leverage":3,"max_drawdown_pct":10}
```

Multiple `type=manual` strategies and HL perps strategies may share a coin (#619/#620). Owner guards prevent cross-strategy mutation; full-close uses `shouldCloseFullPosition` to avoid flattening a peer's position; all TP OIDs are cancelled on full close. Peers must share `leverage` and `margin_mode`; at most one trailing-stop owner per coin.

CLI:

```bash
# Open — pick at most one of --size, --notional, --margin
./go-trader manual-open hl-manual-btc                              # defaults: --side long --margin 50
./go-trader manual-open hl-manual-btc --side long --size 0.01
./go-trader manual-open hl-manual-btc --side long --notional 500
./go-trader manual-open hl-manual-btc --side short --margin 100   # margin × leverage = notional

# Optional: pass live ATR for accurate SL/TP distances; omit to auto-fetch ATR(14)
./go-trader manual-open hl-manual-btc --side long --size 0.01 --atr 850

# Scale in — ADD to an open position (side inferred; blends avg cost, freezes risk plan)
./go-trader manual-add hl-manual-btc --margin 50
./go-trader manual-add hl-manual-btc --size 0.01 --record-only --fill-price 68100

# Edit the stop-loss in place (#1050) — cancel-then-place / remove on-chain, scheduler adopts next cycle
./go-trader manual-update-sl hl-manual-btc --trigger 66000   # ratchet the SL trigger
./go-trader manual-cancel-sl hl-manual-btc                   # remove the resting SL

# Close — full or partial
./go-trader manual-close hl-manual-btc            # full close
./go-trader manual-close hl-manual-btc --qty 0.005

# Record-only (operator placed on HL UI; scheduler tracks)
./go-trader manual-open  hl-manual-btc --side long --size 0.01 --record-only --fill-price 67800
./go-trader manual-close hl-manual-btc --qty 0.005 --record-only --fill-price 68250
```

Notes:

- `--record-only` skips the live HL order; pair with `--fill-price`. SL is **not** auto-armed in record-only mode — place the trigger on the UI manually.
- SL and TP[n] reduce-only orders are placed **inline** on open (#633). `--atr` is optional: when omitted, the binary auto-fetches ATR(14) from the strategy's symbol+timeframe via `check_hyperliquid.py --fetch-atr` (#690), matching what `ensure_atr_indicator` would compute on a baseline strategy open. If the fetch fails (network error, insufficient candles), it falls back to `0.1*fillPrice/leverage` (≈10% margin risked at 1× ATR SL) and emits one combined notifier message. Pass `--atr` explicitly when you have a live indicator value and want to skip the network round-trip.
- `--side` defaults to `long` (#691/#692). When no sizing flag is passed (and not `--record-only`), `--margin 50` is auto-applied so `manual-open <strategy-id>` works as a smoke-test command. Operators who want a different size must still pass `--size`/`--notional`/`--margin` explicitly.
- Default SL multiplier for `type=manual` is **1.5× ATR** (#691/#692), distinct from the fleet-wide `default_stop_loss_atr_mult` (typically 1.0×) used by non-manual HL perps. Explicit `stop_loss_atr_mult`/`stop_loss_pct`/`stop_loss_margin_pct`/`trailing_stop_pct`/`trailing_stop_atr_mult` on the strategy still wins; fleet `default_stop_loss_atr_mult: 0` opts manual out too.
- All four defaults (margin, SL multiplier, side, TP tiers) are overridable via the optional top-level `manual_defaults` block (#696/#697) — see Adjustable Settings. `manual_defaults.stop_loss_atr_mult: 0` is a manual-only opt-out that doesn't affect non-manual HL perps; the block is hot-reloadable via SIGHUP.
- Open blocked when portfolio kill switch active or strategy has pending CB close.
- Fills queued in `pending_manual_actions`, applied at top of next scheduler cycle (need `--once` if daemon idle). If the queue insert fails after a successful on-chain fill, the position is auto-flattened and SL/TP cancelled (#635); cleanup failures notify loudly — flatten manually.
- A 99% partial close is **not** silently collapsed into a full close — the queue carries explicit `is_full_close` intent from `--qty`.
- `manual-update-sl` / `manual-cancel-sl` (#1050) edit the resting stop-loss in place: they cancel-then-place (update) or cancel (remove) the on-chain SL, then queue an `update-sl`/`cancel-sl` action the daemon drains into memory — **no direct `state.db` write, no restart**. They are **hard-rejected** when the strategy's automated protection (ATR/regime `stop_loss_atr_mult`, trailing close) would re-pin the edit on the next cycle — only strategies opted out of auto-SL (`stop_loss_atr_mult: 0`, no trailing) qualify; the error names the opt-out. `update-sl` also refuses a trigger that would fill immediately against the current mark. Same kill-switch / pending-CB guards as `manual-open`; SL edits record no trade (no Discord trade DM).
- External closes (UI, SL, TP) detected by reconciler and cleared automatically (#576) — no ghosts.
- `type=manual` exempt from CB drawdown checks (#574).

### Resting limit orders (#883)

Place a maker/post-only limit open instead of a market open:

```bash
# ALO (post-only, default) — rejected if it would immediately match
./go-trader manual-open hl-manual-btc --limit-price 68000 --side long --margin 50
./go-trader manual-open hl-manual-btc --limit-price 68000 --side long --margin 50 --tif Gtc

# With TTL — auto-cancels after the duration if unfilled
./go-trader manual-open hl-manual-btc --limit-price 68000 --side long --margin 50 --expire-after 4h

# Cancel a resting limit order
./go-trader manual-cancel <limit-order-id>
```

Notes:

- `--tif Alo` (default, post-only) or `--tif Gtc` only — `Ioc` is rejected (it never rests).
- The CLI exits immediately after placing the order. The scheduler polls fill status each cycle via `reconcilePendingLimitOrders` and books fills incrementally — partial fills open the position and grow it each cycle sharing the same PositionID.
- Protection (SL/TP) is NOT placed inline — it is applied by the next scheduler cycle after the first fill, same as any other manual position.
- `manual-cancel <id>` queues a cancel request; the scheduler cancels on-chain and finalizes next cycle. TTL expiry and operator cancel use the same path.
- Limit orders are tracked in the `pending_limit_orders` table; each partial-fill leg is tagged `scale_in` so `#T` counts one position regardless of fill count.

### Scale-in / pyramiding (#873)

Opt-in way to **increase** an open position's size instead of the default skip-on-same-direction. Scope: **HL perps + manual, live + paper**. A same-direction add **blends only price and size** for PnL (`AvgCost`, `Quantity`, `InitialQuantity` grow) and **freezes the original risk plan** — `EntryATR`, the regime label, and the SL/TP trigger geometry stay pinned to the first entry (`RiskAnchorPrice`); the cleared-tier watermark is never reset. Only the on-chain protection **size** is re-based (SL + un-cleared TP tiers cancel+replaced at the unchanged triggers on the next protection sync).

- **Strategy flag (perps):** `allow_scale_in: true` plus an optional `scale_in` block: `max_adds` (0=unlimited), `max_added_notional_usd` (0=unlimited), `add_spacing_atr` (signed: `>0` add-to-winners, `<0` average-down, `0` no gate — measured in ×EntryATR from the last entry leg), `add_notional_usd` (0=standard open notional per add). Fires only when a same-direction signal actually reaches Go (the population the existing skip guards target); close-evaluator strategies use `manual-add`.
- **CLI (manual):** `manual-add <strategy-id>` with the same sizing flags as `manual-open` (`--size`/`--notional`/`--margin`, `--record-only` + `--fill-price`, `--dry-run`). Side is inferred from the open position; refuses when flat; kill-switch + pending-CB guards apply; queued in `pending_manual_actions` and applied next cycle.
- An add leg is booked `trade_type=scale_in` (open-side, same position id) and **excluded from the `#T` open count** so `#T` stays distinct positions; W/L is unaffected.
- **Live perps guard:** `allow_scale_in` requires an ATR/regime or trailing stop-loss (one the resize path can grow). A static scalar SL (`stop_loss_pct`/`stop_loss_margin_pct` or the `max_drawdown` fallback) is rejected at load — it would under-cover the grown position after an add. Manual auto-uses an ATR SL, so it qualifies.
- Hot-reloadable when flat; toggling `allow_scale_in` or editing the `scale_in` block while a position is open is blocked (flatten first). Not backtested; no Python changes.

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
- Rows already on the #954 gross convention (`pnl_gross=1`) are skipped (`gross_convention_row`) — they belong to `backfill trade-ledger` below.

---

## Backfill Trade Ledger (#954)

Migrates legacy trades rows to the gross-PnL convention and trues fee/price/PnL up to HL `userFills`, so the shared-wallet ledger display path (`initial_capital + Σ ledger + owned uPnL`) reads exchange-accurate values:

```bash
# Dry run
./go-trader backfill trade-ledger --all
./go-trader backfill trade-ledger --strategy hl-btc-momentum

# Apply (stop daemon first)
sudo systemctl stop go-trader
./go-trader backfill trade-ledger --all --apply
sudo systemctl start go-trader
```

Notes:

- Two passes per row, chronological: (1) legacy `pnl_gross=0` rows migrate net→gross (the fee deducted at booking — stored real fee, else the modeled taker fee — is stamped into `exchange_fee`; close-leg `realized_pnl` gets it added back; `fee_source` records provenance); rows with `fee_source='reconcile_adjustment'` (#1042 model-only force-close/CB legs) skip migration and userFills true-up but cash replay still includes them; (2) rows whose OID matches a userFills aggregate get the real fee, fill VWAP price, and exchange gross `closedPnl`.
- Rows sharing one OID (partial TP legs, flip close+open pairs, #1030 shared-coin aggregate fills) apportion the aggregate by quantity share — fee across ALL legs, closedPnl across close legs only.
- `strategies.cash` and `closed_positions` replayed under net semantics; same `--reset-cash` divergence gate as `backfill hl-fees`.
- Funding rows (`trade_type='funding'`) are never rewritten and never touch cash.
- `--apply` resets every shared wallet's ledger drift baseline so the next reconciled cycle re-anchors on the repaired ledger instead of alarming on the correction.
- Idempotent: a second run over the same fills reports zero changes.

---

## Backtesting

Use `uv run --no-sync python` for all backtests.

```bash
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode single
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode compare
uv run --no-sync python backtest/run_backtest.py --strategy momentum --timeframe 1h --mode multi
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode optimize
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --since 90

# Close-strategy registry (#535/#641) — single close per strategy (#842); --close-strategy sets it.
# --close-strategy accepts both bare names and JSON refs ({"name","params"}).
# --close-params is removed — fold params into the JSON ref.
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --close-strategy tp_at_pct \
  --close-strategy '{"name":"tiered_tp_atr","params":{"tiers":[{"atr_multiple":1,"close_fraction":0.5},{"atr_multiple":2,"close_fraction":1.0}]}}'

# Backtest a live strategy verbatim (single mode only) — pulls the strategy's
# open + close refs from the live config (#643). Pre-v15 configs are rejected (#951 — start the live binary once to migrate).
uv run --no-sync python backtest/run_backtest.py --config scheduler/config.json --strategy hl-btc-momentum \
  --symbol BTC/USDT --timeframe 1h --mode single

# Regime gate (#549) — blocks entries outside allowed regimes; closes always execute
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --regime-enabled --regime-period 14 --regime-adx-threshold 20 --allowed-regimes trending_up trending_down

# Joint open × close-stack walk-forward co-optimization (#996, backtest-only) — picks the best
# (entry params, exit config) pair per fold. --sweep-close uses the 25-stack default grid; or pass
# --close-stacks-json PATH for a custom grid. --optimize-metric sharpe_ratio|total_return_pct|dd_adjusted_return.
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --mode optimize --sweep-close --optimize-metric dd_adjusted_return --direction long

# A backtest whose equity hits 0 (e.g. a stop-less short losing >100%) prints a LIQUIDATED banner and
# floors return/Sharpe at -100% (#1005) so a deeper blowup can never rank above a shallower one.

uv run --no-sync python backtest/backtest_options.py --underlying BTC --since 90 --capital 10000
uv run --no-sync python backtest/backtest_theta.py --underlying BTC --since 90 --capital 10000
```

---

## Reconfiguration

After edits to `scheduler/config.json`:

```bash
sudo systemctl kill -s HUP go-trader   # hot reload (no state loss)
sudo systemctl restart go-trader       # full restart
```

Hot reload (`SIGHUP`) re-applies a safe subset: capital, drawdown, intervals, params, stop-loss (incl. `%`/ATR-mult trailing), sizing leverage, theta-harvest, portfolio risk knobs, summary cadence, correlation thresholds, `allowed_regimes` per-strategy, auto-update mode, Discord/Telegram channels and tokens; per-strategy `regime_*_window` selectors when flat. Refuses if strategy roster, script/args/type/platform, HTF filter, kill-switch identity, or DB path changed; refuses per-strategy exchange `leverage` / HL `margin_mode` while positions open; refuses `regime_*_window`, `regime_window_divergence`, `regime_directional_policy` while open. Global `regime` block (enabled/period/adx_threshold/windows) requires full restart (mirrors `correlation`). Re-runs HL peer-on-same-coin check (`margin_mode`/exchange `leverage` agreement; at most one trailing-stop owner). On rejection, fall back to restart. Status server reflects new port immediately.

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
| Regime detection | `regime.enabled`, `regime.period`, `regime.adx_threshold`, `regime.windows` | disabled; period=14, threshold=20; `windows` empty = legacy single horizon (#792) |
| Notify on HL TP/SL fill | `notify_tp_sl_fills` | enabled (nil/missing); set `false` to disable owner DMs from reconciler-detected fills |
| Notify on ratchet tier trigger | `notify_ratchet_triggers` | enabled (nil/missing); owner DM when a `trailing_tp_ratchet*` tier clears and tightens the trail. Set `false` to disable (#1110) |
| `type=manual` defaults | `manual_defaults.{margin_usd,stop_loss_atr_mult,side,tp_tiers,trailing_stop_atr_regime}` | Optional top-level overrides for the hardcoded manual-open defaults ($50 margin, 1.5× ATR SL, `long`, `[{2×,0.5},{3×,1.0}]`). Resolution order: CLI/strategy-param → `manual_defaults` → hardcoded constant. `trailing_stop_atr_regime` (#1115) tunes the per-regime opening trail for manuals that default to `trailing_tp_ratchet_regime` (cloned per strategy, resolved against each strategy's classifier labels). Block is additive (no config-version bump). Hot-reloadable via SIGHUP; `tp_tiers: []` is rejected at validation — omit the key to inherit the default (#696/#697). |

Per-strategy:

| Setting | Key | Notes |
| --- | --- | --- |
| Capital | `capital` | Starting capital reference |
| Max drawdown | `max_drawdown_pct` | Strategy CB |
| Circuit breaker | `circuit_breaker` | `false` disables BOTH CB arms (drawdown + 5 consecutive losses), live and paper; nil/omitted → enabled (safe default). Suppresses only NEW fires (a latched CB / pending close still drains); display drawdown still updates. One-shot WARNING when a disabled CB suppresses a breach; `cb=off` in startup summary + `inspect`. Hot-reloadable via SIGHUP while open. `type=manual` exempt. No version bump (#1048). |
| Interval | `interval_seconds` | 0 uses global; auto-accelerates in DD warn band |
| HTF filter | `htf_filter` | Skips counter-trend signals |
| Open strategy params | `open_strategy.params` | Per-open overrides; no longer a flat top-level `params` map (#640). Migrated from legacy on first start |
| Close strategy params | `close_strategy.params` | Close evaluator overrides (e.g. `tiered_tp_atr.tp_tiers`); the ref carries its own params so they don't leak into the open strategy. (Legacy `close_strategies[i].params` array path still read.) |
| Direction | `direction` | Perps gate: `"long"` (default), `"short"` (#656 — open shorts only), or `"both"` (bidirectional). Replaces legacy `allow_shorts`; v14 migration converts `false→"long"`, `true→"both"`. SIGHUP-aware when flat. |
| Invert signal | `invert_signal` | HL perps/manual only. `true` flips BUY↔SELL on every non-zero signal; HOLD (0) never flipped. Allows inverse-trend use of any open strategy without a new Python module. Composes with `direction="short"` (opens short on raw-BUY, distinct from plain short-direction which opens on raw-SELL). SIGHUP-blocked while open. Default `false`. |
| Regime directional policy | `regime_directional_policy` | HL perps only. Per-regime `direction`+`invert_signal` override that auto-switches long/short/inverse mode as market regime changes. Requires top-level `regime.enabled=true`; all three canonical regime labels required. When flat, resolves from current regime; while a position is open, resolves from `pos.Regime` at open (hold-on-transition). Startup state-vs-config validation and `inspect` use the same effective-direction rules (#783). SIGHUP blocks shape changes while open. `base_direction`/`effective_direction` visible in `/status`. Backtestable via `run_backtest.py --config` per-cycle resolver (#1025). **#1085: evidence-gated default-OFF** — resolves to base direction unless the `(asset, timeframe, classifier)` cell is certified in `regime_directional_certifications.json` (shipped empty), so the override is currently inert; configuring it logs a non-breaking `[WARN]` (#1076). |
| Regime window divergence | `regime_window_divergence` | HL perps live only. Shape: `{"short_window": "<name>", "medium_window": "<name>", "on_divergence": "<mode>"}`. Modes: `trust_short` / `trust_medium` / `alert_only`. Overrides `sc.Direction` when short + medium windows diverge (hard = bullish+bearish; soft = one ranging), applied after `regime_directional_policy`. Requires non-empty `regime.windows` with both named windows. Visible in `/status` + DMs + dashboard badge. SIGHUP-blocked while open. Default off. |
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
| Close strategy | `close_strategy` | Single exit ref `{name, params}` (#842 collapsed the array); legacy `close_strategies` array len ≤1 still read, len>1 rejected; nil → open-as-close |
| Regime gate | `allowed_regimes` | Labels allowing entries (`trending_up`, `trending_down`, `ranging`); empty = allow all; needs `regime.enabled=true`; not on type=options |
| Multi-window selectors | `regime_gate_window`, `regime_atr_window`, `regime_directional_window` | Require non-empty `regime.windows`. Route entry gate, regime-aware ATR/TP, and directional policy to different ADX horizons. Empty/`default` → legacy `regime.period`. Stamped labels persist in `pos.RegimeWindows` (#792). SIGHUP when flat; blocked while open. |
| Regime-profile allocation | `regime_profile_allocation` | HL perps (live + paper). Two open-param profiles of one strategy; a slow long-window regime label picks the active one, switched hysteretically (`confirm_bars`, WARN<12) and only while flat (frozen to the open profile while a position is open). Shape `{window, profiles{label→name, all labels}, param_sets{name→overrides, exactly 2}, confirm_bars≥1, initial_profile}`. Requires `regime.enabled=true`. Persisted (`active_profile`); SIGHUP blocks shape change while open, resets state when flat. Backtestable via `--config`. No version bump (#998). |
| Theta harvest | `theta_harvest.*` | Options early-exit |
| User close defaults | `user_close_defaults` (top-level block) | Optional: `{"tiered_tp_atr": {"tp_tiers": [...]}, "tiered_tp_atr_live": {...}, ...}` injects tiers into any matching close ref omitting `tp_tiers`. Three-layer resolution: system → user → strategy (explicit `tp_tiers` wins). Supported evaluators: `tiered_tp_pct`, `tiered_tp_atr`, `tiered_tp_atr_live`, `trailing_tp_ratchet`, and `_regime` variants. SIGHUP-hot-reloadable. Additive: no `config_version` bump. `tp_tiers: []` rejected at validation — omit the key to inherit system defaults (#866/#870). Backtest: `--defaults system\|user` selector. |
| HL on-chain TP tiers | `close_strategies[i].params.tiers` (where ref is `tiered_tp_atr` or `tiered_tp_atr_live`) | HL perps only — list of `{atr_multiple, close_fraction}` (cumulative). **Default `[{1.5×,0.4},{3×,0.8},{5×,1.0}]` (#870 retune from old `[{1×,0.5},{2×,1.0}]`)**; final tier coerced to 1.0; non-numeric rejected per tier. **Live mode:** configuring tiers auto-suppresses the in-process `tiered_tp_atr*` close evaluator to prevent on-chain limit-fill races (#604/#615). **Paper mode:** evaluator is never suppressed (#781). Pre-v13 configs migrated automatically. |
| Post-TP SL adjustment | `close_strategies[i].params.sl_after` (strategy-level) and/or `tiers[j].sl_after` (per-tier) — scalar modes: `"breakeven"`, `{atr_mult: N}` (signed), `{trail_from_here: {atr_mult: M}}`, `{trail_from_here: {tp_atr_fraction: F}}` (trail = F × firing tier ATR multiple). Regime-aware shapes: `{kind:"atr_offset","trend_regime":{...}}`, `{kind:"trail_from_here","trail_from_here":{"trend_regime":{...}}}`, `{trail_from_here:{tp_atr_fraction:{trend_regime:{label:F}}}}`; composite labels follow `regime_atr_window`. | HL perps + manual. Requires fixed SL (`stop_loss_atr_mult`, `stop_loss_atr_regime`, `stop_loss_pct`, or `stop_loss_margin_pct`). SIGHUP blocks scalar↔regime or shape changes while open. Backtester parity for scalar modes including scalar `tp_atr_fraction`; regime-aware `sl_after` HL-live-only (backtester rejects at init, #736/#742/#835). |
| Regime-aware ATR stop/trailing | `stop_loss_atr_regime`, `trailing_stop_atr_regime` | HL perps. Resolves ATR multiplier per `pos.Regime` label. Shape: `{"trend_regime": {"trending_up": {"atr": N}, "trending_down": {"atr": N}, "ranging": {"atr": N}}}` or `{"use_defaults": true}`. Mutually exclusive with scalar SL fields. Requires `regime.enabled=true`. **Backtester parity since #737/#747** — `Backtester(stop_loss_atr_regime=...)`. SIGHUP blocks flips while open (#733/#735). |
| Regime-aware tiered TP | `close_strategies[i].params` with ref `tiered_tp_atr_regime` or `tiered_tp_atr_live_regime` | HL perps on-chain TPs with per-regime tiers. `_live_regime` re-resolves each tick. **Backtester parity since #737/#747.** |
| Trailing-ratchet close | `close_strategy.params.tp_tiers` with ref `trailing_tp_ratchet` or `trailing_tp_ratchet_regime` (#844/#870) | HL perps + `type=manual`. Each tier `{atr_multiple, close_fraction?, trailing_mult_after \| tp_atr_fraction}` tightens the trail (monotonic, never loosens) and optionally scales out (cumulative `close_fraction`, `0`=trail-only). **Scalar form:** requires `trailing_stop_atr_mult > 0` (SL owner + initial trail); rejects other stop fields + `trailing_stop_pct`/`trailing_stop_atr_regime`. **Regime form (`trailing_tp_ratchet_regime`):** requires `trailing_stop_atr_regime` (#870 — `trailing_stop_atr_mult` rejected for regime variant); provides per-regime opening trail; `regime.enabled=true` required. First rung must be ≤ initial trail per regime. Regime `tp_tiers` keyed `{label: [tiers]}`, frozen at open. Places **no on-chain TP**. Default tiers (#866): `use_defaults:true`/omit `tp_tiers` → system default ladder (scalar: `1.5×/1.5×/0.8×` at `2×/2.5×/3×` ATR; regime: per-quality-group). Final-tier trail 0.8×ATR (#887, was 0.5×). Backtestable. SIGHUP blocks tier-table change while open. |

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
- Valid labels: `trending_up`, `trending_down`, `ranging`. `AllowedRegimes` SIGHUP-compatible; global `regime` block (incl. `windows`) needs full restart. Per-strategy `regime_*_window` selectors SIGHUP when flat; blocked while open. Not on type=options.

---

## Strategy Reference

Source of truth:

```bash
uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json
uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json
uv run --no-sync python shared_strategies/options/strategies.py --list-json
```

`DISCOVERY_HIDDEN_STRATEGIES` (`amd_ifvg`, `range_scalper`, `session_breakout`, `vol_momentum`) are omitted from `--list-json` / `go-trader init` after research deprecations (#1034–#1041) but stay registered — explicit `args[0]` / config refs still load. `liquidity_sweeps` is research-deprecated (#1032) but still discoverable.

Platform conventions:

| Platform | ID prefix | Type/script |
| --- | --- | --- |
| BinanceUS spot | none | `spot`, `shared_scripts/check_strategy.py` |
| Hyperliquid perps | `hl-` | `perps`, `shared_scripts/check_hyperliquid.py` |
| Hyperliquid manual | `hl-` | `manual` (#569), no script/interval; `manual-open`/`manual-close`; auto-defaults SL@1.5×ATR + `tiered_tp_atr_live` (TP1@2× / TP2@3×); can share coin with HL perps peers (#619/#620) |
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
- `triple_ema_bidir` is futures/perps only and needs `"direction": "both"` (formerly `"allow_shorts": true`; v14 migrates automatically). Use `"direction": "short"` to run any bidirectional strategy as a dedicated bear-only instrument (#656).
- Short-focused strategies (futures/perps only): `bear_pullback_st` (rally-into-EMA20/50 in EMA50<EMA200 + ADX>20 regime, RSI 55–65 rebound, #655), `vwap_rejection_st` (intraday VWAP/EMA20/EMA50 rejection inside bearish HTF + RSI≤50 confirmation, #657). Both emit `signal=-1` only and are pre-registered as bidirectional so `direction: "short"` or `"both"` is required. Pair with `allowed_regimes: ["trending_down"]` for clean entry gating.
- **New bidirectional strategies (#895):** `momentum_pro` (`mompro`) — stacked-EMA trend-pullback entry (fast>mid>long EMA), ADX-confirmed, volume-backed bar break; requires `direction: "both"`. `mean_reversion_pro` (`mrpro`) — z-score reversion gated by no-trend ADX ceiling + RSI extreme confirmation; requires `direction: "both"`. Both spot + futures/perps. Walk-forward OOS result: `momentum_pro` BTC 4h is marginally validated (~+6% median Sharpe ~1, high variance); `mean_reversion_pro` is not OOS validated — paper-trade before live.
- **Anchored VWAP `anchored_vwap` (`avwap`, #1016)** — single anchored-VWAP S/R flip; bidirectional (emits short on buffered breakdown below the line). Spot + futures/perps. Research-negative at default params (#1039) — treat as experimental; validate before live.
- **Range strategies (#896):** `consolidation_range` (`cr`) — range-edge mean-reversion at the top/bottom of a consolidation box; bidirectional (emits `signal=-1` at the top edge), requires `direction: "both"` for HL perps. Negative OOS at default params — tune `box_width_pct` and `atr_stop_mult` before live. `range_scalper` (`rs`) — **deprecated/hidden from discovery (#1034)**; unidirectional support/resistance scalper kept loadable for explicit configs.
- **`atr_band_revert` (`abr`, #1069):** ranging mean-reversion — fade ATR-scaled bands around an SMA (long below `mid − k·ATR`; short above `mid + k·ATR` on the futures/perps `direction:"both"` variant). Entries only; exit is config not code — pair `tiered_tp_atr` (~`k_entry/2` & `k_entry` ATR tiers) + `stop_loss_atr_mult`. Spot long-only. `init` ships it pre-gated to `allowed_regimes:["ranging_quiet","ranging_volatile"]` on a composite "medium" window. Tunable baseline — backtest before live.
- `donchian_breakout`, `chart_pattern`, `liquidity_sweeps` already emitted `signal=-1` for bearish setups but were generated long-only by `init.go`. Since #654 they default to `direction: "both"` so existing perps configs need a regenerate or a manual `direction` flip to capture the short side. `liquidity_sweeps` research-deprecate (#1032) — still in discovery; prefer other structure strategies.
- `session_breakout` (`sbo`) is futures/perps only and **deprecated/hidden from discovery (#1038)** — short leg failed bull-year held-outs (#1031)
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
uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json > /tmp/spot.json
uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json > /tmp/futures.json
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
uv run --no-sync python -m py_compile platforms/<name>/adapter.py
uv run --no-sync python -m py_compile shared_scripts/check_<name>.py
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
curl -s localhost:8099/status | uv run --no-sync python -c "
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
uv run --no-sync python -m pytest
uv run --no-sync python shared_strategies/open/test_registry_parity.py
```

If Go cache needs an explicit writable path:

```bash
env GOCACHE=/tmp/go-build-cache /opt/homebrew/bin/go -C scheduler test ./...
```

Go CI should not depend on a Python runtime, so tests for subprocess-based live helpers should extract pure parsers/decision helpers rather than invoking Python.
