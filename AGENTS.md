Generated file. Do not edit it. Edit CLAUDE.md and the .claude/rules files it lists, then run `python3 -I scripts/agent_guide.py`.

# go-trader

Guardrails only;mechanism:SKILL.md,docs/POST_UPDATE_HISTORY.md.Sources:this file+the `.claude/rules/` files listed below;`AGENTS.md`=generated full guide for Codex/Cursor,never hand-edit.
After any source edit run `python3 -I scripts/agent_guide.py`,verify with `bash scripts/check_pre_pr.sh agent-docs`.Limits:this file<17500B(target 17000),rule file<=8000B,any line<=400B,`AGENTS.md`<=28000B.Shorten,drop no guardrail.

## Agent guide contract
- Generated: `AGENTS.md`
- Sources: `CLAUDE.md`, `.claude/rules/*.md`
- Regenerate: `python3 -I scripts/agent_guide.py`
- Verify: `bash scripts/check_pre_pr.sh agent-docs`
- Command files: `scripts/agent_guide.py`, `scripts/check_pre_pr.sh`

## Area rules
- Claude Code loads each file below only when it reads or edits a file in that rule's `paths:`.In a subagent,a worktree,after compaction,or before creating a file in an area,Read that rule file first.
- `.claude/rules/backtest.md`:backtest harness,`shared_strategies/`,`shared_tools/`,registry
- `.claude/rules/deploy.md`:update,layout and feed conversion scripts,units
- `.claude/rules/discord.md`:Discord commands,config
- `.claude/rules/hedge.md`:hedge legs (every scheduler file that names a hedge)
- `.claude/rules/market-feed.md`:market feed,HL info reads,`shared_scripts/`
- `.claude/rules/merge-paper.md`:paper merge,inspect,channel routing
- `.claude/rules/robinhood.md`:Robinhood session
- `.claude/rules/ui.md`:UI server,tuning,partition reads
- `.claude/rules/workflows.md`:`.github/`

## Env
- Go 1.26.2.Python:`uv run --no-sync python`;scheduler `.venv/bin/python3`;`uv sync`/worktree.systemd:`ProtectSystem=strict`,no `PATH`/`UV_CACHE_DIR` injected,secrets `/opt/go-trader/.env`,config `/var/lib/go-trader[/<instance>]/config.json`;`scheduler/config.json`=transition symlink

## Priorities
- Best solution:cost/compute/time/effort/code never narrow;branch+PR,issue-claim vs code,destructive-action safety win.ETAs:runs only;complexity=scope+risk

## Repo (`scheduler/*.go`=`main`)
- Side effects via `runPythonSideEffect*`,NEVER `runPython`.Live HL book needs `confirmHyperliquidExecuteFill`(finite `AvgPx>0`+`TotalSz>0`);`check_hyperliquid.py execute` exits 1 on no fill.Live RH book ONLY from settled `robinhood_order_attempts` fill(row pre-send,1 unresolved/coin,unknown=hold);flat ONLY via fresh strict read;sells<=`quantity_available`,emergency<=book
- `planHLCloseOrder`=SOLE size+mode for SIZED live closes(execute/`type=manual`/force-close/hedge/open cleanup,not whole/ForceFullClose);refetch fail=uncapped reduce_only;short fill books fill.Rearm ONLY `resolveHLCloseRemainderStop`;Q=0 verify-first cancels unconfirmed SL/TP;ONE CRITICAL;`order_outcome`=SOLE outcome
- UI:lock `mu>strategiesMu`,loopback only(container wildcard+token).
- `config*.go`:`close_strategy` canonical,unknown-key guard.
  - On-chain TP gate!=`len(tiers)>0`.
  - `CircuitBreaker *bool` via accessors ONLY.
  - `portfolio_risk.paper`/`paper_sources[].portfolio_risk`:evaluators via `partitionRiskConfig`;nested `paper` rejected.
  - `portfolio_warning.go`:paused off Top Contributors ONLY if flat(`Quantity==0` regular+option).
  - Live `type: options` refused everywhere(`optionsLiveModeConfigError`)
- `close_defaults.go`:system>user>strategy;explicit `tp_tiers` wins;`applyUserCloseDefaultRatchetRegimeTrails` in `loadConfig` BEFORE scalar ATR-stop default
- `PortfolioScope` from `isLiveArgs`=SOLE mode classifier;`RiskPartition{Scope,Source}`(`live`/`paper`/`paper:<id>`;`paper_source`=`paper_sources` id,never live)=SOLE risk+storage owner.New surface:`activePartitions`,`filterStatesByPartition`/`strategiesInPartition`,never roster.Paper nice ONLY `checkChildPriority`/`hlBatchGroupPriority`;CPU drop-in ONLY `paper_only`
- State:SQLite-only,idempotent migrations.`AppState.PortfolioRisk`/`CorrelationSnapshot`=per-`RiskPartition` maps,read only via `partitionRisk(IfPresent)`/`partitionCorrelation`;`initial_capital` only via `SetInitialCapital`
- Storage:identity map immutable;1 distinct file/partition;lock+save primary>paper>sources by id.
  - EVERY DB caller via `StateStore`(`dbForStrategy`,live-only `liveFile`);ids translate INSIDE `StateDB`;load maps(file,scope)->partition;diagnostics route by `SourceRole`,NEVER scope.
  - New table=SKILL.md Storage Ownership row.
  - Manual acks by ROW ID in-tx,NEVER high-water;book+queued row=ONE tx;failing rows end;unknown ownership errs BEFORE mutation;combined reads fail whole
- `risk.go`:`CheckRisk` skips `manual`.
  - Corrupt pos(qty/avgCost<=0)=zero-PnL `*_corrupt` leg,cash untouched.
  - Latch:ONE owner/cycle+partition;`DrawdownReadingSubstituted` always labelled;untrusted over-limit defers,never vetoes.
  - Paper `equityTrusted`=true.
  - `ResetPortfolioKillSwitchManual`=sole DM reset,applied ONLY at cycle boundary(prompt code+latch instance);`AutoResetConfirmedFlatKillSwitch` live+paper;`ClearLatchedKillSwitchSharedWallet` `ScopeLive` only.
  - Every reset keeps peak+sets `PeakRebaselinePending`;new peak ONLY first eligible reading(`scopePeakRebaselineEligible`),ONE `SavePartition` tx;new latch cancels;pending blocks peak raise(zero peak still seeds)+basis migrations.
  - `daily_loss.go`:hold-only,UNLATCHED pure read,PRE-FEE realized PnL,never force-closes,per partition;new `portfolio_risk` gates copy it.
  - `exposure_cap.go`:blocking-only,direction-aware;1 exposure model `computeAssetDeltas`(+`ComputeCorrelation`).
  - `notional_cap.go`:hold-only via `pausedBlocksSignal`,never skips cycle,restart-required.
  - `replay_{log,mirror}.go`:DEFAULT-OFF,HL perps,flat-only hot-reload,1 mirror/source
- `hl_batch.go`:shared-state failure=same-cycle per-strategy fallback,never blank close/SL/ratchet/protection/hedge.Missing `closed_bar_decision`/`htf_filter` echo=script error,holds signal
- `migrate-service-layout.py`/`shared-feed-convert.sh`:opt-in,NEVER run by update.Paper HL book changes ONLY via `mutatePaperPerpsBook`;never `DailyPnL`/CB/losses;unresolved=held,NEVER booked 0.`HLFillLookup.Px`=VWAP;`ClosedPnLGross` never into `Trade.RealizedPnL`;unconfirmed SL fills=gaps,never books
- `pause.go`:paused!=`dueStrategies` skip;`pausedBlocksSignal` holds pos-increasing at all 6 regime-gated sites(+per-scope persistence hold,HTF `unavailable`);closes/trailing SL/ratchet/protection pass
- `regime*.go`:stamp all 5 execute dispatches;strip `sl_after` pre ATR parse;`regime_unified.go` owns SL;directional policy DEFAULT-OFF;`regime_side` flat-start,live cert|ack,cert gate probe+changed-reload ONLY,echo-gated;profile allocation flat-only;dynamic/divergence HL-live only.
  - Trade-alert span=bundle spec(`tradeAlertRegimeConfig`):`regime.timeframe` else `Args[2]`,NEVER `sc.Timeframe`;options own fixed spec,never `cfg.Regime`
- `trailing_tp_ratchet*`:NO on-chain TP;HL perps+`manual`.
- `trailing_tp_ratchet*`/`hedge.go`:Both reload-blocked while open;hedge backtester-rejected.
- `hurst_gate.go`:DEFAULT-OFF,no shipped threshold;holds pos-increasing only,fail-closed FLAT-ONLY;`metrics["hurst"]` ONLY from composite classifier;stays in `run_backtest.py` `stop_keys`.
- LLM entry analysis:advisory-only;`spawnPythonProcess` NEVER `runPython*`;sole `trade_diagnostics.llm_verdict` writer.
- Trade marks(`trade_review*.go`,`trade_feedback_analysis*.go`):append-only,no config write;recheck under `configWriteMu`;paid call never retried.
- `paper_close.go`:per-platform marks,never `AvgCost`.
- `removeStrategyGuarded` refuses with pos rows,unpaused,mirror,queued row;`tradeActionMu`>`configWriteMu`>`mu`
- `scale_in.go`:geometry frozen via `RiskAnchorPrice`,never blended `AvgCost`.
- `manual*.go`:kill-switch+CB gated;SL edits queue `PendingManualAction`(book written ONLY by re-arm/restore);rejected close never assumes unconfirmed cancel spared trigger;verify-first SL+cancelled-TP restore,NEVER 2nd placement path;unverified=CRITICAL;adopted SL edits never gate;`force-close` live HL perps only.
- Paper manual(`portfolioScopeFor`):venue cmds+dashboard refuse pre-state via `manualPaperVenueActionRefusal`;record-only+paper-close book;cycle close=`queuePaperManualCycleClose`,never an order.HL writes need `hlWriteAuth`(zero=deny):strategy/hedge/account-recovery ONLY.`paper_only`=no side-effect write;read-only launch never carries write argv
- Paper manual limit row(`paperManualLimitRowOwner`):ALWAYS due for orphan cancel lane,live script ONLY;no live script=`orphanLimitStateUnscripted`+`operator_required_since` once(clear:`manual-clear-limit-row --flattened`);startup WARN+DM per row
- CLI `manual-open`/`manual-add` order:`admitManualEntry` pre-store in ALL feed modes,owner marker+witnesses or standalone locks,else REFUSE;waited regime pending rechecks gates.
- Liquidation guard:CLAMP,never refuse to arm;ONE-WAY TIGHTEN at 0.5% buffer;0=unknown,never persisted;unclampable REFUSES;unreadable outcome keeps state.
- Boot `validateHLStopWithinBankruptcyBound` mirrors `LoadConfig` stop-owner resolution
- HL protection:reduce-only;on-chain TP needs `strategyUsesTieredTPATRClose`+live=`close_owner` marker+echo(no echo=hold),NEVER nil `CloseStrategy`;evaluator fallback ONLY if nothing rests/armed+tiers unplaceable.
  - `SLAfterMoved`:kept trigger ONLY,tighter-only;lost=CRITICAL.
  - Shared floor:<$10 shared-coin close escalates ONLY if every peer flat on-chain(refetched)+in book,pre SL blocks,else 1 alert+hold;`venue_rejected` never resends;re-arm(7 owners) same-cycle ONLY after SUBMITTED cancel;any 2nd refusal holds;manual guard skips queued-row symbol;failed-close owner re-arms SL leg ONLY,3 blocks=CRITICAL;unknown TP place=armed,no re-place.
  - Operator floor(`decideOperatorSharedCloseFloor`):same test+side+no larger,else refuse,no hold;unreadable mark skips escalation
- `probeArgv`/`probeCompositeArgv`:new runtime CLI flag>both.
- `--bootstrap-md`=`AGENTS.generated.md`,NEVER `AGENTS.md`.
- Wire failure notifier per new `run*Check`.
- Shared wallet:PRE-FEE `realized_pnl`,net via `tradeNetPnL*`.
- Pool budgeting:2+ live HL/OKX perps omit capital fields,`margin_per_trade_usd`>0;allocated-pool flat-only.
- Kill switch:`planKillSwitchClose`>`OnChainConfirmedFlat`;reset prompt single-flight;cancel each `pending_limit_orders` ROW pre-flatten;never gate `reconcilePendingLimitOrders` on kill-switch;cancel!=adoption,never auto-delete unadopted fill.
- Orphan limit cancel:cancel-only lane,status-FIRST finalize,books NO fill;`orphanLimitCancelState`=SSoT(off-book fill=UNTRACKED POSITION).
- `limitFillExposure*`:books limit fill ONLY once live exposure confirms;per-coin aggregate,never per-row greedy;fail-closed same-dir+contained;`unreadable`/`unbacked` refuse book+block delete
- `shared_scripts/`:check scripts take `--regime-payload-json`,probed at start.Lot gate(floor,never full close):live partial=`check_hyperliquid.py`,no lot=no gate;HL perps paper(+opens/adds)=Go,sub-min/no lot=HOLD+no write+quiet cycle.`platforms/<name>/adapter.py`:1 `*ExchangeAdapter`,platform-qualified imports;option types=`shared_tools/option_types.py`;HL `_sz_decimals()` via `name_to_asset`.
- `shared_strategies/`:open SSoT `open/registry.py`;`open/{spot,futures}/strategies.py`=shims,never edit.Close:`close/registry.py` via `from close_registry_loader import ..`,never bare `import registry`.HL tier parity:`tp_model: resting_limit` evaluators=`buildHyperliquidProtectionPlan`(SSoT `tp_tier_parity.json`);`close_tier_fill_price` paper-only;ladder load=`validateTPTierLadders`.

## Patterns
- Git from repo root;`go -C scheduler build .`.New platform:SKILL.md Custom Platform Integration;per-strategy flag:Implementation Patterns;check scripts use public methods
- Subprocess:stdout JSON,exit 1 on error;Go parses anyway.`egress*.go`:proxy mode attests first,no direct fallback,no creds in output
- Locking:`mu RWMutex`,6 phases(RLock>Lock(CheckRisk)>no-lock subprocess>Lock(execute)>marks>RLock(status));symbol locks>`mu`.
  - OUTSIDE `mu`:HL fill resolver,recon-close alerts,`cashflow_journal.go`.
  - Skip-reason checks BEFORE spawn;Phase 1 captures `posSide`+`posQty`;`liveExecFailed` guards live exec.
  - Dispatch by `s.Platform`,never ID prefix.
  - HL live=`RunHyperliquidExecute`;futures=`ExecuteFuturesSignalWithFillFee`
- Single `CloseStrategy` owns exit;close before open;partial close keeps `InitialQuantity`,suppresses SL replace.
- `dueStrategies` value-copied:update `cfg.Strategies` first.
- Owner=`OwnerStrategyID`;shared-coin reconcile non-destructive;SL attribution by OID+qty,else `hl_sync_external`.
- Trades:`is_close`/`realized_pnl`;`#T` counts opens by `(strategy_id,position_id)`.
- HL kill-switch shared-coin fill split fail-closed;close side short=buy else sell.
- Invert:composer(`invert_open_signal`+echo);Go never negates;same-side close zeroed.
- ALWAYS `sort.Strings(keys)` for output.
- Regime:`adx` default,`composite` opt-in;bare `ranging_directional` covers `_up`/`_down` for gating,certs exact-match
- Registries:every SKILL.md Add Or Change Strategies step;`no_edge`:1 `--mode=paper`,else `allow_no_edge:true`;snapshot `--list-json` first.CB disable suppresses new fires only;latched HL-perps manage-only(`Signal=0`,not `continue`)
- HL stops(`EffectiveStopLossPct`):7 exclusive owners(none=`DefaultStopLossATRMult=1.0`);scalar-regime swap blocked while open.`risk_per_trade_pct`:unresolvable stop=fail-closed,exclusive vs sizing_leverage/margin/scale_in.Trailing SL replace only past `TrailingStopMinMovePct`;`stop/TP Q=hlOwnStopShare`;snapshot=full protection surface.Peers share `margin_mode`+`leverage`;`update_leverage` if flat
- SIGHUP `validateHotReloadCompatible` blocks add/remove,script/args/type/platform/HTFFilter,kill-switch identity,`(paper_)db_file`/`paper_source(s)`,effective `storage_strategy_id`,`max_notional_usd`/partition,`market_feed`,`egress`,live `allow_no_edge`.
- `SendToPartitionChannels`:live=`SendToAllChannels`;paper=own roster(`resolveTradeChannel`,rebuilt on `ReloadConfig`),never suffix scan,`SendToAllChannels` only if empty

## PRs/issues
- No bare `#N` in lists;verification after `## Summary`.Bodies end `LLM: <model> | <effort> | Harness: <action>`,no `Co-authored-by`
- Long-lived PR:diff `origin/main..HEAD` for reverts.Reviews follow `.github/prompts/pr-review-format-local.md`,never gate on CI;findings restate as invariant,list breaking states(inverse,compound)

## Deploy
- Update only with `bash scripts/update.sh --restart`.
- Never rebuild Go alone:Go+Python:1 argv contract/SHA;`update_resolve_db_exclude` lists all state files(+`paper_sources[].db_file`).
- Container:image-only;mode via `GO_TRADER_RUNTIME` ONLY;restart=drain+save+exit.
- Exit codes(`RestartPreventExitStatus`):probe 78(never from `hyperliquid_agent_status.go`),singleton 79,storage 80.
- Ownership over ALL files(+`--once`) precedes migration/startup write;unit edits:`daemon-reload`
- Post-update:SKILL.md Post-Update Agent Protocol;Python-launcher change:smoke `--once`(daemon off).Failed update restores `update_take_snapshot` copy,never `git reset --hard`

## Backtest
- Look-ahead:bar N signal fills at N+1 open;regime gate reads N-1;closes use closed-bar ATR.
- HL perps backtests `--funding charge`;incomplete=refuse save,block promotion.Backtester rejects HL-live-only closes(`regime_window_divergence`/`tiered_tp_atr_live_regime_dynamic`);no options regime gating.M1-M6,auto_suggest,regime promotion,`tune_live.py`=SUGGEST-ONLY:never write live defaults/config/PRs.

## Testing
- Unit tests only if no run proves it or regression silent:rare venue states(partial/rejected/unknown),money math(sizing/PnL/fees),paper/live parity,DB migrations,large refactors.
  - Deribit/OKX/Luno/TopStep:no venue tests.
  - Else run real binaries/scripts(build/`probe`/`--once`);PR lists commands+log lines/criterion.
  - Kept test edit/removal:Outdated/Wrong/Obsolete+checkable ground,disclosed in commit+PR;no ground=fix code
- Kept suites+`bash scripts/check_pre_pr.sh` pass:`go -C scheduler test ./...`(`-tags pyintegration`=SOLE Python-spawning),pytest `shared_{strategies,tools}/ backtest/`,`shared_scripts/test_*.py` by path.CI `-n auto`:never bare-`import` ambiguous name
- Tabbed Go:Python `replace(old,new,1)`.`tiered_tp_atr`/`trailing_stop_atr_mult` need `Position.EntryATR`;`*_live` recompute via `atr_source`;`avwap_stop`=virtual exit only

# Area rules

Claude Code loads each area below as a path-scoped rule when it reads or edits a file in its scope. Other agents apply each area to the same files.

## Backtest

Source .claude/rules/backtest.md. Scope: backtest/**, shared_strategies/**, shared_tools/**, docs/backtesting-registry.md


- `funding_fetcher.py`:`merge_asof` backward,DISJOINT `funding_coverage`;in-coverage gaps refill by strict download only,`funding_venue_gaps`=venue-absent(charge 0),failed refill writes nothing;`regime.py` ATR `simple`
- `ledger_compare.py`:sim tier close needs winning `resting_fill`;same costs
- Harness map `docs/backtesting-registry.md`:add/deprecate PR updates row.
- SL-vs-TP races default `ohlc_walk`
- HTF series keyed by candle OPEN time MUST `.shift(1)` BEFORE `reindex` ffill.

## Deploy scripts

Source .claude/rules/deploy.md. Scope: scripts/update.sh, scripts/update_*, scripts/install-service.sh, scripts/egress-install.sh, scripts/migrate-service-layout.py, scripts/shared-feed-convert.sh, scripts/test_migrate_service_layout_fixture.sh, scripts/test_update_helpers.sh, systemd/**


- `migrate-service-layout.py`/`shared-feed-convert.sh` Layout:enable-intent target authoritative,recovery NEVER restarts source from stale snapshot;feed:failed switch restores byte copy.
- Scripts:root git via `update_git`(py:exact-tree `-c`),never `*`/config;foreign-owned tree:confined units,Go only from `git archive`,probe as owner;give-back fd-pinned,no hardlinks;root `uv sync` copy-mode;uv/go via `update_resolve_tool`.

## Discord commands

Source .claude/rules/discord.md. Scope: scheduler/discord*.go, scheduler/config*.go


- `discord_*commands.go`:new mutating cmd:`opsCommandNames`+`slashCommands()`+dispatch;sync only `go-trader-`,no bulk PUT;`slash_commands` DEFAULT-OFF;cmd calls ONLY daemon worker(no `mu`/retry).

## Hedge

Source .claude/rules/hedge.md. Scope: scheduler/hedge*.go, scheduler/config.go, scheduler/config_reload.go, scheduler/config_unknown_keys.go, scheduler/db.go, scheduler/discord_commands.go, scheduler/hl_perps_step.go, scheduler/hl_write_auth.go, scheduler/hyperliquid_balance.go, scheduler/hyperliquid_fills.go, scheduler/hyperliquid_liquidation_guard.go, scheduler/hyperliquid_own_share.go, scheduler/hyperliquid_paper_funding_eligibility.go, scheduler/hyperliquid_share_resize.go, scheduler/hyperliquid_step_trade_alerts.go, scheduler/inspect_cmd.go, scheduler/kill_switch_close.go, scheduler/main.go, scheduler/manual.go, scheduler/manual_core.go, scheduler/manual_protection.go, scheduler/model_only_reconcile.go, scheduler/paper_close.go, scheduler/portfolio.go, scheduler/quote_config.go, scheduler/regime_side.go, scheduler/risk.go, scheduler/server.go, scheduler/shared_wallet_reconcile.go, scheduler/state.go, scheduler/state_store.go, scheduler/trade_diagnostics.go, scheduler/trade_protection_snapshot.go, scheduler/trade_review.go


- `hyperliquidPublicTradeAlertRow`:hedge-leg rows private.
- `hedge.go`:HL perps only;ONE reconciler(`hedgeTargetDecision`+`runHedgeSync`);ownership ONLY `Position.HedgeFor`;PnL via `RecordHedgeTradeResult`,never `RecordTradeResult`;fail-closed unwind+CRITICAL DM.

## Market feed

Source .claude/rules/market-feed.md. Scope: scheduler/*feed*.go, scheduler/hyperliquid_balance.go, scheduler/hyperliquid_candles.go, scheduler/hyperliquid_funding.go, scheduler/hyperliquid_marks.go, scheduler/hyperliquid_lot_metadata.go, scheduler/hyperliquid_paper_funding*.go, scheduler/main.go, scheduler/agent_info.go, scheduler/config*.go, scheduler/egress*.go, scheduler/probe*.go, scheduler/server.go, shared_scripts/**, platforms/hyperliquid/**, scripts/shared-feed-convert.sh, scripts/feed-parity.sh, scripts/feed-source-compare.sh, scripts/fixtures/shared_feed_*, scripts/update_helpers.sh, scripts/migrate-service-layout.py, scripts/test_shared_feed_convert_budget.sh, scripts/hl_request_ledger/**


- `market_feed_*.go`:`role: feed` runs pre-`LoadConfig`(no DB/lock/probe).
  - Seal bytes immutable;missed/evicted/pre-start key=`unavailable`,NEVER current data;sealer never takes `mu`.
  - Consumer(`websocket|shared`):1 sealed stdin `marketSnapshot`,paper funding=seal;missing frame=error,NEVER private fetch;1 deadline/cycle,non-nil degraded snapshot,primary>backup;ready backup fills ONLY missing|unready candle key,never by lookback.
  - `/info` ONLY `hlInfoPost`,weight held pre-send(shared:durable,no heap fallback),refund ONLY `settle`;candle window>5000 rows NEVER sent;429=host pause,no retry,`rate_limited` til post-pause fetch,never unhealthy.
  - Key ready ONLY after verified full history(lookback|venue edge)+REST deadline refresh,except `feedClosedFrameReuse`(closed-bar-only readers,host pause refuses,seal v9 `forming_bar_omitted`);funding tail falls back to full;WS seal/repair incremental-only.
  - Correction:outside `feedMu`,closed bars only,never `LastRecvAt`/seals/readiness.
  - Weight file(not SQLite):no `flock` wait under mutex;never unlink locks;stopped owner holds `weight_reserve`(cap=ceiling-max reserve of ANY owner),lowered ONLY by same key(register|full-policy reload|`feed-weight-release`)

## Merge paper instance

Source .claude/rules/merge-paper.md. Scope: scripts/merge-paper-instance.sh, scripts/paper_alias.py, scripts/test_merge_paper_instance.sh, scripts/test_paper_alias.py, scripts/check-live-paper-config-drift.sh, scheduler/inspect*.go, scheduler/storage_inspect*.go, scheduler/merge_paper_instance_test.go, scheduler/config.go, scheduler/notifier.go, scheduler/discord.go


- `merge-paper-instance.sh`:binaries on config COPIES;`inspect`/`storage-inspect`=`LoadConfigReadOnly`;both locks preflight>apply except `--diff`(config-only);`--align-to-live` writes only `.aligned`;compose lists ALL root-key conflicts,refuses;alias always via `paper_alias.py`;`--once`/`--probe-only` never proof;zero paper override inherits.
- `channels`/`trade_alert_channels` `-paper` keys skip if merged bare key routes there(=`resolveChannel`);`dm_channels` `-paper`/`-paper:<id>` NEVER skip;proof refuses paper DM route send path disagrees with

## Robinhood

Source .claude/rules/robinhood.md. Scope: scheduler/robinhood*.go, platforms/robinhood/**, shared_scripts/*robinhood*, shared_scripts/testdata/robinhood_offline/**, scheduler/agent_info.go, scripts/*robinhood*, scripts/update_helpers.sh, scripts/migrate-service-layout.py, scripts/test_migrate_service_layout_fixture.sh, systemd/**


- RH session:owner-only dir beside config(`GO_TRADER_ROBINHOOD_SESSION_DIR`),never `$HOME`;scheduler login never answers device verify(`robinhood-<key>.verification` latch)

## UI

Source .claude/rules/ui.md. Scope: scheduler/ui_*.go, scheduler/server.go, scheduler/static/**, scheduler/executor.go, scheduler/trade_feedback*.go, scheduler/trade_review*.go, scheduler/candle_archive_cmd.go


- `/tuning` never writes config;`ui_tuning.go`=`spawnPythonProcessWithEnv`,NEVER `runPython*`;`POST /api/tuning/apply`=sole promotion.
- `uiPartitionParam`=sole `?partition=` resolver:unparseable=400,unowned=404;diagnostics filter `SourceRole` pre paging+total;cash flow `live_owned`;selected correlation/portfolio-risk NEVER untagged legacy
- Dashboard candles(`ui_candle_*`):published feed copies|read-only archive|trades(display only),NEVER venue/Python/`mu`/feed requirement on a read or miss;review=archive only,generation rechecked under `configWriteMu`(SKILL.md Dashboard Candle Sources)

## Workflows

Source .claude/rules/workflows.md. Scope: .github/**


- `.github/workflows/claude.yml`:mode routing fail-closed(untrusted/fork=review);no-execution in agent;commit/push implement-only;prompt never holds `"`,`` ` ``,`$`.rk-skills workflow skills=CI-only,no settings pin
- Root CLAUDE.md `## Agent guide contract`:read from default branch,never edited by a workflow session.`guide_contract_permit` on `implement` ONLY,=exact Regenerate+Verify commands,plain,comma-joined;neither command EVER in any `extra_allowed_tools`.A command change updates block+permit+`Command files` in 1 PR

End of generated guide: 9 area rule files.
