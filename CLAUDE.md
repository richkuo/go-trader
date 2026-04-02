# go-trader Project Context

## Environment
- Requires Go 1.26.0 — install via Homebrew: `brew install go@1.26`
- Go is not in PATH via shell; use `/opt/homebrew/bin/go` directly (e.g. `cd scheduler && /opt/homebrew/bin/go build .`)
- Python venv at `.venv/bin/python3` (used by executor.go at runtime)
- In git worktrees, `.venv` is NOT copied — use the main repo's venv: `/Users/richardkuo/Work/openclaw/go-trader/.venv/bin/python3`
- Python deps managed with `uv` (see `pyproject.toml` / `uv.lock`)

## Setup
- `uv sync` — install Python deps into `.venv`
- Copy `scheduler/config.example.json` → `scheduler/config.json` and fill in API keys

## Repo Structure
- `scheduler/` — Go scheduler (single `package main`); all .go files compile together
  - `executor.go` — Python subprocess runner; max 4 concurrent, 30s timeout per script
  - `server.go` — HTTP status server (`/status`, `/health` endpoints)
  - `discord.go` — `discordgo.Session` wrapper for two-way Discord communication; `NewDiscordNotifier(token, ownerID string) (*DiscordNotifier, error)` — opens WebSocket gateway; `SendMessage(channelID, content)` — channel posts; `SendDM(userID, content)` — DM send; `AskDM(userID, question, timeout)` — send + block on reply (returns `ErrDMTimeout`); intents: `discordgo.IntentsDirectMessages`; DM detection: `m.GuildID == ""`; `FormatCategorySummary(..., channelKey, asset string)` — `asset` non-empty adds " — BTC" suffix + filters prices line; `extractAsset(sc)` uses `Args[1]` as canonical asset source (strips `/USDT` for spot); `groupByAsset(strats)` groups by asset with BTC/ETH/SOL/BNB-first sort; `channelTradeDetails` keyed as `ch+"|"+asset`; `fmtComma` — always pass absolute values
  - `init.go` — `go-trader init` interactive wizard + `--json <blob>` non-interactive mode; `generateConfig(InitOptions) *Config` is pure/testable; `runInitFromJSON(jsonStr, outputPath)` for scripted config gen (e.g. from OpenClaw); `runInit` orchestrates I/O
  - `prompt.go` — `Prompter` struct (String/YesNo/Choice/MultiSelect/Float); inject `NewPrompterFromReader(r,w)` for tests
  - `updater.go` — update checker; `checkForUpdates(cfg, discord, &lastNotifiedHash, &mu, state)` — git fetch, channel notify + DM upgrade prompt (goroutine); `applyUpgrade(discord, ownerID, mu, state, cfg)` — git pull + go build + state save + restart; `restartSelf()` — systemctl → syscall.Exec fallback; logs `[update]` prefix
  - `correlation.go` — `ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)` — per-asset directional exposure (delta-USD) across all strategies; `CorrelationSnapshot` with `AssetExposure` per asset; warns on concentration and same-direction thresholds; `findSpotPrice(asset, prices)` resolves asset to price
  - `config_migration.go` — `CurrentConfigVersion = 2`; `NewFieldsSince(version)` returns new fields; `MigrateConfig(path, values)` atomic JSON patch + version bump; `runConfigMigrationDM(cfg, discord, configPath)` DMs owner per new field with 10min timeout
- `shared_scripts/` — Python entry-point scripts called by the scheduler
  - `check_strategy.py` — spot strategy signal checker
  - `check_options.py` — unified options checker (`--platform=deribit|ibkr|robinhood`)
  - `check_price.py` — price check script
  - `check_hyperliquid.py` — Hyperliquid perps checker (`<strategy> <symbol> <timeframe> [--mode=paper|live]`; `--execute` for live orders)
  - `check_topstep.py` — TopStep futures checker (`<strategy> <symbol> <timeframe> [--mode=paper|live]`; `--execute` for live orders)
  - `check_robinhood.py` — Robinhood crypto checker (`<strategy> <symbol> <timeframe> [--mode=paper|live]`; `--execute` for live orders; OHLCV via yfinance)
- `platforms/` — platform-specific adapters (deribit, ibkr, binanceus, hyperliquid, topstep, robinhood)
  - `deribit/adapter.py` — DeribitExchangeAdapter (live quotes, real expiries/strikes)
  - `ibkr/adapter.py` — IBKRExchangeAdapter (CME strikes, Black-Scholes pricing)
  - `binanceus/adapter.py` — BinanceUSExchangeAdapter (spot only)
  - `hyperliquid/adapter.py` — HyperliquidExchangeAdapter (live perps prices, paper/live trading via `HYPERLIQUID_SECRET_KEY`)
  - `topstep/adapter.py` — TopStepExchangeAdapter (CME futures, paper mode via yfinance, live via TopStepX API)
  - `robinhood/adapter.py` — RobinhoodExchangeAdapter (crypto spot + stock options, paper mode via yfinance/Black-Scholes, live via robin_stocks + TOTP MFA)
- `shared_tools/` — shared Python utilities (pricing.py, exchange_base.py, data_fetcher, storage)
- `shared_strategies/` — shared strategy logic (spot/, options/, futures/)
- `backtest/` — backtesting and paper trading scripts
- `archive/` — retired/unused modules
- `SKILL.md` — agent operations guide (setup, deploy, backtest commands)

## Key Patterns
- Git commands: always run from repo root, not from `scheduler/` (git add/commit fail with path errors otherwise)
- Adding a new platform: (1) `platforms/<name>/adapter.py` + `__init__.py`, (2) `shared_scripts/check_<name>.py`, (3) `executor.go` (result types + runner), (4) `config.go` (prefix inference + validation), (5) `fees.go` (fee dispatch), (6) `main.go` (dispatch case + helpers), (7) `init.go` (wizard + generateConfig), (8) `pyproject.toml` (deps)
- Adding options to an existing platform: extend adapter with Protocol methods (`get_vol_metrics`, `get_real_expiry`, `get_real_strike`, `get_premium_and_greeks`), add platform to `check_options.py` usage + `CalculateOptionFee` dispatch, add to init wizard `OptionPlatforms`; no main.go/executor.go changes needed (options dispatch is platform-agnostic)
- Platform adapters loaded via `importlib` in `check_options.py`; class discovered by `endswith("ExchangeAdapter")` — only one adapter class per file; `_fetch_ohlcv_closes()` supports adapter-aware fallback via `get_ohlcv_closes()` method for non-BinanceUS platforms
- Scheduler communicates with Python scripts via subprocess stdout JSON; scripts must always output valid JSON even on error
- Python scripts exit 1 on error (Go parses JSON from stdout regardless of exit code)
- Option positions stored in `StrategyState.OptionPositions map[string]*OptionPosition`
- Mutex `mu sync.RWMutex` guards `state`; RLock for reads, Lock for all mutations
- Per-strategy loop uses 6 fine-grained lock phases: RLock(read inputs) → Lock(CheckRisk) → no lock(subprocess) → Lock(execute signal) → RLock/no lock/Lock(mark prices) → RLock(status log)
- Audit lock balance: `grep -n "mu\.\(R\)\?Lock\(\)\|mu\.\(R\)\?Unlock\(\)" scheduler/main.go`
- Platform dispatch: `StrategyConfig.Platform` field (inferred from ID prefix in LoadConfig); use `s.Platform == "ibkr"` not ID prefix checks
- ID prefix → platform: `hl-` → hyperliquid, `ibkr-` → ibkr, `deribit-` → deribit, `ts-` → topstep, `rh-` → robinhood, else → binanceus
- Robinhood options use stock symbols (SPY, QQQ, AAPL) not crypto assets; strategy IDs: `rh-ccall-spy`, `rh-vol-qqq`; options config uses `--platform=robinhood` arg to check_options.py
- Strategy types: "spot", "options", "perps", "futures" — perps paper mode reuses `ExecuteSpotSignal`; live mode calls `RunHyperliquidExecute` before state update; futures use `ExecuteFuturesSignal` with whole-contract sizing and margin-based budgeting
- Hyperliquid sys.path conflict: SDK installs as `hyperliquid` package — clashes with `platforms/hyperliquid/`; fix: add `platforms/hyperliquid/` directly to sys.path (not `platforms/`), then `from adapter import HyperliquidExchangeAdapter`
- Fee dispatch: `CalculatePlatformSpotFee(platform, value)` — 0.035% hyperliquid, 0% robinhood, 0.1% binanceus (replaces bare `CalculateSpotFee` for platform-aware spot/perps trades); `CalculateFuturesFee(contracts, feePerContract)` and `CalculatePlatformFuturesFee(sc, contracts)` for futures per-contract fees
- State persisted to `scheduler/state.json` (path set in config); per-platform files at `platforms/<name>/state.json`
- `cfg.Discord.Channels` is `map[string]string` (not a struct); keys: "spot", "options", "hyperliquid", etc. — old `.Spot`/`.Options` field access is invalid
- `cfg.Discord.OwnerID` — Discord user ID for DM upgrade prompts + config migration; loaded from `DISCORD_OWNER_ID` env var (takes priority over config file)
- `cfg.ConfigVersion` — int, schema version (`0`/missing = v1 baseline); `CurrentConfigVersion = 2` in config_migration.go; startup triggers `runConfigMigrationDM` when below current version
- `cfg.Correlation` — `*CorrelationConfig` with `Enabled` (default false), `MaxConcentrationPct` (default 60), `MaxSameDirectionPct` (default 75); computed under RLock, state assigned under Lock; warnings sent to all Discord channels + owner DM
- `cfg.AutoUpdate` — `"off"` (default), `"daily"` (once/day), `"heartbeat"` (every cycle); handled in main.go loop + startup; uses `dailyCycles = (24*3600)/tickSeconds`
- Strategy registry imports: `check_hyperliquid.py` and `check_strategy.py` import from `shared_strategies/spot/strategies.py`; `check_topstep.py` imports from `shared_strategies/futures/strategies.py` — a new strategy must be registered in both if it needs to work across platforms
- Adding a cross-platform strategy: create core logic in `shared_strategies/<name>.py`, then import+register in both `spot/strategies.py` and `futures/strategies.py` (same pattern as indicators.py)
- Adding a new spot/futures strategy (no new platform): (1) add `@register_strategy` function to `shared_strategies/spot/strategies.py`, (2) add same to `shared_strategies/futures/strategies.py`, (3) add short name to `knownShortNames` in `scheduler/init.go` — auto-discovery handles all platform configs
- Spot and futures have independent `STRATEGY_REGISTRY` dicts — a new strategy must be added to both files with `@register_strategy` decorator; perps auto-discovers from spot via `discoverStrategies()`
- Strategy discovery: `shared_strategies/spot/strategies.py --list-json`, `shared_strategies/options/strategies.py --list-json`, and `shared_strategies/futures/strategies.py --list-json` output JSON arrays of `{"id":..., "description":...}`

## Build & Deploy
- Build: `cd scheduler && /opt/homebrew/bin/go build -o ../go-trader .`
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
- `python3 -m py_compile <file>` — syntax check Python files
- `cd scheduler && /opt/homebrew/bin/go build .` — compile check
- `cd scheduler && /opt/homebrew/bin/go test ./...` — run all unit tests (must run from scheduler/ where go.mod lives; repo root has no go.mod)
- `cd scheduler && /opt/homebrew/bin/gofmt -w <file>.go` — format after editing Go files (`-l *.go` lists all files needing formatting)
- Multi-line Go edits with tabs: Edit tool may fail on tab-indented blocks; use heredoc form (one-liner fails on multi-line strings with quotes): `python3 << 'PYEOF'` / `content=open(f).read()` / `open(f,'w').write(content.replace(old,new,1))` / `PYEOF`
- Strategy listing: `cd shared_strategies/spot && ../../.venv/bin/python3 strategies.py --list-json` (must use venv for numpy/pandas)
- Smoke test: `./go-trader --once`
- Run with config: `./go-trader --config scheduler/config.json`
- Smoke test interactive CLI: `printf "answer1\nanswer2\n" | ./go-trader init`
- Smoke test JSON CLI: `./go-trader init --json '{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10}' --output /tmp/test.json`
