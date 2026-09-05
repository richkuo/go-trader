# Agent Setup Guide — go-trader

Repository: `https://github.com/richkuo/go-trader.git`

Operator runbook for agents that install, configure, run, update, and operate go-trader. Reader-facing overview: [README.md](README.md). Coding constraints and PR conventions: [CLAUDE.md](CLAUDE.md) and [AGENTS.md](AGENTS.md).

Quick flow for a new server: tell OpenClaw `install https://github.com/richkuo/go-trader and init`.

---

## Core Rules

- Run git from the repo root.
- Use `/opt/homebrew/bin/go` (macOS) or `/usr/local/go/bin/go` (Linux) if `go` is not on PATH.
- Use `uv run --no-sync python` for dev, backtest, and manual CLI work. The scheduler calls `.venv/bin/python3` directly, so no PATH configuration is needed for the service.
- Install Python dependencies with `uv sync`.
- Scheduler config: `scheduler/config.json` (start from `scheduler/config.example.json`). On deployments the real file lives outside the deploy tree at `/var/lib/go-trader[/<instance>]/config.json`.
- State is SQLite only: default `scheduler/state.db`. Optional root `paper_db_file` moves the paper scope into a second file (§ Storage Ownership).
- Never store secrets in config files. Put Discord and exchange credentials in systemd environment variables.
- Prefer `./go-trader init` for humans, `./go-trader init --json … --output scheduler/config.json` for agents and scripts.
- TradingView export: ask which strategy IDs (or all) before running.
- **CRITICAL: always update with `scripts/update.sh`. Never run `git pull` + `go build` by hand.** The Go binary and the Python check scripts share an argv contract, so a build at a different commit than the scripts is an asymmetric deploy.

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

If the repo already exists, ask whether to reconfigure, update, or do a fresh install before changing it.

Build:

```bash
VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
/opt/homebrew/bin/go -C scheduler build -ldflags "-X main.Version=$VER" -o ../go-trader .
./go-trader --help
```

The `Version` ldflag appears in Discord summary titles; without it the binary reports `dev`. After the initial install, use `scripts/update.sh` for every rebuild.

---

## Configure

```bash
./go-trader init                                                    # human flow
./go-trader init --json '{"assets":["BTC","ETH"],"enableSpot":true,"spotStrategies":["momentum","rsi"],"spotCapital":1000,"spotDrawdown":60}' --output scheduler/config.json
```

The wizard covers assets, strategy groups, paper/live mode, per-strategy capital, live risk settings, Discord channels, and auto-update mode. It prompts before overwriting.

Config skeleton and the full field list: [README.md](README.md) § Configuration Reference. Rules that govern a hand-written config:

- Strategy entries need `id`, `type`, `script`, `args`, `capital`, `max_drawdown_pct`, `interval_seconds`.
- `open_strategy` and `close_strategy` are objects of shape `{"name": "<id>", "params": {…}}`. Per-evaluator params live on the close ref, never on the strategy. A legacy `close_strategies` array of length ≤1 is still read; length >1 is rejected at load.
- Tier lists use `tp_tiers`; each tier is `{"atr_multiple"|"profit_pct": N, "close_fraction": 0..1, "sl_after"?: {…}}`. Per-tier legacy `atr` / `multiple` / `fraction` aliases still parse. Write the canonical names.
- `discord.channels` / `telegram.channels` keys: `spot`, `options`, `hyperliquid`, `topstep`, `robinhood`, `okx`, `luno`, plus optional paper keys such as `okx-paper`. A non-empty `<platform>-paper` key also splits cycle summaries, leaderboards and Sharpe groups by mode; with no such key the grouping stays merged, exactly as before.
- `summary_frequency` uses the same key scheme. Values: `hourly`, `daily`, `every`, `per_check`, `always`, or a Go duration (`30m`, `2h`). The wall-clock cadence persists in SQLite and survives restart and SIGHUP. `options`, `perps`, `futures`, and `manual` post every channel run; `spot` posts hourly. A trade always forces an immediate post.
- `discord.owner_id` comes from `DISCORD_OWNER_ID`; it enables DM upgrade and migration prompts.
- **Separate live and paper state files (#1523).** Set root `paper_db_file` beside `db_file` and the paper scope's books, risk row, kill-switch events and correlation snapshot move to that file; the live scope, process metadata, the live-only wallet and cash-flow tables and shared regime history stay in `db_file`. Omit `paper_db_file` and the single-file layout is unchanged. The two paths must resolve to different physical files — relative paths, symbolic links and hard links are all checked — or startup refuses with exit code 80.
- **Stored identity.** Optional per-strategy `storage_strategy_id` names the row a strategy owns inside its file; it defaults to `id`. Storage identity is `(scope, storage_strategy_id)` and must be unique **within one file**; the same value in the live and paper files is the supported alias. Rename a strategy's `id` and set `storage_strategy_id` to the old name to keep its cash, positions, latches, queued actions and history. Both keys are restart-required.

Live-mode risk defaults offered by `init`: per-strategy spot drawdown 5%, per-strategy options drawdown 10%, portfolio kill switch 25%, portfolio warn threshold 60% of the kill switch.

**Unified per-regime close block** (`tiered_tp_atr_regime` / `tiered_tp_atr_live_regime`): instead of a tier-keyed list, the close ref carries a top-level `trend_regime` where each label owns its stop loss and tier ladder:

```json
{"name": "tiered_tp_atr_live_regime", "params": {"trend_regime": {
  "trending_up": {"stop_loss_atr": 1.5, "tp_tiers": [
    {"atr_multiple": 2.0, "close_fraction": 0.5, "sl_after": {"kind": "trail_from_here", "tp_atr_fraction": 0.5}},
    {"atr_multiple": 4.0, "close_fraction": 1.0}]},
  "ranging": {"stop_loss_atr": 0.8, "tp_tiers": [
    {"atr_multiple": 1.0, "close_fraction": 1.0}]}
}}}
```

All regime labels must be present (exhaustive, no fallback). Tier counts may differ per label. The block **owns the stop loss** through per-regime `stop_loss_atr`, so declaring any strategy-level stop field alongside it is rejected at load. The whole block is hot-reload-gated as a unit: change it while a position is open and the reload is refused. Flatten first.

**Trailing-ratchet close** (`trailing_tp_ratchet` / `trailing_tp_ratchet_regime`): a trailing-ATR stop where each cleared take-profit tier tightens the trail and optionally scales out. The scalar form needs a positive strategy-level `trailing_stop_atr_mult` (the initial loose trail, and the sole stop owner). The regime form needs `trailing_stop_atr_mult_regime` instead. `tp_tiers` is a list (scalar form) or `{label: [tiers]}` (regime form, frozen at open). Each tier is `{atr_multiple, close_fraction?, trailing_mult_after | tp_atr_fraction}`: `close_fraction` (default `0`, cumulative) scales out, `0` means trail-only; the trail tightens to `trailing_mult_after` (absolute ATR multiple) **or** `tp_atr_fraction × atr_multiple` (relative), never both, and never loosens. The first rung must be ≤ the initial trail. It places **no on-chain take-profit**: partial closes ride the close evaluator, the on-chain stop rides the trailing-stop walker. Tier triggers use entry ATR. Scope: Hyperliquid perps and `type=manual`. Backtestable.

---

## Secrets

Set in systemd overrides or exported environment variables before installation:

| Variable | Description |
| --- | --- |
| `DISCORD_BOT_TOKEN` | Discord bot token |
| `DISCORD_OWNER_ID` | Discord user ID for DM upgrades and migrations |
| `STATUS_AUTH_TOKEN` | Optional bearer token for `/status` |
| `ANTHROPIC_API_KEY` | Required only when a strategy opts into `llm_entry_analysis` |
| `GO_TRADER_GITHUB_TOKEN` | Token for `/go-trader-report-an-issue` (falls back to `GITHUB_TOKEN`) |
| `BINANCE_API_KEY`, `BINANCE_API_SECRET` | Binance live |
| `HYPERLIQUID_SECRET_KEY`, `HYPERLIQUID_ACCOUNT_ADDRESS` | Hyperliquid live |
| `TOPSTEP_API_KEY`, `TOPSTEP_API_SECRET`, `TOPSTEP_ACCOUNT_ID` | TopStep live |
| `ROBINHOOD_USERNAME`, `ROBINHOOD_PASSWORD`, `ROBINHOOD_TOTP_SECRET` | Robinhood live |
| `OKX_API_KEY`, `OKX_API_SECRET`, `OKX_PASSPHRASE`, `OKX_SANDBOX` | OKX live/demo |
| `LUNO_API_KEY_ID`, `LUNO_API_KEY_SECRET` | Luno live |
| `GO_TRADER_ALLOW_MISSING_STATE` | `1` only for a genuine first-run live deployment |
| `GO_TRADER_CASHFLOW_JOURNAL_ALARM` | `0`/`off`/`false`/`no` forces the legacy trade-ledger drift basis for HL shared wallets (default on) |
| `GO_TRADER_HL_BATCH` | `0`/`off`/`false`/`no` disables batched Hyperliquid signal checks |

---

## Run And Install Service

```bash
./go-trader --config scheduler/config.json --once     # smoke test
mkdir -p logs
export DISCORD_BOT_TOKEN="{token}"
sudo bash scripts/install-service.sh
```

The installer copies the unit, runs `daemon-reload`, enables, starts, and pre-creates `logs/` so `ProtectSystem=strict` does not block first-run logging.

Templated multi-instance: `sudo bash scripts/install-service.sh systemd/go-trader@.service paper-testing`. Install without starting: `NO_START=1 sudo bash scripts/install-service.sh`.

```bash
sudo systemctl start|stop|restart|status go-trader
journalctl -u go-trader -n 100 --no-pager
```

**Config out of the deploy tree.** Both units set `StateDirectory=go-trader[/%i]` and point `ExecStart --config` at `/var/lib/go-trader[/<instance>]/config.json`. For an existing in-tree deploy, stop the service first, then run `scripts/migrate-config-out-of-tree.sh [--instance <name>]` — it refuses while the daemon is live. `scheduler/config.json` stays as a transition symlink; a config-version migration that rewrites the file replaces that symlink with a regular in-tree file, so prefer the out-of-tree path.

**File ownership.** Before any migration, startup write, probe or trading, the scheduler takes an exclusive lock on **every** configured state file, in role order (primary, then paper); a failure releases everything already held and exits with code 79 (`ExitSingletonLock`), naming the file and the holder pid. `--once` takes ownership too, so it refuses to run while the daemon is up — the daemon drains queued manual fills on its own next cycle. Startup then runs the read-only ownership check (`storage-inspect`); a rejected layout exits with code 80 (`ExitStorageOwnership`). Manual commands and the dashboard take only the owning file's manual-action lock, so they still work beside a running daemon; `backfill --apply` takes full ownership plus both manual-action locks.

**Startup probe.** Every unique check script runs with `--probe-only`. A non-zero result logs, DMs the owner, and exits with code 78 (`ExitProbeFailure`). Both unit files set `RestartPreventExitStatus=78 79 80`, so the service stays down instead of crash-looping. A probe failure right after an update almost always means `shared_scripts/` was not updated or the binary was not rebuilt — rerun `scripts/update.sh`.

**Graceful shutdown.** The daemon drains side-effecting subprocesses for up to 15 seconds, then SIGKILLs; state save, notifier flush, and DB close run afterwards. The unit sets `TimeoutStopSec=20`. Service-file edits need `daemon-reload`.

---

## Auto-Update

`auto_update`: `off` | `daily` | `heartbeat`. When an update is found the bot notifies active Discord channels. With `DISCORD_OWNER_ID` set it DMs the owner; replying yes within 30 minutes runs `scripts/update.sh`, saves state, and restarts.

```bash
# Systemd deploy (default)
cd /path/to/go-trader && bash scripts/update.sh --restart

# Linux bare-process deploy (no systemd)
cd /path/to/go-trader && bash scripts/update.sh --restart --restart-mode signal

# Sync from a source clone without clobbering secrets/state/venv/binary
bash scripts/update.sh --rsync-from /path/to/source-clone --restart

# Batch-update every discovered deployment (requires --restart)
bash scripts/update.sh --all --restart [--update-all-root <parent-dir>]
```

`scripts/update.sh` is the single source of truth for `git pull --ff-only` + `uv sync` + `go build`, all gated under `set -euo pipefail`. External deploy automation (Ansible, image bake) must call this script rather than reproduce the steps inline.

**`--rsync-from <src>`** replaces `git pull --ff-only` with an rsync from a source clone. It preserves `.git/`, `scheduler/config.json` (or its transition symlink), `state.db` and its WAL sidecars, `.venv/`, and the live binary. Use it when the deployment directory has local changes or was not cloned from origin. Before the restart it warns on stderr about any required `EnvironmentFile=` the unit declares but the disk does not have; optional entries prefixed with `-` are skipped silently.

**Signal mode** (`--restart-mode signal` or `RESTART_MODE=signal`) SIGTERMs the PID in `GO_TRADER_PIDFILE` (default `./go-trader.pid`), respawns through `GO_TRADER_RUN_SH` (default `./run.sh`), then polls `/health` and PID freshness with the same verify-and-rollback flow as systemd mode. Generate a starter `run.sh` with `bash scripts/create-run-sh.sh`. Other signal-mode variables: `GO_TRADER_SIGNAL_LOG`. When systemd mode meets a missing unit (systemctl exit 5), update.sh retries in signal mode automatically if `go-trader.pid` and an executable `run.sh` are present.

**Batch mode** (`--all`) discovers deployments from the systemd `WorkingDirectory` of every loaded `go-trader`, `go-trader-*`, and `go-trader@*` unit, so siblings need not share a parent directory, and runs the full flow in each one sequentially. `--update-all-root <dir>` or `GO_TRADER_UPDATE_ALL_ROOT` pins the legacy `go-trader-*/` glob and skips discovery; that is also the automatic fallback when `systemctl` is absent or no units load. Skipped directories are logged on stderr, and a batch that updates nothing fails loudly. Each child resolves `GO_TRADER_SERVICE` from the active unit that owns its `WorkingDirectory`; a directory no active unit owns falls back to the parent's service and logs a warning.

Verify: `journalctl -u go-trader -f | grep -i "\[update\]"` (systemd) or `tail -f ./go-trader-signal.log` (signal mode).

**Preflight audits before a fleet update:**

```bash
bash scripts/check-config-versions.sh              # every deployment at or above the supported config floor
bash scripts/check-live-paper-config-drift.sh      # live/paper twin cadence + sizing drift
bash scripts/check-hl-stop-bankruptcy-bound.sh     # no HL stop sits past the isolated-margin bankruptcy distance
```

Each one auto-discovers active systemd deployments and accepts explicit deployment directories instead. Each exits non-zero on a finding.

---

## Post-Update Agent Protocol

When invoked after an update (manual `git pull`, auto-update restart, "I just updated", "what changed"), walk the operator through anything new commits change on their existing config, strategies, and open positions, and prompt before applying any opt-in. The binary's own migration DM only covers a small registered field set; newer config-version bumps and opt-ins land silently unless an agent surfaces them.

### Trigger

Run when ANY of:

- The operator says "I updated", "I just pulled", "what's new", or asks about migration.
- `git log -1 --format=%cI` is newer than the running binary's version (`./go-trader --version`, or `curl -s localhost:8099/health` → `version`).
- `git status` is clean and `git rev-list --count <running-version>..HEAD` > 0.

### Steps

1. **Identify the diff.** `git log --oneline <running-version>..HEAD -- scheduler/ shared_scripts/ shared_strategies/ platforms/`. If the running version is unknown, ask the operator, or fall back to the last 30 commits.
2. **Classify** each commit against the table below.
3. **Read current state.** Load the config and query the state DB:
   ```sql
   SELECT strategy_id, symbol, quantity, side FROM positions WHERE quantity > 0;
   SELECT strategy_id, symbol, contracts, action FROM option_positions WHERE contracts > 0;
   ```
4. **Prompt per item.** Default to no change if declined. For runtime defaults, also offer to write the explicit opt-out value.
5. **Apply through SIGHUP-safe edits** when the field supports it (see Reconfiguration); otherwise require a full restart.
6. **Verify.** Tail the logs for `[reload]`. On rejection, show the reason and offer a restart.

### Required prompt template

> Change: `<short description>`
> Affects: `<strategy IDs>` (and any open positions: `<symbol qty side>`)
> Default if you do nothing: `<what happens silently>`
> Options: 1) accept the new default, 2) opt out by setting `<field> = <value>`, 3) opt in to the new feature with `<field> = <value>` (requires flat? Y/N).
> Your choice?

Never apply a runtime-default change silently when the operator has not been shown the affected strategies. "Auto" means an automatic JSON rewrite, not an automatic behavior change.

### Reference: classification table

When in doubt, treat a commit as a runtime default and prompt. Per-release narrative for every archived entry lives in [`docs/POST_UPDATE_HISTORY.md`](docs/POST_UPDATE_HISTORY.md); regenerate a fresh candidate list from `git log --oneline -50`.

| Category | How to recognize it | What to do |
| --- | --- | --- |
| Auto-migration | `CurrentConfigVersion` bumped; the loader rewrites the JSON on next start | Summarize. No prompt. Warn that a rewrite replaces a still-symlinked `scheduler/config.json` with a regular file |
| Runtime default | Behavior shifts on existing strategies with no config edit | Prompt: confirm, or write the explicit opt-out |
| New opt-in field | The feature stays dormant until the field is set | Prompt per affected strategy |
| Open-position constraint | The change needs flat positions to apply | List affected strategies, warn, and skip until flat |
| Internal / no-op | Refactors, tests, docs, dashboard and formatting work | Mention briefly |

**Config-version floor.** `MinSupportedConfigVersion` is 13. A stamped `config_version` below that fails loudly at load instead of migrating. Run `scripts/check-config-versions.sh` and confirm the whole fleet is at or above the floor before raising it again.

**Opt-in fields** stay dormant until set. Adjustable Settings is the complete list, with each shape, default, and reload rule.

**Fields blocked while a position is open** (flatten first, or restart after the close):

- `margin_mode` and exchange `leverage`
- kill-switch identity changes
- `stop_loss_atr_mult` / `trailing_stop_atr_mult` nil↔positive toggles, and any scalar↔regime stop flip
- `invert_signal`
- `regime_*_window` selectors, `regime_directional_policy`, `regime_window_divergence`, `regime_profile_allocation` shape changes
- `hedge` add, remove, or shape change (either leg open)
- `replay_sharing` toggle and `replay_source_id` changes (the paper book would desync from the log mid-trade)
- `atr_method`
- `allow_scale_in` and the `scale_in` block
- the unified per-regime close block, and any ratchet tier table

---

## Status

Default port `8099`. Override with `--status-port <port>` or `status_port` in config. If the port is busy the server tries the next five; the log names the one it took.

```bash
curl -s localhost:8099/status | python3 -m json.tool
curl -s localhost:8099/health
curl -s localhost:8099/history
open http://localhost:8099/dashboard
```

Dashboard JSON endpoints: `/api/strategies`, `/api/strategies/overview`, `/api/strategies/<id>/(candles|trades|status|equity|config|simulate)`, `/api/regime/transitions`, `/api/tuning/runs[/<id>]`. Candles and equity are cached 30 seconds. `config` (GET) and `simulate`/`config` (POST) need `status_token` plus a same-origin header; when `status_token` is set the dashboard page prompts for it and keeps it in browser local storage.

**Never expose the status port publicly.** The server listens on loopback only. Do not rebind to `0.0.0.0`. Front each instance with [Tailscale Serve](https://tailscale.com/kb/1242/tailscale-serve) or another authenticated proxy on the same machine — for example `tailscale serve --bg --https=8443 http://127.0.0.1:8099`, then browse `https://<node>.tailnet.ts.net:8443/dashboard`. A common multi-instance port map (match each `status_port`): live `8099`, paper-testing `8100`, then `8101`+ per paper instance. An agent stack such as OpenClaw may serve its own dashboard on other ports; that UI is not go-trader's.

The dashboard also carries mutating controls behind a typed-confirmation nonce: pause/unpause and ratchet-notification toggles, trade actions (close, manual edits), and structural mutations (add/remove strategy, paper-to-live, apply-regime-gate).

`/tuning` is a read-and-launch research page for suggest-only per-strategy retunes. It re-reads live config on every poll and never writes config itself; `POST /api/tuning/apply` is the only promotion path.

If Discord is enabled, wait for the first cycle and confirm messages in the configured channels. Report success with mode, strategy count, status URL, and the log command.

---

## Discord Slash Commands

Global slash commands register at startup, covering every guild the bot is in plus DMs; a first-time command-shape change can take about an hour to propagate. The bot must be invited with the `applications.commands` OAuth scope in addition to `bot`. Every command carries the `go-trader-` prefix on the wire (`/go-trader-status`); authorization and dispatch operate on the bare ID. Registration failure is non-fatal — it logs and DMs the owner.

**Read-only** — any guild or DM, anyone. They read live in-process state with no HTTP round trip. Replies are public in-channel unless `discord.ephemeral_replies: true`.

`/go-trader-status`, `/go-trader-health`, `/go-trader-positions`, `/go-trader-pnl`, `/go-trader-leaderboard [top]`, `/go-trader-circuit-breakers`, `/go-trader-dead-strategies`, `/go-trader-correlation`, `/go-trader-closing-strategies`. The four that fetch live marks (`status`, `positions`, `pnl`, `leaderboard`) defer the ACK so they do not blow Discord's 3-second deadline.

**Ops and mutating ops** — owner-only AND DM-only:

| Command | What it does |
| --- | --- |
| `logs [n]` | The last N `journalctl -u go-trader` lines. DM-only because logs can carry wallet addresses and error payloads |
| `restart` | `systemctl restart go-trader`; it ACKs, then this instance is replaced |
| `backtest <strategy> <symbol> [timeframe]` | A single-mode backtest, 5-minute timeout, holding one of the four Python semaphore slots; replies with a summary and attaches the report |
| `report-an-issue <title> <body> [label]` | Files a GitHub issue against `discord.report_repo` (default `richkuo/go-trader`). Token from `GO_TRADER_GITHUB_TOKEN`, then `GITHUB_TOKEN`, then `discord.report_github_token`; it says so when none is set |
| `config show` | The running config with secrets redacted |
| `config set <key> <value>` | A top-level or per-strategy field. Per-strategy keys (`strategies.<id>.<field>`) need `config_version` 13 or newer. Writes are atomic and serialized with the dashboard tuner; the reply states whether it applied through SIGHUP or a restart |
| `add-strategy <name> <platform> <asset>` | Generates a Hyperliquid perps (always paper) or BinanceUS spot entry. The name must be a known short name |
| `remove-strategy <id>` | Removes a strategy after an out-of-band DM confirm. Needs a restart |
| `add-platform <name>` | Emits a setup checklist. Secrets go in the environment file, never the config |
| `paper-to-live <strategy>` | Flips `--mode=paper` to `--mode=live` after a DM confirm. Needs a restart, and refuses while the strategy holds an open position |
| `apply-regime-gate` | Interactive picker over type-eligible flat strategies, applies a named regime entry-gate preset, then confirms before writing. It refuses a non-flat target both before and after the confirm. The confirm lists any OTHER strategy whose dormant `allowed_regimes` gate the accompanying `regime.enabled` flip would reactivate — read that list first. Applies through a full restart |
| `clear-cash-reconcile <strategy>` | Clears the cash-reconcile latch after you confirm virtual cash matches the venue. It never invents or adjusts cash; it only drops the block on live spot buys |

Every config write serializes on one mutex, and mutating commands restart deployment-agnostically (systemctl with `GO_TRADER_SERVICE`, falling back to an in-process exec for signal-mode deploys).

---

## TradingView Export

```bash
./go-trader export tradingview --strategy hl-btc-momentum --output tv-hl-btc.csv
./go-trader export tradingview --strategy hl-btc-momentum --strategy okx-eth-breakout --output tv-selected.csv
./go-trader export tradingview --all --output tv-all.csv
```

Ask which strategy IDs (or all) before running. CSV header, symbol mappings, and `tradingview_export.symbol_overrides`: [README.md](README.md) § TradingView Export.

---

## `/go-trader` Command

When the user says `/go-trader`, "check bot status", "show strategy health", or "how are the bots doing":

```bash
curl -s localhost:8099/status | python3 -c "
import json, sys
d = json.load(sys.stdin)
strats = d.get('strategies', {})
print(f'=== GO-TRADER (Cycle {d[\"cycle_count\"]}) ===')
for sym, p in sorted(d.get('prices', {}).items()):
    print(f'  {sym}: \${p:,.2f}')
val = sum(s['portfolio_value'] for s in strats.values())
cap = sum(s['initial_capital'] for s in strats.values())
pct = ((val-cap)/cap)*100 if cap else 0
print(f'\nPortfolio: \${cap:,.0f} -> \${val:,.0f} ({val-cap:+,.0f} / {pct:+.1f}%)')
cb = [(i,s) for i,s in strats.items() if s['risk_state'].get('circuit_breaker_until','').startswith('20')]
print(f'Strategies: {len(strats)} | Circuit breakers active: {len(cb)}')
ranked = sorted(strats.items(), key=lambda x: x[1]['pnl_pct'], reverse=True)
for label, rows in (('Top 5', ranked[:5]), ('Bottom 5', ranked[-5:])):
    print(f'\n{label}:')
    for i, s in rows:
        print(f'  {i}: {s[\"pnl_pct\"]:+.1f}% (\${s[\"pnl\"]:+,.0f}) | {s[\"trade_count\"]} trades')
dead = [i for i,s in strats.items() if s['trade_count'] == 0]
if dead:
    print(f'\nDead (0 trades): {len(dead)} - {dead}')
for i, s in cb:
    rs = s['risk_state']
    print(f'  CB {i}: dd={rs[\"current_drawdown_pct\"]:.1f}% / max={rs[\"max_drawdown_pct\"]:.0f}% | until {rs[\"circuit_breaker_until\"][:19]}')
"
```

Present the output as readable prose. Highlight circuit breakers, dead strategies, large PnL changes, and missing status data. When `/status` reports `drawdown_reading_substituted`, say so — the drawdown figure is carried forward, not measured this cycle.

---

## `/menu` Command

When the user says `/menu`, "show menu", "what can I configure", or "help me get started", present these five groups and pull the live detail from the named source rather than from memory:

1. **Trading platforms** — Strategy Reference § Platform conventions.
2. **Available strategies** — list them from the registries, never from memory (Strategy Reference), plus `/go-trader-closing-strategies` for close evaluators.
3. **Adjustable settings** — Adjustable Settings.
4. **Commands** — the operator CLI below.
5. **Backtesting** — Backtesting.

Operator CLI:

```bash
./go-trader init [--json '{…}' --output scheduler/config.json]
./go-trader manual-open <strategy-id> [--side long|short] [--size N | --notional N | --margin N]
./go-trader manual-open <strategy-id> --limit-price N [--tif Alo|Gtc] [--expire-after N]
./go-trader manual-add <strategy-id> [--size N | --notional N | --margin N]
./go-trader manual-cancel <limit-order-id>
./go-trader manual-close <strategy-id> [--qty N]
./go-trader force-close <strategy-id> [--qty N] [--dry-run]      # live HL perps strategy close
./go-trader manual-update-sl <strategy-id> --trigger N [--symbol Y] [--dry-run]
./go-trader manual-cancel-sl <strategy-id> [--symbol Y] [--dry-run]
./go-trader backfill hl-fees [--strategy <id>|--all] [--apply] [--reset-cash]
./go-trader backfill trade-ledger [--strategy <id>|--all] [--apply] [--reset-cash]
./go-trader diagnostics [--strategy <id>]
./go-trader inspect <strategy-id> [--all] [--json]
./go-trader export tradingview [--strategy <id>|--all] --output <file>
./go-trader agent-info [--bootstrap-md] [--append-changelog]
sudo systemctl start|stop|restart|status go-trader
journalctl -u go-trader -n 50 --no-pager
curl -s localhost:8099/status | python3 -m json.tool
```

`agent-info --bootstrap-md` writes `AGENTS.generated.md`, never `AGENTS.md`.

---

## Manual Trading (HL perps)

`type: "manual"` on Hyperliquid gives hand-driven entries and exits scheduler-tracked P/L, close evaluators, and Discord trade DMs. Skeleton — no `script`, `args`, or `interval_seconds`, the loader fills them:

```json
{"id":"hl-manual-btc","type":"manual","platform":"hyperliquid","symbol":"BTC","capital":1000,"leverage":3,"max_drawdown_pct":10}
```

**Close defaults:** with `regime.enabled` and a resolvable per-regime trail, a manual strategy defaults to `trailing_tp_ratchet_regime` (the regime trail owns the stop). Otherwise `tiered_tp_atr_live` (TP1 at 2× ATR, TP2 at 3×) with a scalar stop at 2.0× ATR. Override through `close_strategy`, an explicit stop field, or `user_defaults.manual`.

Manual and perps strategies may share a coin. Owner guards prevent cross-strategy mutation, a full close never flattens a peer's position, and all take-profit order IDs are cancelled on full close. Peers must share `leverage` and `margin_mode`, and at most one may own a trailing stop.

```bash
# Open — at most one of --size, --notional, --margin
./go-trader manual-open hl-manual-btc                              # defaults: --side long --margin 50
./go-trader manual-open hl-manual-btc --side long --size 0.01
./go-trader manual-open hl-manual-btc --side short --margin 100    # margin × leverage = notional
./go-trader manual-open hl-manual-btc --side long --size 0.01 --atr 850   # skip the ATR fetch

# Scale in — side inferred, blends avg cost, freezes the risk plan
./go-trader manual-add hl-manual-btc --margin 50

# Edit the resting stop in place
./go-trader manual-update-sl hl-manual-btc --trigger 66000
./go-trader manual-cancel-sl hl-manual-btc

# Close — full or partial
./go-trader manual-close hl-manual-btc [--qty 0.005]

# Record-only (order placed on the HL UI; the scheduler tracks it)
./go-trader manual-open  hl-manual-btc --side long --size 0.01 --record-only --fill-price 67800
./go-trader manual-close hl-manual-btc --qty 0.005 --record-only --fill-price 68250
```

Guardrails:

- `--record-only` skips the live order; pair it with `--fill-price`. The stop is **not** auto-armed — place the trigger on the UI yourself.
- Stop and take-profit reduce-only orders are placed inline on open. Omitting `--atr` auto-fetches ATR(14) for the strategy's symbol and timeframe, defaulting the fetch to 1h when the strategy has no `timeframe`. A failed fetch falls back to `0.1 × fillPrice / leverage` with one combined notification. The success log names the timeframe used.
- `--side` defaults to `long`. With no sizing flag and no `--record-only`, `--margin 50` is applied, so a bare `manual-open <strategy-id>` works as a smoke test.
- The manual default stop is 2.0× ATR, distinct from the fleet-wide `default_stop_loss_atr_mult` (typically 1.0×) for non-manual HL perps. An explicit stop field still wins. `user_defaults.manual.stop_loss_atr_mult: 0` opts manual out without touching non-manual perps; the ratchet fallback ignores that 0.
- Opening is blocked while the portfolio kill switch is active or the strategy has a pending circuit-breaker close.
- Live market `manual-open`, `manual-close`, and `manual-add` refuse without queueing when the exchange returns no confirmed fill (`AvgPx>0` and `TotalSz>0` required); state is unchanged. The dashboard trade-actions API returns HTTP 409 with the same error.
- Fills queue in `pending_manual_actions` and apply at the top of the next cycle; the running daemon drains them itself. `--once` now takes file ownership, so it refuses while the daemon is up — start the daemon rather than racing it. Each drained action is acknowledged **by row id inside the transaction that persists its effect**, so an action that fails to apply keeps its row and is never deleted by a later success, in its own file or the other one. If the queue insert fails after a successful on-chain fill, the position is auto-flattened and its protection cancelled; a cleanup failure alerts loudly — flatten by hand.
- A 99% partial close is never silently collapsed into a full close: the queue carries the explicit `--qty` intent.
- `manual-update-sl` / `manual-cancel-sl` cancel-then-place (or cancel) the on-chain stop, then queue an action the daemon drains into memory — no direct state-DB write, no restart. They are **hard-rejected** when the strategy's automated protection would re-pin the edit next cycle; only strategies opted out of auto-stops qualify, and the error names the opt-out. `update-sl` also refuses a trigger that would fill immediately against the current mark. A stop edit records no trade.
- `force-close <strategy-id>` closes a position on a **live Hyperliquid `type=perps`** strategy — the automated-strategy analog of `manual-close`. It rejects paper mode and every non-HL or non-perps strategy. It submits the reduce-only close, defers cancelling the on-chain triggers until the fill is confirmed (so a failed or under-filled close never orphans protection), then queues the confirmed fill for the next cycle. The booked leg records a `force_close` trade and, unlike a manual close, updates the strategy's risk state, so the circuit breaker sees it. A full close is refused while a stop edit is queued. `--qty` closes a partial; `--dry-run` previews with no exchange call and no state write.
- External closes made on the UI, or by a stop or take-profit, are detected by the reconciler and cleared automatically.
- `type=manual` is exempt from circuit-breaker drawdown checks.

### Resting limit orders

```bash
./go-trader manual-open hl-manual-btc --limit-price 68000 --side long --margin 50 [--tif Gtc] [--expire-after 4h]
./go-trader manual-cancel <limit-order-id>
```

- `--tif Alo` (default, post-only) or `--tif Gtc` only. `Ioc` is rejected because it never rests.
- The CLI exits right after placing the order. The scheduler polls fill status each cycle and books fills incrementally; partial fills open the position and grow it under the same position ID.
- Protection is **not** placed inline. The next cycle after the first fill applies it, as for any manual position.
- `manual-cancel <id>` queues a cancel; the scheduler cancels on-chain and finalizes next cycle. Expiry and operator cancel share that path.
- Rows live in `pending_limit_orders`; each partial-fill leg is tagged `scale_in`, so `#T` counts one position however many fills it took.

### Scale-in / pyramiding

An opt-in way to **increase** an open position instead of the default skip-on-same-direction. Scope: Hyperliquid perps and manual, live and paper. A same-direction add blends only price and size for PnL (`AvgCost`, `Quantity`, `InitialQuantity` grow) and **freezes the original risk plan** — entry ATR, the regime label, and the trigger geometry stay pinned to the first entry, and the cleared-tier watermark is never reset. Only the on-chain protection **size** is re-based, at unchanged triggers, on the next protection sync.

- **Strategy flags (perps):** `allow_scale_in: true` plus an optional `scale_in` block — `max_adds` (0 = unlimited), `max_added_notional_usd` (0 = unlimited), `add_spacing_atr` (signed: `>0` adds to winners, `<0` averages down, `0` no gate, measured in entry-ATR multiples from the last leg), `add_notional_usd` (0 = the standard open notional). It fires only when a same-direction signal actually reaches Go; close-evaluator strategies use `manual-add`.
- **CLI (manual):** `manual-add <strategy-id>` takes the same sizing flags as `manual-open`. Side is inferred, it refuses when flat, and the kill-switch and pending-breaker guards apply.
- An add books as `trade_type=scale_in` on the same position ID and is excluded from the `#T` open count, so `#T` stays distinct positions. Win/loss is unaffected.
- **Live perps guard:** `allow_scale_in` needs an ATR, regime, or trailing stop — one the resize path can grow. A static scalar stop is rejected at load because it would under-cover the grown position. Manual auto-uses an ATR stop, so it qualifies.
- Hot-reloadable when flat; toggling either field while open is blocked. Backtestable; add legs simulate against the frozen risk anchor, not the blended average cost.

---

## Backfill HL Fees

Hyperliquid `exchange_fee` was $0 for trades placed before exact-fee resolution shipped.

```bash
./go-trader backfill hl-fees --all                    # dry run
./go-trader backfill hl-fees --strategy hl-btc-momentum
sudo systemctl stop go-trader
./go-trader backfill hl-fees --all --apply
sudo systemctl start go-trader
```

- `--apply` refuses while another `go-trader` process is alive.
- Close-leg `realized_pnl` is adjusted by `(modeled_fee − real_fee)`.
- `strategies.cash` is replayed from `initial_capital` on the corrected fee and PnL stream.
- A cash-replay divergence over $1 (usually a SIGHUP capital top-up) is a warning and blocks `--apply` unless `--reset-cash` is passed.
- Paper-mode HL strategies are skipped (no real order IDs). Manual strategies are included.
- Per-row skip reasons are reported: `missing_oid`, `no_fill_match`, `already_real_fee`. Rows already on the gross convention are skipped as `gross_convention_row` — they belong to the trade-ledger backfill below.

---

## Backfill Trade Ledger

Migrates legacy trade rows to the gross-PnL convention and trues fee, price, and PnL up to Hyperliquid `userFills`, so the shared-wallet ledger display path reads exchange-accurate values.

```bash
./go-trader backfill trade-ledger --all               # dry run
sudo systemctl stop go-trader
./go-trader backfill trade-ledger --all --apply
sudo systemctl start go-trader
```

- Two chronological passes per row. Pass one migrates legacy net rows to gross: the fee deducted at booking (the stored real fee, else the modeled taker fee) is stamped into `exchange_fee`, the close leg gets it added back to `realized_pnl`, and `fee_source` records provenance. Rows whose `fee_source` is `reconcile_adjustment` skip migration and the userFills true-up, but cash replay still includes them. Pass two gives every row whose order ID matches a userFills aggregate the real fee, the fill VWAP price, and the exchange gross closed PnL.
- Rows sharing one order ID (partial take-profit legs, flip close-and-open pairs, shared-coin aggregate fills) apportion the aggregate by quantity share: fee across ALL legs, closed PnL across close legs only.
- `strategies.cash` and `closed_positions` replay under net semantics, with the same `--reset-cash` divergence gate as the fee backfill.
- Funding rows are never rewritten and never touch cash.
- `--apply` resets every shared wallet's ledger drift baseline, so the next reconciled cycle re-anchors on the repaired ledger instead of alarming on the correction.
- Idempotent: a second run over the same fills reports zero changes.

---

## Trade Diagnostics

```bash
./go-trader diagnostics                    # all strategies
./go-trader diagnostics --strategy hl-btc-momentum
```

Every full close eagerly inserts a `trade_diagnostics` row at close time; a background worker fills in MFE, MAE, and capture ratio from hold-window OHLCV afterwards. That worker never blocks or alters a close — a failure just leaves those columns NULL and downgrades `metrics_status`. The report opens the state DB read-only, aggregates NET PnL per strategy through the trades join (so tiered take-profits and partial exits sum correctly across legs), splits by regime-at-open and direction, and prints sample-size-gated hypotheses with the exact backtest command to validate each one. Synthetic closes (`hl_sync_external`, `*_corrupt`, `*_dup_oid`) are excluded. `llm_verdict` shows per row when present, but only the LLM entry-analysis pipeline ever writes it.

---

## Backtesting

Run every backtest through `uv run --no-sync python`. Harness map: [`docs/backtesting-registry.md`](docs/backtesting-registry.md).

```bash
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode single|compare|multi|optimize
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --since 90

# Close evaluator — one close per strategy; --close-strategy takes a bare name or a JSON ref
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --close-strategy '{"name":"tiered_tp_atr","params":{"tp_tiers":[{"atr_multiple":1,"close_fraction":0.5},{"atr_multiple":2,"close_fraction":1.0}]}}'

# Backtest a live strategy verbatim (single mode only) — pulls its open + close refs from live config
uv run --no-sync python backtest/run_backtest.py --config scheduler/config.json --strategy hl-btc-momentum \
  --symbol BTC/USDT --timeframe 1h --mode single

# Regime gate — blocks entries outside the allowed labels; closes always execute
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --regime-enabled --regime-period 14 --regime-adx-threshold 20 --allowed-regimes trending_up trending_down

# Joint open × close-stack walk-forward co-optimization (backtest-only)
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --mode optimize --sweep-close --optimize-metric sharpe_ratio|total_return_pct|dd_adjusted_return

uv run --no-sync python backtest/backtest_options.py --underlying BTC --since 90 --capital 10000
uv run --no-sync python backtest/backtest_theta.py --underlying BTC --since 90 --capital 10000
```

- `--config` needs `config_version` 15 or newer, applies `user_defaults` by default, and `--defaults system` keeps the built-in baseline instead.
- Stop-versus-take-profit races default to `ohlc_walk`; `--intrabar-resolution bar_close` restores the legacy behavior.
- A backtest whose equity hits 0 prints a LIQUIDATED banner and floors return and Sharpe at −100%, so a deeper blowup can never rank above a shallower one.
- The backtester rejects HL-live-only mechanisms: `regime_window_divergence`, `tiered_tp_atr_live_regime_dynamic`, and an enabled `hedge` block.
- Research surfaces (`tune_live.py`, auto-suggest, regime promotion, the `/tuning` page) are **suggest-only**. They never write live defaults, config, or PRs.

---

## Reconfiguration

```bash
sudo systemctl kill -s HUP go-trader   # hot reload, no state loss
sudo systemctl restart go-trader       # full restart
```

Hot reload re-applies a safe subset: capital, drawdown, intervals, params, stop-loss fields (including percentage and ATR-multiple trailing), sizing leverage, theta harvest, `portfolio_risk` knobs and their `portfolio_risk.paper` overrides except `max_notional_usd` on either, summary cadence, per-strategy `allowed_regimes`, `paused`, `circuit_breaker` and its cooldowns, `notify_ratchet_triggers`, `allow_deprecated`, `llm_entry_analysis`, `hurst_gate`, `alert_throttle_interval`, `kill_switch_reset_dm_timeout`, `user_defaults`, `tuning.max_retained_runs`, and the Discord/Telegram **channel maps**. Per-strategy `regime_*_window` selectors, `replay_sharing` and `replay_source_id` reload only while flat.

**Restart-required — a SIGHUP is rejected outright:** the strategy roster; any `script`/`args`/`type`/`platform` field, the HTF filter, or kill-switch identity; `db_file`; `log_dir`; `status_port`; the status token; `auto_update`; `paper_db_file` and any strategy's effective `storage_strategy_id`; `risk_free_rate`; `leaderboard_post_time` and `leaderboard_summaries`; `tradingview_export`; `replay_log_path`; `market_feed`; `portfolio_risk.max_notional_usd` and `portfolio_risk.paper.max_notional_usd`; the whole `correlation` block; the global `regime` block (enabled, period, adx_threshold, windows); `discord.enabled`/`token`/`owner_id` and `telegram.enabled`/`bot_token`/`owner_chat_id`; and a shared-wallet pool↔allocated budgeting switch.

It also refuses when per-strategy exchange `leverage`, `direction`, `invert_signal`, HL `margin_mode`, or the regime timeframe changed while a position is open, and for every field in the blocked-while-open list under Post-Update Agent Protocol. It re-runs the HL peer-on-same-coin check for `margin_mode` and exchange `leverage` agreement and the single-trailing-stop-owner rule, and re-validates every `hedge` block. A rejection names every offending field at once; fall back to a restart.

Common changes:

- Regenerate config: `./go-trader init`, or the scripted `--json` form.
- Channels: edit `discord.channels` / `telegram.channels`; use `trade_alert_channels` to send fills somewhere other than the summaries.
- Token: `sudo systemctl edit go-trader`, add the environment override, restart.
- Add or remove strategies: edit the `strategies` array. Removed strategies are pruned from state.
- Risk: edit strategy `max_drawdown_pct`, portfolio `max_drawdown_pct`, `portfolio_risk.warn_threshold_pct`.
- Paper to live: change `--mode=paper` to `--mode=live`, add `--execute` where required, and configure the exchange credentials.

Changing `capital` does not reset cash or positions. For a full reset, remove `scheduler/state.db` (or that strategy's rows) and restart.

---

## Adjustable Settings

Global — key, default, notes:

| Key | Default | Notes |
| --- | --- | --- |
| `interval_seconds` | 300 | Check interval |
| `db_file` | `scheduler/state.db` | State DB. In the split layout it holds the live scope, process metadata, the live-only wallet/cash-flow tables and shared regime history. **Restart-required.** |
| `paper_db_file` | absent = single file | Optional second state file owning the paper scope's books, risk row, kill-switch events and correlation snapshot. Must resolve to a different physical file than `db_file`. **Restart-required.** |
| `storage_strategy_id` (per strategy) | `id` | The identifier this strategy owns inside its state file. Unique per file; the same value in both files is the supported alias. Lets a strategy `id` be renamed with no stored rewrite. **Restart-required.** |
| `auto_update` | `off` | `off` \| `daily` \| `heartbeat` |
| `status_port` | 8099 | Loopback only |
| `risk_free_rate` | 0.04 | Sharpe basis |
| `max_drawdown_pct` | 25 | Portfolio kill switch, per scope (live and paper latch independently) |
| `portfolio_risk.warn_threshold_pct` | 60 | Percent of the kill-switch limit |
| `portfolio_risk.max_notional_usd` | `0` = off | Gross notional cap. Over it, opens/adds/flips are held and manual open/add/limit-open refuse; closes, reductions and protection keep running. **Restart-required.** |
| `portfolio_risk.daily_max_loss_usd` | `0` = off | Cap on the day's aggregate PRE-FEE realized loss. Hold-only until UTC rollover; nothing is force-closed. Hot-reloadable even while tripped. Ignored inside `platforms.<name>.risk`. |
| `portfolio_risk.daily_max_loss_pct` | `0` = off | Same limit as a percent of Σ per-strategy `initial_capital`. With both arms set the lower resolved USD threshold wins; a zero-capital basis cannot evaluate and `/status` says so. |
| `portfolio_risk.max_same_direction_notional_usd` | `0` = off | Blocks a new same-direction open over the cap. Blocking only, direction-aware. Hot-reloadable. |
| `portfolio_risk.max_asset_concentration_pct` | `0` = off | Same blocking behavior scoped to one asset's share of exposure. Shares its exposure model with `correlation.*`. |
| `portfolio_risk.paper` | absent = inherit | Optional override block with the same fields, applied to the paper scope only. A zero or omitted field inherits the parent value; a nested `paper.paper` is rejected. `paper.max_notional_usd` is restart-required; the rest hot-reload. |
| `alert_throttle_interval` | 6h | Go duration. Coalesces repeat operator alerts. |
| `kill_switch_reset_dm_timeout` | empty = 6h | Go duration. How long the reset prompt waits. Independent of `alert_throttle_interval`. |
| `correlation.enabled`, `.max_concentration_pct`, `.max_same_direction_pct` | off, 60, 75 | Warnings to all active channels plus an owner DM; snapshot in `/status`. Restart-required. |
| `summary_frequency` | see Configure | Per-channel cadence |
| `regime.enabled`, `.period`, `.adx_threshold`, `.windows`, `.gate_on_failure`, `.transitions` | off, 14, 20, empty | Empty `windows` = one legacy horizon. Restart-required as a block. `transitions` is alerting-only — it never gates entries, mutates config, or touches positions. |
| `notify_tp_sl_fills` | enabled when nil | `false` stops owner DMs from reconciler-detected fills |
| `notify_ratchet_triggers` | enabled when nil | Owner DM when a ratchet tier clears and tightens the trail. The per-strategy field overrides it. |
| `market_feed` | `"rest"` | `"rest"` (default; an omitted field means this) keeps legacy per-check polling. `"websocket"` opens one Hyperliquid socket and hands Hyperliquid perps and `manual` checks a sealed market snapshot on stdin. Any other value fails `loadConfig`. **Restart-required.** § Hyperliquid Batched Signal Checks. |
| `atr_method` | `"simple"` | `"simple"` (legacy rolling mean, ≥100 rounding) or `"wilder"` (published RMA, never rounded). Governs the standard ATR surface only — entry-ATR stamping, live market ATR, the manual fetch, backtester injection, tuner simulate. Strategy-internal indicators and the regime classifier are untouched. |
| `default_stop_loss_atr_mult` | `1.0` | Applies to every HL perps strategy omitting all stop-owner fields, shared-coin peers included. `0` restores the `max_drawdown_pct` fallback fleet-wide. |
| `user_defaults.manual.{margin_usd,stop_loss_atr_mult,side,tp_tiers,trailing_stop_atr_mult_regime}` | see Manual Trading | Overrides the manual-open defaults. Order: CLI or strategy param → `user_defaults.manual` → constant. `stop_loss_atr_mult: 0` opts scalar manual out and the ratchet fallback ignores that 0. `tp_tiers: []` is rejected — omit the key to inherit. Hot-reloadable. |
| `user_defaults.close`, `user_defaults.regime_atr` | none | `user_defaults.close` injects `tp_tiers` into matching close refs that omit them; its `trailing_tp_ratchet_regime` entry may carry a coupled `trailing_stop_atr_mult_regime`. `user_defaults.regime_atr` supplies fleet-wide `stop_loss_atr_mult_regime` / `trailing_stop_atr_mult_regime` for standalone `use_defaults` owners. Three layers — system → user → strategy, explicit wins. Hot-reloadable. |
| `tuning.max_retained_runs` | `0` = keep all | Prunes oldest-first terminal research-run directories; never touches a queued or running row. Hot-reloadable. |
| `replay_log_path` | `""` = off | Shared SQLite path for live→paper decision replay. It must live outside every deploy tree, e.g. `/var/lib/go-trader/shared/replay.db`, which the template unit grants through `StateDirectory=go-trader/shared`. Required by `replay_sharing`. **Restart-required.** |

Per-strategy:

| Key | Scope | Notes |
| --- | --- | --- |
| `capital` | all | Starting capital reference |
| `max_drawdown_pct` | all | The strategy circuit breaker |
| `interval_seconds` | all | `0` uses the global; auto-accelerates inside the drawdown warn band |
| `circuit_breaker` | all but manual | `false` disables BOTH arms (drawdown and consecutive losses), live and paper; nil = enabled. It suppresses only NEW fires — a latched breaker or pending close still drains — and displayed drawdown still updates. One warning per suppressed breach, `cb=off` in the startup summary and `inspect`. Hot-reloadable even while open. |
| `cb_drawdown_cooldown_minutes`, `cb_loss_streak_threshold`, `cb_loss_streak_cooldown_minutes` | all but manual | Override the hardcoded breaker parameters; nil keeps 24h, 5 losses, 1h. Positive only; cooldowns ≤ 30 days, threshold ≤ 100. Hot-reloadable even while open, for new fires only — a latched expiry is untouched. |
| `paused` | all | `false`. Holds opens, adds and flips while closes, trailing stop, ratchet and protection sync keep running. Hot-reloadable always, including while open. Shows `⏸️ paused:` in Discord `/status`. |
| `allow_deprecated` | all | Acknowledges a research-deprecated strategy. Live: unset or false gives a startup DM, `true` silences it. Paper auto-suppresses unless set `false`. Warning surface only — never blocks trading. |
| `htf_filter` | all | Skips counter-trend signals. Restart-required. |
| `open_strategy` | all | `{name, params}`; otherwise the name comes from `args[0]` |
| `close_strategy` | all | The single exit ref `{name, params}`; nil = open-as-close. A legacy `close_strategies` array of length ≤1 still parses, length >1 is rejected. |
| `direction` | perps | `"long"` (default), `"short"` (opens shorts only), `"both"`. Hot-reloadable when flat. |
| `invert_signal` | HL perps, manual | `true` flips BUY↔SELL on every non-zero signal; HOLD is never flipped. Composes with `direction="short"`. Blocked while open. |
| `stop_loss_pct` | HL perps | Sole owner auto-derives from `max_drawdown_pct` (cap 50) when omitted; same-coin peers need one explicit positive owner. `0` opts out. |
| `stop_loss_margin_pct` | HL perps | Leverage-aware. `0` opts out. |
| `stop_loss_atr_mult` | HL perps | Trigger at `avg_cost ± mult × entry_atr`, armed once after open. `0` restores the `max_drawdown_pct` fallback. |
| `trailing_stop_pct` | HL perps | Distance from the high-water mark. Live and paper. Capped at 50%; `0` disables. |
| `trailing_stop_atr_mult` | HL perps | `mult × entry_atr / avg_cost` frozen at open. Live and paper. Arms the cycle after open, once ATR exists. |
| `stop_loss_atr_mult_regime`, `trailing_stop_atr_mult_regime` | HL perps | Resolve the ATR multiplier per the position's frozen regime label. `{"trend_regime": {"<label>": {"atr": N}, …}}` or `{"use_defaults": true}`. Need `regime.enabled`. Backtestable. |
| `trailing_stop_min_move_pct` | HL perps | Minimum trigger move before a cancel-and-replace. Default 0.5%. A ratchet tier tighten bypasses it once, same cycle. |

**Exactly one of the seven stop owners** may be positive on an HL perps strategy: `stop_loss_pct`, `stop_loss_margin_pct`, `stop_loss_atr_mult`, `stop_loss_atr_mult_regime`, `trailing_stop_pct`, `trailing_stop_atr_mult`, `trailing_stop_atr_mult_regime` — plus the unified per-regime close block, which owns the stop itself and rejects every strategy-level stop field. Omit all of them and `default_stop_loss_atr_mult` applies. A nil↔positive toggle or a scalar↔regime flip is blocked while a position is open.

| Key | Scope | Notes |
| --- | --- | --- |
| `sl_after` (on the close ref and/or per tier) | HL perps, manual | Post-take-profit stop move. Scalar modes: `"breakeven"`, `{atr_mult: N}` (signed), `{trail_from_here: {atr_mult: M}}`, `{trail_from_here: {tp_atr_fraction: F}}` where the trail is F × the firing tier's ATR multiple. Regime-aware shapes exist for each; composite labels follow `regime_atr_window`. Needs a fixed stop owner. Scalar↔regime or shape change blocked while open. The backtester has scalar parity and rejects the regime-aware shapes at init. |
| `leverage` | perps | Exchange margin and risk leverage, and the HL `update_leverage` call. Default 1×. Applied from flat. |
| `sizing_leverage` | perps | Notional multiplier (`cash × sizing_leverage`); defaults to `leverage`. |
| `margin_per_trade_usd` | perps, opt-in | `notional = min(margin_per_trade_usd, cash) × leverage`. Overrides `sizing_leverage`. In shared-wallet pool mode (2+ live HL/OKX perps where every member omits the capital fields) notional is `min(cap, account equity − deployed wallet margin) × leverage` with entry/mark reservation; a missing balance blocks opens but not closes. Allocated↔pool is flat-only and restart-required. |
| `risk_per_trade_pct` | HL perps, opt-in | `qty = (cash × pct/100) / stop_distance`, capped at `cash × leverage`. Bounds `(0, 10]`. Mutually exclusive with `sizing_leverage`, `margin_per_trade_usd`, `allow_scale_in`. Needs a stop owner resolvable at sizing time; regime-resolved and unified-close owners are rejected at load. **Fail-closed** — an unresolvable stop distance refuses the open rather than falling back to notional sizing. A risk↔notional mode switch is blocked while open. |
| `margin_mode` | HL perps | `isolated` (default) or `cross`. Applied from flat. |
| `allow_scale_in`, `scale_in` | HL perps, manual, opt-in | See Scale-in / pyramiding |
| `hedge` | HL perps, opt-in | `{enabled, symbol, side:"inverse", ratio, margin_mode, leverage}`. Auto-manages a leg on a different coin, mirrored from the primary's quantity by one per-cycle reconciler; the hedge leg has no independent stop, take-profit or close evaluator, and mark drift never re-trades. The hedge coin must be nobody's primary and no other strategy's hedge coin. Hedge PnL is recorded separately and excluded from the primary's lifetime trade and win/loss counts and from its loss streak. **Fail-closed** — a hedge failure on a cycle that added primary exposure unwinds that increment and sends a CRITICAL DM. Hot-reloadable only while flat; the backtester rejects an enabled block. |
| `hurst_gate` | opt-in | `{enabled, mode:"gate"\|"size", min, max, disarm_min, disarm_max, window_key, on_failure, size_floor}`. Sits ON TOP of `allowed_regimes`, which is unchanged. `mode=gate` holds position-increasing signals while disarmed; `mode=size` scales computed open size by `clamp(\|H-0.5\|/0.15, size_floor, 1.0)`, never above 1. Reads the Hurst metric from a composite regime window only — an ADX or missing window is rejected at load. `on_failure` inherits `regime.hurst_gate_on_failure` then `"open"`; fail-closed is flat-only. Hysteresis is keyed by a threshold hash, so editing a threshold resets it. Hot-reloadable always. **No thresholds ship** — calibration was inconclusive. Backtest through `--config`. |
| `allowed_regimes` | not options | Labels that allow an entry. Empty allows all. Needs `regime.enabled`. |
| `regime_gate_on_failure` | not options | `"open"` (default, legacy fail-open) or `"closed"`, which holds fresh opens only — management and closes always pass — while the regime store cannot produce a label. Overrides the global; empty inherits. Hot-reloadable always. `closed` with `allowed_regimes` and `regime.enabled=false` is rejected at load as a permanent block. |
| `regime_gate_window`, `regime_atr_window`, `regime_directional_window` | not options | Route the entry gate, regime-aware ATR and take-profits, and the directional policy to different horizons. Need a non-empty `regime.windows`; empty or `default` uses `regime.period`. Stamped labels persist on the position. Reload only while flat. |
| `regime_directional_policy` | HL perps | Per-regime `direction` plus `invert_signal` override. Needs `regime.enabled` and every canonical label. Resolves from the current regime while flat, from the position's frozen label while open. **Evidence-gated, DEFAULT-OFF** — it resolves to the base direction unless the `(asset, timeframe, classifier)` cell is certified in the shipped-empty artifact, so configuring it today is inert and logs a non-breaking warning. Certification is exact-match: a bare label never certifies its substates. Backtestable through `--config`. |
| `regime_window_divergence` | HL perps live | `{"short_window", "medium_window", "on_divergence": "trust_short"\|"trust_medium"\|"alert_only"}`. Overrides the direction when the two windows diverge (hard = bullish plus bearish, soft = one ranging), applied after the directional policy. Needs both windows in `regime.windows`. Visible in `/status`, DMs and a dashboard badge. The backtester rejects it. |
| `regime_profile_allocation` | HL perps | Two open-param profiles of one strategy; a slow long-window label picks the active one, switched hysteretically (`confirm_bars`, warn below 12) and only while flat — it freezes at open. `{window, profiles{label→name, all labels}, param_sets{name→overrides, exactly 2}, confirm_bars≥1, initial_profile}`. Needs `regime.enabled`. Persisted. Backtestable through `--config`. |
| `atr_method` | not options | Per-strategy override of the global; empty inherits. |
| `replay_sharing` | HL perps | `"none"` (default) or `"live_mirror"`. On live it records exposure-changing decisions to `replay_log_path`; on paper it suppresses its own position-increasing signals and replays the rows of the live source it names — the **same strategy `id`** by default, or the id in `replay_source_id` — opens at live quantity and VWAP, full closes at the paper mark under reason `replay_live_mirror`. Never forwarded to the check scripts. Hot-reloadable flat-only. Book-drift skips warn and DM (throttled) but are never retried; a close-while-flat is INFO only. |
| `replay_source_id` | HL perps, paper, opt-in | The id of the live strategy this paper mirror replays. Empty (default) keeps the pre-#1510 rule: the mirror reads the rows of the live twin that shares its own id, in another process. Set it when the live source and the paper mirror run in one process, where the two ids must differ. Requires `replay_sharing="live_mirror"` on both sides; the named strategy must be a live HL perps strategy in the same config on the same symbol and timeframe, and one live source takes one mirror. The daemon evaluates the source before the mirror in the same cycle, so a live decision reaches the paper book the cycle it is made. Changing it resets the mirror watermark and logs a WARN. Hot-reloadable flat-only. |
| `notify_ratchet_triggers` | HL perps, manual | Overrides the global; nil inherits. Notification-only, so it hot-reloads even while open. |
| `llm_entry_analysis` | all, opt-in | `{enabled, model, max_debate_rounds, timeout_s, notify_dm, notify_channel}`, default off; model default `claude-sonnet-5`, 1 round (0–3), 120s timeout (max 600), `notify_dm` on, `notify_channel` off, both-off legal. After a FRESH open — never an add, flip or manual — an async pipeline posts a short digest to the trade-alert DM and stamps `bullish`/`bearish`/`mixed` into `trade_diagnostics.llm_verdict` at close. **Advisory only**: an error or timeout posts nothing and has zero trade impact. Runs on its own job lane, never the shared Python semaphore. Needs `ANTHROPIC_API_KEY`. Hot-reloadable even while open. |
| `theta_harvest.*` | options | Early exit |
| `close_strategy.params.tp_tiers` with ref `tiered_tp_atr` / `tiered_tp_atr_live` | HL perps | On-chain take-profit tiers, a list of `{atr_multiple, close_fraction}` (cumulative). Default `[{1.5×,0.4},{3×,0.8},{5×,1.0}]`; the final tier is coerced to 1.0 and a non-numeric tier is rejected. **Live:** configuring tiers auto-suppresses the in-process evaluator so an on-chain limit fill cannot race it. **Paper:** never suppressed. |
| ref `tiered_tp_atr_regime` / `tiered_tp_atr_live_regime` | HL perps | Per-regime tiers; the `_live_` variant re-resolves each tick. Backtestable. |
| `close_strategy.params.tp_tiers` with ref `trailing_tp_ratchet` / `trailing_tp_ratchet_regime` | HL perps, manual | Shape and rules in Configure. `use_defaults: true` or an omitted `tp_tiers` takes the system ladder (scalar: trails 1.5×/1.5×/0.8× at 2×/2.5×/3× ATR; regime: per quality group). Tier-table changes blocked while open. |

Discord and Telegram: `enabled`; `channels` (the platform/type map for summaries and, as a fallback, trade alerts); `trade_alert_channels` (an override for fills only, same key scheme, hot-reloadable); `dm_channels`; `owner_id` (prefer `DISCORD_OWNER_ID`); `ephemeral_replies`; `report_repo` and `report_github_token`.

---

## Strategy Reference

**Never enumerate strategies from memory.** The registries are the source of truth and they change:

```bash
uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json
uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json
uv run --no-sync python shared_strategies/options/strategies.py --list-json
```

`/go-trader-closing-strategies` catalogs every registered close evaluator — name, description, platforms, config params — and marks the ones `user_defaults.close` overrides.

Research-deprecated strategies are hidden from `--list-json` and `go-trader init` but stay registered, so an explicit `args[0]` or config ref still loads them. A live strategy on the deprecated roster gets one owner DM at startup unless `allow_deprecated: true`; paper strategies auto-suppress it. A strategy marked backtest-only fails closed on a live config.

What an operator needs to choose one:

- **Direction.** A bidirectional strategy needs `"direction": "both"`, or `"short"` to run it as a dedicated bear-only instrument. Short-only strategies emit sell signals exclusively and are pre-registered bidirectional, so they need one of those two values. Pair a short strategy with `allowed_regimes: ["trending_down"]` for clean entry gating.
- **Validation status.** `--list-json` returns only the ID and the description, and the description carries the research verdict — read it. The deprecation tag itself surfaces as `edge=deprecated_m5` in the startup summary and in `./go-trader inspect`, plus a one-time owner DM for a live strategy. Treat anything tagged deprecated, or described as not out-of-sample validated, as paper-trade-first.
- **Entry versus exit.** Several entry strategies ship entries only and expect the exit to come from config — pair them with a close evaluator and a stop rather than expecting a built-in exit.

Platform conventions:

| Platform | ID prefix | Type / script |
| --- | --- | --- |
| BinanceUS spot | none | `spot`, `shared_scripts/check_strategy.py` |
| Hyperliquid perps | `hl-` | `perps`, `shared_scripts/check_hyperliquid.py` |
| Hyperliquid manual | `hl-` | `manual`, no script or interval; driven by the `manual-*` CLI; may share a coin with HL perps peers |
| TopStep futures | `ts-` | `futures`, `shared_scripts/check_topstep.py` |
| Robinhood | `rh-` | spot through `check_robinhood.py`, options through `check_options.py --platform=robinhood` |
| OKX | `okx-` | `check_okx.py` (spot and perps), `check_options.py --platform=okx` for options |
| Deribit options | `deribit-` | `check_options.py --platform=deribit` |
| IBKR options | `ibkr-` | `check_options.py --platform=ibkr` |
| Luno | `luno-` | Luno adapter and scripts |

ID conventions: `ts-{strategy}-{symbol}`, `rh-{strategy_short}-{asset_or_symbol}`, `okx-{strategy_short}-{asset}` for spot and options, `okx-{strategy_short}-{asset}-perp` for perps. Options short names: `vol_mean_reversion → vol`, `momentum_options → momentum`, `protective_puts → puts`, `covered_calls → calls`, `wheel`, `butterfly`.

Example entries:

```json
{"id":"momentum-btc","type":"spot","script":"shared_scripts/check_strategy.py","args":["momentum","BTC/USDT","1h"],"capital":1000,"max_drawdown_pct":60,"interval_seconds":300}
{"id":"ts-momentum-es","type":"futures","platform":"topstep","script":"shared_scripts/check_topstep.py","args":["momentum","ES","1h","--mode=paper"],"capital":1000,"max_drawdown_pct":5,"interval_seconds":3600}
{"id":"rh-ccall-spy","type":"options","platform":"robinhood","script":"shared_scripts/check_options.py","args":["covered_calls","SPY","--platform=robinhood"],"capital":5000,"max_drawdown_pct":10,"interval_seconds":14400,"theta_harvest":{"enabled":true,"profit_target_pct":60,"stop_loss_pct":200,"min_dte_close":3}}
{"id":"okx-sma-btc-perp","type":"perps","platform":"okx","script":"shared_scripts/check_okx.py","args":["sma_crossover","BTC","1h","--mode=paper","--inst-type=swap"],"capital":1000,"max_drawdown_pct":5,"interval_seconds":3600}
```

**Peers on one Hyperliquid coin** share a single on-chain position. They must agree on `margin_mode` and exchange `leverage`; `sizing_leverage` may differ. Each peer places its own per-strategy-sized reduce-only protection, so several peers may own fixed-ATR, margin, or trailing stops at once. Peers that omit every stop field fall back to `default_stop_loss_atr_mult`. A per-strategy circuit-breaker drain skips the on-chain close when peers share the coin, so the exchange leg stays open until another path flattens it. Sub-account isolation is the only route to full per-strategy independence.

---

## Add Or Change Strategies

Open registry: `shared_strategies/open/registry.py`. Close registry: `shared_strategies/close/registry.py`.

New spot or futures strategy:

1. Add the implementation and its `@register(...)` in `shared_strategies/open/registry.py`.
2. Set `platforms=(…)` correctly; use variants for platform-specific defaults.
3. Append the name to `PLATFORM_ORDER`.
4. Add the short name and default entries in `scheduler/init.go`.
5. Add a param grid to `DEFAULT_PARAM_RANGES` in `backtest/optimizer.py`.
6. Run the registry and optimizer tests.

For a close evaluator, add an `evaluate(position, market, params)` implementation under `shared_strategies/close/` and register it in `close/registry.py`.

Do not edit `shared_strategies/open/{spot,futures}/strategies.py` — they are thin shims.

Before refactoring a registry or shim, snapshot discovery and diff it afterwards unless the change is meant to alter discovery:

```bash
uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json > /tmp/spot.json
uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json > /tmp/futures.json
```

---

## Custom Platform Integration

Gather first: platform name and ID prefix; products (spot, perps, futures, options); API docs URL or `ccxt` coverage; credential environment variable names; fees; assets and strategies; paper and live requirements.

1. `platforms/<name>/__init__.py`
2. `platforms/<name>/adapter.py` — exactly one class whose name ends in `ExchangeAdapter`
3. Implement public adapter methods only; check scripts must never touch private attributes
4. `shared_scripts/check_<name>.py`, only if an existing entry script does not fit
5. ID-prefix inference in `scheduler/config.go`
6. Fee dispatch in `scheduler/fees.go`
7. Executor wiring, only if a new live execution path is needed
8. Config examples
9. The init wizard and `generateConfig`, if the platform is user-selectable
10. `pyproject.toml` for any new SDK dependency, then `uv sync`
11. Tests, including pure-helper tests for the Go logic

Options platforms also need volatility, expiry, strike, and premium helpers, `CalculateOptionFee`, and an `OptionPlatforms` entry. Reference adapters: spot `binanceus`, perps `hyperliquid`, futures `topstep`, options `deribit`.

```bash
uv run --no-sync python -m py_compile platforms/<name>/adapter.py
uv run --no-sync python -m py_compile shared_scripts/check_<name>.py
/opt/homebrew/bin/go -C scheduler build .
./go-trader --config scheduler/config.json --once
```

---

## Storage Ownership

Set `paper_db_file` and the scheduler runs both modes in one process against two state files. `db_file` is the **primary** file; `paper_db_file` is the **paper** file. Omit `paper_db_file` and every behaviour below collapses to the single-file layout that has always shipped.

**Who owns what**

| Table | Owner | Notes |
| --- | --- | --- |
| `app_state` | primary | The paper file's row is read once at boot and its summary/leaderboard stamps are unioned in with primary precedence; it is never written again. |
| `strategies`, `positions`, `option_positions`, `trades`, `closed_positions`, `closed_option_positions`, `trade_diagnostics`, `pending_manual_actions` | the strategy's scope | Identifiers are translated in both directions at the SQL boundary. |
| `portfolio_risk`, `kill_switch_events`, `correlation_snapshot` | the row's explicit scope | A legacy unscoped row is placed using the owning file's validated scope. |
| `wallet_ledger_state`, `wallet_transfers`, `cashflow_journal`, `cashflow_journal_state`, `pending_limit_orders` | primary, live-only | A write naming a paper-scope strategy is refused. |
| `regime_window_history`, `regime_window_transitions`, `regime_reversal_alerts` | primary for new writes | A paper file's pre-split history is read with its source named and is never pruned or marked. |
| `decisions` (replay log) | unchanged | Its own `replay_log_path` file. |

**Identity translation.** A strategy's process identifier (`id`) and its stored identifier (`storage_strategy_id`, default `id`) are separate namespaces. Every strategy identifier is translated inside the per-file handle, so no caller can skip it. Values that stay in the process namespace: `position_id`, `replay_mirror_watermark_source`, `hedge_for` (a symbol) and `replay_source_id`. A stored row that maps to no configured strategy in its file is an **orphan**: it is reported, never keyed into the roster, and dropped by that file's next full save. Historical trade, closed-position and diagnostics rows with no current strategy keep their stored identifier and carry their source file; nothing assigns them a scope by identifier suffix.

**Acknowledgement.** A drained manual action is deleted **by row id inside the transaction that persists its effect**. A failed action keeps its row. A high-water mark is never used, so one file's acknowledgement can never delete another file's row.

**Persistence hold.** Each scope carries its own save-failure counter. Any unacknowledged failure holds position-increasing signals for that scope at all six regime-gated dispatch sites and refuses `manual-open` / `manual-add`; closes, trailing stops, ratchet, protection sync and hedge management keep running. Three consecutive failures skip that scope's strategies for the cycle, as before. A successful save clears the hold. In the single-file layout both counters move together, so the outcome equals the previous whole-cycle skip.

**Combined reads.** History, counts, statistics, exports and diagnostics query every required file, translate identifiers, merge, then order and page globally by `(timestamp DESC, scope ASC, rowid DESC)`. A file that cannot be read fails the whole read; no page or map is ever returned with a scope missing.

**Inspection.** `go-trader storage-inspect [--config <path>] [--json] [--require-idle]` prints, per file, the canonical path, the scopes it owns, any held lock and its pid, every mapped strategy with its position count, orphans (flagged when they hold positions), the pending-action count and the risk rows with their latch. It opens each file read-only, tolerates a pre-#1509 schema, and never migrates, moves or deletes a row. It exits 1 on a rejection: a book in the wrong file, a risk row whose scope the file does not own, an ambiguous legacy row, or a duplicate stored identifier. `--require-idle` also rejects a file owned by a running process. Startup runs the same check and exits 80 on a rejection, before the first migrating open. A mixed single-file deployment that already holds paper books cannot be split automatically — resolve it by hand or stay single-file.

**Backup and restore.** Stop the service, then copy **both** files with their `-wal` and `-shm` sidecars; restore them together. `scripts/update.sh` excludes both paths from its rsync. There is no transaction across the two files: a crash stops both modes, and separate files give record separation without fault isolation inside one process.

---

## Portfolio Kill Switch And Latch Ownership

**The latch is partitioned by mode.** One scheduler holds two portfolio scopes, `live` and `paper`, decided only by `--mode=live` in the strategy args. Each scope keeps its own peak, drawdown, latch, events, daily-loss ledger, notional total, exposure total and correlation model, and each is evaluated once per cycle over its own strategies. A paper drawdown can never latch live, and a live drawdown can never latch paper. Only a scope with at least one configured strategy is evaluated, so a single-mode deployment behaves exactly as before, with the scope name added to the operator surfaces. On upgrade, an existing unscoped `portfolio_risk` row moves into `live` when the config holds any live strategy, else into `paper`; the move is written back on the first boot and is a no-op afterwards. With `paper_db_file` set, each file's unscoped row is placed from that file's own owned scope (§ Storage Ownership).

A live latch runs the exchange close plan over the live roster only. A paper latch closes paper books virtually at mark, sends no exchange order, and posts to the `-paper` channels; it never auto-resets without an owner. Because paper has no wallet fetch, its equity reading is always trusted, so a paper scope never shows a substituted reading or a deferred latch.

The portfolio kill switch latches on drawdown and halts new trading until it is reset. **Exactly one measurement owns the latch each cycle** — there is no tie-break:

- **Equity drawdown owns it** when the equity guard is armed: a portfolio total is available AND the recorded peak is above zero. The kill switch then trips on equity drawdown over `max_drawdown_pct`.
- **Perps margin drawdown owns it** only when the equity guard is not armed.

When equity owns the latch, a margin drawdown over the limit is a throttled WARNING, not a trip: per-strategy circuit breakers own margin protection. The warning is coalesced by `alert_throttle_interval` and re-fires early on a band entry or a 1-point drawdown escalation.

**Untrusted readings.** A cycle's total is trusted only when the pooled equity read is complete AND no portfolio-value fallback and no stale risk balance were used. On an untrusted cycle the latch stays with equity, but:

- The peak never ratchets up, so a bad total cannot inflate the peak.
- The equity drawdown reading is FLOORED at the last reading, clamped to `max_drawdown_pct` so the floor alone can never latch.
- The substitution is persisted and flagged as `drawdown_reading_substituted`. **Label it on every operator surface** — the number is carried forward, not measured this cycle. The warning DM marks it as "carried forward; balance substituted this cycle, does not reconcile with the figures below".

**An untrusted over-limit reading defers the latch; it never vetoes it.** The first such cycle records the timestamp and a `latch_deferred` event naming the untrusted basis. While the run is unbroken and under 15 minutes old, the full-book latch is held and per-strategy circuit breakers are the active protection. Past 15 minutes the latch escalates and the reason names the untrusted basis and the deferral. A trusted reading landing first clears the timer. The deferral is loud: the log line is `[CRITICAL]`, and the warning DM bypasses the throttle for as long as the deferral stands. `/go-trader-circuit-breakers` shows the deferral, when it started, and when it escalates.

Reset is owner-DM only and clears one scope. With no DM owner configured, the live scope auto-resets only after a confirmed-flat close, and the paper scope auto-resets right after its virtual close. The reset DM names the scope in its header, and carries the drawdown reason, the trader-instance label, the HL wallet address (live prompts only), and a protection-gap warning when the close plan has not confirmed flat. Reply `reset` while one scope is latched. While both are latched, one prompt covers both scopes: reply `reset live` or `reset paper`; a bare `reset` is refused and names both, and the scope still latched is re-prompted next cycle. One single-flight prompt waits at a time, so one owner reply is never consumed by the wrong waiter. `kill_switch_reset_dm_timeout` sets how long that prompt waits (empty = 6h).

**Auto-reset.** Once every platform is confirmed flat the next cycle clears virtual state and resumes trading, posting `Virtual state cleared. Kill switch auto-reset; trading will resume next cycle.` Auto-reset also needs every resting limit order resolved (see below) and no operator-required venue outstanding.

**Resting limit orders are cancelled before the flatten.** The kill-switch close cancels every `pending_limit_orders` row first, keyed on the ROW rather than a position, under a 60-second deadline. Each row goes cancel → status check → delete; a row whose outcome cannot be resolved clears the confirmed-flat flag and blocks auto-reset without an owner. A row carrying an unadopted fill is never auto-deleted. Cancellation and adoption are separate eligibilities: a row belonging to a strategy that is absent from the config, or is no longer `type=manual`, is still cancelled even though it can no longer be adopted. Ordinary limit-order reconciliation is never gated on kill-switch state.

**Multi-strategy HL coins.** Kill-switch fills split by virtual quantity at snapshot time, and the split fails closed. If HL flattens to about zero, a sole-stop trigger fires with the residual matching non-owner peers, or a single take-profit tier fills externally, the next cycle closes the affected virtual peers automatically; an ambiguous gap stays a gap.

Warn-band messages repeat while the drawdown sits inside `portfolio_risk.warn_threshold_pct`. Silence them by resolving the drawdown or changing the threshold.

Drain and live-execution failure alerts: `journalctl -u go-trader -n 100 | grep "liveExec\|drain"`.

---

## Hyperliquid Liquidation Guard

A stop-loss trigger placed past the exchange liquidation price can never fill: Hyperliquid force-closes first, at liquidation-engine pricing. The guard makes that geometry unreachable.

**Boot check.** For every live isolated-margin HL perps strategy, a stop distance at or beyond the bankruptcy distance (`100 / leverage` percent) fails the load with a message naming the field, the bound, and the leverage. It covers `stop_loss_pct`, the price-percentage derived from `stop_loss_margin_pct`, and the `max_drawdown_pct` fallback. Cross-margin strategies are exempt. Run the same audit across the fleet before an update with `bash scripts/check-hl-stop-bankruptcy-bound.sh`, which reads raw JSON, mirrors the loader's stop-owner resolution, and exits 1 on a finding.

**Runtime clamp.** The guard reads the per-coin liquidation price and matches it against the coin's net side; a side mismatch means unknown and the coin is skipped. It **clamps, never refuses to arm**, and it tightens **one way only**: a trigger past liquidation moves to 0.5% inside it, and a replacement is submitted only when it is strictly tighter than what is resting. The liquidation price is never persisted, so 0 always means unknown. Geometry that cannot be clamped is refused rather than guessed. Before the dispatch of each cycle an audit tightens every stop owner; when nothing is due, an off-cycle pass runs at half the shortest live HL interval, floored at 60 seconds, over live HL perps and `manual`.

**Refusals that protect peers.** The audit will not touch a coin whose recorded size across live strategies exceeds the on-chain snapshot — moving a reduce-only trigger there could close a peer's real position. It reports that as `not reconciled`: reconcile the coin and the audit heals it on the next pass.

**Alerts.** Each outcome DMs the owner and posts to the channels, deduplicated per strategy and symbol and re-sent on an action change or after `alert_throttle_interval`. The action names what happened:

| Action | Meaning | What to do |
| --- | --- | --- |
| `clamped` | The trigger was tightened to just inside liquidation | Nothing. Lower the leverage or the stop distance so the configured geometry is reachable |
| `replace deferred` | The replacement could not be placed; the ORIGINAL stop is still resting | Nothing. The scheduler retries next cycle |
| `protection lost` | The old trigger was cancelled and the replacement did NOT rest — **the position has no exchange-side stop** | Act now. The message names when the scheduler re-arms |
| `re-armed` | A position with no stop got one | Nothing |
| `re-arm failed` | The re-arm did not rest — **no exchange-side stop** | Act now |
| `not reconciled` | Recorded size does not match the on-chain snapshot, so the audit did not touch the order | Reconcile the coin |
| `SL filled` / `exited` | The original stop already fired, or the replacement filled at submit | Nothing; the reconciler books the close |
| `outcome unknown` / `placement unknown` | The result could not be read; an order may be resting untracked | Verify the order book on Hyperliquid. Recorded state is kept and nothing is re-placed |

An unreadable outcome always keeps the recorded state. A cancel that leaves nothing resting gets exactly one in-cycle retry, and only when the placement was positively rejected — classification comes from what actually rests, never from error text.

---

## Model-Only Close Reconciliation

When a circuit-breaker close books a row from the model rather than a real fill, the row carries `fee_source='reconcile_adjustment'` and no exchange order ID. Once the real Hyperliquid fill lands, the scheduler corrects that row **in place** — quantity, fill VWAP price, exchange fee, gross realized PnL — instead of writing a second close. Partial fills accumulate against the closed basis over successive cycles, and the row stops accepting corrections after 48 hours.

If the coin goes flat on-chain while the row still covers only part of the close, the residual was finished by another mechanism such as a resting stop. That raises an owner alert, at most once a day per strategy and symbol, saying that the trade row, `closed_positions`, and cash are inconsistent. Fix it with `backfill trade-ledger` or reconcile by hand.

---

## Hyperliquid Batched Signal Checks

Due Hyperliquid perps strategies that share market data run ONE batched signal check instead of one process per strategy. Same decisions, fewer subprocesses, faster cycles. Strategies batch together when they share data platform, symbol, timeframe, OHLCV limit, and ATR method.

Operator-visible behavior:

- Set `GO_TRADER_HL_BATCH=0` (or `off`/`false`/`no`) to disable batching entirely and go back to one process per strategy.
- A batch that fails on the shared market-data step makes **every member spawn its own check the same cycle**, so a batch failure never blanks a close, stop, ratchet, protection sync, or hedge. One alert is raised per group, not per strategy.
- Three consecutive shared-state failures on one group revert it to per-strategy checks and retry the batch every 10 cycles. A success clears the state and re-alerts as recovered.
- A slot whose configuration changed between snapshot and dispatch spawns its own check instead of trusting the batch.

The log line names the group as `platform/symbol/timeframe/limit=N/atr=M`.

### Market feed (`market_feed: websocket`)

Root `market_feed` chooses where Hyperliquid candles come from. `rest` (the default, and what an omitted field means) keeps today's polling and scheduling untouched. `websocket` opens ONE Hyperliquid socket, keeps the candle history and mid prices in the Go process, and hands every covered check a sealed snapshot on standard input. The mode is logged at startup as `Market feed: rest (legacy polling)` or `Market feed: websocket (Hyperliquid perps + manual)`, and changing it is restart-required.

**Scope.** Hyperliquid `perps` and `manual` strategies only. Every other platform keeps legacy polling under both values.

**Six REST consumers stay outside the feed** and outside the zero-call criterion: the manual-open ATR fetch (`--fetch-atr`), the LLM entry-review candles, the UI candle chart, the status-server mid fetch, the liquidation-guard off-cycle mid fetch, and OKX perps checks that fetch their own candles.

**Scheduling.** In-scope strategies become due on epoch-aligned deadlines (`floor(now / interval) * interval`), so live and paper twins on the same cadence share one evaluation identifier and one snapshot no matter how long each check takes. A missed deadline is never replayed: the next cycle consumes only the newest one. Strategies on other platforms keep last-run scheduling. Cadences that differ (a drawdown-accelerated strategy, say) get their own identifier, and the cycle log says so.

**Freshness.** A key is ready once it holds at least 30 bars (`coverage_short` is reported when the venue's history is shorter than the lookback). It is stale when its newest bar closed more than two intervals plus 60s ago, or when a connected socket has sent nothing for that key in `max(2 × interval, 5m)`. Mids expire after 15s. A reconnect gets a 30s grace before silence counts. A sealed snapshot older than `max(60s, interval / 2)` at dispatch holds entries.

**Outage behavior.** A key that is not ready and cannot be recovered yields no candle frame. The strategy still evaluates: signal 0, close fraction 0, price from the freshest verified mid, and a `degraded` reason in the log plus one throttled owner alert per key. Trailing stops, ratchets, protection sync, hedge sync and reconciliation keep running on verified inputs; candle-dependent closes report degraded rather than guessing. Entries are held at both in-scope dispatch sites. With no verified mark either, the strategy is skipped with a CRITICAL line. The operator pause state is never written.

**Never a silent private fetch.** With the feed on, a malformed, stale, incomplete or mismatched payload is an explicit error. Python sets the adapter aside for candle, higher-timeframe and funding reads whenever a market payload is present.

**Counting.** Steady-state evaluations make zero candle REST calls. Bootstrap, reconnect repair and stale recovery are counted separately and printed each cycle:

```
[feed] snapshot=300s/1788592500/1 keys=1 ready=1 stale=0 rest{bootstrap,repair,recovery}=1,0,0 steady_candle_rest=0
```

`/status` carries a `market_feed` block with the mode, connection state, generation, last snapshot identifier and per-key readiness.

---

## Operator-Required Circuit Breakers

Some venues have no safe automated close path:

| Platform | Type | Pending key |
| --- | --- | --- |
| OKX | spot | `okx_spot` |
| Robinhood | options | `robinhood_options` |

When one triggers, the scheduler enqueues `operator_required: true` and emits a CRITICAL warning every cycle until you intervene.

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

Response: open the venue UI, flatten the listed positions, confirm through `/status`, then let the scheduler clear the pending entry on the next circuit-breaker reset — or reset the portfolio kill switch by owner DM if trading must resume sooner.

This is not the portfolio kill switch. Operator-required is per-strategy and affects only the strategy that breached drawdown.

---

## Cash Reconcile Latch

A live spot fill that overshoots virtual cash is always booked, because the venue already filled it. The strategy then latches `CashReconcileRequired`, raises a CRITICAL alert, and blocks further live buys. Closes keep running. Clear it with `/go-trader-clear-cash-reconcile <strategy>` only after the books match the venue — the command drops the buy block and never invents or adjusts cash.

---

## Implementation Patterns

Full coding constraints live in [CLAUDE.md](CLAUDE.md) § Key Patterns. Notes that bite most often:

- A new trade-recording path must populate `Trade.PositionID`, or rely on the recorder's lookup against the strategy's positions, so partial closes collapse into one round trip.
- A new summary-posting path must thread the last-post map and call the shared cadence helper.
- Category-summary row labels use a fixed-width label helper; assert the exact text in tests.
- A new side-effecting subprocess wrapper goes through the side-effect runner, never the plain runner.
- Hedge PnL is recorded through the hedge recorder, never the ordinary trade recorder.

Audits:

```bash
grep -n "mu\.\(R\)\?Lock\(\)\|mu\.\(R\)\?Unlock\(\)" scheduler/main.go
grep -n "liveExecFailed" scheduler/main.go
```

---

## Subsystem Mechanism Reference

Per-subsystem mechanism notes for coding agents. `CLAUDE.md` keeps only the guardrail for each file; the mechanism behind it lives here. Grouped by `scheduler/` file.

### Execution and fill confirmation (`executor.go`, `shutdown.go`)

- `runPythonSideEffect` is the runner for every subprocess that can place, cancel, or modify an order; `runPython` is read-only. Graceful shutdown drains side-effecting subprocesses for at most `shutdownDrainCap=15s` and then SIGKILLs; state save, notifier flush, and DB close run afterwards through deferred LIFO. `TimeoutStopSec=20` in the units.
- `confirmHyperliquidExecuteFill` covers open, close, scale-in, hedge, manual, and UI paths. A response without a confirmed fill returns `"exchange returned no confirmed fill"` and books nothing. `hyperliquidExecuteSucceededCancelOIDs` retains the confirmed cancel OIDs so protection reconciliation can tell a cancelled trigger from a lost one.
- Bidirectional perps: a short entry registers in `bidirectionalPerpsStrategies`; flip sizing uses `perpsLiveOrderSize`.

### Loopback UI and tuning (`server.go`, `ui_*.go`, `static/ui/*`, `ui_tuning.go`)

- `applyStrategyConfigPatch` needs `config_version>=13`. `/tuning` (`static/ui/tuning.html`) is read-and-launch: it re-reads `/api/strategies/<id>/config` on every poll.
- `/api/tuning/runs` is a suggest-only lane. `tuning.max_retained_runs` prunes terminal run directories. `POST /api/tuning/apply` is refused on drift against the schema-v2 `promotion_baseline` and applies through `mutateConfigRoot` as an exact replace.

### Config and close defaults (`config.go`, `config_migration.go`, `close_defaults.go`)

- v19 renames the per-regime stop fields to `*_atr_mult_regime`; legacy-key presence gates the boot rewrite. The seven HL stop owners are mutually exclusive and all-omitted resolves to `DefaultStopLossATRMult=1.0`. A single `*StrategyRef` is accepted, `close_strategy` canonical.
- `portfolio_risk.paper` is an optional override block with the parent's fields. `scopeRiskConfig(cfg, scope)` merges non-zero override fields over a clone of the parent (zero = inherit). `paper.max_notional_usd` is restart-required like the parent; the other override fields hot-reload.
- Close defaults resolve system → user → strategy. A reserved `regime_atr` section serves standalone `*_atr_mult_regime` `use_defaults` owners. `user_defaults.close["trailing_tp_ratchet_regime"]` may carry `trailing_stop_atr_mult_regime`; `applyUserCloseDefaultRatchetRegimeTrails` applies it inside `loadConfig` before the scalar ATR-stop default so the scalar default never shadows it.

### Portfolio scope and state (`portfolio_scope.go`, `state.go`, `db.go`)

- `hyperliquidModeFromArgs` is a wrapper over `PortfolioScope`. `measureScopeCycleRisk` and `applyScopeCycleRisk` own the per-scope cycle read; `dueStrategiesNotLatched` filters the due set per scope.
- `portfolio_risk` is keyed by `scope` (rebuild migration `migratePortfolioRiskScopeColumns`); `kill_switch_events` and `correlation_snapshot` carry `scope`. Legacy unscoped rows load under `scopeUnassigned`; `placeLegacyPortfolioRisk` puts each file's row into the scope that file owns and the placement is saved at once, so it is idempotent. In a split layout with no live strategy the primary file's unscoped row is rejected instead of guessed.
- `StateStore` (`state_store.go`) owns the physical handles and the immutable identity map, routes every write to the owning file and combines every cross-scope read. `LoadStateWithStore` composes `loadProcessMeta` and `loadScopeBooks` per file, so paper books load with no process-metadata row in the primary. `SaveAll` runs one transaction per file and returns an error per scope; a failed file leaves its scope in memory for retry and cannot duplicate the other file's committed effects. Test hook `storeCommitHook` injects a per-file commit failure. § Storage Ownership.
- `LoadState` bounds per-strategy trades in SQL (`ORDER BY timestamp DESC, rowid DESC LIMIT maxTradeHistory`, index `idx_trades_strategy_timestamp`). `ValidatePerpsDirectionConfig` runs at startup; `CheckStatePresence` is bypassed with `GO_TRADER_ALLOW_MISSING_STATE=1`.

### Risk, latch, and the portfolio gates (`risk.go`, `strategy_interval.go`, `daily_loss.go`, `exposure_cap.go`, `notional_cap.go`)

- `collectPerpsMarkSymbols` feeds `type=manual` positions at live mids. A one-shot `PortfolioRisk.PeakValue` migration runs on first load.
- Latch ownership per cycle: equity drawdown when `equityGuardArmed`, else margin drawdown, with no tie-break. When equity owns the latch, margin drawdown over the limit is a throttled WARN. With `equityTrusted` false the latch stays equity-side, the peak ratchet is skipped, and drawdown is floored at the last reading (`DrawdownReadingSubstituted`). An untrusted over-limit reading defers until `untrustedEquityLatchDeferral` (15m), then latches loudly. Per-scope single-flight flags guard the reset prompt; the prompt names the scope and accepts `reset` for one latched scope, `reset live`/`reset paper` when both are latched. A paper latch calls `forceClosePaperScopePositions` (virtual close at mark, no exchange call), guarded once by `KillSwitchCloseApplied`. Per-position margin protection is the per-strategy circuit breaker (`circuit_breaker:false` opt-out). Full operator behavior: § Portfolio Kill Switch And Latch Ownership.
- Daily loss limit: `portfolio_risk.daily_max_loss_usd`/`daily_max_loss_pct`, 0 = off; both set → the lower resolved USD wins; the pct basis is the sum of strategy `initial_capital` per scope. The gate shape to copy for any new `portfolio_risk` gate: RLock evaluation per scope, `pausedBlocksSignal` holds, `manualStateView` refusals, `clonePortfolioRiskConfig` hot-reload.
- Exposure cap: `portfolio_risk.max_same_direction_notional_usd`/`max_asset_concentration_pct`, 0 = off; `exposureCapBlocksSignal` decides; TopStep futures are ungated. `computeAssetDeltas` in `correlation.go` is the one exposure model shared with `ComputeCorrelation`. Hot-reloadable via SIGHUP.
- Notional cap: `portfolio_risk.max_notional_usd`, 0 = off. `notionalCapSkipsStrategyCycle` is always false: closes, SL, and TP maintenance keep running; manual open, add, and limit-open refuse. Restart-required.
- Kill-switch fill attribution on shared HL coins goes through `hyperliquidKillSwitchFillShare`, which fails closed when the split cannot be determined. The reset prompt is single-flight via an `atomic.Bool`; the deferral timestamp is `UntrustedOverLimitSince`.

### Live-to-paper replay (`replay_log.go`, `replay_mirror.go`)

Enabled by `replay_log_path` plus per-strategy `replay_sharing="live_mirror"`. Paper suppresses its own entries and replays the live decisions. The source id comes from `replayMirrorSourceID` (`replay_source_id`, else the strategy's own id); `orderReplaySourcesBeforeMirrors` runs an in-process source before its mirror in the same cycle. The watermark is keyed on the paper strategy plus `ReplayMirrorWatermarkSource`; a source change resets it with a WARN. Book drift raises WARN plus DM; a close while flat is INFO.

### Batched HL checks and fills (`hl_batch.go`, `hyperliquid_fills.go`, `hyperliquid_balance.go`)

- Batching is a pure partition on `hlBatchKey` into one `check_hyperliquid.py --batch-check`; the fingerprint is re-checked at dispatch. Three strikes revert a group to per-strategy checks with a batch retry every 10 cycles. Operator view: § Hyperliquid Batched Signal Checks.

### Market feed (`market_feed*.go`, `hyperliquid_candles.go`, `hyperliquid_funding.go`)

- `marketFeedOwner` owns one websocket, its own `feedMu`, and a bar ring per `marketFeedKey{Host, Namespace, Symbol, Timeframe}` sized `required + 50`. It never takes the scheduler state lock; alerts leave through a channel the cycle drains outside `mu`. A race test and a source guard hold that line.
- `deriveFeedRequirements(cfg)` builds the key set from every in-scope consumer: signal frames, higher-timeframe filter frames (`hlFeedHTFMap` mirrors Python `_HTF_MAP` through `shared_tools/testdata/htf_map.json`), regime timeframe overrides, mid coins and funding needs. Each key stores the highest lookback any consumer asks for; `snapshot.frameFor(key, required)` slices it per consumer, so legacy row counts are preserved. `ApplyGeneration` bootstraps new keys and publishes a generation only once every required key is ready or has failed explicitly.
- `hlFetchCandleHistory` mirrors the Python adapter's widening loop, and `hlCandleRowFromRaw` mirrors its row conversion including the exchange close-timestamp preference; one golden fixture proves both converters agree.
- `sealCycleMarketSnapshot` runs at most one REST recovery per stale key per cycle, then freezes a deep copy. Later socket updates belong to a later snapshot. The payload rides stdin: the batch envelope becomes `v:2` with a `market` object; individual and regime-bundle checks get `--market-stdin` and the same envelope. Go caps the payload at 8 MiB and treats a serialization or size failure as a degraded dispatch, never a private fetch.
- The fill resolver is built outside `mu.Lock`; a resolver failure falls back to the modeled fee. Reconcile paths treat unconfirmed SL fills as gaps, never as books.
- `reconcileHyperliquidAccountPositions` sends public trade alerts for reconciliation closes via `sendTradeAlertRows` in the deferred unlock path. `hyperliquidPublicTradeAlertRows` drops hedge-leg rows so a hedge close alerts only its owner DM. A sole-owner reconciled SL close also queues a `ProtectionFillAlert`.

### Pause, regime, ratchet, hedge, Hurst (`pause.go`, `regime*.go`, `post_tp_sl.go`, `trailing_tp_ratchet.go`, `hedge.go`, `hurst_gate.go`)

- `StrategyConfig.Paused` (`"paused"`) runs a full manage-only cycle and hot-reloads at any time, including while open.
- Regime store failure displays `regime=-`; the entry empty-label policy comes from `resolveRegimeGateOnFailure` (`"open"`|`"closed"`). `regime_atr.go` treats the v15 `atr_multiple` as canonical. The regime label is display-only; regime transitions are alerting-only.
- The ratchet sets SL through `trailing_stop_atr_mult`/`trailing_stop_atr_mult_regime`. A same-cycle tier tighten replaces the resting SL and bypasses `TrailingStopMinMovePct`. The open DM shows the ratchet or trail block; it is suppressed on scale-in and on a non-default `regime_atr_window`.
- Hedge legs run through one reconciler (`hedgeTargetDecision` then `runHedgeSync`); the collision matrix between hedge and owner positions is load-bearing.
- Hurst gate: sits on top of the label gate; `resolveHurstGateOnFailure` fails closed flat-only. Size multiplier `clamp(|H-0.5|/0.15, floor, 1.0)`; hysteresis lives in `strategies.hurst_gate_state`. Hot-reloadable while open. Backtest counterpart `backtest/hurst_gate.py`.

### LLM review, scale-in, manual (`llm_entry_analysis.go`, `llm_review.py`, `scale_in.go`, `manual*.go`)

- The LLM verdict is advisory; it is written only to `trade_diagnostics.llm_verdict`.
- Scale-in freezes the stop geometry on `RiskAnchorPrice` instead of the blended `AvgCost`; HL perps plus `manual`; backtested.
- Manual actions run under the kill switch and circuit breaker. `force-close` is live HL perps only.

### Liquidation guard and protection (`hyperliquid_liquidation_guard.go`, `hyperliquid_open_trailing.go`, `hyperliquid_protection.go`)

- `hlLiquidationPx` is a NET per-coin map read via `hlLiquidationPxForSide` against `hlNetSideByCoin`. Healing runs through the trailing `trailingReplacePolicy.liquidationPx` or the static/regime `buildHyperliquidProtectionPlan`, strictly tighter only. `runHyperliquidLiquidationAudit` tightens every owner; `hlLiquidationClampReplace` is tri-state (`protection lost` / re-arm / refuse over-virtual-net). One in-cycle retry on a positively rejected cancel-with-nothing-resting; classification comes from what rests. The off-cycle pass runs at `liquidationAuditIntervalSeconds`, floored at 60s. Preflight: `scripts/check-hl-stop-bankruptcy-bound.sh`. `recordPositionOpen` runs after the deferred-open execute leg. Operator view: § Hyperliquid Liquidation Guard.
- On-chain TP suppression nils `CloseStrategy` for live tiered-TP strategies; paper never places on-chain TPs.

### Probes, diagnostics, alerts, commands

- `version_probe.go`/`probe_cmd.go`/`exit_codes.go`: every unique check script runs with `--probe-only` at startup; `probeFailureScriptMissing` detects `"can't open file"`; a failure logs, DMs the owner, and exits 78 (`EX_CONFIG`).
- `trade_diagnostics*.go`: eager insert in `recordClosedPosition`; MFE/MAE computed async outside `mu`.
- `agent_info.go`: read-only dump.
- `failure_alerts.go`/`script_failure_alerts.go`: primary alert at 3 strikes; transient 429/5xx/timeout stays WARN until 15 strikes or 75 minutes.
- `discord_commands.go`/`discord_mutating_commands.go`: `/clear-cash-reconcile` mutating, `/closing-strategies` read-only.
- `missing_mark_alerts.go`: throttled DM per `(strategy_id, symbol)`. `hl_reconcile_gap_alerts.go`: alerting only. `portfolio_warning.go`, `circuit_breaker_alert.go`: alert routing.
- `model_only_reconcile.go`: § Model-Only Close Reconciliation. `portfolio.go`: `CashReconcileRequired` (§ Cash Reconcile Latch). `kill_switch_close.go` and `*_close.go`: `type=manual` HL positions join the flatten via `hlKillSwitchAll`.

### Shared wallet, cashflow, limit orders (`shared_wallet*.go`, `cashflow_journal.go`, `kill_switch_limit_orders.go`, `orphan_limit_cancel_alerts.go`, `limit_fill_exposure.go`)

- Shared-wallet drift tolerance is $0.01 over 2 cycles. Pool sizing comes from account equity minus deployed margin; switching a strategy between allocated and pool budgeting needs a flat book and a restart.
- Cashflow journal: HL total-drift is live; OKX and TopStep run in shadow.
- Kill-switch limit orders: each row goes cancel → `--limit-status` → delete under a 60s pre-flatten deadline; an unresolved row clears `OnChainConfirmedFlat` and blocks `CanAutoResetWithoutOwner`. Operator view: § Portfolio Kill Switch And Latch Ownership.
- Orphan cancel lane: rows in `cancel_requested` or expired that fail `killSwitchLimitOrderAdoptionBlock`; roster from `killSwitchLimitOrderRoster` plus `collectKillSwitchLimitOrderCandidates`. Severity-gated throttle; `operator_required_since` backs off the poll. `applyLimitExposureOperatorRequired` sets the marker on `unbacked`, leaves it on `unreadable`, clears otherwise; the marker is the sole gate for `manual-clear-limit-row <oid> --flattened`.
- The orphan cancel lane is `cancelOrphanedLimitOrder`.
- Limit fill exposure: `hlLiveExposureReader` polls rows, decides per coin, finalizes; `snapshotNewerThan` is the sole reader; `applyCoinLimitFills` aggregates per coin; `classifyLimitFillLiveExposure` requires same-direction and contained. DM severity is gated by outcome.

### Python side (`shared_scripts/`, `platforms/`, `shared_tools/`, `shared_strategies/`, `backtest/`)

- `check_hyperliquid.py` splits into `build_shared_signal_state` and `evaluate_signal_slot` (on `shared["df"].copy()`); single mode and `--batch-check` both run that pair. A shared-state failure raises `SharedSignalStateError` → one `error_scope="shared_state"` sentinel; a slot exception stays in its slot.
- HL adapter caches in `/tmp`, lazy `_ensure_exchange`, sparse indices via `_normalize_spot_meta`. The SDK's `asset_to_sz_decimals` keys by integer asset index; resolve via `name_to_asset(symbol)` (fallback `coin_to_asset`); a direct symbol lookup is a legacy test-mock-only fallback.
- `shared_tools/atr.py` shims `shared_strategies/open/indicators_core.py`; `atr_method` resolves via `resolveATRMethod`. `indicators_core.py` holds ATR/RSI plus `hurst_exponent` (DFA, live SSoT) and research-only `hurst_rescaled_range`. Options strategies live in `options/strategies.py`.
- Strategy DSL: config params sit under runtime; `HTFFilter` is not available on options or `delta_neutral_funding`. M5 `deprecated_m5` holds 32 names; live use DMs unless `allow_deprecated:true`; paper auto-suppresses via `AllowDeprecatedEffective()`.
- Backtest files: `backtester.py`, `optimizer.py`, `run_backtest.py`, `backtest_{options,theta,pairs}.py`, `parity_diff.py`. `--config <path> --strategy <id>` reads the single `close_strategy` and applies `user_defaults` by default (`--defaults system` keeps the built-in baseline). `--intrabar-resolution bar_close` is the legacy race resolution. Regime: `--config` threads `allowed_regimes` (the CLI flag is rejected); composite via `regime.windows`; the open name falls back to `args[0]`. `regime_directional_policy` is backtestable behind its flag; scalar `sl_after` and `*_atr_mult_regime` stop dicts are backtestable. Liquidation floor: a sticky equity floor at 0 from the first bust; blown legs report `±LIQUIDATED_METRIC_FLOOR`. `tune_live.py` writes SCHEMA_VERSION=2 `promotion_baseline`.
- Close evaluators: `avwap_stop` is a virtual exit only and is absent from `isTieredTPATRCloseName` and `closeStrategiesSuppressedByOnChainProtection`; `atr_stop` and `avwap_stop` follow the same `atr_source` rule as `tiered_tp_atr_live`, which recomputes from `market_ctx["atr"]` (`atr_source` `live`|`entry`). Backtest regime gating blocks entries when `bar_regime ∉ allowed_regimes`.

### Notifications and channels

Channels are `spot`, `options`, `<platform>`, `<platform>-paper`. `resolveChannelKey(platform, type, isLive)` prefers a non-empty `<platform>-paper` key for paper strategies, so summaries, leaderboards, and Sharpe groups split by mode only when that key is configured. `SendToScopeChannels(scope, msg)` targets `-paper` channels for paper and falls back to `SendToAllChannels`.

### Build, deploy, and test mechanics

- `scripts/update.sh --restart` is atomic: preflight → `pull --ff-only` (or `--rsync-from <src>`) → `uv sync` → build → probe → binary swap (previous kept as `.prev`) → restart and verify → rollback on timeout. `--all --restart` discovers deployments via `discover_deployment_dirs_from_systemd`. A one-off build is `go build -ldflags "-X main.Version=$(git describe --tags --always --dirty=-mod)" -o go-trader .`; config-only reload is `kill -HUP $(pgrep go-trader)` and Python picks the change up next cycle.
- Post-update classification diffs `<running>..HEAD` per § Post-Update Agent Protocol.
- Test placement: Go `_test.go` beside the file; Python `test_*.py`. Pure helpers to extract from subprocess wrappers include `perpsLiveOrderSize`, `*OrderSkipReason`, `parseXxxCloseOutput`, and the Sharpe computation. `shared_scripts/test_*.py` sits outside pytest `testpaths`; registry and sys.path tests import through `importlib.util.spec_from_file_location`.

### The `@claude` GitHub workflow (`.github/workflows/claude.yml`)

- `classify` plus review and implement callers of `richkuo/rk-skills/.../claude-run.yml@main`; only the call-site `permissions:` differ (review has no `id-token: write`).
- git and gh run on the Claude GitHub App token. Review comments post as `github-actions[bot]`; implement posts as `claude[bot]`. Patch steps key on `RUN_ID`.
- Mode routing: `pull_request_review*` → `review`; issues, non-PR comments, docs-sync, and release → `implement`; otherwise the keyword after `@claude` (untrusted or fork → `review`; trusted → `fix-pr`).
- Comment patching uses `patch_claude_comment.sh` and `compose_claude_comment.py`, staged from `rk-skills` `templates/claude-workflow/scripts/` into `$RUNNER_TEMP` each run. `.github/scripts/test_workflow_logic.py` executes the real `run:` blocks. Concurrency `group: claude-<N>`, `cancel-in-progress: false`. `timeout-minutes: 90`. `issues: types: [opened]` only.
- The CLAUDE.md revision step must `git commit` and `git push origin HEAD`, then diff `HEAD -- CLAUDE.md` against `origin/main` to catch a silent revert.
- Operator lookup for the latest bot review: `gh api repos/richkuo/go-trader/issues/<N>/comments --jq '[.[] | select(.user.login=="claude[bot]" or .user.login=="github-actions[bot]")] | last | .body'`.

---

## Tests

```bash
/opt/homebrew/bin/go -C scheduler test ./...
uv run --no-sync python -m pytest
uv run --no-sync python shared_strategies/open/test_registry_parity.py
```

If the Go cache needs an explicit writable path: `env GOCACHE=/tmp/go-build-cache /opt/homebrew/bin/go -C scheduler test ./...`.

Go CI must not depend on a Python runtime, so a test for a subprocess-based live helper extracts the pure parser or decision helper rather than invoking Python.
