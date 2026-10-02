# go-trader

Guardrails only; mechanism/flows in SKILL.md, docs/POST_UPDATE_HISTORY.md. Ceiling 17,000 bytes (CI job `docs` fails at it), target 16,500. Never split: root `CLAUDE.md`, `AGENTS.md` symlink. Shorten wording, drop no guardrail.

## Env
- Go 1.26.2 (`/opt/homebrew/bin/go`). Python: `uv run --no-sync python`; scheduler `.venv/bin/python3`; `uv sync` per worktree.
- systemd: `ProtectSystem=strict`, no `PATH`/`UV_CACHE_DIR` injected, secrets `/opt/go-trader/.env`, config `/var/lib/go-trader[/<instance>]/config.json`; `scheduler/config.json`=transition symlink.

## Priorities
- **Always the best solution.** Cost/compute/time/effort/code never narrow options; branch+PR, issue-claim vs code, destructive-action safety win.
- **Never give time/effort estimates.** Complexity=scope+risk

## Repo (`scheduler/`=one `package main`)
- `executor.go`/`shutdown.go`: side effects via `runPythonSideEffect`, NEVER `runPython`. Live HL book needs `confirmHyperliquidExecuteFill` (finite `Fill.AvgPx>0`+`TotalSz>0`); `check_hyperliquid.py execute` exits 1 if no fill.
- `planHLCloseOrder`=SOLE size+mode for SIZED live closes (execute, `type=manual`, force-close, hedge, open cleanup; not whole/ForceFullClose); refetch fail=uncapped reduce_only; short fill books fill. Rearm ONLY `resolveHLCloseRemainderStop`; Q=0 verify-first cancels unconfirmed SL/TP; ONE CRITICAL; `order_outcome`=SOLE outcome.
- `server.go`/`ui_*.go`: lock `mu>strategiesMu`, loopback only. `/tuning` never writes config; `ui_tuning.go`=`spawnPythonProcessWithEnv`, NEVER `runPython*`; `POST /api/tuning/apply`=sole promotion. `uiPartitionParam`=sole `?partition=` resolver: unparseable=400, unowned=404; diagnostics filter `SourceRole` before paging+total; cash flow `live_owned`; selected correlation/portfolio-risk NEVER use untagged legacy.
- `config*.go`: `CurrentConfigVersion=19`, `MinSupportedConfigVersion=13`; `close_strategy` canonical, unknown-key guard. On-chain TP gate!=`len(tiers)>0`. `CircuitBreaker *bool` via accessors ONLY. `portfolio_risk.paper`/`paper_sources[].portfolio_risk`: evaluators use `partitionRiskConfig`; nested `paper` rejected. `portfolio_warning.go` drops paused from Top Contributors ONLY if flat (`Quantity==0` regular+option).
- `close_defaults.go`: system>user>strategy; explicit `tp_tiers` wins; `applyUserCloseDefaultRatchetRegimeTrails` runs in `loadConfig` BEFORE scalar ATR-stop default.
- `portfolio_scope.go`/`risk_partition.go`: `PortfolioScope` from `isLiveArgs`=SOLE mode classifier; `RiskPartition{Scope,Source}` (`live`/`paper`/`paper:<id>`; `paper_source`=`paper_sources` id, never live)=SOLE risk+storage owner. New surface: `activePartitions`, `filterStatesByPartition`/`strategiesInPartition`, never roster.
- `state.go`/`db.go`: SQLite-only, idempotent migrations. `AppState.PortfolioRisk`/`CorrelationSnapshot`=per-`RiskPartition` maps, read only via `partitionRisk(IfPresent)`/`partitionCorrelation`; `initial_capital` only via `SetInitialCapital`.
- `state_store*.go`/`storage_*.go`: identity map immutable; 1 distinct file/partition; lock+save order primary>paper>sources by id. EVERY DB caller via `StateStore` (`dbForStrategy`, live-only `liveFile`); ids translate INSIDE `StateDB`; load maps (file,scope)->partition; diagnostics route by `SourceRole`, NEVER scope. New table=SKILL.md Storage Ownership row. Manual acks by ROW ID in-tx, NEVER high-water mark; book+queued row=ONE tx; failing rows end; unknown ownership errs BEFORE mutation; combined reads fail whole.
- `merge-paper-instance.sh`: binaries on config COPIES; `inspect`/`storage-inspect`=`LoadConfigReadOnly`; both locks preflight>apply except `--diff` (config-only); `--align-to-live` writes only `.aligned`; compose lists ALL root-key conflicts+refuses; alias always via `paper_alias.py`; `--once`/`--probe-only` never proof; zero paper override inherits. `channels`/`trade_alert_channels` `-paper` keys skip if merged bare key routes there (=`resolveChannel`); `dm_channels` `-paper`/`-paper:<id>` NEVER skip; proof refuses a paper DM route the send path disagrees with.
- `risk.go`: `CheckRisk` skips `manual`. Corrupt pos (qty/avgCost<=0)=zero-PnL `*_corrupt` leg, cash untouched. Latch: ONE owner per cycle+partition; `DrawdownReadingSubstituted` always labelled; untrusted over-limit defers, never vetoes. Paper `equityTrusted` always true. `ResetPortfolioKillSwitchManual`=sole DM reset; `AutoResetConfirmedFlatKillSwitch`/`ClearLatchedKillSwitchSharedWallet` `ScopeLive` only.
- `daily_loss.go`: hold-only, UNLATCHED pure read, PRE-FEE realized PnL, never force-closes, per partition; new `portfolio_risk` gates copy it.
- `exposure_cap.go`: blocking-only, direction-aware; one exposure model `computeAssetDeltas` (`ComputeCorrelation` too). `notional_cap.go`: hold-only via `pausedBlocksSignal`, never skips cycle, restart-required.
- `replay_{log,mirror}.go`: DEFAULT-OFF, HL perps, flat-only hot-reload, 1 mirror/source.
- `hl_batch.go`: shared-state failure=same-cycle per-strategy fallback, never blank close/SL/ratchet/protection/hedge; `GO_TRADER_HL_BATCH=0` disables. `market_feed=websocket|shared`: checks read one sealed stdin `marketSnapshot`; missing frame=error, NEVER private fetch.
- `market_feed_*.go`: `role: feed` runs BEFORE `LoadConfig` (no state/lock/probe). Seal bytes immutable; missed/evicted/pre-start key=`unavailable`, NEVER current data; sealer never takes `mu`. Consumer: 1 deadline/cycle, non-nil degraded snapshot, primary>backup. REST feed: `/info` via `feedBudgetAcquire`; key ready ONLY if refreshed that deadline. Closed-bar correction (`market_feed_correction.go`): reads outside `feedMu`, closed bars only, never `LastRecvAt`, never touches seals or readiness; ledger reasons `correction`/`correction_retry`. `shared-feed-convert.sh`: opt-in, NEVER run by update; failed switch restores byte copy.
- `migrate-service-layout.py`: opt-in, NEVER run by update; target authoritative from enable-intent; recovery NEVER restarts source from a stale snapshot.
- `hyperliquid_fills.go`: `HLFillLookup.Px`=VWAP; `ClosedPnLGross` never into `Trade.RealizedPnL`; unconfirmed SL fills=gaps, never books.
- `hyperliquid_balance.go`: hedge-leg rows private. `pause.go`: paused is NOT `dueStrategies` skip; `pausedBlocksSignal` holds position-increasing signals at all 6 regime-gated dispatch sites (+per-scope persistence hold); closes/trailing SL/ratchet/protection pass.
- `regime*.go`: stamp all 5 execute dispatches; strip `sl_after` before ATR parse; `regime_unified.go` owns SL; directional policy DEFAULT-OFF; profile allocation flat-only; dynamic/divergence HL-live only. Trade-alert span=bundle spec (`tradeAlertRegimeConfig`): `regime.timeframe` else `Args[2]`, NEVER `sc.Timeframe`; options: own fixed spec, never `cfg.Regime`.
- `trailing_tp_ratchet.go`: NO on-chain TP; HL perps+`manual`. `hedge.go`: HL perps only; ONE reconciler (`hedgeTargetDecision`+`runHedgeSync`); ownership ONLY `Position.HedgeFor`; PnL via `RecordHedgeTradeResult`, never `RecordTradeResult`; fail-closed unwind+CRITICAL DM. Both hot-reload-blocked while open; hedge backtester-rejected.
- `hurst_gate.go`: DEFAULT-OFF, no shipped threshold, `config.example.json` clean; holds position-increasing signals only, fail-closed FLAT-ONLY; `metrics["hurst"]` ONLY from composite classifier; stays in `run_backtest.py` `stop_keys`.
- `llm_entry_analysis.go`: advisory-only; `spawnPythonProcess` NEVER `runPython*`; sole writer of `trade_diagnostics.llm_verdict`.
- `scale_in.go`: geometry frozen via `RiskAnchorPrice`, never blended `AvgCost`. `manual*.go`: kill-switch+CB gated; SL edits queue `PendingManualAction` (no bare book write; only re-arm/restore write it); rejected close never assumes unconfirmed cancel spared trigger; verify-first SL+cancelled-TP restore, NEVER 2nd placement path; unverified=CRITICAL; adopted SL edits never gate; `force-close` live HL perps only.
- `hyperliquid_liquidation_guard.go`: CLAMP, never refuse to arm; ONE-WAY TIGHTEN at 0.5% buffer; 0=unknown, never persisted; unclampable REFUSES; unreadable outcome keeps state. Boot `validateHLStopWithinBankruptcyBound` mirrors `LoadConfig` stop-owner resolution.
- `hyperliquid_protection.go`: reduce-only; on-chain TP needs `strategyUsesTieredTPATRClose`+live=`close_owner` marker+echo (no echo holds), NEVER nil `CloseStrategy`; evaluator fallback ONLY if nothing rests/armed AND tiers unplaceable. `SLAfterMoved`: kept trigger ONLY, tighter-only; lost=CRITICAL. `hyperliquid_shared_close_floor.go`: <$10 shared-coin close escalates ONLY if each peer flat on-chain (refetched)+in book, before SL blocks, else ONE alert+hold; `venue_rejected` never resends; re-arm (7 owners) same-cycle ONLY after SUBMITTED cancel; 2nd refusal holds, any wording; manual guard skips queued-row symbol; failed-close owner re-arms SL leg ONLY, 3 blocks=CRITICAL; unknown TP place=armed, no re-place. Operator floor (`decideOperatorSharedCloseFloor`): same test+side+no larger, else refuse (no hold); unreadable mark skips escalation.
- `version_probe.go`/`probe_cmd.go`: new runtime CLI flag>both probe argvs. `agent_info.go`: `--bootstrap-md`=`AGENTS.generated.md`, NEVER `AGENTS.md`. `failure_alerts.go`: wire notifier per new `run*Check`. `discord_*commands.go`: new mutating command: `opsCommandNames`+`slashCommands()`+dispatch.
- `shared_wallet*.go`: PRE-FEE `realized_pnl`, net via `tradeNetPnL*`. Pool budgeting: 2+ live HL/OKX perps omit capital fields, `margin_per_trade_usd`>0; allocated-pool flat-only.
- `kill_switch_limit_orders.go`: cancel each `pending_limit_orders` ROW pre-flatten; **never gate `reconcilePendingLimitOrders` on kill-switch**; cancel!=adoption, never auto-delete unadopted fill.
- `orphan_limit_cancel_alerts.go`: cancel-only lane, status-FIRST finalize, books NO fill; `orphanLimitCancelState`=SSoT (off-book fill=UNTRACKED POSITION). `limit_fill_exposure.go`: books limit fill ONLY once live exposure confirms; per-coin aggregate, never per-row greedy; fail-closed same-dir+contained; `unreadable`/`unbacked` refuse book+block delete.
- `shared_scripts/`: check scripts take `--regime-payload-json`, probed at start. `check_hyperliquid.py` close gate: live-only, lot-floored, never full close, no lot=no gate. `platforms/<name>/adapter.py`: one `*ExchangeAdapter`; HL `_sz_decimals()` via `name_to_asset`. `funding_fetcher.py`: `merge_asof` backward, DISJOINT `funding_coverage`; `regime.py` ATR `simple`.
- `shared_strategies/`: open SSoT `open/registry.py`; **`open/{spot,futures}/strategies.py`=shims, never edit.** Close: `close/registry.py` via `from close_registry_loader import ..`, never bare `import registry`. `hurst_exponent`=SSoT.
- HL tier parity: `tp_model: resting_limit` evaluators == `buildHyperliquidProtectionPlan` (SSoT `tp_tier_parity.json`); `close_tier_fill_price` paper-only; ladder load=`validateTPTierLadders`.

## Patterns
- Git from repo root; `go -C scheduler build .`, never `cd scheduler &&`.
- New platform: SKILL.md Custom Platform Integration touchpoints; adapters via `importlib`, class `endswith("ExchangeAdapter")`; check scripts use public methods.
- Subprocess: JSON on stdout, exit 1 on error; Go parses anyway.
- Locking: `mu RWMutex`, 6 phases (RLock>Lock(CheckRisk)>no-lock subprocess>Lock(execute)>marks>RLock(status)); symbol locks>`mu`. OUTSIDE `mu`: HL fill resolver, reconciliation-close alerts, `cashflow_journal.go`. Skip-reason checks BEFORE spawn; Phase 1 captures `posSide`+`posQty`; `liveExecFailed` guards live exec.
- Dispatch by `s.Platform`, never ID prefix. Perps paper=`ExecuteSpotSignalWithFillFee`, live=`RunHyperliquidExecute`; futures=`ExecuteFuturesSignalWithFillFee`.
- Single `CloseStrategy` owns exit; close before open; partial close keeps `InitialQuantity`, suppresses SL replace.
- `dueStrategies` value-copied: update `cfg.Strategies` first. Owner=`OwnerStrategyID`; shared-coin reconcile non-destructive; SL attribution by OID+qty, else `hl_sync_external`.
- Trades: `is_close`/`realized_pnl`; `#T` counts opens by `(strategy_id,position_id)`. HL kill-switch shared-coin fill split fails closed; close side short=buy else sell. Invert: composer (`invert_open_signal`+echo); Go never negates; same-side close zeroed.
- ALWAYS `sort.Strings(keys)` for operator/test output. Regime: `adx` default, `composite` opt-in; bare `ranging_directional` covers `_up`/`_down` for gating, certs exact-match.
- Registries: `open/registry.py`+`PLATFORM_ORDER`+`knownShortNames`+`DEFAULT_PARAM_RANGES`; `backtest_only=True` fail-closes live; snapshot `--list-json` first.
- CB disable suppresses new fires only; latched HL-perps manage-only (`Signal=0`, not `continue`). Kill switch: `planKillSwitchClose`>`OnChainConfirmedFlat`; reset prompt single-flight.
- HL stops (`EffectiveStopLossPct`): 7 exclusive owners (none=`DefaultStopLossATRMult=1.0`); scalar-regime swap blocked while open. `risk_per_trade_pct` fails closed on unresolvable stop, exclusive vs sizing_leverage/margin/scale_in. Trailing SL replace only past `TrailingStopMinMovePct`; `stop/TP Q=hlOwnStopShare`; snapshot=full protection surface. Peers share `margin_mode`+`leverage`; `update_leverage` if flat.
- SIGHUP `validateHotReloadCompatible` blocks add/remove, script/args/type/platform/HTFFilter, kill-switch identity, `db_file`/`paper_db_file`/`paper_source(s)`, effective `storage_strategy_id`, `max_notional_usd` (per partition), `market_feed`.
- New per-strategy flag: field, `run*Check` CLI, Python parse, InitOptions/wizard (+probe argvs if runtime-required).
- Notifications: `MultiNotifier`; paper routes via `resolveChannelKey`. `SendToPartitionChannels`: live=`SendToAllChannels`; paper=own roster (`resolveTradeChannel`, rebuilt on `ReloadConfig`), never suffix scan, `SendToAllChannels` only if empty.

## PRs and issues
- Title `type(#<N>): summary [C<score>, <model>, <effort>]`; never bare `#N` in lists. Body: `## Summary`+verification, `## Plain simple English` (<55 words) last.
- Commits, PR and issue bodies end `LLM: <model> | <effort> | Harness: <action>`, no `Co-authored-by`.
- Before merging a long-lived PR, diff `origin/main..HEAD` for reverts.
- Reviews also follow `.github/prompts/pr-review-format-local.md`, never gate on CI; findings restate as invariant, list breaking states (inverse, compound).
- `.github/workflows/claude.yml`: mode routing fail-closed (untrusted/fork=review); no-execution in agent; commit/push implement-only; prompt never holds `"`, `` ` ``, `$`; `.github/scripts/` keeps ONLY `test_workflow_logic.py`.
- rk-skills workflow skills=CI-only, no settings pin.

## Deploy
- **Update only with `bash scripts/update.sh --restart`. Never rebuild Go alone**: Go+Python share 1 argv contract per SHA; `update_resolve_db_exclude` lists all state files (incl. `paper_sources[].db_file`).
- Exit codes: probe 78, singleton 79, storage 80, units `RestartPreventExitStatus=78 79 80`. Ownership over ALL files (incl. `--once`) precedes migration/startup write; unit edits: `daemon-reload`.
- Post-update: SKILL.md Post-Update Agent Protocol; after Python-launcher change smoke `./go-trader --once` (daemon off).
- Scripts: root git via `update_git` (py: exact-tree `-c`), never `*`/config; foreign-owned tree: confined units, Go only from `git archive`, probe as owner; give-back fd-pinned, no hard links; root `uv sync` copy-mode; uv/go via `update_resolve_tool`.

## Backtest
- Harness map `docs/backtesting-registry.md`: adding/deprecating PR updates its row.
- `--config` needs `config_version>=15`; SL-vs-TP races default `ohlc_walk`.
- Look-ahead: bar N signal fills at N+1 open; regime gate reads N-1; closes use closed-bar ATR. **HTF series indexed by candle OPEN time MUST `.shift(1)` BEFORE `reindex(..,method="ffill")`.**
- Backtester rejects HL-live-only closes (`regime_window_divergence`/`tiered_tp_atr_live_regime_dynamic`); no options regime gating.
- M1-M6, auto_suggest, regime promotion, `tune_live.py`=SUGGEST-ONLY: **never write live defaults/config/PRs.**

## Testing
- **Unit tests only** where a run can't prove it or a regression is silent: rare venue states (partial fill, rejected/unknown order), money math (sizing, PnL, fees), paper/live parity, DB migrations, large refactors. Else run real binaries/scripts (build, `probe`, `--once`); PR lists commands+log lines per criterion.
- Kept test edit/removal: Outdated/Wrong/Obsolete with checkable ground, disclosed in commit+PR (`fix-pr-review` step 6); no ground=fix code.
- Kept suites pass: `go -C scheduler test ./...`, pytest `uv run --no-sync python -m pytest shared_strategies/ shared_tools/ backtest/`, `shared_scripts/test_*.py` by path, `scripts/test_*.sh`. CI `-n auto`: never bare-`import` ambiguous name. Go CI never spawns Python.
- `gofmt -w` after Go edits; tabbed Go: Python `replace(old,new,1)`.
- Entry ATR guard: `stampEntryATRIfOpened` rejects ATR>50% of AvgCost.
- `tiered_tp_atr`/`trailing_stop_atr_mult` need `Position.EntryATR`; `*_live` recompute via `atr_source`; `avwap_stop`=virtual exit only.
