# go-trader Project Context

## Environment
- Requires Go 1.26.2 — install via Homebrew (macOS): `brew install go@1.26` or Linux tarball: `curl -sL https://go.dev/dl/go1.26.2.linux-amd64.tar.gz | tar -C /usr/local -xzf -`
- Go is not in PATH via shell; use `/opt/homebrew/bin/go` directly (e.g. `cd scheduler && /opt/homebrew/bin/go build .`)
- Python venv at `.venv/bin/python3` (used by executor.go at runtime)
- In git worktrees, `.venv` is NOT copied — use the main repo's venv: `<main-repo>/.venv/bin/python3`
- Python deps managed with `uv` (see `pyproject.toml` / `uv.lock`)

## Quick Flow
- **New server:** tell OpenClaw `install https://github.com/richkuo/go-trader and init`.

## Setup
- `uv sync` — install Python deps into `.venv`
- Copy `scheduler/config.example.json` → `scheduler/config.json` and fill in API keys

## Repo Structure
- `scheduler/` — Go scheduler (single `package main`); all .go files compile together
  - `executor.go` — Python subprocess runner; max 4 concurrent, 30s timeout per script
  - `server.go` — HTTP status server (`/status`, `/health` endpoints)
  - `discord.go` — `discordgo.Session` wrapper for two-way Discord communication; `SendMessage`, `SendDM`, `AskDM` (blocking DM with timeout); `FormatCategorySummary` per-asset Discord messages; `fmtComma` — always pass absolute values
  - `init.go` — `go-trader init` interactive wizard + `--json <blob>` non-interactive mode; `generateConfig(InitOptions) *Config` is pure/testable; `runInitFromJSON(jsonStr, outputPath)` for scripted config gen (e.g. from OpenClaw); `runInit` orchestrates I/O
  - `prompt.go` — `Prompter` struct (String/YesNo/Choice/MultiSelect/Float); inject `NewPrompterFromReader(r,w)` for tests
  - `updater.go` — update checker; `checkForUpdates(cfg, discord, &lastNotifiedHash, &mu, state)` — git fetch, channel notify + DM upgrade prompt (goroutine); `applyUpgrade(discord, ownerID, mu, state, cfg)` — git pull + go build + state save + restart; `restartSelf()` — systemctl → syscall.Exec fallback; logs `[update]` prefix
  - `correlation.go` — per-asset directional exposure tracking; `ComputeCorrelation` warns on concentration/same-direction thresholds
  - `config_migration.go` — `CurrentConfigVersion = 7`; auto-migrates config via Discord DM on startup
  - `balance.go` — balance tracking and capital management
  - `hyperliquid_balance.go` — Hyperliquid-specific balance sync (`syncHyperliquidAccountPositions`)
  - `leaderboard.go` — pre-computed strategy leaderboard for Discord summaries
  - `logger.go` — structured logging utilities
  - `notifier.go` — `MultiNotifier` wraps Discord + Telegram backends
  - `options.go` — options position management and expiry tracking
  - `portfolio.go` — portfolio-level aggregation and reporting
  - `risk.go` — per-strategy risk checks (drawdown limits, position sizing)
  - `telegram.go` — Telegram notification backend
  - `pricer.go` — `OptionPricer` interface; `ibkr_pricer.go` — IBKRPricer with Black-Scholes
  - `db.go` — SQLite state persistence (`modernc.org/sqlite` pure-Go driver); `OpenStateDB(path)`, `SaveStateWithDB`, `LoadStateWithDB`; tables: `app_state`, `strategies`, `positions`, `closed_positions`, `option_positions`, `closed_option_positions`, `trades`, `portfolio_risk`, `kill_switch_events`, `correlation_snapshot`; `InsertTrade` writes trades immediately via `tradeRecorder` hook (wired to `StateDB.InsertTrade` at startup) — trades survive mid-cycle crashes; `QueryClosedPositions(strategyID, symbol, since, until, limit, offset)` queries closed position history; `ClosedPosition` struct + transient `StrategyState.ClosedPositions` buffer flushed by `SaveState` inside the same transaction
  - `hyperliquid_marks.go` — `fetchHyperliquidMids(coins)` native Go HL `/info allMids` fetcher; correct oracle for HL perps PnL (replaces BinanceUS spot basis spoofing, issue #263)
  - `okx_marks.go` — `fetchOKXPerpsMids(coins)` native Go OKX `/api/v5/market/tickers?instType=SWAP` fetcher; USDT-margined swaps only (PR #280, issue #279); test stub via `okxMainnetURL` var
  - `deribit.go` — native Go Deribit `/public/ticker` fetcher (mark/underlying price + greeks)
  - `shared_wallet.go` — `SharedWalletKey{Platform, Account}` + `walletKeyFor(sc)`; prevents double-counting capital when multiple strategies trade from the same on-exchange wallet (currently HL live perps all share `HYPERLIQUID_ACCOUNT_ADDRESS`)
- `shared_scripts/` — Python entry-point scripts called by the scheduler
  - `check_strategy.py` — spot strategy signal checker
  - `check_options.py` — unified options checker (`--platform=deribit|ibkr|robinhood|okx`)
  - `check_price.py` — price check script
  - `check_hyperliquid.py` — Hyperliquid perps checker (`<strategy> <symbol> <timeframe> [--mode=paper|live]`; `--execute` for live orders)
  - `check_topstep.py` — TopStep futures checker (`<strategy> <symbol> <timeframe> [--mode=paper|live]`; `--execute` for live orders)
  - `check_robinhood.py` — Robinhood crypto checker (`<strategy> <symbol> <timeframe> [--mode=paper|live]`; `--execute` for live orders; OHLCV via yfinance)
  - `check_okx.py` — OKX spot/perps checker (`<strategy> <symbol> <timeframe> [--mode=paper|live] [--inst-type=spot|swap]`; `--execute` for live orders; CCXT)
  - `check_balance.py` — balance/position checker for live account reconciliation
  - `fetch_futures_marks.py` — CME futures mark-price fetcher; revalues open TopStep positions at live marks (issue #261); TopStep adapter auto-selects TopStepX live quotes vs yfinance paper
- `platforms/` — platform-specific adapters (deribit, ibkr, binanceus, hyperliquid, topstep, robinhood, okx, luno)
  - `deribit/adapter.py` — DeribitExchangeAdapter (live quotes, real expiries/strikes)
  - `ibkr/adapter.py` — IBKRExchangeAdapter (CME strikes, Black-Scholes pricing)
  - `binanceus/adapter.py` — BinanceUSExchangeAdapter (spot only)
  - `hyperliquid/adapter.py` — HyperliquidExchangeAdapter (live perps prices, funding rates, paper/live trading via `HYPERLIQUID_SECRET_KEY`)
  - `topstep/adapter.py` — TopStepExchangeAdapter (CME futures, paper mode via yfinance, live via TopStepX API)
  - `robinhood/adapter.py` — RobinhoodExchangeAdapter (crypto spot + stock options, paper mode via yfinance/Black-Scholes, live via robin_stocks + TOTP MFA)
  - `okx/adapter.py` — OKXExchangeAdapter (spot + perps + options via CCXT; paper mode uses public API, live mode requires `OKX_API_KEY`, `OKX_API_SECRET`, `OKX_PASSPHRASE`)
  - `luno/adapter.py` — LunoExchangeAdapter (South African crypto exchange)
- `shared_tools/` — shared Python utilities (pricing.py, exchange_base.py, data_fetcher, storage)
- `shared_strategies/` — shared strategy logic (spot/, options/, futures/)
- `backtest/` — backtesting and paper trading scripts
- `archive/` — retired/unused modules
- `SKILL.md` — agent operations guide (setup, deploy, backtest commands)
- `.github/workflows/claude.yml` — general-purpose `@claude` handler for PR/issue comments; handles code review, fixes, etc. — no separate review workflow needed

## Key Patterns
- Git commands: always run from repo root, not from `scheduler/` (git add/commit fail with path errors otherwise)
- Bash working dir persists across tool calls — prefer `go -C scheduler build .` / `go -C scheduler test ./...` over `cd scheduler && ...` to avoid cwd drift on the next call
- Adding a new platform: (1) `platforms/<name>/adapter.py` + `__init__.py`, (2) `shared_scripts/check_<name>.py`, (3) `executor.go` (result types + runner), (4) `config.go` (prefix inference + validation), (5) `fees.go` (fee dispatch), (6) `main.go` (dispatch case + helpers), (7) `init.go` (wizard + generateConfig), (8) `pyproject.toml` (deps)
- Adding options to an existing platform: extend adapter with Protocol methods (`get_vol_metrics`, `get_real_expiry`, `get_real_strike`, `get_premium_and_greeks`), add platform to `check_options.py` usage + `CalculateOptionFee` dispatch, add to init wizard `OptionPlatforms`; no main.go/executor.go changes needed (options dispatch is platform-agnostic)
- Platform adapters loaded via `importlib` in `check_options.py`; class discovered by `endswith("ExchangeAdapter")` — only one adapter class per file; `_fetch_ohlcv_closes()` supports adapter-aware fallback via `get_ohlcv_closes()` method for non-BinanceUS platforms
- Check scripts (`shared_scripts/check_*.py`) must only call public adapter methods — never access private attributes like `_exchange` directly; if a needed method doesn't exist on the adapter, add it there first
- Scheduler communicates with Python scripts via subprocess stdout JSON; scripts must always output valid JSON even on error
- Python scripts exit 1 on error (Go parses JSON from stdout regardless of exit code)
- Option positions stored in `StrategyState.OptionPositions map[string]*OptionPosition`
- Mutex `mu sync.RWMutex` guards `state`; RLock for reads, Lock for all mutations
- Per-strategy loop uses 6 fine-grained lock phases: RLock(read inputs) → Lock(CheckRisk) → no lock(subprocess) → Lock(execute signal) → RLock/no lock/Lock(mark prices) → RLock(status log)
- Audit lock balance: `grep -n "mu\.\(R\)\?Lock\(\)\|mu\.\(R\)\?Unlock\(\)" scheduler/main.go`
- Platform dispatch: `StrategyConfig.Platform` field (inferred from ID prefix in LoadConfig); use `s.Platform == "ibkr"` not ID prefix checks
- ID prefix → platform: `hl-` → hyperliquid, `ibkr-` → ibkr, `deribit-` → deribit, `ts-` → topstep, `rh-` → robinhood, `okx-` → okx, `luno-` → luno, else → binanceus
- Robinhood options use stock symbols (SPY, QQQ, AAPL) not crypto assets; strategy IDs: `rh-ccall-spy`, `rh-vol-qqq`; options config uses `--platform=robinhood` arg to check_options.py
- Strategy types: "spot", "options", "perps", "futures" — perps paper mode reuses `ExecuteSpotSignal`; live mode calls `RunHyperliquidExecute` before state update; futures use `ExecuteFuturesSignal` with whole-contract sizing and margin-based budgeting
- Live execution guard: every platform dispatch in main.go must use `liveExecFailed` pattern — when `runXxxExecuteOrder` returns `ok2=false`, set `liveExecFailed = true` and skip state update; audit with `grep -n "liveExecFailed" scheduler/main.go`
- Bidirectional perps strategies (those emitting `signal=-1` as short entries, e.g. `triple_ema_bidir`) must be listed in `bidirectionalPerpsStrategies` in `scheduler/init.go` so `generateConfig` sets `StrategyConfig.AllowShorts=true`. Without the flag, `ExecutePerpsSignal` and `PerpsOrderSkipReason` drop flat+(-1) as a no-op — strategy appears to work in Python but produces zero executed trades (#328)
- `ExecutePerpsSignal(..., allowShorts, logger)` and `PerpsOrderSkipReason(signal, posSide, allowShorts)` — any new perps live-order helper (new platform, new adapter) must thread `sc.AllowShorts` through both, mirroring `runHyperliquidExecuteOrder` / `runOKXExecuteOrder`
- Live order helpers (`runHyperliquidExecuteOrder`, `runOKXExecuteOrder`, `runRobinhoodExecuteOrder`, `runTopStepExecuteOrder`) must check the same skip conditions as the corresponding `ExecuteXxxSignal` BEFORE spawning the Python executor; otherwise on-chain fills land with no Trade record (#298, #300). Use `PerpsOrderSkipReason(signal, posSide)` for perps, `SpotOrderSkipReason` for spot (Robinhood, OKX spot), `FuturesOrderSkipReason` for futures (TopStep). OKX dispatches on `sc.Type` (perps vs spot). Capture `posSide` alongside `posQty` in Phase 1 RLock — `Position.Quantity` is always positive and does NOT encode side.
- `dueStrategies` is built by value-copying `StrategyConfig` from `cfg.Strategies` — mutations to `dueStrategies` elements do NOT persist; any function that needs to update capital/config must operate on `cfg.Strategies` before `dueStrategies` is built
- `notifier.go` — `MultiNotifier` wraps Discord + Telegram backends; new notification features should add methods to `MultiNotifier`, not access `backends` directly
- Hyperliquid sys.path conflict: SDK installs as `hyperliquid` package — clashes with `platforms/hyperliquid/`; fix: add `platforms/hyperliquid/` directly to sys.path (not `platforms/`), then `from adapter import HyperliquidExchangeAdapter`
- Hyperliquid SDK funding rates: `info.meta_and_asset_ctxs()` returns current predicted funding rate per asset (NOT `info.meta()` which only returns universe metadata); `info.funding_history(coin, startTime)` for historical rates; response uses parallel arrays — universe[i] matches asset_ctxs[i]
- Fee dispatch: `CalculatePlatformSpotFee(platform, value)` — 0.035% hyperliquid, 0% robinhood, 0.1% binanceus (replaces bare `CalculateSpotFee` for platform-aware spot/perps trades); `CalculateFuturesFee(contracts, feePerContract)` and `CalculatePlatformFuturesFee(sc, contracts)` for futures per-contract fees
- Position ownership: `Position.OwnerStrategyID` tracks which strategy opened a position; `syncHyperliquidAccountPositions` syncs on-chain positions once per cycle (not per-strategy) and only reconciles positions with their owner; `syncHyperliquidLiveCapital` is a no-op — capital is set from config or `capital_pct`
- State persisted exclusively to SQLite: `scheduler/state.db` (`cfg.DBFile`, default `scheduler/state.db`) via `SaveStateWithDB`/`LoadStateWithDB`. Legacy `state_file` / `StateFile` config field and `LoadState`/`loadJSONPlatformStates` JSON paths were removed in #283; leaderboard messages are built on-demand (`BuildLeaderboardMessages`) at post time — the previous per-cycle `leaderboard.json` pre-compute was removed in #313; trades are written immediately via `RecordTrade` → `StateDB.InsertTrade` (not just on cycle-end flush); closed positions are appended to `StrategyState.ClosedPositions` at every closure site and flushed atomically by `SaveState`
- Native Go mark fetchers: `fetchHyperliquidMids` (hyperliquid_marks.go), `fetchOKXPerpsMids` (okx_marks.go), and Deribit ticker fetcher (deribit.go) replace per-cycle Python subprocess calls — pattern: expose base URL as `var xxxMainnetURL` so httptest stubs can redirect in tests
- `cfg.Discord.Channels` is `map[string]string` (not a struct); keys: "spot", "options", "hyperliquid", etc. — old `.Spot`/`.Options` field access is invalid
- `cfg.Discord.OwnerID` — Discord user ID for DM upgrade prompts + config migration; loaded from `DISCORD_OWNER_ID` env var (takes priority over config file)
- `cfg.ConfigVersion` — int, schema version (`0`/missing = v1 baseline); `CurrentConfigVersion = 7` in config_migration.go; startup triggers `runConfigMigrationDM` when below current version
- `cfg.Correlation` — `*CorrelationConfig` with `Enabled` (default false), `MaxConcentrationPct` (default 60), `MaxSameDirectionPct` (default 75); computed under RLock, state assigned under Lock; warnings sent to all Discord channels + owner DM
- `cfg.AutoUpdate` — `"off"` (default), `"daily"` (once/day), `"heartbeat"` (every cycle); handled in main.go loop + startup; uses `dailyCycles = (24*3600)/tickSeconds`
- Strategy registry imports: `check_strategy.py` imports from `shared_strategies/spot/strategies.py`; `check_hyperliquid.py`, `check_topstep.py`, and `check_okx.py` (swap mode) import from `shared_strategies/futures/strategies.py` — a new strategy must be registered in both if it needs to work across platforms
- Adding a cross-platform strategy: create core logic in `shared_strategies/<name>.py` (see `chart_patterns.py`, `liquidity_sweeps.py`), then import+register in both `spot/strategies.py` and `futures/strategies.py`; thin wrapper: `@register_strategy(...)` + `def x(df, **params): return x_core(df, **params)`
- Adding a new spot/futures strategy (no new platform): (1) add `@register` function to `shared_strategies/registry.py` with appropriate `platforms=(...)` tuple (default `("spot","futures")`); (2) append the name to the matching list(s) in `PLATFORM_ORDER` at the bottom of `registry.py` (controls `--list-json` order — keep this byte-stable for agent tooling); (3) if spot and futures flavors differ in description or defaults (see `momentum`, `rsi`, `macd`, `mean_reversion`), pass `variants={"futures": {"description": ..., "default_params": {...}}}` rather than duplicating the function; (4) add short name to `knownShortNames` in `scheduler/init.go`; (5) add param grid entry to `DEFAULT_PARAM_RANGES` in `backtest/optimizer.py` or CI's `test_param_ranges_cover_every_registered_strategy` will fail — auto-discovery handles all platform configs. `shared_strategies/spot/strategies.py` and `shared_strategies/futures/strategies.py` are thin shims that materialize a platform-filtered view via `build_registry(platform)` — **do not edit them to add strategies**
- Single source of truth: every strategy implementation lives exactly once in `shared_strategies/registry.py`. Spot-only strategies (`pairs_spread`) set `platforms=("spot",)`; futures-only strategies (`breakout`, `delta_neutral_funding`, `triple_ema_bidir`) set `platforms=("futures",)`. `shared_strategies/test_registry_parity.py` enforces invariants (no double-registration, variants subset of platforms, PLATFORM_ORDER matches platform tags, every shim strategy applies cleanly on synthetic data)
- Spot and futures shims each build a **fresh** `STRATEGY_REGISTRY` dict via `_load_registry_module()` + `build_registry()` — they're not the same object (regression test: `spot.STRATEGY_REGISTRY is not fut.STRATEGY_REGISTRY`). Perps auto-discovers from futures via `discoverStrategies()` in `scheduler/init.go`
- Before refactoring `shared_strategies/registry.py` or either shim, snapshot the baseline: `.venv/bin/python3 shared_strategies/spot/strategies.py --list-json > /tmp/spot.json` (same for futures), refactor, then `diff` — agent tooling (Go `discoverStrategies`) depends on byte-identical output
- New strategies also need: (1) `knownShortNames` entry in `init.go` for the `"name": "abbrev"` mapping, (2) `defaultSpotStrategies` / `defaultPerpsStrategies` / `defaultFuturesStrategies` fallback entries in `init.go`
- Strategy discovery: `shared_strategies/spot/strategies.py --list-json`, `shared_strategies/options/strategies.py --list-json`, and `shared_strategies/futures/strategies.py --list-json` output JSON arrays of `{"id":..., "description":...}`
- `apply_strategy(name, df, params)` — optional `params` dict merges with strategy defaults; used to inject external data (e.g. funding rates) into strategies that need non-OHLCV inputs
- Adding a per-strategy config flag (cross-cutting): (1) add field to `StrategyConfig` in `config.go`, (2) in `main.go` `run*Check` functions append CLI flag to args when enabled, (3) parse flag in each Python check script, (4) add to `InitOptions` + wizard prompt + `generateConfig` in `init.go`
- `StrategyConfig.Params` — `map[string]interface{}` (`"params"` in JSON); Go serializes to JSON and appends `--params '...'` to script args; Python scripts parse and pass to `apply_strategy(name, df, params)`; for scripts with runtime params (hyperliquid/okx funding rates), config params are merged UNDER runtime params (runtime takes priority)
- `check_strategy.py` uses manual `sys.argv` parsing (not argparse) — when adding flags, filter `--` prefixed args from positional args before indexing; other check scripts (hyperliquid, topstep, robinhood) use `argparse` so just add `parser.add_argument("--flag")`
- `shared_tools/htf_filter.py` — `htf_trend_filter(symbol, timeframe, fetch_fn)` returns HTF trend via 50 EMA; `apply_htf_filter(signal, htf_trend)` filters counter-trend signals; `fetch_fn` is a callable `(symbol, tf, limit) → DataFrame` so it works across all platforms
- `StrategyConfig.HTFFilter` — per-strategy bool (`htf_filter` in JSON); Go appends `--htf-filter` to script args; not applied to options strategies or `delta_neutral_funding` (funding-rate harvest is direction-agnostic); guard in both `generateConfig` and all Python check scripts
- `delta_neutral_funding` is perps-only (not in spot registry); function lives in `spot/strategies.py` but without `@register_strategy`; registered only in `futures/strategies.py`
- Perps vs futures at Position level: both set `Multiplier > 0`; only perps sets `Leverage > 0`. Use the two-field check (`Multiplier > 0 && Leverage > 0`) when iterating positions for leverage-aware metrics — see `perpsMarginDrawdownInputs` in `risk.go` (#292)
- Per-strategy drawdown: spot/options/futures use `(peak - portfolio) / peak`; perps uses `unrealized_loss_on_open_positions / deployed_margin` when positions are open (referenced to currently-open positions so prior realized losses do not inflate drawdown against a fresh small position's margin), falling back to peak-relative when no margin is deployed — see `perpsMarginDrawdownInputs` + `CheckRisk` in `risk.go` (#292)
- Bidirectional perps (#328/#330): per-strategy `StrategyConfig.AllowShorts` opt-in (default false = legacy long-only). `ExecutePerpsSignal(..., allowShorts, ...)` opens shorts from flat and flips long↔short on opposite signals; `flipCloseQty` is subtracted from `fillQty` on live flips so a single net-flip fill isn't double-counted. Live sizing uses `perpsLiveOrderSize(signal, price, cash, posQty, avgCost, lev, posSide, allowShorts)` — flip size = `posQty + (cash+closePnL)*lev*0.95/price`; catastrophic flip (`effectiveCash*lev*0.95 < 1`) degrades to close-only (returns `posQty`) so bidirectional strategies aren't worse than long-only at exiting a drawdown. Call sites (`runHyperliquidExecuteOrder`, `runOKXExecuteOrder`) must thread `pos.AvgCost` from the Phase 1 RLock snapshot.
- Testability pattern: pure helpers for sizing/guard logic (e.g. `perpsLiveOrderSize`, `PerpsOrderSkipReason`, `SpotOrderSkipReason`) — extract from live-path functions that spawn subprocesses so tests exercise the decision without side effects. Live helpers (`runHyperliquidExecuteOrder`, etc.) become thin wrappers that delegate the logic.
- Sort map keys before formatting any operator-facing or test-asserted output — Go map iteration is randomized, so `for k := range m` produces different ordering across runs. Every kill-switch error summary, log line, and Discord message that iterates a map must `sort.Strings(keys)` first (caught in #342 review for `HyperliquidLiveCloseReport.Errors`)
- Portfolio kill-switch live close (#341/#345/#347/#350): `scheduler/kill_switch_close.go` owns the cross-platform plan builder; adapters live in `hyperliquid_balance.go` (HL via clearinghouseState), `okx_close.go`, `robinhood_close.go`, `topstep_close.go`. Pattern: each platform supplies a `<Plat>LiveCloser` + `<Plat>PositionsFetcher`; `planKillSwitchClose(KillSwitchCloseInputs)` returns a pure `KillSwitchClosePlan` with `OnChainConfirmedFlat bool`, per-platform `*LiveCloseReport`, and a `FormatKillSwitchMessage` summary. Main loop gates virtual-state mutation on `OnChainConfirmedFlat=true` — if any platform cannot confirm flat, virtual state is preserved so the next cycle retries. Adding a new live platform = add fields to `KillSwitchCloseInputs` + a close/fetcher pair, not a signature change to existing call sites. OKX spot and Robinhood options are surfaced via `OKXSpotLive` / `RHLiveOptions` for operator warning but do NOT block `OnChainConfirmedFlat` (no safe automated close semantic yet). Each platform gets its own `context.WithTimeout` from per-platform overrides (`HLCloseTimeout`, `RHCloseTimeout`, etc.) so a slow platform can't starve another's budget
- `initial_capital` immutable baseline (#343): `SaveState` snapshots existing `initial_capital` per strategy and refuses to let a stale in-memory `StrategyState.InitialCapital` overwrite it. Use `StateDB.SetInitialCapital(strategyID, value)` — the ONLY sanctioned write path — when the baseline legitimately needs to change. SaveState logs the override attempt and keeps the DB value
- Startup state-DB presence check (#339): `scheduler/state_presence.go` — `CheckStatePresence(dbPath, strategies)` returns a CRITICAL warning string if any live strategy is configured and the DB file is missing (runs BEFORE `OpenStateDB` because that call creates the file). `HasLiveStrategy` detects `--mode=live` (or `--mode live` split form) in any strategy's `Args`. `AllowMissingState()` checks `GO_TRADER_ALLOW_MISSING_STATE=1` for genuine first-run deployments. Warning is also DM'd to the owner in main.go
- Go CI without Python: tests that cover subprocess-based live helpers extract a pure parser (e.g. `parseHyperliquidCloseOutput(stdout, stderrStr, runErr)`) from the wrapper and test the parser directly — same pattern as `perpsLiveOrderSize` / `PerpsOrderSkipReason`. Go CI doesn't install `.venv/bin/python3`, so any test that calls `RunPythonScript` / `RunHyperliquidExecute` / `RunHyperliquidClose` will pass locally but fail in CI
- Per-strategy circuit-breaker pending close (#356/#359–#363): `RiskState.PendingCircuitCloses map[string]*PendingCircuitClose` holds queued closes keyed by platform constants — `PlatformPendingCloseHyperliquid` (#356), `PlatformPendingCloseOKX` (#360 perps), `PlatformPendingCloseRobinhood` (#361 crypto), `PlatformPendingCloseTopStep` (#362 futures), plus operator-required gaps `PlatformPendingCloseOKXSpot` / `PlatformPendingCloseRobinhoodOptions` (#363). Always use `setPendingCircuitClose` / `clearPendingCircuitClose` / `getPendingCircuitClose` accessors — never write the map directly. `CheckRisk` takes `*PlatformRiskAssist` (renamed from `*HLRiskAssist` in #359); HL/OKX/TS fields are populated at the CheckRisk call site, RH fields are nil and the RH enqueue runs exclusively from the drain's stuck-CB recovery path (skips TOTP cost when no CB fires). Each platform has its own `setXxxCircuitBreakerPending` sizing helper (`setHyperliquidCircuitBreakerPending`, `setOKXCircuitBreakerPending`, `setRobinhoodCircuitBreakerPending`, `setTopStepCircuitBreakerPending`). DB column: `risk_pending_circuit_closes_json`; `UnmarshalPendingCircuitClosesJSON` accepts both new map shape and legacy `{"coins":[...]}` array.
- Operator-required per-strategy CB (#363): `PlatformPendingCloseOKXSpot` / `PlatformPendingCloseRobinhoodOptions` carry `PendingCircuitClose.OperatorRequired=true`. The drain does NOT submit orders — `drainOperatorRequiredPendingCloses` (main.go, wired after the auto-close drains) emits one CRITICAL log line + one `CIRCUIT BREAKER — OPERATOR INTERVENTION REQUIRED` notifier message per cycle until the CB naturally resets or the portfolio kill switch clears it. `planOperatorRequiredWarning` (operator_required_close.go) is a pure function — entries are sorted by `(StrategyID, Platform)` for byte-stable output per the map-iteration rule. Keep these keys distinct from `"okx"` / `"robinhood"` so auto-close drains never dequeue an operator-required entry.
- SQLite column rename migration pattern (#359): use `PRAGMA table_info(tableName)` to detect the three states (neither column, legacy-only, new-only) and branch accordingly — ADD COLUMN for pre-#356 DBs, RENAME COLUMN for post-#356 pre-#359 DBs, no-op for already-migrated. This is idempotent under repeated startups and avoids re-adding ghost columns. `UnmarshalPendingCircuitClosesJSON` accepts both new map shape and legacy `{"coins":[...]}` array shape — DB self-heals to new shape within one cycle.
- New test helper files added for phases 2-5 close plumbing: `scheduler/okx_close.go` + `_test.go`, `scheduler/robinhood_pending_close.go` + `_test.go` (per-strategy RH drain, separate from portfolio-kill `robinhood_close.go`), `scheduler/topstep_close.go` + `_test.go`, `scheduler/operator_required_close.go` + `_test.go`. Python-side: `shared_scripts/close_okx_position.py`, `close_robinhood_position.py`, `close_topstep_position.py`, `fetch_okx_balance.py`, `fetch_okx_positions.py`, `fetch_robinhood_positions.py`, `fetch_topstep_positions.py` — all invoked once per cycle from main.go to populate `PlatformRiskAssist` and drain pending closes.

## Pull Requests
- PR descriptions must reference the related GitHub issue if one exists, using `Closes #<number>` in the body (e.g. `Closes #46`)
- In GitHub comments and PR reviews, avoid using `#N` notation for numbered list items or steps (e.g. "step #1", "point #2") — GitHub auto-links these to issues/PRs. Use `1.` instead. Only use `#N` when intentionally linking to a specific issue or PR.
- Fetch latest claude[bot] review on a PR: `gh api repos/richkuo/go-trader/issues/<N>/comments --jq '[.[] | select(.user.login=="claude[bot]")] | last | .body'` (top-level review summary lives on the **issues** endpoint, not pulls; inline review comments via `/pulls/<N>/comments`, review-object summaries via `/pulls/<N>/reviews`)
- Before merging a long-running PR, `git fetch origin main && git diff origin/main..HEAD -- <paths>` to catch silent reverts from unrelated merges that landed on main while the PR was open. Rebase onto main if the diff shows unexpected deletions.

## Build & Deploy
- Build: `cd scheduler && /opt/homebrew/bin/go build -o ../go-trader .` — always rebuild before smoke-testing `./go-trader`; stale binary gives misleading results
- Restart: `systemctl restart go-trader`
- Only needed when `scheduler/*.go` files change
- Python script changes take effect on next scheduler cycle (no rebuild needed)
- Config changes: `systemctl restart go-trader` (no rebuild)
- Service file changes: `systemctl daemon-reload && systemctl restart go-trader`

## Backtest
- `run_backtest.py`: `.venv/bin/python3 backtest/run_backtest.py --strategy <n> --symbol BTC/USDT --timeframe 1h --mode single`
- `backtest_options.py`: `.venv/bin/python3 backtest/backtest_options.py --underlying BTC --since YYYY-MM-DD --capital 10000`
- `backtest_theta.py`: `.venv/bin/python3 backtest/backtest_theta.py --underlying BTC --since YYYY-MM-DD --capital 10000`

## ISSUES.md
- When marking an issue fixed: update the row (`NO` → `YES`) **and** the Summary table at the bottom (`Fixed` count +1, `Unfixed` count -1 for that category and Total)

## Testing
- **New functionality must include tests** — Go changes need `_test.go` coverage; Python changes need `test_*.py` coverage. Bug fixes should include a regression test when feasible.
- `python3 -m py_compile <file>` — syntax check Python files; run from repo root (`python3 -m py_compile shared_scripts/check_*.py`) — paths are relative to cwd
- `cd scheduler && /opt/homebrew/bin/go build .` — compile check
- `cd scheduler && /opt/homebrew/bin/go test ./...` — run all unit tests (must run from scheduler/ where go.mod lives; repo root has no go.mod)
- `cd scheduler && /opt/homebrew/bin/gofmt -w <file>.go` — format after editing Go files (`-l *.go` lists all files needing formatting)
- Multi-line Go edits with tabs: Edit tool may fail on tab-indented blocks; use heredoc form (one-liner fails on multi-line strings with quotes): `python3 << 'PYEOF'` / `content=open(f).read()` / `open(f,'w').write(content.replace(old,new,1))` / `PYEOF`
- Strategy listing: `cd shared_strategies/spot && ../../.venv/bin/python3 strategies.py --list-json` (must use venv for numpy/pandas; in worktrees use absolute path: `<main-repo>/.venv/bin/python3`)
- Smoke test: `./go-trader --once`
- Run with config: `./go-trader --config scheduler/config.json`
- Smoke test interactive CLI: `printf "answer1\nanswer2\n" | ./go-trader init`
- Smoke test JSON CLI: `./go-trader init --json '{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10}' --output /tmp/test.json`
- Smoke test HTF filter: `./go-trader init --json '{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10,"htfFilter":true}' --output /tmp/test.json` — verify `htf_filter: true` in output
- Python pytest: `uv run pytest shared_strategies/ -v` (spot + futures + options); `uv run pytest shared_tools/ -v`; `uv run pytest platforms/ -v`; `uv run pytest backtest/ -v` (registry coverage + backtester — run this when adding/modifying strategies)
- Strategy tests must assert actual signal values (e.g. `assert (result["signal"] == 1).any()`), not just column existence
- Strategy smoke tests that iterate every registered strategy must supply a `DatetimeIndex` (e.g. `pd.date_range("2024-01-01", periods=200, freq="15min")`) — `amd_ifvg` reads `index.hour`, `vwap_reversion` buckets by `index.date`; a `RangeIndex` crashes both
- Python test imports use `importlib.util.spec_from_file_location` to avoid module naming conflicts (two `strategies.py` files, adapter naming collisions)
- Go tests: always check `json.Unmarshal` return errors — silent discard masks struct tag/type regressions
- Go tests must NEVER call `RunPythonScript` (or wrappers like `RunHyperliquidExecute`/`RunHyperliquidClose`) — Go CI doesn't install `.venv/bin/python3`, so subprocess-based tests pass locally but fail in CI. Extract a pure parser (`parseXxxOutput(stdout, stderrStr, runErr)`) from the wrapper and test the parser directly with canned input. Same pattern as `perpsLiveOrderSize` / `PerpsOrderSkipReason` (#341, #342)
- `shared_scripts/test_*.py` are NOT in pytest's default `testpaths` (`pyproject.toml` lists only `shared_tools`/`platforms`/`backtest/tests`) — invoke explicitly: `uv run pytest shared_scripts/test_close_hyperliquid_position.py -v`
- Test helpers (stub builders, mock factories) live in `package main` alongside tests — name them with platform/feature prefixes (`stubHLLiveCloser`, not `stubCloser`) to avoid collisions across `*_test.go` files (e.g. `shared_wallet_test.go` already defines `stubFetcher`)
