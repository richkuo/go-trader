# go-trader

Guardrails only. Mechanism: SKILL.md § Subsystem Mechanism Reference; flows: SKILL.md, docs/POST_UPDATE_HISTORY.md; <15,000 bytes, never split (agents load only root `CLAUDE.md`; `AGENTS.md` symlinks). Shorten wording, drop no guardrail.

## Env
- Go 1.26.2 (`/opt/homebrew/bin/go`). Python: `uv run --no-sync python`; scheduler runs `.venv/bin/python3`; `uv sync` per worktree.
- systemd units: `ProtectSystem=strict`, no `PATH`/`UV_CACHE_DIR` injection, secrets: `/opt/go-trader/.env`; config: `/var/lib/go-trader[/<instance>]/config.json`; `scheduler/config.json` = transition symlink.

## Priorities
- **Always the absolute best technical solution.** Cost, compute, time, effort, tests, code volume never narrow options; branch+PR flow, issue-claim checks vs code, destructive-action safety win.
- **Never give time/duration/effort estimates.** Complexity = scope+risk

## Repo (`scheduler/` = one Go `package main`)
- `executor.go`/`shutdown.go`: **side-effecting wrappers use `runPythonSideEffect`, NEVER `runPython`.** `confirmHyperliquidExecuteFill` gates each live HL book: finite `AvgPx>0`+`TotalSz>0` in `Execution.Fill`, else no book; `check_hyperliquid.py execute` exits 1 on no fill.
- `server.go`/`ui_*.go`: **lock order `mu > strategiesMu`**. **Loopback only.** `/tuning` never writes config; `ui_tuning.go` uses `spawnPythonProcessWithEnv` (NEVER `runPython*`); `POST /api/tuning/apply` = sole promotion.
- `config.go`/`config_migration.go`: `CurrentConfigVersion=19`, `MinSupportedConfigVersion=13`; 7 exclusive HL stop fields (none → `DefaultStopLossATRMult=1.0`); `close_strategy` canonical, unknown-key guard. `strategyUsesTieredTPATRClose(sc)` gates on-chain TPs, NOT `len(tiers)>0`. `CircuitBreaker *bool` ONLY via accessors. `portfolio_risk.paper`: evaluators use `scopeRiskConfig`, nested `paper` rejected.
- `close_defaults.go`: system>user>strategy, explicit `tp_tiers` wins; `applyUserCloseDefaultRatchetRegimeTrails` runs in `loadConfig` **before** the scalar ATR-stop default.
- `portfolio_scope.go`: `PortfolioScope` from `isLiveArgs` = **sole mode classifier**; `activeScopes` evaluates ONLY configured scopes. New portfolio-wide surface: subset via `filterStatesByScope`/`strategiesInScope`, never whole roster.
- `state.go`/`db.go`: SQLite-only, idempotent migrations. `AppState.PortfolioRisk`/`CorrelationSnapshot` = per-scope maps read only via `scopeRisk`/`scopeRiskIfPresent`/`scopeCorrelation`. `initial_capital` only via `SetInitialCapital`.
- `state_store*.go`/`storage_*.go`: identity map immutable. EVERY DB caller via `StateStore` (`dbForStrategy`, live-only `liveFile`); ids translate INSIDE `StateDB`. New table: a SKILL.md § Storage Ownership row. Manual acks by ROW ID in persisting tx, NEVER a high-water mark; unknown ownership errs BEFORE mutation; combined reads fail whole.
- `merge-paper-instance.sh`: binaries run on config COPIES; `inspect`/`storage-inspect` use `LoadConfigReadOnly` (never rewrite); both locks held preflight>apply; `--once`/`--probe-only` never proof, zero paper override inherits.
- `risk.go`: `CheckRisk` skips `manual`. Corrupt position (qty<=0 or avgCost<=0) → zero-PnL `*_corrupt` leg, cash untouched. Latch: ONE owner per cycle per scope; `DrawdownReadingSubstituted` labelled everywhere, untrusted over-limit defers, never vetoes. **Paper `equityTrusted` always true.** `ResetPortfolioKillSwitchManual` = sole DM reset; `AutoResetConfirmedFlatKillSwitch`/`ClearLatchedKillSwitchSharedWallet` `ScopeLive` only.
- `daily_loss.go`: **hold-only, UNLATCHED pure read**, PRE-FEE realized PnL, never force-closes, per scope. **New `portfolio_risk` gate: copy this shape.**
- `exposure_cap.go`: **blocking-only, direction-aware**; **single exposure model** `computeAssetDeltas`, shared by `ComputeCorrelation`. `notional_cap.go`: **hold-only via `pausedBlocksSignal`**, never skips a cycle, restart-required.
- `replay_log.go`/`replay_mirror.go`: **DEFAULT-OFF**, HL perps, flat-only hot-reload, 1 mirror per source (`replayMirrorSourceID`).
- `hl_batch.go`: shared-state failure ⇒ per-strategy fallback same cycle, never blank a close/SL/ratchet/protection/hedge. `GO_TRADER_HL_BATCH=0` disables. `market_feed=websocket`: each check path reads one sealed stdin `marketSnapshot`, missing frame = error, NEVER a private fetch.
- `hyperliquid_fills.go`: fill resolver built **outside `mu.Lock`**; `HLFillLookup.Px`=VWAP; `ClosedPnLGross` never into `Trade.RealizedPnL`; unconfirmed SL fills = gaps, never books.
- `hyperliquid_balance.go`: reconciliation-close alerts outside `mu`, hedge-leg rows never reach public route.
- `pause.go`: paused is NOT a `dueStrategies` skip. `pausedBlocksSignal` holds position-increasing signals at all 6 regime-gated dispatch sites (+per-scope persistence hold); closes/trailing SL/ratchet/protection pass.
- Regime (`regime*.go`): stamp all 5 execute dispatches; strip `sl_after` before ATR parse; `regime_unified.go` owns SL; directional policy DEFAULT-OFF; profile allocation flat-only; dynamic/divergence HL-live-only.
- `trailing_tp_ratchet.go`: **NO on-chain TP**; HL perps+`manual`, hot-reload blocked while open.
- `hedge.go`: HL perps only; ONE reconciler (`hedgeTargetDecision`+`runHedgeSync`), ownership `Position.HedgeFor` ONLY; hedge PnL: `RecordHedgeTradeResult`, never `RecordTradeResult`; fail-closed unwind + CRITICAL DM; hot-reload blocked while open, backtester rejects.
- `hurst_gate.go`: **DEFAULT-OFF**, no shipped threshold, `config.example.json` clean; holds position-increasing signals only, fail-closed FLAT-ONLY; `metrics["hurst"]` ONLY from composite classifier; keep in `run_backtest.py` `stop_keys`.
- `llm_entry_analysis.go`: advisory-only; `spawnPythonProcess` NEVER `runPython*`; sole writer of `trade_diagnostics.llm_verdict`, `trade_diagnostics*.go` never.
- `scale_in.go`: geometry frozen via `RiskAnchorPrice`, never blended `AvgCost`. `manual*.go`: kill-switch+CB gated; SL edits queue `PendingManualAction`, never a bare book write; only the re-arm also writes it; a rejected close never assumes an unconfirmed cancel spared the trigger; verify-first restore, unverified = CRITICAL; adopted SL edits never gate; `force-close` live HL perps only.
- `hyperliquid_liquidation_guard.go`: **CLAMP, never refuse to arm; ONE-WAY TIGHTEN** at 0.5% buffer; 0 = unknown, never persisted; unclampable REFUSES, unreadable outcome keeps state. Boot `validateHLStopWithinBankruptcyBound` mirrors `LoadConfig` stop-owner resolution.
- `hyperliquid_protection.go`: reduce-only, on-chain TP also needs live (paper never); `hyperliquid_open_trailing.go` arms SL at open. `hyperliquid_shared_close_floor.go`: <$10 shared-coin full close escalates ONLY if each peer is flat on-chain (refetched, raw keys) AND in book, before SL blocks; else ONE alert+hold, `venue_rejected` never resends; same-cycle re-arm (all 7 owners) ONLY after a SUBMITTED order that REQUESTED the cancel; 2nd escalated refusal holds, any wording.
- `version_probe.go`/`probe_cmd.go`: new runtime CLI flag > both probe argvs. `agent_info.go`: `--bootstrap-md` → `AGENTS.generated.md`, NEVER `AGENTS.md`.
- `failure_alerts.go`: wire notifier on each new `run*Check`. `discord_*commands.go`: new mutating command: `opsCommandNames`+`slashCommands()`+dispatch.
- `shared_wallet*.go`: PRE-FEE `realized_pnl`, net via `tradeNetPnL*`. Pool budgeting: 2+ live HL/OKX perps omit capital fields, positive `margin_per_trade_usd` each; allocated↔pool flat-only. `cashflow_journal.go` OUTSIDE `mu`.
- `kill_switch_limit_orders.go`: cancel each `pending_limit_orders` row BEFORE flatten (keyed on ROW); **never gate `reconcilePendingLimitOrders` on kill-switch**; cancel≠adoption, never auto-delete an unadopted fill.
- `orphan_limit_cancel_alerts.go`: cancel-only lane, status-FIRST finalize, books NO fill; `orphanLimitCancelState` = SSoT (off-book fill = UNTRACKED POSITION). `limit_fill_exposure.go`: books a limit fill ONLY once live exposure confirms; per-coin aggregate, never per-row greedy; fail-closed same-direction+contained, `unreadable`/`unbacked` refuse book AND block delete.
- `shared_scripts/`: check scripts take `--regime-payload-json`, probed at start. `check_hyperliquid.py` close gate: live-only, lot-floored, never a full close, no lot = no gate. `platforms/<name>/adapter.py`: one `*ExchangeAdapter`, HL `_sz_decimals()` via `name_to_asset`. `funding_fetcher.py`: `merge_asof` backward, DISJOINT `funding_coverage`; `regime.py` ATR `simple`.
- `shared_strategies/`: open SSoT `open/registry.py`; **`open/{spot,futures}/strategies.py` = shims, never edit.** Close: `close/registry.py` via `from close_registry_loader import …`, never bare `import registry`. `hurst_exponent` (DFA) = SSoT.

## Patterns
- Git from repo root; `go -C scheduler build .`, never `cd scheduler &&`.
- New platform: touchpoints in SKILL.md § Custom Platform Integration. Adapters load via `importlib`, class `endswith("ExchangeAdapter")`; check scripts: public methods only.
- Subprocess contract: JSON on stdout, exit 1 on error; Go parses regardless.
- Locking: `mu sync.RWMutex`, 6 phases (RLock > Lock(CheckRisk) > no-lock subprocess > Lock(execute) > marks > RLock(status)). Skip-reason checks BEFORE spawn; capture `posSide` with `posQty` in Phase 1; `liveExecFailed` guards live exec.
- Dispatch by `s.Platform`, never ID prefix. Perps paper=`ExecuteSpotSignalWithFillFee`, live=`RunHyperliquidExecute`; futures=`ExecuteFuturesSignalWithFillFee`.
- Single `CloseStrategy` owns exit; close before open; partial close keeps `InitialQuantity`, suppresses SL replace.
- `dueStrategies` value-copied: update `cfg.Strategies` first. Ownership via `OwnerStrategyID`; shared-coin reconcile non-destructive; SL attribution by OID+qty, else `hl_sync_external`.
- Trades: `is_close`/`realized_pnl`; `#T` counts opens by `(strategy_id, position_id)`. HL kill-switch shared-coin fill split fails closed; close side short=buy, else sell.
- Map iteration: ALWAYS `sort.Strings(keys)` for operator/test output.
- Regime: `adx` default, `composite` opt-in; bare `ranging_directional` covers `_up`/`_down` for gating, certs exact-match.
- Registries: `open/registry.py`+`PLATFORM_ORDER`+`knownShortNames`+`DEFAULT_PARAM_RANGES`; `backtest_only=True` fail-closes live, snapshot `--list-json` first.
- CB disable suppresses new fires only; latched HL-perps manage-only (`Signal=0`, not `continue`). Kill switch: `planKillSwitchClose` > `OnChainConfirmedFlat`; reset prompt single-flight.
- HL stops: `EffectiveStopLossPct` 7 exclusive owners; scalar↔regime blocked while open. `risk_per_trade_pct` fails closed on unresolvable stop, exclusive vs sizing_leverage/margin/scale_in. Trailing SL replace only past `TrailingStopMinMovePct`; `hlSLEffectiveQty=min(virtual,onChain)`, snapshot = full protection surface. Peers share `margin_mode`+`leverage`, `update_leverage` when flat.
- SIGHUP `validateHotReloadCompatible` blocks add/remove, script/args/type/platform/HTFFilter, kill-switch identity, `db_file`/`paper_db_file`, effective `storage_strategy_id`, `max_notional_usd`, `market_feed`.
- New per-strategy flag: field, `run*Check` CLI, Python parse, InitOptions/wizard; runtime-required: probe argvs.
- Notifications: `MultiNotifier`, paper routes via `resolveChannelKey`/`SendToScopeChannels`.

## PRs
- `Closes #<N>` in body; never bare `#N` in lists. Title `type(#<N>): summary [C<score>, <model>, <effort>]` (`, fableplan` if Fable planned). Body: `## Plain simple English` (<55 words) first, then `## Summary` + verification.
- Commits, PR and issue bodies end `LLM: <model> | <effort> | Harness: <action>`, no `Co-authored-by` trailer.
- Bot reviews land on issue-comments endpoint; before merging a long-lived PR diff `origin/main..HEAD` for reverts.
- Review format: rk-skills `pr-review-format.md` + `.github/prompts/pr-review-format-local.md`, reviews never gate on CI.
- Review findings: restate as invariant, list breaking states (inverse, compound), add class tests.
- `.github/workflows/claude.yml`: least-privilege split; mode routing fail-closed (untrusted/fork = review); no-execution ban in agent, commit/push implement-mode only; prompt never holds `"`, `` ` ``, `$`; `.github/scripts/` keeps ONLY `test_workflow_logic.py`.

## Issues
- `gh issue create`, title `[C<0-100>] <title>`; first body line `**Complexity: N/100** - scope; risk; uncertainty` (money/data/protection risk weighs most, never time).
- rk-skills workflow skills = CI-only, no project settings pin.

## Deploy
- **Update only with `bash scripts/update.sh --restart`. Never rebuild Go alone**: Go+Python share 1 argv contract per SHA; `update_resolve_db_exclude` lists all state files.
- Exit codes: probe 78, singleton 79, storage 80, units set `RestartPreventExitStatus=78 79 80`. Ownership over ALL files (incl. `--once`) precedes any migration or startup write; unit edits: `daemon-reload`.
- Post-update: SKILL.md § Post-Update Agent Protocol. After a Python-launcher change smoke `./go-trader --once` (daemon stopped).

## Backtest
- Harness map `docs/backtesting-registry.md`: update its row in the adding/deprecating PR.
- `--config` gates on `config_version>=15`; entry ATR guard 50% of AvgCost, SL-vs-TP races default `ohlc_walk`.
- Look-ahead: bar N signal fills at N+1 open; regime gate reads N-1, closes use closed-bar ATR. **HTF series indexed by candle OPEN time MUST `.shift(1)` BEFORE `reindex(…, method="ffill")`.**
- Backtester rejects HL-live-only closes (`regime_window_divergence`, `tiered_tp_atr_live_regime_dynamic`); options regime gating unsupported.
- M1–M6, auto_suggest, regime promotion, `tune_live.py` = SUGGEST-ONLY: **never write live defaults, config or PRs.**

## Testing
- Each new feature and bug fix needs a test guarding a behavior contract (money, state, protection, subprocess contracts, migration, backtest parity). Assert outcomes; pin only operator-decision wording; no constants or round-trips, table-driven variants.
- **Test budget.** Only that contract list; max one table-driven test per new function. `check_test_budget.py` fails CI on a wording-only test outside `scripts/test_budget_baseline.json` or a stale entry; entries only for operator-decision wording; `--write-baseline` after a delete.
- Go CI never spawns Python: pure helpers out of subprocess wrappers; Go tests check `json.Unmarshal` errors.
- `go test ./...` after edits, then `gofmt -w`; tabbed Go edits: Python `replace(old,new,1)`.
- Pytest: `uv run --no-sync python -m pytest shared_strategies/ shared_tools/ platforms/ backtest/`; `shared_scripts/test_*.py` by path; Registry/sys.path tests: FULL suite. CI `-n auto`: never bare-`import` an ambiguous name; intermittent failure = isolation, not flake.
- `stampEntryATRIfOpened` rejects ATR > 50% of AvgCost. Strategy tests assert real signal values, smoke tests need `DatetimeIndex`.
- `tiered_tp_atr`/`trailing_stop_atr_mult` need `Position.EntryATR`; `*_live` recompute via `atr_source`; `avwap_stop` = virtual exit only.
