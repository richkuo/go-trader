# go-trader Project Context

Coding guardrails only. Mechanism detail lives in `SKILL.md` (§ Subsystem Mechanism Reference and the sections cited below); operator flows and history in `SKILL.md`, `docs/POST_UPDATE_HISTORY.md`, and git history. Docs syncs keep this file under 15,000 bytes and never split it: agents auto-load only the root `CLAUDE.md`, and `AGENTS.md` is a symlink to it.

## Environment
- Go 1.26.2 (`/opt/homebrew/bin/go` if not on PATH). Python via `uv run --no-sync python`; the scheduler calls `.venv/bin/python3` directly. `uv sync` once per worktree.
- systemd units use `ProtectSystem=strict` with no `PATH`/`UV_CACHE_DIR` injection; secrets in `/opt/go-trader/.env`. Deployed config lives OUT of the deploy tree at `/var/lib/go-trader[/<instance>]/config.json`; `scheduler/config.json` is a transition symlink. See SKILL.md § Configure.

## Decision Priorities
- **Always pursue the absolute best technical solution.** Cost, compute, time, effort, tests, and code volume never narrow the option space; they govern quality, not scope. They never override the branch+PR workflow, verifying issue claims against code, or destructive-action safety.
- **Never give time, duration, or effort estimates** anywhere. Describe complexity by scope and risk only.

## Repo Structure (`scheduler/` is one Go `package main`)
- `executor.go`/`shutdown.go`: **new side-effecting wrapper → `runPythonSideEffect`, NEVER `runPython`.** `confirmHyperliquidExecuteFill` gates every live HL book path: requires `Execution.Fill` with finite `AvgPx>0` and `TotalSz>0`, else no book. `check_hyperliquid.py execute` exits 1 on an unconfirmed fill.
- `server.go`/`ui_*.go`: **lock order `mu → strategiesMu`** (reusing `mu` deadlocks SIGHUP). **Loopback only.** `/tuning` never writes config; `ui_tuning.go` runs via `spawnPythonProcessWithEnv` (NEVER `runPython*`); `POST /api/tuning/apply` is the sole promotion path.
- `config.go`/`config_migration.go`: `CurrentConfigVersion=19`, `MinSupportedConfigVersion=13`. Seven mutually-exclusive HL stop fields (all omitted → `DefaultStopLossATRMult=1.0`); `close_strategy` canonical; unknown-key guard. `strategyUsesTieredTPATRClose(sc)` gates on-chain TPs, NOT `len(tiers)>0`. `CircuitBreaker *bool` read ONLY via its accessors. `portfolio_risk.paper`: every evaluator takes `scopeRiskConfig`, never `cfg.PortfolioRisk`; nested `paper.paper` rejected.
- `close_defaults.go`: system→user→strategy; explicit `tp_tiers` wins; `applyUserCloseDefaultRatchetRegimeTrails` runs in `loadConfig` **before** the scalar ATR-stop default.
- `portfolio_scope.go`: `PortfolioScope` from `isLiveArgs` is **the single mode classifier** (`strategyMode` retired). `activeScopes` evaluates ONLY scopes with a configured strategy. New portfolio-wide surface → subset with `filterStatesByScope`/`strategiesInScope`, never the whole roster.
- `state.go`/`db.go`: SQLite-only, idempotent migrations. `AppState.PortfolioRisk`/`CorrelationSnapshot` are per-scope maps read via `scopeRisk`/`scopeRiskIfPresent`/`scopeCorrelation`, never a bare field. `initial_capital` only via `StateDB.SetInitialCapital`.
- `risk.go`: `CheckRisk` skips `manual`. Corrupt position (qty≤0 or avgCost≤0) → zero-PnL `*_corrupt` close leg, cash untouched. Portfolio latch: ONE owner per cycle per scope; `DrawdownReadingSubstituted` labelled on every surface; untrusted over-limit defers, never vetoes. **Paper `equityTrusted` is always true.** `ResetPortfolioKillSwitchManual` is the sole DM reset. Live-only auto-clear (`AutoResetConfirmedFlatKillSwitch`, `ClearLatchedKillSwitchSharedWallet` touch `ScopeLive` only). See SKILL.md § Portfolio Kill Switch And Latch Ownership.
- `daily_loss.go`: **hold-only, UNLATCHED pure read**, PRE-FEE realized PnL, never force-closes, per scope. **New `portfolio_risk` gate → copy this shape.**
- `exposure_cap.go`: **blocking-only, direction-aware**; **single exposure model** `computeAssetDeltas` shared with `ComputeCorrelation`. `notional_cap.go`: **hold-only via `pausedBlocksSignal`**, never skips the strategy cycle; restart-required.
- `replay_log.go`/`replay_mirror.go`: **DEFAULT-OFF**; HL perps, flat-only hot-reload; one mirror per source (`replayMirrorSourceID`).
- `hl_batch.go`: shared-state failure ⇒ per-strategy fallback the same cycle; never blank a close/SL/ratchet/protection/hedge. `GO_TRADER_HL_BATCH=0` disables. See SKILL.md § Hyperliquid Batched Signal Checks.
- `hyperliquid_fills.go`: fill resolver built **outside `mu.Lock`**; `HLFillLookup.Px`=VWAP; `ClosedPnLGross` is gross, never into `Trade.RealizedPnL`; unconfirmed SL fills are gaps, not books.
- `hyperliquid_balance.go`: reconciliation-close alerts sent outside `mu`; hedge-leg rows never reach the public route.
- `pause.go`: paused is NOT a `dueStrategies` skip. `pausedBlocksSignal` holds position-increasing signals at all 6 regime-gated dispatch sites; closes/trailing SL/ratchet/protection pass through.
- Regime (`regime*.go`): stamp on all 5 execute dispatches; strip `sl_after` before ATR parse; `regime_unified.go` owns SL; directional policy DEFAULT-OFF, exact-match certs; profile allocation flat-only; dynamic/divergence HL-live-only.
- `trailing_tp_ratchet.go`: **NO on-chain TP**; HL perps+`manual`; hot-reload blocked while open.
- `hedge.go`: HL perps only; ONE reconciler (`hedgeTargetDecision`+`runHedgeSync`); ownership via `Position.HedgeFor` ONLY; hedge PnL → `RecordHedgeTradeResult`, never `RecordTradeResult`; fail-closed unwind + CRITICAL DM; hot-reload blocked while open; backtester rejects.
- `hurst_gate.go`: **DEFAULT-OFF**, no shipped threshold, `config.example.json` clean; holds position-increasing signals only; fail-closed FLAT-ONLY; `metrics["hurst"]` ONLY from the composite classifier; keep in `run_backtest.py` `stop_keys`.
- `llm_entry_analysis.go`: advisory-only; `spawnPythonProcess` NEVER `runPython*`; sole writer of `trade_diagnostics.llm_verdict`. `trade_diagnostics*.go` never write it.
- `scale_in.go`: geometry frozen via `RiskAnchorPrice`, not blended `AvgCost`. `manual*.go`: kill-switch+CB gated; SL edits queue `PendingManualAction`, NEVER a direct UPDATE; `force-close` live HL perps only.
- `hyperliquid_liquidation_guard.go`: **CLAMP, never refuse to arm; ONE-WAY TIGHTEN** at 0.5% buffer; 0 = unknown, never persisted; unclampable geometry REFUSES; unreadable outcome keeps state. Boot `validateHLStopWithinBankruptcyBound` mirrors `LoadConfig` stop-owner resolution. See SKILL.md § Hyperliquid Liquidation Guard.
- `hyperliquid_protection.go`: reduce-only; on-chain TP only when `strategyUsesTieredTPATRClose` AND live (paper never). `hyperliquid_open_trailing.go` arms the initial SL inline at open.
- `version_probe.go`/`probe_cmd.go`: new runtime CLI flag → both probe argvs; `ExitProbeFailure=78`.
- `agent_info.go`: `--bootstrap-md` → `AGENTS.generated.md`, NEVER `AGENTS.md`.
- `failure_alerts.go`: wire the notifier on every new `run*Check`. `discord_*commands.go`: new mutating command → `opsCommandNames`+`slashCommands()`+dispatch.
- `shared_wallet*.go`: PRE-FEE `realized_pnl`, net via `tradeNetPnL*`. Pool budgeting: 2+ live HL/OKX perps omit capital fields, positive `margin_per_trade_usd` each; allocated↔pool switch flat-only. `cashflow_journal.go` runs OUTSIDE `mu`.
- `kill_switch_limit_orders.go`: cancel every `pending_limit_orders` row BEFORE flatten (keyed on ROW); **never gate `reconcilePendingLimitOrders` on kill-switch**; cancel≠adoption; never auto-delete an unadopted fill.
- `orphan_limit_cancel_alerts.go`: cancel-only lane, status-FIRST finalize, books NO fill; `orphanLimitCancelState` is the SSoT (off-book fill = UNTRACKED POSITION). `limit_fill_exposure.go`: book a limit fill ONLY after live exposure confirms; per-coin aggregate, never per-row greedy; fail-closed same-direction+contained; `unreadable`/`unbacked` refuse the book AND block row delete.
- `shared_scripts/`: all check scripts accept `--regime-payload-json` and are probed at startup. `platforms/<name>/adapter.py`: one `*ExchangeAdapter` per file; HL `_sz_decimals()` resolves via `name_to_asset(symbol)` first. `shared_tools/funding_fetcher.py`: `merge_asof` backward, DISJOINT `funding_coverage`; `regime.py` ATR pinned `simple`.
- `shared_strategies/`: open SSoT `open/registry.py`; **`open/{spot,futures}/strategies.py` are shims, do not edit.** Close via `close/registry.py`; import with `from close_registry_loader import …`, never bare `import registry`. `hurst_exponent` (DFA) is the live SSoT.

## Key Patterns
- Run git from repo root; `go -C scheduler build .` over `cd scheduler &&`.
- New platform: see SKILL.md § Custom Platform Integration for the required touchpoints. Adapters load via `importlib`, class `endswith("ExchangeAdapter")`; check scripts use public methods only.
- Subprocess contract: JSON on stdout even on error; exit 1 on error; Go parses regardless of code.
- State locking: `mu sync.RWMutex`, 6-phase cycle (RLock → Lock(CheckRisk) → no-lock subprocess → Lock(execute) → marks → RLock(status)). Skip-reason checks BEFORE spawn; capture `posSide` with `posQty` in Phase 1; `liveExecFailed` guards live exec.
- Platform dispatch by `s.Platform`, never ID prefix. Perps paper→`ExecuteSpotSignalWithFillFee`, live→`RunHyperliquidExecute`; futures→`ExecuteFuturesSignalWithFillFee`.
- Single `CloseStrategy` owns exit; close before open; partial-close preserves `InitialQuantity` and suppresses SL replace.
- `dueStrategies` is value-copied: update `cfg.Strategies` first. Position ownership via `OwnerStrategyID`; shared-coin reconcile non-destructive.
- Trades: `is_close`/`realized_pnl`; `#T` counts opens by `(strategy_id, position_id)`. HL kill-switch shared-coin fill split fails closed.
- Map iteration: ALWAYS `sort.Strings(keys)` for operator/test output.
- Regime: `adx` default, `composite` opt-in; bare `ranging_directional` covers `_up`/`_down` for gating, certs stay exact-match.
- Registries: `open/registry.py`+`PLATFORM_ORDER`+`knownShortNames`+`DEFAULT_PARAM_RANGES`; `backtest_only=True` fail-closes live; snapshot `--list-json` before refactors.
- CB disable suppresses new fires only; latched HL-perps manage-only (`Signal=0`, not `continue`). Kill switch: `planKillSwitchClose`→`OnChainConfirmedFlat`; reset prompt single-flight.
- HL stops: `EffectiveStopLossPct` seven exclusive owners; scalar↔regime blocked while open. `risk_per_trade_pct` fails closed on an unresolvable stop; exclusive vs sizing_leverage/margin/scale_in. Trailing SL replace only past `TrailingStopMinMovePct`; `hlSLEffectiveQty=min(virtual,onChain)`. Peers share `margin_mode`+`leverage`; `update_leverage` from flat only.
- SIGHUP `validateHotReloadCompatible` blocks add/remove, script/args/type/platform/HTFFilter, kill-switch identity, DB path, `max_notional_usd`.
- New per-strategy flag: field → `run*Check` CLI → Python parse → InitOptions/wizard; runtime-required → both probe argvs.
- Notifications via `MultiNotifier`; paper routing via `resolveChannelKey`/`SendToScopeChannels`.

## Pull Requests
- `Closes #<N>` in body; never bare `#N` for list items. Title `type(#<N>): summary [C<score>, <model>, <effort>]` (`, fableplan` when a Fable plan ran). Body leads with `## Plain simple English` (under 55 words), then `## Summary` and verification.
- Commits, PR bodies, and issue bodies end with `LLM: <model> | <effort> | Harness: <action>`; no `Co-authored-by` trailer.
- Bot reviews land on the issues comments endpoint, not pulls. Before merging a long-running PR, diff `origin/main..HEAD` for silent reverts.
- Review format SSoT: rk-skills `pr-review-format.md` + `.github/prompts/pr-review-format-local.md`. Reviews never gate on CI.
- Before implementing review findings: restate each as an invariant, enumerate states that break the fix (inverse and compound cases), add tests for the class.
- `.github/workflows/claude.yml`: least-privilege split; mode routing fail-closed (untrusted/fork → review); no-execution ban in the workflow agent, commit/push implement-mode only; assembled prompt must not contain `"`, `` ` ``, or `$`; `.github/scripts/` holds ONLY `test_workflow_logic.py`. Detail: SKILL.md § Subsystem Mechanism Reference.

## GitHub Issues
- `gh issue create`; title `[C<0-100>] <title>`; first body line `**Complexity: N/100** — scope; risk; uncertainty`. Score weighs scope, risk (money/data/protection heaviest), uncertainty; never time.
- rk-skills workflow skills are CI-only; no project-level settings pin.

## Build & Deploy
- **Update only with `bash scripts/update.sh --restart`. Never rebuild Go alone**: Go and Python share an argv contract and must be at the same SHA.
- Startup probe: non-zero `--probe-only` → `os.Exit(78)`; both unit files set `RestartPreventExitStatus=78`. Service-file changes need `daemon-reload`.
- Post-update: follow SKILL.md § Post-Update Agent Protocol. After a Python-launcher change, smoke with `./go-trader --config scheduler/config.json --once`.

## Backtest
- Harness map: `docs/backtesting-registry.md`; update its row in the same PR that adds or deprecates a harness.
- `--config` gates on `config_version>=15`; entry ATR guard 50% of AvgCost; SL-vs-TP races default `ohlc_walk`.
- Look-ahead invariants: signal at bar N fills at N+1 open; regime gate reads N-1; closes use closed-bar ATR. **HTF series indexed by candle OPEN time MUST `.shift(1)` BEFORE `reindex(..., method="ffill")`.**
- Backtester rejects HL-live-only closes (`regime_window_divergence`, `tiered_tp_atr_live_regime_dynamic`); options regime gating unsupported.
- M1–M6, auto_suggest, regime promotion, `tune_live.py` are SUGGEST-ONLY: **never write live defaults, config, or PRs.**

## Testing
- New functionality and every bug fix need tests that guard a behavior contract (money, state, protection, subprocess contracts, migration, backtest parity). Assert outcomes, not log wording, constants, or struct round-trips; table-driven for variants.
- Go CI must not spawn Python: extract pure helpers from subprocess wrappers. Go tests check `json.Unmarshal` errors.
- `go build`/`go test ./...` from repo root; `gofmt -w` after edits. Multi-line tabbed Go edits: Python `read()`+`replace(old,new,1)`+`write()`.
- Pytest: `uv run --no-sync python -m pytest shared_strategies/ shared_tools/ platforms/ backtest/`; `shared_scripts/test_*.py` invoked explicitly. Registry/sys.path tests → run the FULL suite. CI uses `-n auto`: never bare-`import` an ambiguous module name; an intermittent failure is test isolation, never a flake.
- `stampEntryATRIfOpened` rejects ATR > 50% of AvgCost. Strategy tests assert actual signal values; smoke tests need a `DatetimeIndex`.
- `tiered_tp_atr`/`trailing_stop_atr_mult` need `Position.EntryATR`; `*_live` recompute via `atr_source`. `avwap_stop` is virtual exit only.
