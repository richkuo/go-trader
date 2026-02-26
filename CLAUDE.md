# go-trader Project Context

## Environment
- Requires Go 1.26.0 — install via Homebrew: `brew install go@1.26`
- Go is not in PATH via shell; use `/opt/homebrew/bin/go` directly (e.g. `cd scheduler && /opt/homebrew/bin/go build .`)
- Python venv at `.venv/bin/python3` (used by executor.go at runtime)
- Python deps managed with `uv` (see `pyproject.toml` / `uv.lock`)

## Setup
- `uv sync` — install Python deps into `.venv`
- Copy `scheduler/config.example.json` → `scheduler/config.json` and fill in API keys

## Repo Structure
- `scheduler/` — Go scheduler (single `package main`); all .go files compile together
  - `executor.go` — Python subprocess runner; max 4 concurrent, 30s timeout per script
  - `server.go` — HTTP status server (`/status`, `/health` endpoints)
  - `discord.go` — Discord alert notifications
- `shared_scripts/` — Python entry-point scripts called by the scheduler
  - `check_strategy.py` — spot strategy signal checker
  - `check_options.py` — unified options checker (`--platform=deribit|ibkr`)
  - `check_price.py` — price check script
- `platforms/` — platform-specific adapters (deribit, ibkr, binanceus)
  - `deribit/adapter.py` — DeribitExchangeAdapter (live quotes, real expiries/strikes)
  - `ibkr/adapter.py` — IBKRExchangeAdapter (CME strikes, Black-Scholes pricing)
  - `binanceus/adapter.py` — BinanceUSExchangeAdapter (spot only)
- `shared_tools/` — shared Python utilities (pricing.py, exchange_base.py, data_fetcher, storage)
- `shared_strategies/` — shared strategy logic (spot/, options/)
- `core/` — legacy data utilities used by backtest (data_fetcher, storage)
- `strategies/` — legacy spot strategy logic used by backtest
- `backtest/` — backtesting and paper trading scripts; `run_backtest.py` needs `PYTHONPATH=core:strategies`
- `archive/` — retired/unused modules
- `SKILL.md` — agent operations guide (setup, deploy, backtest commands)

## Key Patterns
- Git commands: always run from repo root, not from `scheduler/` (git add/commit fail with path errors otherwise)
- Platform adapters loaded via `importlib` in `check_options.py`; class discovered by `endswith("ExchangeAdapter")` — all adapter classes must use that suffix
- Scheduler communicates with Python scripts via subprocess stdout JSON; scripts must always output valid JSON even on error
- Python scripts exit 1 on error (Go parses JSON from stdout regardless of exit code)
- Option positions stored in `StrategyState.OptionPositions map[string]*OptionPosition`
- Mutex `mu sync.RWMutex` guards `state`; RLock for reads, Lock for all mutations
- Per-strategy loop uses 6 fine-grained lock phases: RLock(read inputs) → Lock(CheckRisk) → no lock(subprocess) → Lock(execute signal) → RLock/no lock/Lock(mark prices) → RLock(status log)
- Audit lock balance: `grep -n "mu\.\(R\)\?Lock\(\)\|mu\.\(R\)\?Unlock\(\)" scheduler/main.go`
- Platform dispatch: `StrategyConfig.Platform` field (inferred from ID prefix in LoadConfig); use `s.Platform == "ibkr"` not ID prefix checks
- State persisted to `scheduler/state.json` (path set in config); per-platform files at `platforms/<name>/state.json`

## Build & Deploy
- Build: `cd scheduler && /opt/homebrew/bin/go build -o ../go-trader .`
- Restart: `systemctl restart go-trader`
- Only needed when `scheduler/*.go` files change
- Python script changes take effect on next scheduler cycle (no rebuild needed)
- Config changes: `systemctl restart go-trader` (no rebuild)
- Service file changes: `systemctl daemon-reload && systemctl restart go-trader`

## Backtest
- `run_backtest.py`: `PYTHONPATH=core:strategies .venv/bin/python3 backtest/run_backtest.py --strategy <n> --symbol BTC/USDT --timeframe 1h --mode single`
- `backtest_options.py`: `.venv/bin/python3 backtest/backtest_options.py --underlying BTC --since YYYY-MM-DD --capital 10000`
- `backtest_theta.py`: `.venv/bin/python3 backtest/backtest_theta.py --underlying BTC --since YYYY-MM-DD --capital 10000`

## ISSUES.md
- When marking an issue fixed: update the row (`NO` → `YES`) **and** the Summary table at the bottom (`Fixed` count +1, `Unfixed` count -1 for that category and Total)

## Testing
- `python3 -m py_compile <file>` — syntax check Python files
- `cd scheduler && /opt/homebrew/bin/go build .` — compile check
- `cd scheduler && /opt/homebrew/bin/go test ./...` — run all unit tests
- `cd scheduler && /opt/homebrew/bin/gofmt -w <file>.go` — format after editing Go files (`-l *.go` lists all files needing formatting)
- Smoke test: `./go-trader --once`
- Run with config: `./go-trader --config scheduler/config.json`
