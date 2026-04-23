# Architecture (high level)

## Flow

Config lists strategies → Go tick loop picks **due** strategies → loads **prices/marks** (mix of Go HTTP + Python) → **portfolio risk / kill switch** → per strategy: **RLock** snapshot → **CheckRisk** → **Python check script** (signal JSON) → optional **Python --execute** (live fill JSON) → **Lock** and update virtual state → **SaveState** SQLite; **Discord/Telegram** read the same channel routing from config.

Go owns scheduling, locks, persistence, risk gates, kill-switch orchestration, and which script/args run. Python owns OHLCV, adapter calls, strategy math, and most live order submission.

## Platform surface (what each venue needs)

Everything below is “per platform” unless noted. Duplication often appears where two platforms implement the same concern in different files (Go dispatch + Python check + adapter).

| Concern | Where it lives |
|--------|----------------|
| Market data & signals | `platforms/<venue>/adapter.py` + `shared_scripts/check_<venue>.py` (or generic `check_strategy.py` / `check_options.py` with `--platform=`) |
| Live orders | Same check script with `--execute` (or dedicated close scripts for kill switch) |
| Go dispatch | `main.go`: branch on `StrategyConfig.Type` (`spot`, `options`, `perps`, `futures`) and `Platform` |
| Result parsing / types | `executor.go` |
| Fees | `fees.go` (`CalculatePlatformSpotFee`, options/futures helpers) |
| Marks (some venues) | Native Go: HL, OKX perps, Deribit ticker; else Python subprocess |

**Spot-ish** (cash/spot or spot-style sizing): Binance US via `check_strategy.py`; OKX spot and Robinhood spot have their own check + `run*Check` / `execute*Result` in Go.

**Perps**: Hyperliquid (`check_hyperliquid.py`), OKX swap (`check_okx.py`). Shared futures strategy registry in Python; Go paths duplicate the “check → maybe execute → apply state” pattern.

**Futures (contracts)**: TopStep (`check_topstep.py`) + CME marks subprocess.

**Options**: Single `check_options.py` + `importlib` adapters (Deribit, IBKR, Robinhood, OKX); Go side is mostly type-agnostic once JSON is parsed.

**Kill-switch flatten**: Per platform: `kill_switch_close.go` plus `*_close.go` / balance fetchers; not every venue has full auto-close (gaps called out in planner for OKX spot, RH options).

**Notifications**: `notifier.go` — same `MultiNotifier` for Discord and Telegram; **channels** are keyed by platform/type string maps in config, not separate code paths per backend.

## Strategies (cross-platform)

Single registry: `shared_strategies/registry.py`. `spot/strategies.py` and `futures/strategies.py` are thin filters for `--list-json` / Go discovery. Adding behavior usually touches registry once; platform duplication is in **check scripts and Go execute helpers**, not strategy definitions.

## Duplication analysis (four areas)

### 1. Go: `main.go` check → live execute → apply state

**What repeats:** Per venue you see the same skeleton: append `--htf-filter` / `--params`, log `Running: python3 …`, parse stderr, map signal to `BUY`/`SELL`, validate price, then `liveExecFailed` + `run*ExecuteOrder` + `execute*Result` under `Lock`. `runOKXExecuteOrder` and friends also duplicate skip-reason and sizing logic that must stay aligned with `Execute*Signal` (see comments around `#298` / `#300`).

**Shared module vs shared function:** A **table of small structs** (e.g. `runCheck func(...)`, `runLive func(...)`, `apply func(...)`) keyed by `(Type, Platform)` removes lines but not subprocesses. Extracting **helpers** only (`appendScriptArgs(sc)`, `signalString(sig int)`, `logScriptFailure`) is low risk. **Generics/interfaces** across `*HyperliquidResult` vs `*OKXResult` either force `any` + casts or a thin per-type wrapper — maintainability win, not a free lunch.

**Efficiency:** **Runtime:** essentially unchanged (same number of Python runs per strategy). **Engineering:** fewer places to miss `liveExecFailed` parity. **Risk:** medium — easy to break skip-reason symmetry; needs tight regression tests before/after.

### 2. Python: `check_<venue>.py` boilerplate and OHLCV

**What repeats:** Identical patterns appear across scripts (e.g. `_make_dataframe` in `check_okx.py`, `check_hyperliquid.py`, `check_topstep.py`, `check_robinhood.py`; JSON error payloads; path bootstrapping). `check_hyperliquid.py` also carries a `SafeEncoder` class — other scripts may diverge on NaN handling.

**Shared library (`shared_scripts/` e.g. `check_runtime.py`):** One module for **dataframe from candles**, **safe JSON stdout**, **common argparse flags** (`--params`, `--htf-filter`, `--mode`) would shrink files and keep error JSON consistent. **CPU / wall time:** negligible (one import vs duplicated inline code).

**OHLCV refetch:** The real cost is **N strategies × cold subprocess × network fetch** for the same `(venue, symbol, timeframe)`. Fixing that needs **either** (a) a **disk or shared-memory cache** keyed by candle set / last closed bar time, **or** (b) **batching** multiple strategies into one Python invocation per cycle. That is a **large** design change but the only option here with **large runtime efficiency** upside.

### 3. Fees and marks (`fees.go`, Go mark fetchers, `executor.go`)

**What repeats:** Fee **rates** are already centralized in Go (`CalculatePlatformSpotFee`, option helpers in `fees.go`). Marks for HL/OKX perps and Deribit live in dedicated Go files to avoid Python round-trips.

**Shared module:** Splitting `package fees` / `package marks` under `scheduler/internal/` is **organizational** — same algorithms, **no** meaningful runtime change. **Efficiency wins** come from **moving more mark paths from Python to Go** (fewer subprocesses / less JSON) or **reusing one HTTP response** across strategies in a cycle — not from renaming packages.

**Python fee math:** If any script still duplicates Go fee constants, **delete script-side fee logic** and keep Go as single source of truth for virtual PnL — avoids drift, not a big CPU win.

### 4. Channels: Discord vs Telegram

**What repeats:** Config carries **parallel** maps (`discord.channels` vs `telegram.channels`, DM maps, etc.). Runtime is already unified: `MultiNotifier` + `Notifier` interface + `resolveChannelKey` — **no duplicated send logic per provider**.

**Shared “module”:** A **config schema** that lists destinations as `{provider, channel_id, key}` rows instead of two trees would reduce **operator copy-paste** and config drift. **Optional:** generate one map from the other in `init` — still config-layer. **Runtime efficiency:** none; benefit is **fewer mis-wired channels** and simpler edits.
