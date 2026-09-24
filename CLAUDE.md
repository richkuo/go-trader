# go-trader

Guardrails only. Mechanism/flows: SKILL.md, docs/POST_UPDATE_HISTORY.md; <15k bytes, never split (root `CLAUDE.md` only; `AGENTS.md` symlink). Shorten wording, drop no guardrail.

## Env
- Go 1.26.2 (`/opt/homebrew/bin/go`). Python: `uv run --no-sync python`; scheduler `.venv/bin/python3`; `uv sync` per worktree.
- systemd units: `ProtectSystem=strict`, no `PATH`/`UV_CACHE_DIR` injection, secrets `/opt/go-trader/.env`, config `/var/lib/go-trader[/<instance>]/config.json`; `scheduler/config.json` = transition symlink.

## Priorities
- **Always the best solution.** Cost/compute/time/effort/code never narrow options; branch+PR, issue-claim vs code, destructive-action safety win.
- **Never give time/effort estimates.** Complexity=scope+risk
## Repo (`scheduler/` = one `package main`)
- `executor.go`/`shutdown.go`: side-effect wrappers = `runPythonSideEffect`, NEVER `runPython`. Live HL book needs `confirmHyperliquidExecuteFill` (finite `AvgPx>0`+`TotalSz>0` in `Execution.Fill`); `check_hyperliquid.py execute` exits 1 on no fill.
- `server.go`/`ui_*.go`: lock `mu > strategiesMu`; loopback only. `/tuning` never writes config; `ui_tuning.go` = `spawnPythonProcessWithEnv` (NEVER `runPython*`); `POST /api/tuning/apply` = sole promotion. `uiPartitionParam` = sole `?partition=` resolver (`/api/strategies[/overview|/dead]`, `/api/leaderboard`, `/api/diagnostics`); unparseable=400, unowned=404; diagnostics filter `SourceRole` before paging+total; cash flow `live_owned`; selected correlation/portfolio-risk NEVER fall back to untagged legacy field.
- `config*.go`: `CurrentConfigVersion=19`, `MinSupportedConfigVersion=13`; 7 exclusive HL stop fields (none = `DefaultStopLossATRMult=1.0`); `close_strategy` canonical, unknown-key guard. On-chain TP gate != `len(tiers)>0`. `CircuitBreaker *bool` ONLY via accessors. `portfolio_risk.paper`/`paper_sources[].portfolio_risk`: evaluators use `partitionRiskConfig`, nested `paper` rejected. `portfolio_risk.include_paused_in_warning` (default false, root>paper>source; false never overrides enabled layer): `portfolio_warning.go` drops paused strategy from Top Contributors ONLY if flat (`Quantity==0` regular AND option); open positions stay visible in all partitions.
- `close_defaults.go`: system>user>strategy, explicit `tp_tiers` wins; `applyUserCloseDefaultRatchetRegimeTrails` runs in `loadConfig` BEFORE scalar ATR-stop default.
- `portfolio_scope.go`/`risk_partition.go`: `PortfolioScope` from `isLiveArgs` = SOLE mode classifier; `RiskPartition{Scope,Source}` (`live`/`paper`/`paper:<id>`; `paper_source` = `paper_sources` id, never live) = SOLE risk+storage owner. New surface: `activePartitions`, `filterStatesByPartition`/`strategiesInPartition`, never roster.
- `state.go`/`db.go`: SQLite-only, idempotent migrations. `AppState.PortfolioRisk`/`CorrelationSnapshot` = per-`RiskPartition` maps read only via `partitionRisk[IfPresent]`/`partitionCorrelation`. `initial_capital` only via `SetInitialCapital`.
- `state_store*.go`/`storage_*.go`: identity map immutable; 1 pairwise-distinct file per partition; lock+save order primary>paper>sources by id. EVERY DB caller via `StateStore` (`dbForStrategy`, live-only `liveFile`); ids translate INSIDE `StateDB`; load maps (file,scope)->partition; diagnostics route by `SourceRole` NEVER scope. New table: SKILL.md Storage Ownership row. Manual acks by ROW ID in-tx, NEVER high-water mark; book+queued row = ONE tx; failing rows end; unknown ownership errs BEFORE mutation; combined reads fail whole.
- `merge-paper-instance.sh`: binaries on config COPIES; `inspect`/`storage-inspect` = `LoadConfigReadOnly` (no rewrite); both locks preflight>apply except `--diff` (config-only); `--align-to-live` writes `.aligned`, never source; compose lists ALL root-key conflicts, refuses; alias always via `paper_alias.py`; `--once`/`--probe-only` never proof; zero paper override inherits. `channels`/`trade_alert_channels` `-paper` keys skip if merged bare key routes there (= `resolveChannel`); `dm_channels` `-paper`/`-paper:<id>` NEVER skip (`tradeAlertRoutes` reads literal key, no fallback); `--diff` = `channel-plan` lines. `inspect` DM view = same exact-key lookup (`resolveDMKeyOverMaps`); proof refuses staged paper DM route the send path disagrees with.
- `risk.go`: `CheckRisk` skips `manual`. Corrupt position (qty or avgCost<=0) = zero-PnL `*_corrupt` leg, cash untouched. Latch: ONE owner per cycle+partition; `DrawdownReadingSubstituted` always labelled; untrusted over-limit defers, never vetoes. Paper `equityTrusted` always true. `ResetPortfolioKillSwitchManual` = sole DM reset; `AutoResetConfirmedFlatKillSwitch`/`ClearLatchedKillSwitchSharedWallet` `ScopeLive` only.
- `daily_loss.go`: hold-only, UNLATCHED pure read, PRE-FEE realized PnL, never force-closes, per partition; new `portfolio_risk` gates copy it.
- `exposure_cap.go`: blocking-only, direction-aware; one exposure model `computeAssetDeltas` (`ComputeCorrelation` too). `notional_cap.go`: hold-only via `pausedBlocksSignal`, never skips cycle, restart-required.
- `replay_log.go`/`replay_mirror.go`: DEFAULT-OFF, HL perps, flat-only hot-reload, 1 mirror/source (`replayMirrorSourceID`).
- `hl_batch.go`: shared-state failure = same-cycle per-strategy fallback, never blank close/SL/ratchet/protection/hedge; `GO_TRADER_HL_BATCH=0` disables. `market_feed=websocket`: every check path reads one sealed stdin `marketSnapshot`; missing frame=error, NEVER private fetch.
- `hyperliquid_fills.go`: resolver built OUTSIDE `mu.Lock`; `HLFillLookup.Px`=VWAP; `ClosedPnLGross` never into `Trade.RealizedPnL`; unconfirmed SL fills=gaps, never books.
- `hyperliquid_balance.go`: reconciliation-close alerts outside `mu`; hedge-leg rows never public. `pause.go`: paused is NOT `dueStrategies` skip; `pausedBlocksSignal` holds position-increasing signals at all 6 regime-gated dispatch sites (+per-scope persistence hold); closes/trailing SL/ratchet/protection pass.
- Regime (`regime*.go`): stamp all 5 execute dispatches; strip `sl_after` before ATR parse; `regime_unified.go` owns SL; directional policy DEFAULT-OFF; profile allocation flat-only; dynamic/divergence HL-live-only. Trade-alert span = bundle spec (`tradeAlertRegimeConfig`): `regime.timeframe` else `Args[2]`, NEVER `sc.Timeframe`; options = own fixed spec (window+period+tf), never `cfg.Regime`.
- `trailing_tp_ratchet.go`: NO on-chain TP; HL perps+`manual`. `hedge.go`: HL perps only; ONE reconciler (`hedgeTargetDecision`+`runHedgeSync`); ownership ONLY `Position.HedgeFor`; PnL via `RecordHedgeTradeResult`, never `RecordTradeResult`; fail-closed unwind+CRITICAL DM. Both hot-reload-blocked while open; hedge backtester-rejected.
- `hurst_gate.go`: DEFAULT-OFF, no shipped threshold, `config.example.json` clean; holds only position-increasing signals, fail-closed FLAT-ONLY; `metrics["hurst"]` ONLY from composite classifier; stays in `run_backtest.py` `stop_keys`.
- `llm_entry_analysis.go`: advisory-only; `spawnPythonProcess` NEVER `runPython*`; sole writer of `trade_diagnostics.llm_verdict`.
- `scale_in.go`: geometry frozen via `RiskAnchorPrice`, never blended `AvgCost`. `manual*.go`: kill-switch+CB gated; SL edits queue `PendingManualAction` (never bare book write; only re-arm/restore write it); rejected close never assumes unconfirmed cancel spared trigger; verify-first SL+cancelled-TP restore, NEVER 2nd placement path; unverified=CRITICAL; adopted SL edits never gate; `force-close` live HL perps only.
- `hyperliquid_liquidation_guard.go`: CLAMP, never refuse to arm; ONE-WAY TIGHTEN at 0.5% buffer; 0=unknown, never persisted; unclampable REFUSES; unreadable outcome keeps state. Boot `validateHLStopWithinBankruptcyBound` mirrors `LoadConfig` stop-owner resolution.
- `hyperliquid_protection.go`: reduce-only, on-chain TP needs `strategyUsesTieredTPATRClose`+live (paper never) = `close_owner` marker+echo (missing echo holds), NEVER nil `CloseStrategy`; evaluator fallback ONLY if nothing rests/armed AND tiers unplaceable; `hyperliquid_open_trailing.go` arms SL at open. `hyperliquid_shared_close_floor.go` cycle: <$10 shared-coin close escalates ONLY if each peer flat on-chain (refetched)+in book, before SL blocks, else ONE alert+hold; `venue_rejected` never resends; re-arm (7 owners) same-cycle ONLY after SUBMITTED cancel; 2nd refusal holds, any wording; manual guard skips queued-row symbol; failed-close owner re-arms SL leg ONLY, 3 blocks=CRITICAL; unknown TP place=armed, never re-placed. Operator floor (`decideOperatorSharedCloseFloor`): same test+side+no larger, else refuse (not hold); unreadable mark skips escalation.
- `version_probe.go`/`probe_cmd.go`: new runtime CLI flag > both probe argvs. `agent_info.go`: `--bootstrap-md` = `AGENTS.generated.md`, NEVER `AGENTS.md`. `failure_alerts.go`: wire notifier per new `run*Check`. `discord_*commands.go`: new mutating command: `opsCommandNames`+`slashCommands()`+dispatch.
- `shared_wallet*.go`: PRE-FEE `realized_pnl`, net via `tradeNetPnL*`. Pool budgeting: 2+ live HL/OKX perps omit capital fields, positive `margin_per_trade_usd`; allocated-pool flat-only. `cashflow_journal.go` OUTSIDE `mu`.
- `kill_switch_limit_orders.go`: cancel each `pending_limit_orders` ROW BEFORE flatten; **never gate `reconcilePendingLimitOrders` on kill-switch**; cancel != adoption, never auto-delete unadopted fill.
- `orphan_limit_cancel_alerts.go`: cancel-only lane, status-FIRST finalize, books NO fill; `orphanLimitCancelState`=SSoT (off-book fill=UNTRACKED POSITION). `limit_fill_exposure.go`: books limit fill ONLY once live exposure confirms; per-coin aggregate, never per-row greedy; fail-closed same-dir+contained; `unreadable`/`unbacked` refuse book+block delete.
- `shared_scripts/`: check scripts take `--regime-payload-json`, probed at start. `check_hyperliquid.py` close gate: live-only, lot-floored, never full close, no lot=no gate. `platforms/<name>/adapter.py`: one `*ExchangeAdapter`; HL `_sz_decimals()` via `name_to_asset`. `funding_fetcher.py`: `merge_asof` backward, DISJOINT `funding_coverage`; `regime.py` ATR `simple`.
- `shared_strategies/`: open SSoT `open/registry.py`; **`open/{spot,futures}/strategies.py` = shims, never edit.** Close: `close/registry.py` via `from close_registry_loader import ..`, never bare `import registry`. `hurst_exponent` (DFA) = SSoT.
- HL tier parity: `tp_model: resting_limit` evaluators == `buildHyperliquidProtectionPlan` (SSoT `tp_tier_parity.json`); `close_tier_fill_price` paper-only; ladder load = `validateTPTierLadders`.

## Patterns
- Git from repo root; `go -C scheduler build .`, never `cd scheduler &&`.
- New platform: SKILL.md Custom Platform Integration touchpoints; adapters via `importlib`, class `endswith("ExchangeAdapter")`; check scripts: public methods.
- Subprocess: JSON on stdout, exit 1 on error; Go parses regardless.
- Locking: `mu sync.RWMutex`, 6 phases (RLock > Lock(CheckRisk) > no-lock subprocess > Lock(execute) > marks > RLock(status)); symbol locks > `mu`. Skip-reason checks BEFORE spawn; Phase 1 captures `posSide`+`posQty`; `liveExecFailed` guards live exec.
- Dispatch by `s.Platform`, never ID prefix. Perps paper=`ExecuteSpotSignalWithFillFee`, live=`RunHyperliquidExecute`; futures=`ExecuteFuturesSignalWithFillFee`.
- Single `CloseStrategy` owns exit; close before open; partial close keeps `InitialQuantity`, suppresses SL replace.
- `dueStrategies` value-copied: update `cfg.Strategies` first. Ownership via `OwnerStrategyID`; shared-coin reconcile non-destructive; SL attribution by OID+qty, else `hl_sync_external`.
- Trades: `is_close`/`realized_pnl`; `#T` counts opens by `(strategy_id, position_id)`. HL kill-switch shared-coin fill split fails closed; close side short=buy else sell.
- Map iteration: ALWAYS `sort.Strings(keys)` for operator/test output. Regime: `adx` default, `composite` opt-in; bare `ranging_directional` covers `_up`/`_down` for gating, certs exact-match.
- Registries: `open/registry.py`+`PLATFORM_ORDER`+`knownShortNames`+`DEFAULT_PARAM_RANGES`; `backtest_only=True` fail-closes live, snapshot `--list-json` first.
- CB disable suppresses new fires only; latched HL-perps manage-only (`Signal=0`, not `continue`). Kill switch: `planKillSwitchClose` > `OnChainConfirmedFlat`; reset prompt single-flight.
- HL stops (`EffectiveStopLossPct`): scalar-regime swap blocked while open. `risk_per_trade_pct` fails closed on unresolvable stop; exclusive vs sizing_leverage/margin/scale_in. Trailing SL replace only past `TrailingStopMinMovePct`; `hlSLEffectiveQty=min(virtual,onChain)`; snapshot = full protection surface. Peers share `margin_mode`+`leverage`; `update_leverage` when flat.
- SIGHUP `validateHotReloadCompatible` blocks add/remove, script/args/type/platform/HTFFilter, kill-switch identity, `db_file`/`paper_db_file`/`paper_source[s]`, effective `storage_strategy_id`, `max_notional_usd` (per partition), `market_feed`.
- New per-strategy flag: field, `run*Check` CLI, Python parse, InitOptions/wizard; runtime-required: probe argvs.
- Notifications: `MultiNotifier`; paper routes via `resolveChannelKey` (`<platform>-paper:<id>` > `-paper` > bare). `SendToPartitionChannels`: each partition's destinations from its own roster (`resolveTradeChannel`, rebuilt on `ReloadConfig`), never suffix scan; `SendToAllChannels` only if that set is empty.

## PRs
- `Closes #<N>` in body; never bare `#N` in lists. Title `type(#<N>): summary [C<score>, <model>, <effort>]` (`, fableplan` if Fable planned). Body: `## Plain simple English` (<55 words), then `## Summary` + verification.
- Commits, PR and issue bodies end `LLM: <model> | <effort> | Harness: <action>`, no `Co-authored-by`.
- Bot reviews land on issue-comments endpoint; before merging long-lived PR diff `origin/main..HEAD` for reverts.
- Review format: rk-skills `pr-review-format.md`+`.github/prompts/pr-review-format-local.md`, reviews never gate on CI.
- Review findings: restate as invariant, list breaking states (inverse, compound).
- `.github/workflows/claude.yml`: mode routing fail-closed (untrusted/fork = review); no-execution in agent; commit/push implement-only; prompt never holds `"`, `` ` ``, `$`; `.github/scripts/` keeps ONLY `test_workflow_logic.py`.

## Issues
- `gh issue create`, title `[C<0-100>] <title>`; body line 1 `**Complexity: N/100** - scope; risk; uncertainty` (money/data/protection risk > time). rk-skills workflow skills = CI-only, no settings pin.

## Deploy
- **Update only with `bash scripts/update.sh --restart`. Never rebuild Go alone**: Go+Python share 1 argv contract per SHA; `update_resolve_db_exclude` lists all state files (incl. `paper_sources[].db_file`).
- Exit codes: probe 78, singleton 79, storage 80; units set `RestartPreventExitStatus=78 79 80`. Ownership over ALL files (incl. `--once`) precedes migration/startup write; unit edits: `daemon-reload`.
- Post-update: SKILL.md Post-Update Agent Protocol; after Python-launcher change smoke `./go-trader --once` (daemon stopped).

## Backtest
- Harness map `docs/backtesting-registry.md`: update its row in adding/deprecating PR.
- `--config` gates on `config_version>=15`; entry ATR guard 50% of AvgCost, SL-vs-TP races default `ohlc_walk`.
- Look-ahead: bar N signal fills at N+1 open; regime gate reads N-1, closes use closed-bar ATR. **HTF series indexed by candle OPEN time MUST `.shift(1)` BEFORE `reindex(..,method="ffill")`.**
- Backtester rejects HL-live-only closes (`regime_window_divergence`, `tiered_tp_atr_live_regime_dynamic`); options regime gating unsupported.
- M1-M6, auto_suggest, regime promotion, `tune_live.py` = SUGGEST-ONLY: **never write live defaults, config or PRs.**

## Testing
- **Never write unit tests, except** where a run can't prove it or a regression is silent: rare venue states (partial fill, rejected/unknown order), money math (sizing, PnL, fees), paper/live parity, DB migrations, large refactors. Else verify by running real binaries/scripts (build, `probe`, `--once`, local runs); PR lists commands+log lines per criterion. Suites #1597 kept stay.
- Touching a kept test: Outdated/Wrong/Obsolete with checkable ground, disclosed in commit+PR (`fix-pr-review` step 6); no ground = fix code.
- Kept suites pass: `go -C scheduler test ./...`, pytest `uv run --no-sync python -m pytest shared_strategies/ shared_tools/ backtest/`, `shared_scripts/test_*.py` by path, `scripts/test_*.sh`. CI `-n auto`: never bare-`import` ambiguous name. Go CI never spawns Python.
- `gofmt -w` after Go edits; tabbed Go: Python `replace(old,new,1)`.
- `stampEntryATRIfOpened` rejects ATR>50% of AvgCost.
- `tiered_tp_atr`/`trailing_stop_atr_mult` need `Position.EntryATR`; `*_live` recompute via `atr_source`; `avwap_stop` = virtual exit only.
