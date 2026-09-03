# go-trader Project Context

## Environment
- Go 1.26.2; full binary path if not on PATH (`/opt/homebrew/bin/go`).
- Python via `uv run --no-sync python` for dev/backtest/manual CLI; Go subprocess (scheduler) calls `.venv/bin/python3` directly — deterministic after `uv sync`, no systemd PATH config. Worktrees: `uv sync` once per checkout.
- **systemd:** `go-trader.service`, `systemd/go-trader@.service` use `ProtectSystem=strict`; no `PATH`/`UV_CACHE_DIR` injection. `/opt/go-trader/.env` (or per-instance) sets `DISCORD_BOT_TOKEN` + secrets. config lives OUT of the deploy tree at `/var/lib/go-trader[/<instance>]/config.json` (`StateDirectory=go-trader[/%i]`); `ExecStart … --config <that path>`; `WorkingDirectory` stays the checkout; `scheduler/config.json` is a transition symlink (`scripts/migrate-config-out-of-tree.sh`). template unit also grants `StateDirectory=go-trader/shared` for the live→paper `replay_log_path`.

## Setup
- `uv sync`; copy `scheduler/config.example.json` → `scheduler/config.json`, fill API keys. **(deployments):** stop the service, run `scripts/migrate-config-out-of-tree.sh` once (refuses while the daemon is live), then point `ExecStart --config` + `StateDirectory` at it.

## Decision Priorities
- **Always pursue the absolute best technical solution — full stop.** Cost, compute, resources, time-to-implement, effort, manpower, tests, and code volume are NOT design constraints and must never narrow the option space: choose the best solution as if these were unlimited, then update/rewrite tests after the feature is done. These govern *quality*, not *scope*; don't override the branch+PR workflow, "verify issue claims against code", or destructive-action safety rules.
- **Never give time, duration, or effort estimates** ("2–4 days", "low effort", headcount/manpower) in responses, PRs, issues, or commits. Describe complexity only in terms of scope and risk.

## Repo Structure
`scheduler/` is one Go `package main`. **Guardrails only below.**
- `executor.go`/`shutdown.go` — **New side-effecting wrapper → `runPythonSideEffect`, NEVER `runPython`.** `confirmHyperliquidExecuteFill` gates every live HL book path before state mutation — requires `Execution.Fill` with finite `AvgPx>0` and `TotalSz>0`; else `"exchange returned no confirmed fill"` and no book (open/close/scale-in/hedge/manual/UI). `hyperliquidExecuteSucceededCancelOIDs` retains confirmed cancel OIDs for protection reconciliation. `check_hyperliquid.py` execute exits 1 on unconfirmed fill.
- `server.go`/`ui_*.go`/`static/ui/*` — **Lock order `mu → strategiesMu`** (reusing `mu` deadlocks SIGHUP). **Loopback only**; `applyStrategyConfigPatch` needs `config_version>=13`. `/tuning` (`ui_server.go`, `static/ui/tuning.html`) is read-and-launch for tuning runs; re-reads `/api/strategies/<id>/config` every poll; never writes config.
- `ui_tuning.go` — `/api/tuning/runs` suggest-only lane via `spawnPythonProcessWithEnv` (NEVER `runPython*`); `tuning.max_retained_runs` prunes terminal dirs; `POST /api/tuning/apply` sole promotion (drift-refused vs schema-v2 `promotion_baseline`, `mutateConfigRoot` exact replace).
- `config.go`/`config_migration.go` — `CurrentConfigVersion=19` (v19 renames per-regime stop fields to `*_atr_mult_regime`; legacy-key presence gates boot rewrite). `MinSupportedConfigVersion=13`. Seven mutually-exclusive HL stop fields (all-omitted→`DefaultStopLossATRMult=1.0`); single `*StrategyRef` (`close_strategy` canonical); unknown-key guard; `strategyUsesTieredTPATRClose(sc)` gates on-chain TPs, NOT `len(tiers)>0`; `CircuitBreaker *bool` read ONLY via `CircuitBreaker*` accessors.
- `close_defaults.go` — three-layer resolution (system→user→strategy); explicit `tp_tiers` wins; reserved `regime_atr` section for standalone `*_atr_mult_regime` `use_defaults` owners. `user_defaults.close["trailing_tp_ratchet_regime"]` may carry `trailing_stop_atr_mult_regime` — `applyUserCloseDefaultRatchetRegimeTrails` runs in `loadConfig` **before** the scalar ATR-stop default.
- `state.go`/`db.go` — SQLite-only (`modernc.org/sqlite`); idempotent migrations. `LoadState` bounds per-strategy trades in SQL (`ORDER BY timestamp DESC, rowid DESC LIMIT maxTradeHistory` + `idx_trades_strategy_timestamp`). `ValidatePerpsDirectionConfig` startup check; `CheckStatePresence` (`GO_TRADER_ALLOW_MISSING_STATE=1`).
- `risk.go`/`strategy_interval.go` — `CheckRisk` skips `manual`. corrupt position (qty≤0 OR avgCost≤0) → zero-PnL `*_corrupt` close leg, cash untouched. `collectPerpsMarkSymbols` feeds `type=manual` at live mids; one-shot `PortfolioRisk.PeakValue` migration. **portfolio latch:** ONE owner/cycle — equity DD when `equityGuardArmed`, else margin DD (no tie-break); margin DD over limit = throttled WARN. `equityTrusted` false: latch stays equity-side, peak ratchet skipped, DD floored at last reading (`DrawdownReadingSubstituted` — label everywhere). Untrusted over-limit = DEFERRAL until `untrustedEquityLatchDeferral` (15m), then loud latch. `ResetPortfolioKillSwitchManual` sole DM reset. Per-position margin via per-strategy CB (`circuit_breaker:false` opt-out).
- `daily_loss.go` — portfolio-wide daily loss limit (`portfolio_risk.daily_max_loss_usd`/`daily_max_loss_pct`, 0=off; both set → lower resolved USD wins; pct basis = Σ strategy `initial_capital`). **Hold-only, UNLATCHED pure read**; measures PRE-FEE realized PnL; never force-closes. **New `portfolio_risk` gate → copy this shape** (RLock eval, `pausedBlocksSignal` holds, `manualStateView` refusals, `clonePortfolioRiskConfig` hot-reload).
- `exposure_cap.go` — same-direction exposure cap (`portfolio_risk.max_same_direction_notional_usd`/`max_asset_concentration_pct`, 0=off). **Blocking-only + direction-aware** (`exposureCapBlocksSignal`); TS futures site ungated. **Single exposure model** — `computeAssetDeltas` (correlation.go) shared with `ComputeCorrelation`. Hot-reloadable via SIGHUP (unlike `max_notional_usd`).
- `notional_cap.go` — portfolio gross notional cap (`portfolio_risk.max_notional_usd`, 0=off). **Hold-only via `pausedBlocksSignal`** — never skip the strategy cycle (`notionalCapSkipsStrategyCycle` always false). Closes/SL/TP maintenance keep running; manual open/add/limit-open refuse. Restart-required.
- `replay_log.go`/`replay_mirror.go` — **DEFAULT-OFF** live→paper replay via `replay_log_path` + per-strategy `replay_sharing="live_mirror"` (HL perps, flat-only hot-reload). Paper suppresses own entries, replays live decisions. Source id via `replayMirrorSourceID` (`replay_source_id` else own id; one mirror per source); `orderReplaySourcesBeforeMirrors` runs an in-process source before its mirror, same cycle. Watermark keyed on the paper strategy + `ReplayMirrorWatermarkSource` — source change resets it with a WARN. Book-drift WARN+DM; close-while-flat INFO.
- `hl_batch.go` — shared `hlBatchKey` → one `check_hyperliquid.py --batch-check`. Pure partition; fingerprint re-checked at dispatch. Shared-state failure ⇒ per-strategy fallback same cycle (never blank close/SL/ratchet/protection/hedge); 3 strikes ⇒ per-strategy retry every 10 cycles. `GO_TRADER_HL_BATCH=0` disables.
- `hyperliquid_fills.go` — fill resolver built **outside `mu.Lock`** (failure→modeled fee); `HLFillLookup.Px`=VWAP; reconcile paths treat unconfirmed SL fills as gaps, not books.
- `pause.go` — `StrategyConfig.Paused` (`"paused"`); NOT a `dueStrategies` skip, full manage-only cycle. `pausedBlocksSignal(...)` holds position-increasing signals at all 6 regime-gated dispatch sites; closes/trailing SL/ratchet/protection sync pass through. Hot-reloadable always incl. while open.
- Regime cluster (`regime*.go`, `post_tp_sl.go`):
 - Stamp on all 5 execute dispatches; store failure → display `regime=-`; ENTRY empty-label policy via `resolveRegimeGateOnFailure` (`"open"`|`"closed"`).
 - `regime_atr.go`: strip `sl_after` before ATR parse; v15 `atr_multiple` canonical. `regime_unified.go` owns SL. Directional policy DEFAULT-OFF + exact-match certs (no family fallback). Profile allocation flat-only. display-only label. Dynamic/divergence HL-live-only. Transitions alerting-only.
- `trailing_tp_ratchet.go` — **NO on-chain TP.** SL via `trailing_stop_atr_mult`/`trailing_stop_atr_mult_regime`; HL perps+`manual`; hot-reload blocked while open. Same-cycle tier tighten replaces resting SL (bypass `TrailingStopMinMovePct`). Open DM shows ratchet/trail block; suppressed on scale-in and non-default `regime_atr_window`.
- `hedge.go` — correlated hedge legs, **HL perps only**. ONE reconciler: `hedgeTargetDecision`+`runHedgeSync`; ownership `Position.HedgeFor` ONLY. Collision matrix load-bearing. Hedge PnL → `RecordHedgeTradeResult`, never `RecordTradeResult`. Fail-closed unwind + CRITICAL DM. Hot-reload blocked while open; backtester rejects.
- `hurst_gate.go` — Hurst entry gate + persistence-scaled sizing, **DEFAULT-OFF**. No shipped threshold; `config.example.json` clean. ON TOP of label gate; holds position-increasing signals only (`pausedBlocksSignal`, 6 sites). `resolveHurstGateOnFailure` fail-closed FLAT-ONLY. `metrics["hurst"]` ONLY from composite classifier. Size mult `clamp(|H-0.5|/0.15, floor, 1.0)`. Hysteresis in `strategies.hurst_gate_state`. Hot-reloadable while open. Backtest `backtest/hurst_gate.py`; keep in `run_backtest.py` `stop_keys`.
- `llm_entry_analysis.go`/`llm_review.py` — advisory-only; `spawnPythonProcess` NEVER `runPython*`; verdict → `trade_diagnostics.llm_verdict` (sole writer).
- `scale_in.go` — freeze geometry via `RiskAnchorPrice` (not blended AvgCost); HL perps+manual; backtested.
- `manual.go`/`manual_limit.go`/`manual_sl.go` — kill-switch+CB; SL edits queue `PendingManualAction` (NEVER direct UPDATE); `force-close` live HL perps only.
- `hyperliquid_liquidation_guard.go` — SL vs HL liquidation price; `hlLiquidationPx` NET per-coin map, read via `hlLiquidationPxForSide` vs `hlNetSideByCoin`. **CLAMP, never refuse to arm; ONE-WAY TIGHTEN** at 0.5% buffer; 0 = unknown, never persisted. Healing via trailing `trailingReplacePolicy.liquidationPx` or static/regime `buildHyperliquidProtectionPlan` (strictly tighter only; unclampable REFUSES). `runHyperliquidLiquidationAudit` tightens every owner; `hlLiquidationClampReplace` tri-state (`protection lost` / re-arm / refuse over-virtual-net). One in-cycle retry on positively rejected cancel-with-nothing-resting; unreadable outcome keeps state (classify by what RESTS). Off-cycle pass (`liquidationAuditIntervalSeconds`, floor 60s). Boot `validateHLStopWithinBankruptcyBound` mirrors `LoadConfig` stop-owner resolution; preflight `scripts/check-hl-stop-bankruptcy-bound.sh`. `recordPositionOpen` after deferred-open execute leg.
- `hyperliquid_open_trailing.go` — arm initial SL inline at open. `hyperliquid_protection.go` — reduce-only TP/SL; on-chain TP = `strategyUsesTieredTPATRClose` AND live (paper never).
- `version_probe.go`/`probe_cmd.go`/`exit_codes.go` — new runtime CLI flag → both probe argvs; `ExitProbeFailure=78`.
- `trade_diagnostics*.go` — EAGER insert in `recordClosedPosition`; MFE/MAE async outside `mu`; never write `llm_verdict`.
- `agent_info.go` — read-only dump; `--bootstrap-md`→`AGENTS.generated.md` (NEVER `AGENTS.md`).
- `failure_alerts.go`/`script_failure_alerts.go` — wire notifier on new `run*Check`; primary at 3; transient 429/5xx/timeout → WARN until 15 strikes or 75m.
- `discord_commands.go`/`discord_mutating_commands.go`/… — new mutating cmd → `opsCommandNames`+`slashCommands()`+dispatch; `/clear-cash-reconcile`; `/closing-strategies` read-only.
- `shared_wallet.go`/`shared_wallet_reconcile.go`/… — PRE-FEE `realized_pnl`; net via `tradeNetPnL*`; drift $0.01/2-cycle. **pool budgeting:** 2+ live HL/OKX perps omit capital fields + positive `margin_per_trade_usd` each; sizing from account equity − deployed margin; allocated↔pool flat-only/restart.
- `cashflow_journal.go`/… — HL total-drift LIVE; OKX/TopStep shadow; OUTSIDE `mu`.
- `hl_reconcile_gap_alerts.go` — alerting only.
- Also: `portfolio_warning.go`/`circuit_breaker_alert.go`; `portfolio.go` (`CashReconcileRequired`); `model_only_reconcile.go` (in-place CB model-only close correction once real HL fill lands); `kill_switch_close.go`+`*_close.go` (`type=manual` HL joins flatten via `hlKillSwitchAll`); `missing_mark_alerts.go` (throttled DM `(strategy_id, symbol)`); marks/deribit/init/sharpe/correlation/leaderboard/notifier/telegram/updater/pricer/tradingview_export/config_reload/backfill_hl_fees/hyperliquid_trailing_stop.
- `kill_switch_limit_orders.go` — cancel every `pending_limit_orders` row BEFORE flatten (keyed on ROW); cancel→`--limit-status`→delete; unresolved clears `OnChainConfirmedFlat`, blocks `CanAutoResetWithoutOwner`; **never gate `reconcilePendingLimitOrders` on kill-switch**; cancel≠adoption; never auto-delete unadopted fill; 60s pre-flatten deadline.
- `orphan_limit_cancel_alerts.go`+`cancelOrphanedLimitOrder` — cancel-only lane above adoption for `cancel_requested`/expired rows failing `killSwitchLimitOrderAdoptionBlock`; roster from `killSwitchLimitOrderRoster`+`collectKillSwitchLimitOrderCandidates`; status-FIRST finalize; books NO fill; `orphanLimitCancelState` SSoT (off-book fill = UNTRACKED POSITION); severity-gated throttle; `operator_required_since` backs off poll; `applyLimitExposureOperatorRequired` sets marker on `unbacked`, leaves on `unreadable`, clears otherwise; marker sole gate for `manual-clear-limit-row <oid> --flattened`.
- `limit_fill_exposure.go` — book limit fill ONLY after live exposure confirms (`hlLiveExposureReader`: poll rows→decide per coin→finalize; `snapshotNewerThan` sole reader; `applyCoinLimitFills` per-coin aggregate, never per-row greedy; `classifyLimitFillLiveExposure` fail-closed same-direction+contained; `unreadable`/`unbacked` refuse book AND block row delete; severity-gated DM).

Other dirs (guardrails):
- `shared_scripts/` — per-platform `check_*.py` + `check_{strategy,options,price,regime}.py`, tuner/simulate. All accept `--regime-payload-json`; probed at startup. `check_hyperliquid.py` splits into `build_shared_signal_state` + `evaluate_signal_slot` (on `shared["df"].copy()`); single mode and `--batch-check` both run that pair. Shared-state failure raises `SharedSignalStateError` → one `error_scope="shared_state"` sentinel; a slot exception stays in its slot.
- `platforms/<name>/adapter.py` — one `*ExchangeAdapter`/file. HL caches in `/tmp`; lazy `_ensure_exchange`; sparse indices via `_normalize_spot_meta`.
- `shared_tools/` — `regime.py`; `funding_fetcher.py` (`merge_asof` backward; DISJOINT `funding_coverage`); `atr.py` shims `shared_strategies/open/indicators_core.py`; `atr_method` via `resolveATRMethod` (`regime.py` pinned `simple`).
- `shared_strategies/` — open SSoT `open/registry.py`; **`open/{spot,futures}/strategies.py` are shims — do not edit.** Close `close/registry.py`; options `options/strategies.py`. `open/indicators_core.py`: ATR/RSI + Hurst — `hurst_exponent` (DFA, live SSoT) and research-only `hurst_rescaled_range`.
- `backtest/` — `backtester.py`,`optimizer.py`,`run_backtest.py`,`backtest_{options,theta,pairs}.py`,`parity_diff.py`. See § Backtest.

## Key Patterns
- Run git from repo root. Prefer `go -C scheduler build .` over `cd scheduler &&`.
- **New platform (8):** adapter+`__init__`, `check_<name>.py`, `executor.go`, `config.go`, `fees.go`, `main.go` dispatch, `init.go`+`generateConfig`, `pyproject.toml`. Options also need vol/expiry/strike/premium helpers + `CalculateOptionFee` + `OptionPlatforms`.
- Adapters via `importlib`, class `endswith("ExchangeAdapter")`; check scripts use public methods only.
- **Close registry import:** `from close_registry_loader import evaluate, list_strategies, build_close_registry` — never bare `import registry`.
- Subprocess contract: JSON on stdout even on error; exit 1 on error; Go parses regardless of code.
- **State locking:** `mu sync.RWMutex`. 6-phase: RLock → Lock(CheckRisk) → no-lock(subprocess) → Lock(execute) → marks → RLock(status).
- Platform dispatch by `s.Platform` (never ID prefix). Prefix map `hl-`/`ibkr-`/`deribit-`/`ts-`/`rh-`/`okx-`/`luno-`, else BinanceUS.
- Types `spot`/`options`/`perps`/`futures`/`manual`. Perps paper→`ExecuteSpotSignalWithFillFee`, live→`RunHyperliquidExecute`; futures→`ExecuteFuturesSignalWithFillFee`. Manual auto-fills hold script; close default regime→ratchet else `tiered_tp_atr_live`+SL@2×ATR.
- **Open/close split:** single `CloseStrategy` owns exit; close before open; partial-close preserves `InitialQuantity`, suppresses SL replace.
- **Live exec / skip-reason guards:** `liveExecFailed`; skip-reason checks BEFORE spawn; capture `posSide` with `posQty` in Phase 1 RLock.
- **Bidirectional perps:** short-entry → `bidirectionalPerpsStrategies`; flip sizing `perpsLiveOrderSize`.
- `dueStrategies` value-copied — update `cfg.Strategies` first. Notifications via `MultiNotifier`; channels `spot`/`options`/`<platform>`/`<platform>-paper`.
- HL: put `platforms/hyperliquid/` on sys.path before adapter import; funding via `meta_and_asset_ctxs()`.
- **Position ownership:** `OwnerStrategyID`; shared-coin reconcile non-destructive; SL attribution OID+qty else `hl_sync_external`.
- **State SQLite-only.** Trades: `is_close`/`realized_pnl`; `#T` counts opens by `(strategy_id, position_id)`; `HLFillLookup.ClosedPnLGross` is gross — never into `Trade.RealizedPnL`.
- **HL kill-switch fills:** shared-coin split by virtual qty; `hyperliquidKillSwitchFillShare` fails closed. Close-side: short→buy else sell.
- **Map iteration:** ALWAYS `sort.Strings(keys)` for operator/test output.
- **Regime:** `adx` default / `composite` opt-in. bare `ranging_directional` covers `_up`/`_down` for gating (exact wins); certs stay exact-match.
- **Strategy registries:** `open/registry.py`+`PLATFORM_ORDER` + `knownShortNames` + `DEFAULT_PARAM_RANGES`. M5 `deprecated_m5` (32); live DM unless `allow_deprecated:true`; paper auto-suppress via `AllowDeprecatedEffective()`. `backtest_only=True` fail-closes live. Snapshot `--list-json` before registry refactors.
- Strategy DSL: config params under runtime; `HTFFilter` not on options/`delta_neutral_funding`.
- **CB:** disable suppresses new fires only; tunable cooldowns; latched HL-perps manage-only (`Signal=0`, not `continue`).
- **Kill switch:** `planKillSwitchClose`→`OnChainConfirmedFlat`. `kill_switch_reset_dm_timeout` (empty→6h). reset-prompt `atomic.Bool` single-flight.
- `initial_capital`: only `StateDB.SetInitialCapital`. **HL stops:** `EffectiveStopLossPct` seven mutual-exclusive owners; scalar↔regime blocked while open.
- **Risk-per-trade:** `risk_per_trade_pct` HL perps; fail-closed on unresolvable stop; exclusive vs sizing_leverage/margin/scale_in.
- **HL trailing SL:** cancel+replace > `TrailingStopMinMovePct`; `hlSLEffectiveQty=min(virtual,onChain)`; snapshot must carry full protection surface.
- **HL margin:** peers share `margin_mode`+`leverage`; `update_leverage` from flat only. On-chain TP suppression nils `CloseStrategy` for live tiered-TP (paper never).
- **SIGHUP:** `validateHotReloadCompatible` blocks add/remove, script/args/type/platform/HTFFilter, kill-switch identity, DB path, `max_notional_usd`.
- **New per-strategy flag:** field → `run*Check` CLI → Python parse → InitOptions/wizard. Runtime-required → both probe argvs.

## Pull Requests
- Reference issue with `Closes #<N>` in body. In GitHub comments avoid `#N` for list items; use `1.`.
- **PR title:** `type(#<N>): summary [C<score>, <model>, <effort>]` — Conventional Commits type, `#<N>` scope when closing an issue (else short component or none), lowercase imperative summary, bracket = issue `[C<score>]` + model/effort used (append `, fableplan` when a Fable plan ran first). E.g. `feat: add strategy tuning page [C40, GPT Sol, high]`.
- **PR body lead:** start with `## Plain simple English` (one paragraph under 55 words, no jargon) stating what changed and why; then `## Summary` / verification, scannable. Don't restate the issue.
- Latest bot review: `gh api repos/richkuo/go-trader/issues/<N>/comments --jq '[.[] | select(.user.login=="claude[bot]" or .user.login=="github-actions[bot]")] | last | .body'` (issues endpoint, not pulls).
- Before merging a long-running PR: `git fetch origin main && git diff origin/main..HEAD -- <paths>` catches silent reverts.
- **Commits and PR bodies:** end with `LLM: <model> | <effort> | Harness: <action>` (default Claude Code footer slot). Do **not** append any `Co-authored-by:` / `Co-Authored-By` trailer.

### PR review format (`@claude review`)
**SSoT: `richkuo/rk-skills` `templates/claude-workflow/prompts/pr-review-format.md`** + delta `.github/prompts/pr-review-format-local.md`. First line `LGTM` or `Needs Updates`. Safety never dropped. Needs Fixing/Optional need **Invariant:** + **Must survive:**; every finding ends **Plain simple English:** (≤55 words). **Reviews never gate on CI.**

### The Claude Code workflow itself (`.github/workflows/claude.yml` + the central run body)
- **least-privilege split:** `classify` + review/implement callers of `richkuo/rk-skills/.../claude-run.yml@main`; only call-site `permissions:` differ (review: no `id-token: write`).
- **Token model:** git/gh on Claude GitHub App token. Review comments as **`github-actions[bot]`**; implement as `claude[bot]`. Patch steps key on `RUN_ID`.
- **Mode routing fail-closed:** `pull_request_review*` → `review`; issues / non-PR comments / docs-sync/release → `implement`; else keyword after `@claude` (untrusted/fork → `review`; trusted → `fix-pr`).
- Comment patching: `patch_claude_comment.sh`+`compose_claude_comment.py`, staged from `rk-skills` `templates/claude-workflow/scripts/` into `$RUNNER_TEMP` each run (no copy here; `.github/scripts/` holds ONLY `test_workflow_logic.py`, which executes the real `run:` blocks of `claude.yml`). Concurrency `group: claude-<N>`, `cancel-in-progress: false`.
- **No-execution ban** in workflow agent; commit/push implement-mode-only, exact-match. Prompt-file guard: the assembled prompt — shared body plus any `*-local.md` append — must not contain `"`, `` ` ``, or `$`.
- CLAUDE.md revision step must `git commit`+`git push origin HEAD`; immediately diff `HEAD -- CLAUDE.md` vs `origin/main` to catch silent reverts. `timeout-minutes: 90`. `issues: types: [opened]` only.

### Addressing review findings
Before implementing `@claude review` findings: restate each item as an invariant, enumerate states that would break the proposed fix (especially the inverse of the reported scenario and compound/concurrent cases), and add tests for that class — not only the reported example — before pushing.

## GitHub Issues
- Create with `gh issue create`; prefix the title with a complexity score: `[C<0-100>] <title>` (e.g. `[C70] Fix order-fill race`).
- **Complexity score (0–100):** an approximation, **not** a time/effort estimate. Weigh **scope**, **risk** (money/data-integrity/security/auto-protective paths weigh heaviest), and **uncertainty**.
- First body line is a one-line rationale naming the drivers, number matching the title: `**Complexity: 70/100** — scope: medium; risk: high (order-fill path); uncertainty: exchange API behavior unverified`.
- End the body with the same footer as commits/PRs — `LLM: <model> | <effort> | Harness: <action>` — no `Co-authored-by` trailer.

## Claude Code Skills (rk-skills plugin)
- The rk-skills workflow skills (`new-issue`, `validate-issue`, `work-on-issue`, `fix-pr-review`, `-loop` variants, `github-issue-format`, `pr-review-format`) are **CI-only**: `@claude` workflows load the plugin from [richkuo/rk-skills](https://github.com/richkuo/rk-skills) at run time.
- No project-level settings pin (`.gitignore` ignores `.claude/*`). Interactive users install personally: `/plugin marketplace add richkuo/rk-skills` then `rk-skills@rk-skills` via `/plugin`, or `npx rk-skills`.

## Build & Deploy
- **Update:** `bash scripts/update.sh --restart` — atomic: preflight → `pull --ff-only` (or `--rsync-from <src>`) → `uv sync` → build → probe → swap (`.prev`) → restart+verify → rollback on timeout. **Never rebuild Go alone — argv contract requires both sides (Go+Python) at same SHA.** Batch `--all --restart` auto-discovers deployments via `discover_deployment_dirs_from_systemd`.
- **Graceful shutdown:** drains side-effecting subprocesses ≤ `shutdownDrainCap=15s` then SIGKILLs; state-save/notifier-flush/DB-close after via deferred LIFO. Unit `TimeoutStopSec=20`; service-file changes require `daemon-reload`.
- Build (one-off): `go build -ldflags "-X main.Version=$(git describe --tags --always --dirty=-mod)" -o go-trader .` (rebuild before smoke-testing). Restart `systemctl restart go-trader`; config-only `kill -HUP $(pgrep go-trader)`; Python picks up next cycle.
- **Startup probe:** each unique check script runs with `--probe-only`; non-zero → log + owner DM + `os.Exit(ExitProbeFailure)` (78, `EX_CONFIG`); `probeFailureScriptMissing` detects `"can't open file"`. Both service files set `RestartPreventExitStatus=78`. Failures after update → ensure `shared_scripts/` updated + rebuild.
- **Post-update:** follow `SKILL.md` "Post-Update Agent Protocol" (diff `<running>..HEAD`, classify via Reference table, prompt per item). **Post-deploy smoke:** after a Python-launcher change, `./go-trader --config scheduler/config.json --once`.

## Backtest
**Harness map → `docs/backtesting-registry.md` (update the row in the same PR that adds/deprecates a harness).**
- Run: `uv run --no-sync python backtest/run_backtest.py --strategy <n> --symbol BTC/USDT --timeframe 1h --mode single`; `backtest_options.py`/`backtest_theta.py --underlying BTC --since YYYY-MM-DD --capital 10000`; `backtest_pairs.py` (beta-hedged z-score, no live path).
- `--config <path> --strategy <id>` reads the single `close_strategy`, applies `user_defaults` by default; `--defaults system` preserves the built-in baseline. **`--config` gates on `config_version>=15`.** Entry ATR guard 50%-of-AvgCost. SL-vs-TP races default `ohlc_walk` (`--intrabar-resolution bar_close` = legacy).
- Regime: entries blocked when `bar_regime ∉ allowed_regimes`; closes always execute; options unsupported. `--config` threads `allowed_regimes` (CLI flag rejected). composite via `regime.windows`. open name falls back to `args[0]`.
- **Backtester rejects (HL-live-only):** `regime_window_divergence`, `tiered_tp_atr_live_regime_dynamic`. `regime_directional_policy` backtestable behind its flag. Scalar `sl_after` + `*_atr_mult_regime` stop dicts backtestable.
- **Look-ahead invariants:** signal at bar N fills at N+1 open; regime gate reads N-1 regime; closes use closed-bar ATR. **HTF series indexed by candle OPEN time MUST `.shift(1)` BEFORE `reindex(..., method="ffill")`.**
- **liquidation floor:** sticky equity floor at 0 from first bust; blown legs → `±LIQUIDATED_METRIC_FLOOR`.
- **M1–M6 / auto_suggest / regime promotion / `tune_live.py` (SCHEMA_VERSION=2 `promotion_baseline`):** SUGGEST-ONLY research surfaces — **never write live defaults/config/PRs.**

## Testing
- **New functionality must include tests.** Go `_test.go`; Python `test_*.py`; regression-test bug fixes. Extract pure helpers from subprocess wrappers — Go CI must not depend on spawning Python (`perpsLiveOrderSize`,`*OrderSkipReason`,`parseXxxCloseOutput`, Sharpe testable without subprocesses).
- Each test must guard a behavior contract. Examples include money, state, auto-protective mechanisms, exchange or subprocess contracts, config migration, and backtest parity. Regression tests for any bug fix and tests for specified new functionality also qualify. Assert the required outcome. Do not pin exact log or DM wording unless it drives an operator decision. Do not assert intermediate values, constants, or plain-struct field round-trips. Use one table-driven test for variants of one function.
- `uv run --no-sync python -m py_compile <file>` from repo root. `go build -ldflags "…" .` / `go test ./...` from repo root (not `cd scheduler`); `gofmt -w <file>.go` post-edit. Multi-line Go edits with tabs may fail Edit tool; use Python heredoc with `read()`+`replace(old,new,1)`+`write()`.
- Strategy listing `…/open/{spot,futures}/strategies.py --list-json`; smoke `./go-trader --once`; JSON init `./go-trader init --json '{...}' --output /tmp/test.json`.
- Pytest `uv run --no-sync python -m pytest shared_strategies/ shared_tools/ platforms/ backtest/`; `shared_scripts/test_*.py` NOT in `testpaths` (invoke explicitly). **Registry/sys.path tests → run the FULL suite**; imports use `importlib.util.spec_from_file_location`. **CI runs pytest with `-n auto`** — never bare-`import` an ambiguous module name; treat an intermittent CI failure as test-isolation, never a flake. Go tests check `json.Unmarshal` errors.
- `stampEntryATRIfOpened` rejects ATR > 50% of AvgCost; Go tests using `atr` must use values < 50% of entry price. Strategy tests must assert actual signal values; smoke tests iterating registered strategies need `DatetimeIndex` (`amd_ifvg` reads `index.hour`, `vwap_reversion` buckets by `index.date`).
- **ATR for close evaluators:** `tiered_tp_atr`/`trailing_stop_atr_mult` need `Position.EntryATR`; `tiered_tp_atr_live` recomputes from `market_ctx["atr"]` (`atr_source` `live`|`entry`). `atr_stop` and `avwap_stop` follow the same `atr_source` rule. `avwap_stop` is **virtual exit only** (not in `isTieredTPATRCloseName`/`closeStrategiesSuppressedByOnChainProtection`).
