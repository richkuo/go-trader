# go-trader Project Context

## Environment
- Go is not in PATH; `go build` will fail — verify syntax manually or via `python3 -m py_compile` for Python files
- Python venv at `.venv/bin/python3` (used by executor.go at runtime)
- Python deps managed with `uv` (see `pyproject.toml` / `uv.lock`)

## Repo Structure
- `scheduler/` — Go scheduler (single `package main`); all .go files compile together
- `scripts/` — Python strategy scripts called as subprocesses by the scheduler
- `core/` — shared Python data utilities (data_fetcher, storage)
- `strategies/` — strategy logic imported by check_strategy.py

## Key Patterns
- Scheduler communicates with Python scripts via subprocess stdout JSON; scripts must always output valid JSON even on error
- Python scripts exit 1 on error (Go parses JSON from stdout regardless of exit code)
- Option positions stored in `StrategyState.OptionPositions map[string]*OptionPosition`
- Mutex `mu sync.RWMutex` guards `state`; RLock for reads, Lock for all mutations
- `deribit_utils.py` is imported by `check_options.py` — both must be updated together for Deribit API changes

## Testing
- `python3 -m py_compile <file>` — syntax check Python files
- `go build ./scheduler/` — compile check (requires Go in PATH)
- No automated test suite; smoke test with `./go-trader --once`
