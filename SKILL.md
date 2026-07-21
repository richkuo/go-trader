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
- **#844 trailing_tp_ratchet / trailing_tp_ratchet_regime:** a trailing-ATR stop where each cleared TP tier tightens the trail and optionally scales out. The strategy declares a positive strategy-level `trailing_stop_atr_mult` (the initial loose trail — and the SL owner; no other stop fields allowed). The close ref's `tp_tiers` is a list (plain) or `{regime: [tiers]}` (regime form, frozen at open via `Position.Regime`, keys matched to the `regime_atr_window` classifier — 3-state adx or 9-state composite; a bare `ranging_directional` key covers its `_up`/`_down` substates). Each tier is `{atr_multiple, close_fraction?, trailing_mult_after | tp_atr_fraction}`: `close_fraction` (default `0`, cumulative target) scales out, `0` = trail-only rung; the trail tightens to `trailing_mult_after` (absolute ATR mult) **or** `tp_atr_fraction × atr_multiple` (relative) — mutually exclusive — monotonically (never loosens; the first rung must be ≤ the initial trail). Places **no on-chain TP**: partial closes ride the close evaluator, the on-chain SL rides the trailing-stop walker. Tier triggers use **entry ATR**. **Scope: HL perps + `manual`.** Backtestable. Example: `{"trailing_stop_atr_mult": 3.0, "close_strategy": {"name": "trailing_tp_ratchet", "params": {"tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.0, "trailing_mult_after": 2.0}, {"atr_multiple": 3.0, "close_fraction": 0.3, "tp_atr_fraction": 0.33}]}}}`.
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

When in doubt, treat as runtime default and prompt. **Full narrative** for older commits and every archived PR bullet lives in [`docs/POST_UPDATE_HISTORY.md`](docs/POST_UPDATE_HISTORY.md). Regenerate from `git log --oneline -50` when stale.

**Auto-migration** (silent JSON rewrite on start — summarize, no prompt)
- `config_version` bump + deprecated field removal via `MigrateConfig`
- **v12→v13** (#640): co-located `open_strategy` / `close_strategy` refs; pre-v13 backtests rejected
- **v13→v14** (#658): `allow_shorts` → `direction` enum (`long`|`short`|`both`)
- **v15** (#841/#853): single `close_strategy`; `close_strategies` length>1 rejected at load

**Runtime default** (recent — prompts required; detail in history doc)
- **#842/#853** single `close_strategy` — collapse multi-close configs before upgrade
- **#954** trade-ledger PRE-FEE gross + net display; run `backfill trade-ledger --all --apply` after upgrade
- **#1008** force-close `trade_type` relabel (`perps` not `futures`); display-only
- **#1009** corrupt-position zero-PnL force-close; flip sizing fix under `direction=both`
- **#1030** shared-coin aggregate fill apportionment by virtual qty
- **#1042** `reconcile_adjustment` fee_source on model-only force-closes
- **#1046** latched CB manage-only — trailing SL/TP ratchet continues on open HL perps
- **#1048** per-strategy `circuit_breaker: false` opt-out (nil→enabled); hot-reloadable
- **#1055** `update.sh --all` discovers deploy dirs from systemd `WorkingDirectory`
- **#1058** backtest `--config` threads composite `regime.windows` (re-run old regime backtests)
- **#1059** composite ranging ratchet ladder split (`ranging_volatile` / `ranging_directional`); on-chain SL reposition
- **#1085** `regime_directional_policy` evidence-gated DEFAULT-OFF (empty cert artifact); per-state freeze at open; #822 orphan auto-close
- **#1088/#1089** shared-wallet drift + SL-gap WARN throttled (hourly heartbeat)
- **#1092** kill-switch already-flat fill repair (`kPEPE`-style coins)
- **#1100/#1103–#1106** HL cashflow journal drives total-drift alarm; trade-ledger fail-closed fallback; `GO_TRADER_CASHFLOW_JOURNAL_ALARM=0` opts out
- **#1115** manual close defaults to `trailing_tp_ratchet_regime` when regime on + resolvable trail; tiered-TP drift owner DM
- **#1120** composite opening-trail `use_defaults` system table retuned: `trending_*_clean` 2.0→**2.5×ATR**, `trending_*_choppy` 2.0→**2.25×**, `ranging_volatile` 1.0→**1.25×**, `ranging_directional`/`_up`/`_down` 1.0→**1.5×** (`ranging_quiet` 1.0 + ADX labels + tier ladders unchanged). ⚠️ **On-chain**: a full restart/deploy re-expands `use_defaults` trailing blocks, so open HL-live positions on that path see their reduce-only SL **widen** on the next protection sync (SIGHUP does NOT apply it while open). Set explicit per-regime ATR to keep the old geometry.
- **#1121** manual default SL + ratchet fallback **2.0×ATR**; `RatchetFallbackNormalizePending` one-shot widen
- **#1131** `manual-open` ATR fetch defaults to **1h** when the strategy timeframe is unset (was: dropped to coarse heuristic). No config change; display-only nuance
- **#1140** new operator command `force-close <id>` closes a **live HL `type=perps`** strategy position (analog of `manual-close` for automated strategies) — reduce-only, scheduler adopts the fill, records a `force_close` trade + updates RiskState PnL. See Manual Trading. No config change
- **#1110/#1118** ratchet tier-clear owner DM; per-strategy `notify_ratchet_triggers` shadows global
- **#1124** composite `ranging_directional` splits into `_up`/`_down` (9 labels); a bare `ranging_directional` in `allowed_regimes`/`*_atr_regime`/`regime_directional_policy` covers both subs (one-way; explicit `_up` gates out `_down`). No config change required; an explicit composite block listing the subs but omitting bare now errors at load. `regime_directional_policy` certification stays exact-match (bare does NOT certify subs)
- **#1157** owner DM added on DEFAULT-OFF/EXPIRED `regime_directional_policy` cert lines (startup + SIGHUP, deduped by snapshot — only new lines DM'd on a state change). `/status` `effective_direction`/`effective_invert_signal` now gated through the #1085 cert check (previously showed the ungated policy resolution while flat); new `directional_certification_status` (`certified`/`expired`/`uncertified`) and `directional_certification_cell` fields; Discord `/status` gets a `directional_policy:` suffix. No config change
- **#1137** new per-strategy `llm_entry_analysis` block (`{enabled, model, max_debate_rounds, timeout_s, notify_dm, notify_channel}`, default off) — after a fresh position-open, an async LLM analyst/debate pipeline (`shared_scripts/llm_review.py`, needs `ANTHROPIC_API_KEY`) posts a word-capped digest (DM by default, `notify_channel` opt-in for the shared channel) and stamps the verdict into `trade_diagnostics.llm_verdict` at close. Advisory only — never gates/sizes/closes; runs on its own subprocess lane (never the shared 4-slot semaphore); hot-reloadable always. No version bump
- **#1150** new per-strategy `paused` flag (bool, default `false`) — holds position-increasing signals (fresh opens, adds, flips) while closes, trailing SL/ratchet, and protection sync keep running; hot-reloadable always, including while open. Surfaces in `[config]` startup summary, `inspect`, `/status` JSON, and Discord `/status ⏸️ paused:`
- **#1147** new operator command `go-trader diagnostics [--strategy <id>]` — per-trade quality report (MFE/MAE/capture, regime/direction splits, sample-gated hypotheses with a ready-to-run backtest command). No config change; diagnostics-only, never blocks or alters a close.
- **#1189** dashboard/`/status`/status-log regime label for a strategy overriding `regime_gate_window` now shows that window's live label instead of the shared-default window's. Display-only — no trading-decision change.
- **#1190** kill-switch reset DM now includes the drawdown reason, trader-instance label, HL wallet address, and a protection-gap warning when the close plan hasn't confirmed flat. No config change.
- **#1368** new global `kill_switch_reset_dm_timeout` (Go duration string, e.g. `"6h"`; empty→6h default) replaces the old hard-coded 30m wait on the kill-switch reset owner DM — independent of `alert_throttle_interval`; SIGHUP-reloadable. No action needed to keep the (now-longer) default wait; set explicitly to restore the old 30m.
- **#1205** new operator command `/go-trader-apply-regime-gate` — interactively wires a named regime entry-gate preset onto a chosen flat strategy (restart-applied). See Discord Slash Commands.
- **#1224** new `regime.transitions` block (alerting-only — never gates entries, mutates config, or touches positions); per-window transition history + bar-accurate debounce + cross-window reversal alerts; `/status` note + `GET /api/regime/transitions`. No config change required to keep prior behavior.
- **#1229 (Phases 3–5, #1256/#1257/#1258)** dashboard gains mutating controls: pause/unpause + ratchet-notification toggles (low-risk), trade actions (close/manual edits), and structural mutations (add/remove strategy, paper-to-live, apply-regime-gate) — all behind a confirm-nonce typed-confirmation flow. No config change; operator-facing UI surface only.
- **#1264** `/paper-to-live` now refuses while the target strategy holds an open position (was silently reachable mid-position). No config change.
- **#1266** new global `alert_throttle` interval (6h default) coalescing repeat operator alerts. No config change to opt out of the default; tune the interval if 6h is too coarse/fine for your fleet.
- **#1268** new opt-in per-strategy `risk_per_trade_pct` (HL perps) — fixed-fractional position sizing off the resolved stop distance. See Adjustable Settings.
- **#1269** new `portfolio_risk.daily_max_loss_usd`/`daily_max_loss_pct` hard daily loss limit (0=off) — holds new entries until UTC rollover. See Adjustable Settings.
- **#1270** new `portfolio_risk.max_same_direction_notional_usd`/`max_asset_concentration_pct` same-direction/asset exposure caps (0=off) — blocks new correlated opens. See Adjustable Settings.
- **#1273** CB cooldowns/loss-streak threshold now per-strategy tunable (`cb_drawdown_cooldown_minutes`/`cb_loss_streak_threshold`/`cb_loss_streak_cooldown_minutes`); nil/omitted keeps the historical defaults. See Per-strategy table.
- **#1275/#1402** M5-deprecated-edge roster (32 strategies) now hidden from discovery + tagged `edge=deprecated_m5` with a one-time owner DM on startup/reload for **live** strategies; `allow_deprecated: true` silences the DM (tag stays `(ack)`). **Paper** strategies (`!isLiveArgs`) auto-suppress the warning/DM by default and tag `edge=deprecated_m5(paper)`; set `"allow_deprecated": false` explicitly to keep the warning on paper. Existing live configs keep today's behavior — this is a warning surface, not a block.
- **#1277** new `atr_method` global default + per-strategy override (`"simple"`|`"wilder"`, config v17, stamp-only — no on-disk rewrite). Default stays `"simple"`; switching a strategy to `"wilder"` is blocked while it has an open position. See Adjustable Settings.
- **#1278** new `regime.gate_on_failure` (global) / `regime_gate_on_failure` (per-strategy) entry-gate failure policy (`"open"` default | `"closed"`). Default preserves the pre-#1278 fail-open behavior — no action needed unless opting a strategy into fail-closed. See Adjustable Settings.
- **#1285** `MinSupportedConfigVersion` raised to 13 — a stamped `config_version<13` now fails loudly at load instead of migrating (v6–v12 handlers deleted). Run `scripts/check-config-versions.sh` before updating a fleet with any config that old.
- **#1315** Hyperliquid taker fee corrected 0.035%→0.045% (base tier) + new 0.015% maker constant — affects the modeled-fee fallback (`hyperliquid_fills` miss, `backfill-hl-fees`, `backfill-trade-ledger`) and every Hyperliquid-platform backtest. No config change; re-run `backfill-hl-fees`/`backfill-trade-ledger --apply` if you rely on modeled (non-exact) historical fees.
- **#1339/#1340/#1382** new dashboard `/tuning` page + persistent `POST /api/tuning/runs` / `GET /api/tuning/runs[/<id>]` API — launches suggest-only per-strategy research retunes (`tune_live.py`) on a dedicated serial lane that survive restarts (in-flight `queued`/`running` rows become `interrupted`); the page re-reads live config on every poll so diffs/baseline banners never go stale, and it never writes config. New optional `tuning.max_retained_runs` (0/omitted = keep-all) caps retained terminal run artifacts. See Adjustable Settings.
- **#1341/#1386** new `POST /api/tuning/apply` + dashboard Apply button — the one operator-explicit path to promote a ranked `/tuning` survivor into live config (identity triple `run_id`/`strategy_id`/`suggestion_key` only; unknown fields rejected). Refuses when the artifact predates schema v2 (`legacy_artifact`), the row isn't a survivor, or the live config has drifted from the tuner's recorded `promotion_baseline` since the run completed. A successful apply (or a crash-recovered pending finalize) triggers the same config reload as a manual edit. Every promotion — applied or refused — is journaled to `tuning_runs/promotions.json` for audit. No config change; still suggest-only until a human clicks Apply.

**Internal / no ops impact** (recent — detail in history doc)
- **#1128** HL adapter lazy `Exchange` init (fewer `/info` bursts on regime/OHLCV-only subprocesses); transient 429/rate-limit script failures WARN-only until 15 strikes or 75m sustained — then operator DM

**Opt-in field** (dormant until set — shape/detail in history doc)
- HL stops: `trailing_stop_atr_mult`, `trailing_stop_atr_regime`, `stop_loss_margin_pct`, `margin_per_trade_usd`
- Closes: `tiered_tp_atr_live`, `trailing_tp_ratchet*`, `*_atr_regime`, `sl_after`, N-tier `params.tiers`, `avwap_stop` (#1196 — exits on a `buffer_atr_mult`-ATR breach of the anchored VWAP; virtual exit only, no on-chain trigger)
- Regime: `regime.enabled`, `allowed_regimes`, `regime.display_windows`, `regime_directional_policy`, `regime_window_divergence`, `regime_profile_allocation`
- Manual: `type: manual` + `manual-open`/`manual-close` CLI; `user_defaults.manual` (legacy top-level `manual_defaults` migrates on load); shares coin with HL perps (#619)
- Alerts: `discord.trade_alert_channels`, `notify_ratchet_triggers`, `circuit_breaker`
- Open strategies: see registry / `go-trader init --list-json` (incl. hidden deprecated: `amd_ifvg`, `donchian_breakout`, `range_scalper`, `session_breakout`, `vol_momentum`)

**Internal / no ops impact** — dashboard, Discord formatting, probe/shutdown hardening, backtest parity fixes, etc. → [`docs/POST_UPDATE_HISTORY.md`](docs/POST_UPDATE_HISTORY.md) § Internal

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
`/go-trader-correlation`, `/go-trader-closing-strategies` (#1203 — catalogs every registered
close evaluator: name, description, platforms, config params; marks params overridden by
`user_defaults.close`; caches the registry after first read-only subprocess call). These read live in-process state via the
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
- `/go-trader-apply-regime-gate` (#1205) — interactive: `AskDM` numbered-list picker of type-eligible (`futures`/`perps`) live/paper strategies, applies a named regime entry-gate preset (ships `comp_up_clean_p21` — composite `trending_up_clean`@period 21, #1197), then an `AskDM` confirm before writing. Refuses a non-flat target (checked before AND after the confirm). The confirm also lists any OTHER strategy whose dormant `allowed_regimes` gate gets reactivated by the accompanying `regime.enabled` flip — read that list before confirming. Applies via a full restart (adding a `regime.windows` entry is SIGHUP-rejected).

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
   ./go-trader force-close <strategy-id> [--qty N] [--dry-run]   # live HL perps strategy close
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

Use `type: "manual"` on Hyperliquid for hand-driven entries/exits with scheduler-tracked P/L, close evaluators (default SL@2.0×ATR + `tiered_tp_atr_live` TP1@2× / TP2@3× when regime is off; #1115 regime-ratchet path when enabled), and Discord trade DMs (#569).

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
- Default SL multiplier for `type=manual` is **2.0× ATR** (#691/#692, widened #1121), distinct from the fleet-wide `default_stop_loss_atr_mult` (typically 1.0×) used by non-manual HL perps. Explicit `stop_loss_atr_mult`/`stop_loss_pct`/`stop_loss_margin_pct`/`trailing_stop_pct`/`trailing_stop_atr_mult` on the strategy still wins; fleet `default_stop_loss_atr_mult: 0` opts manual out too. Ratchet regime-read fallback uses the same resolver but ignores `user_defaults.manual.stop_loss_atr_mult: 0` (#1121).
- All four defaults (margin, SL multiplier, side, TP tiers) are overridable via the optional `user_defaults.manual` block (#696/#697/#1135) — see Adjustable Settings. `user_defaults.manual.stop_loss_atr_mult: 0` is a manual-only opt-out that doesn't affect non-manual HL perps; the block is hot-reloadable via SIGHUP. Legacy top-level `manual_defaults` is a deprecated alias that migrates to `user_defaults.manual` on load.
- Open blocked when portfolio kill switch active or strategy has pending CB close.
- Fills queued in `pending_manual_actions`, applied at top of next scheduler cycle (need `--once` if daemon idle). If the queue insert fails after a successful on-chain fill, the position is auto-flattened and SL/TP cancelled (#635); cleanup failures notify loudly — flatten manually.
- A 99% partial close is **not** silently collapsed into a full close — the queue carries explicit `is_full_close` intent from `--qty`.
- `manual-update-sl` / `manual-cancel-sl` (#1050) edit the resting stop-loss in place: they cancel-then-place (update) or cancel (remove) the on-chain SL, then queue an `update-sl`/`cancel-sl` action the daemon drains into memory — **no direct `state.db` write, no restart**. They are **hard-rejected** when the strategy's automated protection (ATR/regime `stop_loss_atr_mult`, trailing close) would re-pin the edit on the next cycle — only strategies opted out of auto-SL (`stop_loss_atr_mult: 0`, no trailing) qualify; the error names the opt-out. `update-sl` also refuses a trigger that would fill immediately against the current mark. Same kill-switch / pending-CB guards as `manual-open`; SL edits record no trade (no Discord trade DM).
- `manual-open` auto-fetches ATR against the strategy's timeframe; when the strategy `timeframe` is unset it now defaults the fetch to **1h** (the manual flow's canonical default) instead of dropping straight to the coarse heuristic (#1131). The success log prints the timeframe the ATR was actually computed against.
- `force-close <strategy-id> [--qty N] [--dry-run]` (#1140) closes a position on a **live Hyperliquid `type=perps` strategy** — the automated-strategy analog of `manual-close` (which only works on `type=manual`). Rejects paper mode and non-perps/non-HL strategies. It submits the reduce-only close, defers cancelling the on-chain SL/TP triggers until the close fill is confirmed (a failed or under-filled close never orphans protection), then queues the confirmed fill for the scheduler to adopt into state/trades on the next cycle (`--once` if the daemon is idle). The booked leg records a `force_close` trade and, unlike manual closes, **updates the strategy's RiskState** with the realized PnL (circuit-breaker-visible). A **full** close is refused while a stop-loss edit is queued for that position (run the scheduler first). `--qty` closes a partial; `--dry-run` previews without any exchange call or state write.
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
- Hot-reloadable when flat; toggling `allow_scale_in` or editing the `scale_in` block while a position is open is blocked (flatten first). **Backtestable since #1276** — `Backtester(allow_scale_in=…, scale_in=…)` or live `--config`; add legs simulate against the frozen risk anchor, not blended AvgCost.

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

## Trade Diagnostics (#1147)

Per-trade quality report over the closed-trade history:

```bash
./go-trader diagnostics                    # all strategies
./go-trader diagnostics --strategy hl-btc-momentum
```

Every full close eagerly inserts a `trade_diagnostics` row (identity/outcome) at
close time; a background worker fills in MFE/MAE/capture-ratio from hold-window
OHLCV afterward (never blocks or alters the close — failure just leaves those
columns NULL and `metrics_status` downgraded). The report opens the state DB
read-only, aggregates NET PnL per strategy via the trades join (so tiered-TP and
partial exits sum correctly across legs), splits by regime-at-open/direction, and
prints sample-size-gated hypotheses with the exact `run_backtest.py` command to
validate each one. Synthetic closes (`hl_sync_external`, `*_corrupt`,
`*_dup_oid`) are excluded. `llm_verdict` (from #1137 below) shows per row when
present but is written only by the LLM entry-analysis pipeline, never here.

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
| Notify on ratchet tier trigger | `notify_ratchet_triggers` | enabled (nil/missing); owner DM when a `trailing_tp_ratchet*` tier clears and tightens the trail. Set `false` to disable (#1110). Per-strategy `notify_ratchet_triggers` overrides this global (#1118) — see the per-strategy table. |
| `type=manual` defaults | `user_defaults.manual.{margin_usd,stop_loss_atr_mult,side,tp_tiers,trailing_stop_atr_regime}` | Optional overrides for the hardcoded manual-open defaults ($50 margin, 2.0× ATR SL, `long`, `[{2×,0.5},{3×,1.0}]`). Resolution order: CLI/strategy-param → `user_defaults.manual` → hardcoded constant. `trailing_stop_atr_regime` (#1115) tunes the per-regime opening trail for manuals that default to `trailing_tp_ratchet_regime` (cloned per strategy, resolved against each strategy's classifier labels). `stop_loss_atr_mult: 0` opts scalar manual out; ratchet fallback ignores 0 (#1121). Hot-reloadable via SIGHUP; `tp_tiers: []` is rejected at validation — omit the key to inherit the default (#696/#697/#1135). Legacy top-level `manual_defaults` is a deprecated alias migrated on load and rejected if it conflicts with the canonical section. |
| Daily loss limit (USD) | `portfolio_risk.daily_max_loss_usd` | `0` (disabled). Hard portfolio-wide cap on the day's aggregate PRE-FEE realized loss; once reached, position-increasing actions (fresh opens/adds/flips/manual-open/add) are held until UTC rollover — closes and SL/TP management keep running, nothing is force-closed. Hot-reloadable incl. while tripped. Ignored inside `platforms.<name>.risk` overrides (#1269). |
| Daily loss limit (%) | `portfolio_risk.daily_max_loss_pct` | `0` (disabled). Same limit as a percent of Σ per-strategy `initial_capital`. Both arms may be set — the lower resolved USD threshold wins; a 0-capital basis can't evaluate (surfaced in `/status`) (#1269). |
| Same-direction exposure cap (USD) | `portfolio_risk.max_same_direction_notional_usd` | `0` (disabled). Blocks new same-direction opens once aggregate same-direction notional (crypto dispatch sites + options coarse-delta filter + manual open/add/limit-open) would exceed the cap; hot-reloadable via SIGHUP (#1270). |
| Asset concentration cap (%) | `portfolio_risk.max_asset_concentration_pct` | `0` (disabled). Same blocking behavior scoped to a single asset's share of exposure; shares the exposure model with `correlation.*` (#1270). |
| ATR smoothing method | `atr_method` | `"simple"` (default; legacy rolling mean, `round_large` ≥100 rounding) or `"wilder"` (published Wilder RMA, never rounded). Global default for the `standard_atr` surface only — EntryATR stamping, live `market_ctx["atr"]`, manual fetch-atr, backtester injection, tuner simulate; strategy-internal indicator math and `regime.py` (pinned `simple`) are untouched. Per-strategy `atr_method` overrides (see Per-strategy table) (v17, #1277). |
| Tuning run retention | `tuning.max_retained_runs` | `0` (keep-all; prune off). Caps retained terminal `/tuning` research-run dirs/metadata; a positive N prunes oldest-first (result-less runs evicted before runs with `results.json`, then by completion/creation time, then ID) after startup load and after each terminal run persist. Never deletes `queued`/`running` runs. SIGHUP-adoptable (#1382). |

Per-strategy:

| Setting | Key | Notes |
| --- | --- | --- |
| Capital | `capital` | Starting capital reference |
| Max drawdown | `max_drawdown_pct` | Strategy CB |
| Circuit breaker | `circuit_breaker` | `false` disables BOTH CB arms (drawdown + consecutive losses), live and paper; nil/omitted → enabled (safe default). Suppresses only NEW fires (a latched CB / pending close still drains); display drawdown still updates. One-shot WARNING when a disabled CB suppresses a breach; `cb=off` in startup summary + `inspect`. Hot-reloadable via SIGHUP while open. `type=manual` exempt. No version bump (#1048). |
| CB timing/threshold | `cb_drawdown_cooldown_minutes` / `cb_loss_streak_threshold` / `cb_loss_streak_cooldown_minutes` | Optional per-strategy overrides of the CB's hardcoded parameters; nil/omitted → historical defaults (24h drawdown cooldown, 5-loss streak, 1h loss-streak cooldown). Positive only; cooldowns ≤ 30 days, threshold ≤ 100; rejected on `type=manual`. Read only via the `CircuitBreaker*` accessors — the same threshold accessor drives the firing arm and the #1048 suppression warning. Hot-reloadable via SIGHUP incl. while open (new fires only; a latched `CircuitBreakerUntil` is untouched). Non-defaults surface as `cb[…]` in startup summary + `inspect`. No version bump (#1273). |
| Notify on ratchet tier trigger | `notify_ratchet_triggers` | Per-strategy override of the global `notify_ratchet_triggers` (#1110) ratchet-tighten owner DM. Nil/omitted → inherit the global value; explicit `true`/`false` wins. Notification-only — hot-reloadable via SIGHUP even while a position is open (masked in `strategyRestartShape`, no state-compat guard). No version bump (#1118). |
| LLM entry analysis | `llm_entry_analysis` | `{enabled, model, max_debate_rounds, timeout_s, notify_dm, notify_channel}` (default off; model default `claude-sonnet-5`, rounds 1 [0–3], timeout 120s [max 600]; `notify_dm` on / `notify_channel` off by default, both per-strategy `*bool` overrides, both-off legal). After a FRESH position-open (not adds/flips/manual), an async pipeline posts an ELI18, ≤55-words-per-topic digest to the strategy's trade-alert DM (channel opt-in) and stamps the verdict (`bullish`/`bearish`/`mixed`) into `trade_diagnostics.llm_verdict` at close. Advisory only — an error/timeout posts nothing, zero trade impact. Dedicated job lane (own queue/concurrency, cancelled at shutdown, never the shared `pythonSemaphore`). Needs `ANTHROPIC_API_KEY`; `llm_review.py` probed at startup when any strategy opts in. Hot-reloadable via SIGHUP even while open. No version bump (#1137). |
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
| Risk-per-trade sizing | `risk_per_trade_pct` | HL perps only, opt-in — `qty = (cash × pct/100) / stop_distance`, capped at `cash × exchange_leverage`. Bounds `(0, 10]`. Mutually exclusive with `sizing_leverage`/`margin_per_trade_usd`/`allow_scale_in`; requires a stop owner resolvable at sizing time (regime-resolved/unified-close owners rejected at load). Fail-closed: an unresolvable stop distance refuses the open rather than falling back to notional sizing. Hot-reload: value tweaks always apply, risk↔notional mode switch blocked while open. Backtestable via `Backtester(risk_per_trade_pct=…)`/`--config` (#1268). |
| Regime-gate failure policy | `regime_gate_on_failure` | `"open"` (default; legacy fail-open) or `"closed"` (holds fresh opens only — posQty>0 management and closes always pass — while the regime store can't produce a gate label: subprocess failure, sealed budget, missing window). Overrides global `regime.gate_on_failure`; empty inherits. Hot-reloadable always, incl. while open. `closed` + `allowed_regimes` + `regime.enabled=false` rejected at load (permanent block) (#1278). |
| ATR smoothing method (override) | `atr_method` | Per-strategy override of the global `atr_method` (`"simple"`\|`"wilder"`; empty inherits). Same scope as the global default (`standard_atr` surface only). Rejected on `type=options`. Hot-reload blocked while open (#1277). |
| Margin mode | `margin_mode` | HL perps, `isolated` (default) or `cross`. Applied from flat. |
| Open strategy | `open_strategy` | Override entry strategy name (else `args[0]`) |
| Close strategy | `close_strategy` | Single exit ref `{name, params}` (#842 collapsed the array); legacy `close_strategies` array len ≤1 still read, len>1 rejected; nil → open-as-close |
| Regime gate | `allowed_regimes` | Labels allowing entries (`trending_up`, `trending_down`, `ranging`); empty = allow all; needs `regime.enabled=true`; not on type=options |
| Multi-window selectors | `regime_gate_window`, `regime_atr_window`, `regime_directional_window` | Require non-empty `regime.windows`. Route entry gate, regime-aware ATR/TP, and directional policy to different ADX horizons. Empty/`default` → legacy `regime.period`. Stamped labels persist in `pos.RegimeWindows` (#792). SIGHUP when flat; blocked while open. |
| Regime-profile allocation | `regime_profile_allocation` | HL perps (live + paper). Two open-param profiles of one strategy; a slow long-window regime label picks the active one, switched hysteretically (`confirm_bars`, WARN<12) and only while flat (frozen to the open profile while a position is open). Shape `{window, profiles{label→name, all labels}, param_sets{name→overrides, exactly 2}, confirm_bars≥1, initial_profile}`. Requires `regime.enabled=true`. Persisted (`active_profile`); SIGHUP blocks shape change while open, resets state when flat. Backtestable via `--config`. No version bump (#998). |
| Theta harvest | `theta_harvest.*` | Options early-exit |
| User close defaults | `user_defaults.close` and `user_defaults.regime_atr` | Optional `user_defaults.close` close-evaluator keys (`tiered_tp_atr`, `trailing_tp_ratchet_regime`, …) inject `tp_tiers` into matching close refs omitting `tp_tiers`. `trailing_tp_ratchet_regime` may also carry coupled `trailing_stop_atr_regime` (#1133). `user_defaults.regime_atr` supplies fleet-wide `stop_loss_atr_regime` / `trailing_stop_atr_regime` for standalone `use_defaults`-only strategy owners (#1134). Three-layer resolution: system → user → strategy (explicit wins). SIGHUP-hot-reloadable. Backtest: `--defaults system\|user`. Legacy top-level `user_close_defaults` is a deprecated alias migrated on load; its reserved `regime_atr` key moves to `user_defaults.regime_atr`, and non-equivalent canonical+legacy duplicates are rejected (#1135). |
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

`DISCOVERY_HIDDEN_STRATEGIES` (`amd_ifvg`, `donchian_breakout`, `range_scalper`, `session_breakout`, `vol_momentum`) are omitted from `--list-json` / `go-trader init` after research deprecations (#1034–#1041, #985) but stay registered — explicit `args[0]` / config refs still load. `liquidity_sweeps` is research-deprecated (#1032) but still discoverable.

Platform conventions:

| Platform | ID prefix | Type/script |
| --- | --- | --- |
| BinanceUS spot | none | `spot`, `shared_scripts/check_strategy.py` |
| Hyperliquid perps | `hl-` | `perps`, `shared_scripts/check_hyperliquid.py` |
| Hyperliquid manual | `hl-` | `manual` (#569), no script/interval; `manual-open`/`manual-close`; auto-defaults SL@2.0×ATR + `tiered_tp_atr_live` (TP1@2× / TP2@3×) when regime off (#1115 ratchet path when enabled); can share coin with HL perps peers (#619/#620) |
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
- **New bidirectional strategies (#895):** `momentum_pro` (`mompro`) — stacked-EMA trend-pullback entry (fast>mid>long EMA), ADX-confirmed, volume-backed bar break; requires `direction: "both"`. `mean_reversion_pro` (`mrpro`) — z-score reversion gated by no-trend ADX ceiling + RSI extreme confirmation; requires `direction: "both"`. Both spot + futures/perps. Walk-forward OOS result: `momentum_pro` BTC 4h is marginally validated (~+6% median Sharpe ~1, high variance); `mean_reversion_pro` is not OOS validated — paper-trade before live. `momentum_pro` (#980) and `mean_reversion_pro` (#981) both gained default-off research kwargs (`vol_target_atr_pct`/`vol_target_atr_period`/`vol_target_min_fraction` on momentum_pro; `touch_entry`/`turn_entry` on mean_reversion_pro) — M1-validated negative for a default change, live behavior unchanged unless explicitly set. `momentum_pro`'s standalone short leg is also negative (#1166: regime-gating the short via `regime_directional_policy` doesn't clear the M1 bar either).
- **Anchored VWAP family:** `anchored_vwap` (`avwap`, #1016) — single anchored-VWAP S/R flip; bidirectional (emits short on buffered breakdown below the line). Research-negative at default params (#1039) — treat as experimental; validate before live. **#1017 rider B:** default-off `gate_rsi_period`/`gate_rsi_level` (0/50.0) and `gate_ema_period` (0) momentum/trend gates — when set, longs need RSI≥level (or close≥EMA) on the signal bar, shorts the mirror; NaN warmup fails open (veto). Defaults keep `--list-json` bit-identical to pre-#1017. `anchored_vwap_channel` (`avwapch`, #1169) — dual-anchor channel (swing-low support / swing-high resistance line), range-edge mean reversion: long a bounce off support, short a rejection off resistance. `anchored_vwap_reversion` (`avwaprev`, #1170) — fades ATR-measured stretches beyond the anchored VWAP back toward the line (long below, short above), distinct per-bar from `anchored_vwap`'s breach trade. All three spot + futures/perps, pre-gated to composite `{ranging_quiet, ranging_volatile}` by default (channel/reversion) or unbounded (plain `anchored_vwap`). Pair `anchored_vwap` with the new `avwap_stop` close evaluator (exits on loss of the same line) for a self-consistent entry/exit — see `/go-trader-closing-strategies`.
- **Range strategies (#896):** `consolidation_range` (`cr`) — range-edge mean-reversion at the top/bottom of a consolidation box; bidirectional (emits `signal=-1` at the top edge), requires `direction: "both"` for HL perps. Negative OOS at default params — tune `box_width_pct` and `atr_stop_mult` before live. `range_scalper` (`rs`) — **deprecated/hidden from discovery (#1034)**; unidirectional support/resistance scalper kept loadable for explicit configs.
- **`atr_band_revert` (`abr`, #1069):** ranging mean-reversion — fade ATR-scaled bands around an SMA (long below `mid − k·ATR`; short above `mid + k·ATR` on the futures/perps `direction:"both"` variant). Entries only; exit is config not code — pair `tiered_tp_atr` (~`k_entry/2` & `k_entry` ATR tiers) + `stop_loss_atr_mult`. Spot long-only. `init` ships it pre-gated to `allowed_regimes:["ranging_quiet","ranging_volatile"]` on a composite "medium" window. Tunable baseline — backtest before live.
- `donchian_breakout`, `chart_pattern`, `liquidity_sweeps` already emitted `signal=-1` for bearish setups but were generated long-only by `init.go`. Since #654 they default to `direction: "both"` so existing perps configs need a regenerate or a manual `direction` flip to capture the short side. `liquidity_sweeps` research-deprecate (#1032) — still in discovery; prefer other structure strategies. `donchian_breakout` (`dbo`) is **deprecated/hidden from discovery (#985)** — long leg fails all M1 windows ungated and under every regime-gate variant; short leg has a real OOS edge but fails both bull-year held-outs; kept loadable for explicit configs.
- **`chart_pattern` HTF trend gate (#982):** four default-off params — `htf_gate_factor` (0 disables, default), `htf_gate_mode` (`veto`|`align`), `htf_gate_ema_fast`/`_slow` — veto/align pattern entries against a higher-timeframe EMA trend. M1-recommended opt-in: `{"htf_gate_factor": 4}` (mode `veto`) passes the protocol window and improves held-outs 1/3→2/3, but regresses 2024 — ships opt-in, not default. A regime-switched variant (gate on in `trending_down`/`ranging`, off in `trending_up`) was researched and rejected (#1167 — fails protocol OOS below both static parents); the static `{"htf_gate_factor": 4}` opt-in is the only recommended shape.
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
