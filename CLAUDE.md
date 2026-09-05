# go-trader Project Context

Guardrails only. Mechanism: `SKILL.md` § Subsystem Mechanism Reference; operator flows: `SKILL.md`, `docs/POST_UPDATE_HISTORY.md`. Keep under 15,000 bytes; never split (agents auto-load only the root `CLAUDE.md`; `AGENTS.md` symlinks to it). Shorten wording, never drop a guardrail.

## Environment
- Go 1.26.2 (`/opt/homebrew/bin/go`). Python via `uv run --no-sync python`; the scheduler calls `.venv/bin/python3`. `uv sync` per worktree.
- systemd units: `ProtectSystem=strict`, no `PATH`/`UV_CACHE_DIR` injection; secrets in `/opt/go-trader/.env`. Deployed config lives OUT of the tree at `/var/lib/go-trader[/<instance>]/config.json`; `scheduler/config.json` is a transition symlink.

## Decision Priorities
- **Always the absolute best technical solution.** Cost, compute, time, effort, tests and code volume never narrow the option space, and never override the branch+PR workflow, verifying issue claims against code, or destructive-action safety.
- **Never give time, duration or effort estimates.** Describe complexity by scope and risk.

## Repo Structure (`scheduler/` is one Go `package main`)
- `executor.go`/`shutdown.go`: **new side-effecting wrapper → `runPythonSideEffect`, NEVER `runPython`.** `confirmHyperliquidExecuteFill` gates every live HL book path: `Execution.Fill` with finite `AvgPx>0` and `TotalSz>0`, else no book. `check_hyperliquid.py execute` exits 1 on no confirmed fill.
- `server.go`/`ui_*.go`: **lock order `mu → strategiesMu`** (reusing `mu` deadlocks SIGHUP). **Loopback only.** `/tuning` never writes config; `ui_tuning.go` uses `spawnPythonProcessWithEnv` (NEVER `runPython*`); `POST /api/tuning/apply` is the only promotion.
- `config.go`/`config_migration.go`: `CurrentConfigVersion=19`, `MinSupportedConfigVersion=13`. Seven exclusive HL stop fields (all omitted → `DefaultStopLossATRMult=1.0`); `close_strategy` canonical; unknown-key guard. `strategyUsesTieredTPATRClose(sc)` gates on-chain TPs, NOT `len(tiers)>0`. `CircuitBreaker *bool` ONLY via accessors. `portfolio_risk.paper`: evaluators take `scopeRiskConfig`; nested `paper` rejected.
- `close_defaults.go`: system→user→strategy; explicit `tp_tiers` wins; `applyUserCloseDefaultRatchetRegimeTrails` runs in `loadConfig` **before** the scalar ATR-stop default.
- `portfolio_scope.go`: `PortfolioScope` from `isLiveArgs` is **the single mode classifier**. `activeScopes` evaluates ONLY configured scopes. New portfolio-wide surface → subset via `filterStatesByScope`/`strategiesInScope`, never the whole roster.
- `state.go`/`db.go`: SQLite-only, idempotent migrations. `AppState.PortfolioRisk`/`CorrelationSnapshot` are per-scope maps read via `scopeRisk`/`scopeRiskIfPresent`/`scopeCorrelation`, never bare. `initial_capital` only via `SetInitialCapital`.
- `state_store*.go`/`storage_*.go`: identity map immutable. EVERY DB caller via `StateStore` (`dbForStrategy`; live-only `liveFile`); ids translate INSIDE `StateDB`. New table → a row in SKILL.md § Storage Ownership. Manual acks by ROW ID in the persisting tx, NEVER a high-water mark. Unknown ownership errors BEFORE mutation; combined reads fail whole.
- `risk.go`: `CheckRisk` skips `manual`. Corrupt position (qty≤0 or avgCost≤0) → zero-PnL `*_corrupt` leg, cash untouched. Latch: ONE owner per cycle per scope; `DrawdownReadingSubstituted` labelled everywhere; untrusted over-limit defers, never vetoes. **Paper `equityTrusted` always true.** `ResetPortfolioKillSwitchManual` is the sole DM reset; `AutoResetConfirmedFlatKillSwitch`/`ClearLatchedKillSwitchSharedWallet` are `ScopeLive` only.
- `daily_loss.go`: **hold-only, UNLATCHED pure read**, PRE-FEE realized PnL, never force-closes, per scope. **New `portfolio_risk` gate → copy this shape.**
- `exposure_cap.go`: **blocking-only, direction-aware**; **single exposure model** `computeAssetDeltas`, shared with `ComputeCorrelation`. `notional_cap.go`: **hold-only via `pausedBlocksSignal`**, never skips the cycle; restart-required.
- `replay_log.go`/`replay_mirror.go`: **DEFAULT-OFF**; HL perps, flat-only hot-reload; one mirror per source (`replayMirrorSourceID`).
- `hl_batch.go`: shared-state failure ⇒ per-strategy fallback same cycle; never blank a close/SL/ratchet/protection/hedge. `GO_TRADER_HL_BATCH=0` disables.
- `hyperliquid_fills.go`: fill resolver built **outside `mu.Lock`**; `HLFillLookup.Px`=VWAP; `ClosedPnLGross` never into `Trade.RealizedPnL`; unconfirmed SL fills are gaps, not books.
- `hyperliquid_balance.go`: reconciliation-close alerts outside `mu`; hedge-leg rows never reach the public route.
- `pause.go`: paused is NOT a `dueStrategies` skip. `pausedBlocksSignal` holds position-increasing signals at all 6 regime-gated dispatch sites (so does the per-scope persistence hold); closes/trailing SL/ratchet/protection pass.
- Regime (`regime*.go`): stamp all 5 execute dispatches; strip `sl_after` before ATR parse; `regime_unified.go` owns SL; directional policy DEFAULT-OFF, exact-match certs; profile allocation flat-only; dynamic/divergence HL-live-only.
- `trailing_tp_ratchet.go`: **NO on-chain TP**; HL perps+`manual`; hot-reload blocked while open.
- `hedge.go`: HL perps only; ONE reconciler (`hedgeTargetDecision`+`runHedgeSync`); ownership `Position.HedgeFor` ONLY; hedge PnL → `RecordHedgeTradeResult`, never `RecordTradeResult`; fail-closed unwind + CRITICAL DM; hot-reload blocked while open; backtester rejects it.
- `hurst_gate.go`: **DEFAULT-OFF**, no shipped threshold, `config.example.json` clean; holds position-increasing signals only; fail-closed FLAT-ONLY; `metrics["hurst"]` ONLY from the composite classifier; keep in `run_backtest.py` `stop_keys`.
- `llm_entry_analysis.go`: advisory-only; `spawnPythonProcess` NEVER `runPython*`; sole writer of `trade_diagnostics.llm_verdict`; `trade_diagnostics*.go` never write it.
- `scale_in.go`: geometry frozen via `RiskAnchorPrice`, never blended `AvgCost`. `manual*.go`: kill-switch+CB gated; SL edits queue `PendingManualAction`, NEVER a direct UPDATE; `force-close` live HL perps only.
- `hyperliquid_liquidation_guard.go`: **CLAMP, never refuse to arm; ONE-WAY TIGHTEN** at 0.5% buffer; 0 = unknown, never persisted; unclampable REFUSES; unreadable outcome keeps state. Boot `validateHLStopWithinBankruptcyBound` mirrors `LoadConfig` stop-owner resolution.
- `hyperliquid_protection.go`: reduce-only; on-chain TP only when `strategyUsesTieredTPATRClose` AND live (paper never). `hyperliquid_open_trailing.go` arms the first SL at open.
- `version_probe.go`/`probe_cmd.go`: new runtime CLI flag → both probe argvs.
- `agent_info.go`: `--bootstrap-md` → `AGENTS.generated.md`, NEVER `AGENTS.md`.
- `failure_alerts.go`: wire the notifier on each new `run*Check`. `discord_*commands.go`: new mutating command → `opsCommandNames`+`slashCommands()`+dispatch.
- `shared_wallet*.go`: PRE-FEE `realized_pnl`, net via `tradeNetPnL*`. Pool budgeting: 2+ live HL/OKX perps omit capital fields, positive `margin_per_trade_usd` each; allocated↔pool flat-only. `cashflow_journal.go` OUTSIDE `mu`.
- `kill_switch_limit_orders.go`: cancel every `pending_limit_orders` row BEFORE flatten (keyed on ROW); **never gate `reconcilePendingLimitOrders` on kill-switch**; cancel≠adoption; never auto-delete an unadopted fill.
- `orphan_limit_cancel_alerts.go`: cancel-only lane, status-FIRST finalize, books NO fill; `orphanLimitCancelState` is the SSoT (off-book fill = UNTRACKED POSITION). `limit_fill_exposure.go`: book a limit fill ONLY after live exposure confirms; per-coin aggregate, never per-row greedy; fail-closed same-direction+contained; `unreadable`/`unbacked` refuse book AND block delete.
- `shared_scripts/`: every check script accepts `--regime-payload-json`, probed at startup. `platforms/<name>/adapter.py`: one `*ExchangeAdapter` per file; HL `_sz_decimals()` via `name_to_asset(symbol)`. `funding_fetcher.py`: `merge_asof` backward, DISJOINT `funding_coverage`; `regime.py` ATR pinned to `simple`.
- `shared_strategies/`: open SSoT `open/registry.py`; **`open/{spot,futures}/strategies.py` are shims, do not edit.** Close via `close/registry.py`, imported `from close_registry_loader import …`, never bare `import registry`. `hurst_exponent` (DFA) is the SSoT.

## Key Patterns
- Run git from repo root; `go -C scheduler build .`, never `cd scheduler &&`.
- New platform: SKILL.md § Custom Platform Integration lists the touchpoints. Adapters load via `importlib`, class `endswith("ExchangeAdapter")`; check scripts use public methods only.
- Subprocess contract: JSON on stdout even on error; exit 1 on error; Go parses regardless.
- State locking: `mu sync.RWMutex`, 6-phase cycle (RLock → Lock(CheckRisk) → no-lock subprocess → Lock(execute) → marks → RLock(status)). Skip-reason checks BEFORE spawn; capture `posSide` with `posQty` in Phase 1; `liveExecFailed` guards live exec.
- Platform dispatch by `s.Platform`, never ID prefix. Perps paper→`ExecuteSpotSignalWithFillFee`, live→`RunHyperliquidExecute`; futures→`ExecuteFuturesSignalWithFillFee`.
- Single `CloseStrategy` owns exit; close before open; partial close keeps `InitialQuantity`, suppresses SL replace.
- `dueStrategies` is value-copied: update `cfg.Strategies` first. Ownership via `OwnerStrategyID`; shared-coin reconcile non-destructive; SL attribution by OID+qty, else `hl_sync_external`.
- Trades: `is_close`/`realized_pnl`; `#T` counts opens by `(strategy_id, position_id)`. HL kill-switch shared-coin fill split fails closed; close side short→buy, else sell.
- Map iteration: ALWAYS `sort.Strings(keys)` for operator/test output.
- Regime: `adx` default, `composite` opt-in; bare `ranging_directional` covers `_up`/`_down` for gating; certs stay exact-match.
- Registries: `open/registry.py`+`PLATFORM_ORDER`+`knownShortNames`+`DEFAULT_PARAM_RANGES`; `backtest_only=True` fail-closes live; snapshot `--list-json` first.
- CB disable suppresses new fires only; latched HL-perps manage-only (`Signal=0`, not `continue`). Kill switch: `planKillSwitchClose`→`OnChainConfirmedFlat`; reset prompt single-flight.
- HL stops: `EffectiveStopLossPct` seven exclusive owners; scalar↔regime blocked while open. `risk_per_trade_pct` fails closed on an unresolvable stop; exclusive vs sizing_leverage/margin/scale_in. Trailing SL replace only past `TrailingStopMinMovePct`; `hlSLEffectiveQty=min(virtual,onChain)`; the snapshot carries the whole protection surface. Peers share `margin_mode`+`leverage`; `update_leverage` when flat.
- SIGHUP `validateHotReloadCompatible` blocks add/remove, script/args/type/platform/HTFFilter, kill-switch identity, `db_file`/`paper_db_file`, effective `storage_strategy_id`, `max_notional_usd`.
- New per-strategy flag: field → `run*Check` CLI → Python parse → InitOptions/wizard; runtime-required → both probe argvs.
- Notifications via `MultiNotifier`; paper routes via `resolveChannelKey`/`SendToScopeChannels`.

## Pull Requests
- `Closes #<N>` in body; never bare `#N` for list items. Title `type(#<N>): summary [C<score>, <model>, <effort>]` (`, fableplan` after a Fable plan). Body leads with `## Plain simple English` (under 55 words), then `## Summary` and verification.
- Commits, PR and issue bodies end with `LLM: <model> | <effort> | Harness: <action>`; no `Co-authored-by` trailer.
- Bot reviews land on the issues comments endpoint. Before merging a long-lived PR, diff `origin/main..HEAD` for reverts.
- Review format SSoT: rk-skills `pr-review-format.md` + `.github/prompts/pr-review-format-local.md`; reviews never gate on CI.
- Before implementing review findings: restate each as an invariant, enumerate states that break the fix (inverse, compound), add tests for the class.
- `.github/workflows/claude.yml`: least-privilege split; mode routing fail-closed (untrusted/fork → review); no-execution ban in the agent, commit/push implement-mode only; the prompt must not hold `"`, `` ` `` or `$`; `.github/scripts/` keeps ONLY `test_workflow_logic.py`.

## GitHub Issues
- `gh issue create`; title `[C<0-100>] <title>`; first body line `**Complexity: N/100** — scope; risk; uncertainty` (money/data/protection risk weighs heaviest; never time).
- rk-skills workflow skills are CI-only; no project settings pin.

## Build & Deploy
- **Update only with `bash scripts/update.sh --restart`. Never rebuild Go alone**: Go and Python share an argv contract at one SHA. `update_resolve_db_exclude` lists every state file.
- Exit codes: probe 78, singleton 79, storage 80; units set `RestartPreventExitStatus=78 79 80`. Ownership over ALL files (incl. `--once`) precedes any migration or startup write. Service-file edits need `daemon-reload`.
- Post-update: SKILL.md § Post-Update Agent Protocol. After a Python-launcher change, smoke `./go-trader --once` (daemon stopped).

## Backtest
- Harness map `docs/backtesting-registry.md`: update its row in the PR that adds or deprecates a harness.
- `--config` gates on `config_version>=15`; entry ATR guard 50% of AvgCost; SL-vs-TP races default `ohlc_walk`.
- Look-ahead: signal at bar N fills at N+1 open; regime gate reads N-1; closes use closed-bar ATR. **HTF series indexed by candle OPEN time MUST `.shift(1)` BEFORE `reindex(…, method="ffill")`.**
- Backtester rejects HL-live-only closes (`regime_window_divergence`, `tiered_tp_atr_live_regime_dynamic`); options regime gating unsupported.
- M1–M6, auto_suggest, regime promotion, `tune_live.py` are SUGGEST-ONLY: **never write live defaults, config or PRs.**

## Testing
- New functionality and each bug fix need tests guarding a behavior contract (money, state, protection, subprocess contracts, migration, backtest parity). Assert outcomes; pin log/DM wording only when it drives an operator decision; no constants or round-trips; table-driven variants.
- **Test budget.** Only for that contract list; one table-driven test per new function at most; none asserting only wording, a constant or a round-trip (operator-decision wording aside). `check_test_budget.py` fails CI on a wording-only test outside `scripts/test_budget_baseline.json`, or a stale entry; add entries only for operator-decision wording; `--write-baseline` after a delete.
- Go CI must not spawn Python: extract pure helpers from subprocess wrappers. Go tests check `json.Unmarshal` errors.
- `go build`/`go test ./...` from repo root; `gofmt -w` after edits. Tabbed Go edits: Python `read()`+`replace(old,new,1)`+`write()`.
- Pytest: `uv run --no-sync python -m pytest shared_strategies/ shared_tools/ platforms/ backtest/`; `shared_scripts/test_*.py` explicitly. Registry/sys.path tests → FULL suite. CI uses `-n auto`: never bare-`import` an ambiguous name; an intermittent failure is isolation, not a flake.
- `stampEntryATRIfOpened` rejects ATR > 50% of AvgCost. Strategy tests assert real signal values; smoke tests need a `DatetimeIndex`.
- `tiered_tp_atr`/`trailing_stop_atr_mult` need `Position.EntryATR`; `*_live` recompute via `atr_source`. `avwap_stop` is virtual exit only.
