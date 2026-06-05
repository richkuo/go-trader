# Shared Regime Bundle (issue #879) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move regime computation out of each strategy's per-cycle Python check subprocess into a single Go-scheduler-owned global regime store, computed once per distinct `(symbol, interval, windows-spec)` signature per cycle, injected into every check, and read directly by flat-`manual`, `options`, and the dashboard.

**Architecture:** A per-cycle in-memory two-layer store in the Go scheduler (`regime_calculator.go`). Before Phase 3, the scheduler collects the distinct regime signatures of due strategies (+ flat `manual`/`options`), runs one dedicated read-only regime subprocess (`shared_scripts/fetch_regime.py`) per distinct signature (concurrent, gated by `pythonSemaphore`), and stores the resulting `RegimePayload` keyed by signature. Each check dispatch **injects** the precomputed payload into the check script via `--regime-injected --regime-payload-json <json>`; the check skips `prepare_check_regime` and echoes the payload back as `result.Regime`, so existing Go consumers (`applyRegimeGate`, `syncStrategyRegimeState`, `stampPositionRegimeIfOpened`, `strategyCurrentATRRegime`, `applyRegimeDirectionalPolicy`) are unchanged — the *source* of `result.Regime` is now the store. Flat `manual`, `options`, and the dashboard read the store directly. Failure policy: a failed regime subprocess clears that signature to empty; every consumer falls back to its existing empty-case (fail-open) behavior.

**Why parity holds:** Regime windows live in **global** `cfg.Regime.Windows` (one spec for all strategies; per-strategy fields are only *selectors* — gate/atr/directional window, `allowed_regimes`, ATR-regime blocks). So the dedup unit is `(symbol, interval)` + a fingerprint of the resolved global windows spec. The subprocess computes each configured window with **its own classifier/period** via the existing `compute_multi_regime`/`classify_window` — identical to what each check computes inline today. An ADX window at `period=20` already uses full-period ADX; the `COMPOSITE_ADX_PERIOD_CAP=14` only applies *inside* composite windows. No single-pass prefix-collapse is used, so labels are byte-identical pre/post migration for the same candles.

**Tech Stack:** Go (scheduler, `modernc.org/sqlite`), Python 3 (`shared_tools/regime.py`, platform adapters via `importlib`), subprocess contract (JSON stdout, exit 1 on error), systemd `PrivateTmp` `/tmp` OHLCV cache (#839).

---

## Key invariants (must hold across every task)

1. **No behavior change for any strategy's label** for the same candles — inline-computed `result.Regime` (pre) == store-injected `result.Regime` (post). Enforced by parity tests.
2. **`pos.Regime` stays the frozen-at-open stamp** (coordination with #873). `stampPositionRegimeIfOpened` semantics unchanged — only the payload source changes.
3. **Failure = clear-to-empty (policy b).** Regime subprocess failure → store empty for that signature → consumers fall open. Open positions read frozen `pos.Regime`/`pos.RegimeAppliedLabel`, never the live store, so they are unaffected.
4. **One documented semantic delta:** post-split a check can succeed while regime is empty, so a fresh open fires **fail-open instead of skip** on a regime-data outage. Accepted.
5. **Look-ahead unchanged:** the subprocess computes on the same closed candles the check would fetch; gate reads the resolved label exactly as today.
6. **Go+Python same-SHA deploy:** every new required CLI flag is appended to `version_probe.go`.

---

## File Structure

**New files:**
- `scheduler/regime_calculator.go` — `RegimeSignature`, `RegimeStore`, signature collection, per-cycle build orchestration, store reads.
- `scheduler/regime_calculator_test.go` — store dedup, failure-clear, signature collection, payload reads.
- `shared_scripts/fetch_regime.py` — platform-aware OHLCV fetch + `compute_multi_regime` + emit payload JSON. Read-only.
- `shared_scripts/test_fetch_regime.py` — subprocess output shape + parity vs `prepare_check_regime`.

**Modified files:**
- `scheduler/main.go` — build store before Phase 3; inject store payload into each `runXxxCheck`; flat-`manual` + `options` read store; dashboard wiring.
- `scheduler/strategy_composition.go` — `appendInjectedRegimeArgs(args, payload)` builder; keep `appendRegimeArgs` for the windows-spec the subprocess uses.
- `scheduler/regime_run.go` (new, or in regime_calculator.go) — `runRegimeSubprocess(platform, symbol, interval, specJSON) (RegimePayload, error)`.
- `scheduler/version_probe.go` — probe `fetch_regime.py`; probe `--regime-injected`/`--regime-payload-json` on each check script.
- `scheduler/script_failure_alerts.go` — reuse `scriptFailureTracker` for regime subprocess failures (per-signature key).
- `shared_scripts/check_hyperliquid.py`, `check_strategy.py`, `check_options.py`, `check_okx.py`, `check_robinhood.py`, `check_topstep.py` — accept `--regime-injected`/`--regime-payload-json`; skip `prepare_check_regime` when injected; echo payload back.
- `shared_tools/regime.py` — `resolve_injected_regime(payload, *, primary_key, atr_window)` helper returning `(stdout_payload, live_atr_label, strategy_params_snapshot)` from an injected multi-window payload (mirrors `prepare_check_regime`'s return contract without recomputing).
- `scheduler/ui_server.go` / dashboard — portfolio-level regime view from the store.
- `backtest/` — **no functional change**; add parity test only.

---

## Task sequence (build order)

Phase A (Tasks 1-5): Go store + subprocess + injection helper, fully tested in isolation.
Phase B (Tasks 6-9): wire HL as the reference platform end-to-end (inject + echo + store reads), parity test.
Phase C (Tasks 10-13): replicate injection to spot/okx/robinhood/topstep; options; flat-manual.
Phase D (Tasks 14-16): dashboard view; backtest parity test; probe argv + smoke + full parity matrix.

---

### Task 1: Regime signature + store types (Go)

**Files:**
- Create: `scheduler/regime_calculator.go`
- Test: `scheduler/regime_calculator_test.go`

- [ ] **Step 1: Write failing test** for signature derivation + store get/put.

```go
package main

import "testing"

func TestRegimeSignatureForStrategy(t *testing.T) {
    rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
    sc := StrategyConfig{ID: "hl-btc", Symbol: "BTC", Args: []string{"momentum", "BTC", "1h"}, Platform: "hyperliquid"}
    sig := regimeSignatureForStrategy(sc, rc)
    if sig.Symbol != "BTC" || sig.Interval != "1h" {
        t.Fatalf("got %+v", sig)
    }
    // Two strategies, same asset/interval/global-spec → identical signature.
    sc2 := StrategyConfig{ID: "hl-btc-2", Symbol: "BTC", Args: []string{"meanrev", "BTC", "1h"}, Platform: "hyperliquid"}
    if regimeSignatureForStrategy(sc2, rc) != sig {
        t.Fatal("peers on same asset/interval must share a signature")
    }
}

func TestRegimeStorePutGet(t *testing.T) {
    s := newRegimeStore()
    sig := RegimeSignature{Symbol: "BTC", Interval: "1h", SpecHash: "abc"}
    pl := RegimePayload{Legacy: "trending_up"}
    s.put(sig, pl, nil)
    got, ok := s.get(sig)
    if !ok || got.PrimaryLabel(nil) != "trending_up" {
        t.Fatalf("get failed: %+v ok=%v", got, ok)
    }
}

func TestRegimeStoreFailureClears(t *testing.T) {
    s := newRegimeStore()
    sig := RegimeSignature{Symbol: "BTC", Interval: "1h", SpecHash: "abc"}
    s.put(sig, RegimePayload{}, errSentinel)
    got, ok := s.get(sig)
    if !ok {
        t.Fatal("failed signature must still be present (empty), to signal fail-open not missing")
    }
    if !got.IsEmpty() {
        t.Fatal("failed signature payload must be empty")
    }
    if !s.failed(sig) {
        t.Fatal("failed() must report the failure for alerting")
    }
}

var errSentinel = errString("boom")
type errString string
func (e errString) Error() string { return string(e) }
```

- [ ] **Step 2: Run test, verify FAIL** (`go test ./scheduler/ -run TestRegime -v`) — undefined symbols.

- [ ] **Step 3: Implement** `regime_calculator.go`:

```go
package main

import (
    "crypto/sha256"
    "encoding/hex"
    "sync"
)

// RegimeSignature dedups regime computation. Windows live in global
// cfg.Regime, so the unit is (symbol, interval) + a fingerprint of the
// resolved global windows-spec JSON (constant today, future-proof if
// per-strategy specs are ever added).
type RegimeSignature struct {
    Symbol   string
    Interval string
    SpecHash string
}

func regimeSignatureForStrategy(sc StrategyConfig, rc *RegimeConfig) RegimeSignature {
    return RegimeSignature{
        Symbol:   regimeSignatureSymbol(sc),
        Interval: strategyTimeframe(sc),
        SpecHash: regimeSpecHash(rc),
    }
}

// regimeSignatureSymbol returns the bare asset symbol used by the platform's
// OHLCV fetch (e.g. "BTC" for HL, "BTC/USDT" for spot). Mirrors the symbol the
// check script passes to adapter.get_ohlcv.
func regimeSignatureSymbol(sc StrategyConfig) string {
    if len(sc.Args) >= 2 {
        return sc.Args[1]
    }
    return sc.Symbol
}

func strategyTimeframe(sc StrategyConfig) string {
    if len(sc.Args) >= 3 {
        return sc.Args[2]
    }
    return ""
}

func regimeSpecHash(rc *RegimeConfig) string {
    blob := regimeWindowsSpecJSON(rc)
    sum := sha256.Sum256([]byte(blob))
    return hex.EncodeToString(sum[:8])
}

type regimeStoreEntry struct {
    payload RegimePayload
    failed  bool
}

// RegimeStore is the per-cycle two-layer store. Rebuilt every cycle (empty at
// loop start), never persisted. A failed signature is present-but-empty so
// reads fail-open rather than treating "missing" as "compute inline".
type RegimeStore struct {
    mu      sync.RWMutex
    entries map[RegimeSignature]regimeStoreEntry
}

func newRegimeStore() *RegimeStore {
    return &RegimeStore{entries: make(map[RegimeSignature]regimeStoreEntry)}
}

func (s *RegimeStore) put(sig RegimeSignature, pl RegimePayload, err error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if err != nil {
        s.entries[sig] = regimeStoreEntry{payload: RegimePayload{}, failed: true}
        return
    }
    s.entries[sig] = regimeStoreEntry{payload: pl}
}

func (s *RegimeStore) get(sig RegimeSignature) (RegimePayload, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    e, ok := s.entries[sig]
    return e.payload, ok
}

func (s *RegimeStore) failed(sig RegimeSignature) bool {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.entries[sig].failed
}

// payloadForStrategy is the consumer entry point: returns the store payload for
// a strategy's signature (empty payload when missing/failed → fail-open).
func (s *RegimeStore) payloadForStrategy(sc StrategyConfig, rc *RegimeConfig) RegimePayload {
    if rc == nil || !rc.Enabled {
        return RegimePayload{}
    }
    pl, _ := s.get(regimeSignatureForStrategy(sc, rc))
    return pl
}
```

- [ ] **Step 4: Run test, verify PASS.**

- [ ] **Step 5: Commit.**
```bash
git add scheduler/regime_calculator.go scheduler/regime_calculator_test.go
git commit -m "feat(regime): per-cycle global regime store + signature (#879)"
```

> **NOTE for implementer:** confirm `regimeSignatureSymbol`/`strategyTimeframe` against each platform's actual `sc.Args` layout (HL: `[strategy, SYMBOL, tf, ...]`). If any platform deviates, add a platform switch here. Verify with a unit test per platform before relying on it.

---

### Task 2: Regime subprocess invoker (Go)

**Files:**
- Modify: `scheduler/regime_calculator.go`
- Test: `scheduler/regime_calculator_test.go`

- [ ] **Step 1: Write failing test** for argv construction (pure, no subprocess — mirror `version_probe.go` stub pattern).

```go
func TestRegimeSubprocessArgv(t *testing.T) {
    argv := regimeSubprocessArgv("hyperliquid", "BTC", "1h",
        `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`, 200)
    joined := strings.Join(argv, " ")
    for _, want := range []string{"--platform=hyperliquid", "--symbol=BTC", "--interval=1h",
        "--regime-windows-spec-json", "--ohlcv-limit=200"} {
        if !strings.Contains(joined, want) {
            t.Fatalf("argv missing %q: %v", want, argv)
        }
    }
}
```

- [ ] **Step 2: Run test, verify FAIL.**

- [ ] **Step 3: Implement** the argv builder + a subprocess runner that reuses the existing `runPython` (read-only — NOT `runPythonSideEffect`) path. Parse stdout JSON `{"regime": <payload>, "bar_time": <int>}` into `RegimePayload`.

```go
const regimeFetchScript = "shared_scripts/fetch_regime.py"

func regimeSubprocessArgv(platform, symbol, interval, specJSON string, ohlcvLimit int) []string {
    return []string{
        "--platform=" + platform,
        "--symbol=" + symbol,
        "--interval=" + interval,
        "--regime-windows-spec-json", specJSON,
        fmt.Sprintf("--ohlcv-limit=%d", ohlcvLimit),
    }
}

type regimeSubprocessOutput struct {
    Regime  RegimePayload `json:"regime"`
    BarTime int64         `json:"bar_time"`
    Error   string        `json:"error"`
}

// runRegimeSubprocessFn is a package var so tests stub it (Go CI has no .venv).
var runRegimeSubprocessFn = runRegimeSubprocess

func runRegimeSubprocess(platform, symbol, interval, specJSON string, ohlcvLimit int) (RegimePayload, error) {
    argv := regimeSubprocessArgv(platform, symbol, interval, specJSON, ohlcvLimit)
    stdout, stderr, err := runPython(regimeFetchScript, argv) // read-only; respects pythonSemaphore/scriptTimeout
    if err != nil {
        return RegimePayload{}, fmt.Errorf("regime subprocess %s/%s/%s: %w (stderr: %s)", platform, symbol, interval, err, strings.TrimSpace(stderr))
    }
    var out regimeSubprocessOutput
    if jerr := json.Unmarshal([]byte(stdout), &out); jerr != nil {
        return RegimePayload{}, fmt.Errorf("regime subprocess %s/%s/%s: bad json: %w", platform, symbol, interval, jerr)
    }
    if out.Error != "" {
        return RegimePayload{}, fmt.Errorf("regime subprocess %s/%s/%s: %s", platform, symbol, interval, out.Error)
    }
    return out.Regime, nil
}
```

> **Implementer:** confirm the exact signature of the existing `runPython` wrapper in `executor.go` (it may return `(stdout string, stderr string, err error)` or a struct). Match it. Do NOT use `runPythonSideEffect`. Add `regimePlatformForStrategy(sc)` returning the `--platform` token each adapter expects (`hyperliquid`, `binanceus`, `okx`, `robinhood`, `topstep`); derive from `sc.Platform`.

- [ ] **Step 4: Run test, verify PASS.**
- [ ] **Step 5: Commit.** `feat(regime): regime subprocess invoker (#879)`

---

### Task 3: Per-cycle store build orchestration (Go)

**Files:**
- Modify: `scheduler/regime_calculator.go`
- Test: `scheduler/regime_calculator_test.go`

- [ ] **Step 1: Write failing test** — collect distinct signatures from due strategies, dedup peers, stub the runner, assert one call per distinct signature and failure-clear.

```go
func TestBuildRegimeStoreDedupsAndClearsFailures(t *testing.T) {
    rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
    due := []StrategyConfig{
        {ID: "a", Symbol: "BTC", Platform: "hyperliquid", Args: []string{"m", "BTC", "1h"}},
        {ID: "b", Symbol: "BTC", Platform: "hyperliquid", Args: []string{"r", "BTC", "1h"}}, // peer → dedup
        {ID: "c", Symbol: "ETH", Platform: "hyperliquid", Args: []string{"m", "ETH", "1h"}},
        {ID: "d", Symbol: "ETH", Platform: "hyperliquid", Args: []string{"m", "ETH", "4h"}}, // distinct interval
    }
    var calls int32
    orig := runRegimeSubprocessFn
    defer func() { runRegimeSubprocessFn = orig }()
    runRegimeSubprocessFn = func(platform, symbol, interval, spec string, limit int) (RegimePayload, error) {
        atomic.AddInt32(&calls, 1)
        if symbol == "ETH" && interval == "4h" {
            return RegimePayload{}, errSentinel // failure path
        }
        return RegimePayload{Legacy: "trending_up"}, nil
    }
    store := buildRegimeStore(due, rc)
    if calls != 3 {
        t.Fatalf("expected 3 distinct signatures, got %d", calls)
    }
    if lbl := store.payloadForStrategy(due[0], rc).PrimaryLabel(rc); lbl != "trending_up" {
        t.Fatalf("BTC/1h label = %q", lbl)
    }
    sigFail := regimeSignatureForStrategy(due[3], rc)
    if !store.failed(sigFail) {
        t.Fatal("ETH/4h must be marked failed")
    }
    if !store.payloadForStrategy(due[3], rc).IsEmpty() {
        t.Fatal("failed signature must read empty (fail-open)")
    }
}
```

- [ ] **Step 2: Run, verify FAIL.**
- [ ] **Step 3: Implement** `buildRegimeStore` — concurrent fan-out gated by the existing `pythonSemaphore` (do NOT exceed it; reuse the same semaphore the check fan-out uses so total concurrency is bounded). One subprocess per distinct signature.

```go
func collectRegimeSignatures(due []StrategyConfig, rc *RegimeConfig) map[RegimeSignature]StrategyConfig {
    out := make(map[RegimeSignature]StrategyConfig)
    if rc == nil || !rc.Enabled {
        return out
    }
    for _, sc := range due {
        if !strategyParticipatesInRegime(sc) {
            continue
        }
        sig := regimeSignatureForStrategy(sc, rc)
        if sig.Symbol == "" || sig.Interval == "" {
            continue
        }
        if _, seen := out[sig]; !seen {
            out[sig] = sc // representative strategy supplies platform
        }
    }
    return out
}

func buildRegimeStore(due []StrategyConfig, rc *RegimeConfig) *RegimeStore {
    store := newRegimeStore()
    sigs := collectRegimeSignatures(due, rc)
    if len(sigs) == 0 {
        return store
    }
    specJSON := regimeWindowsSpecJSON(rc)
    limit := regimeRequiredOhlcvLimit(rc)
    var wg sync.WaitGroup
    for sig, rep := range sigs {
        wg.Add(1)
        go func(sig RegimeSignature, rep StrategyConfig) {
            defer wg.Done()
            pythonSemaphore <- struct{}{}
            defer func() { <-pythonSemaphore }()
            pl, err := runRegimeSubprocessFn(regimePlatformForStrategy(rep), sig.Symbol, sig.Interval, specJSON, limit)
            store.put(sig, pl, err)
        }(sig, rep)
    }
    wg.Wait()
    return store
}
```

> **Implementer:** `strategyParticipatesInRegime(sc)` = strategy is a regime consumer (any spot/perps/futures/manual/options whose platform fetches OHLCV at a timeframe). Include flat `manual` and `options` here so they get a store entry without a position. Confirm `pythonSemaphore` is the package-level buffered channel in `executor.go`; reuse it, don't make a new one.

- [ ] **Step 4: Run, verify PASS.**
- [ ] **Step 5: Commit.** `feat(regime): per-cycle store build orchestration (#879)`

---

### Task 4: `fetch_regime.py` subprocess (Python)

**Files:**
- Create: `shared_scripts/fetch_regime.py`
- Test: `shared_scripts/test_fetch_regime.py`

- [ ] **Step 1: Write failing test** — call the module's `compute()` with a synthetic DataFrame and a windows spec; assert payload equals `prepare_check_regime(...)[0]` (parity), and JSON shape.

```python
# test_fetch_regime.py
import importlib.util, json
from pathlib import Path
import pandas as pd

ROOT = Path(__file__).resolve().parents[1]

def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod); return mod

def _df():
    import numpy as np
    n = 120
    close = pd.Series(range(100, 100 + n)).astype(float)  # clean uptrend
    return pd.DataFrame({"timestamp": range(n), "open": close, "high": close + 1,
                         "low": close - 1, "close": close, "volume": [1.0]*n})

def test_fetch_regime_matches_prepare_check_regime():
    fr = _load("fetch_regime", ROOT / "shared_scripts" / "fetch_regime.py")
    regime = _load("regime_mod", ROOT / "shared_tools" / "regime.py")
    spec = {"default": {"classifier": "adx", "period": 14, "adx_threshold": 20}}
    df = _df()
    payload = fr.compute_payload(df, spec)
    expected, _, _ = regime.prepare_check_regime(df, regime_enabled=True, windows_spec=spec)
    assert payload == expected
```

- [ ] **Step 2: Run, verify FAIL** (`uv run --no-sync python -m pytest shared_scripts/test_fetch_regime.py -v`).

- [ ] **Step 3: Implement** `fetch_regime.py`. It mirrors the OHLCV-fetch + DataFrame construction of the check scripts (`_make_dataframe`, `adapter.get_ohlcv`), then computes the multi-window payload with `compute_multi_regime`. `--probe-only` short-circuits before any adapter call (per probe contract). Platform dispatch reuses the `importlib` adapter-loading convention (class `endswith("ExchangeAdapter")`). Emits `{"regime": <payload>, "bar_time": <ts>}` to stdout, JSON on error with exit 1.

```python
#!/usr/bin/env python3
"""#879: dedicated read-only regime subprocess.

Fetches OHLCV for one (platform, symbol, interval), computes the multi-window
regime payload via shared_tools/regime.py, and emits it. The scheduler runs this
ONCE per distinct (symbol, interval, windows-spec) signature per cycle and
injects the payload into every check via --regime-payload-json, so checks no
longer compute regime inline. Read-only: never places orders.
"""
import argparse, json, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "shared_tools"))
sys.path.insert(0, str(ROOT))

import pandas as pd
from regime import compute_multi_regime, parse_regime_windows_spec_json, required_ohlcv_limit


def _make_dataframe(candles):
    df = pd.DataFrame(candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    for col in ("open", "high", "low", "close", "volume"):
        df[col] = pd.to_numeric(df[col], errors="coerce")
    return df


def compute_payload(df: pd.DataFrame, spec: dict) -> dict:
    """Multi-window payload identical to prepare_check_regime(..., windows_spec=spec)[0]."""
    return compute_multi_regime(df, spec)


def _load_adapter(platform: str):
    # Mirrors the check-script importlib convention: one *ExchangeAdapter per file.
    import importlib.util, inspect
    plat_dir = ROOT / "platforms" / platform
    if platform == "hyperliquid":
        sys.path.insert(0, str(plat_dir))  # SDK clash workaround (see CLAUDE.md)
    spec = importlib.util.spec_from_file_location(f"{platform}_adapter", plat_dir / "adapter.py")
    mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
    for _, obj in inspect.getmembers(mod, inspect.isclass):
        if obj.__name__.endswith("ExchangeAdapter"):
            return obj()
    raise RuntimeError(f"no adapter class for platform {platform!r}")


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--platform", required=True)
    p.add_argument("--symbol", required=True)
    p.add_argument("--interval", required=True)
    p.add_argument("--regime-windows-spec-json", default="")
    p.add_argument("--ohlcv-limit", type=int, default=200)
    p.add_argument("--probe-only", action="store_true")
    args = p.parse_args()

    if args.probe_only:
        print(json.dumps({"regime": "", "bar_time": 0}))
        return 0

    try:
        spec = parse_regime_windows_spec_json(args.regime_windows_spec_json or None)
        if not spec:
            print(json.dumps({"regime": "", "bar_time": 0}))
            return 0
        limit = max(args.ohlcv_limit, required_ohlcv_limit(windows=spec))
        adapter = _load_adapter(args.platform)
        candles = adapter.get_ohlcv(args.symbol, interval=args.interval, limit=limit)
        if not candles or len(candles) < 30:
            print(json.dumps({"error": f"insufficient candles: {len(candles) if candles else 0}"}))
            return 1
        df = _make_dataframe(candles)
        payload = compute_payload(df, spec)
        bar_time = int(df["timestamp"].iloc[-1]) if len(df) else 0
        print(json.dumps({"regime": payload, "bar_time": bar_time}))
        return 0
    except Exception as exc:  # subprocess contract: JSON on stdout, exit 1
        print(json.dumps({"error": str(exc)}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
```

> **Implementer:** verify each adapter's `get_ohlcv(symbol, interval=, limit=)` signature matches (HL uses `interval=`/`limit=`; confirm okx `--inst-type`, robinhood, topstep, binanceus signatures — some may need a market-type arg). If a platform needs extra args (e.g. OKX `inst_type`, options underlying), thread them through from Go via the representative strategy's args. **Compile-check:** `uv run --no-sync python -m py_compile shared_scripts/fetch_regime.py`.

- [ ] **Step 4: Run test, verify PASS.**
- [ ] **Step 5: Commit.** `feat(regime): fetch_regime.py read-only regime subprocess (#879)`

---

### Task 5: `resolve_injected_regime` helper (Python)

**Files:**
- Modify: `shared_tools/regime.py`
- Test: `shared_tools/test_regime.py` (add cases)

- [ ] **Step 1: Write failing test** — injected payload reproduces `prepare_check_regime`'s 3-tuple exactly.

```python
def test_resolve_injected_regime_matches_prepare():
    from regime import prepare_check_regime, resolve_injected_regime
    import pandas as pd
    n = 120
    close = pd.Series(range(100, 100 + n)).astype(float)
    df = pd.DataFrame({"timestamp": range(n), "open": close, "high": close+1,
                       "low": close-1, "close": close, "volume": [1.0]*n})
    spec = {"medium": {"classifier": "adx", "period": 14, "adx_threshold": 20},
            "macro": {"classifier": "composite", "period": 30}}
    want_payload, want_live, want_strat = prepare_check_regime(df, regime_enabled=True, windows_spec=spec, atr_window="macro")
    inj_payload, inj_live, inj_strat = resolve_injected_regime(want_payload, primary_key="medium", atr_window="macro")
    assert inj_payload == want_payload
    assert inj_live == want_live
    assert inj_strat == want_strat
```

- [ ] **Step 2: Run, verify FAIL.**
- [ ] **Step 3: Implement** in `regime.py` — pure projection over the injected multi-window payload (or legacy string), no recomputation:

```python
def resolve_injected_regime(
    payload: dict | str,
    *,
    primary_key: str = "",
    atr_window: str = "",
) -> tuple[dict | str, str, dict]:
    """#879: reconstruct prepare_check_regime's 3-tuple from a scheduler-injected
    payload WITHOUT recomputing. `payload` is either the multi-window map
    {window: snapshot} or a legacy single snapshot/label string."""
    disabled = {"regime": "", "score": 0.0, "metrics": dict(_DEFAULT_METRICS)}
    if not payload:
        return "", "", disabled
    if isinstance(payload, str):
        legacy = {"regime": payload, "score": 0.0, "metrics": dict(_DEFAULT_METRICS), "classifier": CLASSIFIER_ADX}
        return payload, payload, legacy
    # multi-window map
    keys = sorted(payload.keys())
    pkey = primary_key if (primary_key and primary_key in payload) else (
        REGIME_PRIMARY_WINDOW_KEY if REGIME_PRIMARY_WINDOW_KEY in payload else (keys[0] if keys else ""))
    strategy_payload = payload.get(pkey, disabled)
    akey = (atr_window or pkey).strip() or pkey
    atr_entry = payload.get(akey, strategy_payload)
    live_atr = str(atr_entry.get("regime") or "") if isinstance(atr_entry, dict) else ""
    return payload, live_atr, strategy_payload
```

> **Parity check:** the `pkey`/`akey`/`live_atr` selection logic above must match `prepare_check_regime` (regime.py:457-469) line-for-line. Diff them.

- [ ] **Step 4: Run, verify PASS.**
- [ ] **Step 5: Commit.** `feat(regime): resolve_injected_regime projection helper (#879)`

---

### Task 6: Injection args builder (Go)

**Files:**
- Modify: `scheduler/strategy_composition.go`
- Test: `scheduler/strategy_composition_test.go`

- [ ] **Step 1: Write failing test:**

```go
func TestAppendInjectedRegimeArgs(t *testing.T) {
    pl := RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{
        "default": {Regime: "trending_up", Score: 0.4}}}
    got := appendInjectedRegimeArgs(nil, pl)
    j := strings.Join(got, " ")
    if !strings.Contains(j, "--regime-injected") || !strings.Contains(j, "--regime-payload-json") {
        t.Fatalf("missing injection flags: %v", got)
    }
    // empty payload still injects (fail-open: do NOT recompute inline)
    got2 := appendInjectedRegimeArgs(nil, RegimePayload{})
    if !strings.Contains(strings.Join(got2, " "), "--regime-injected") {
        t.Fatal("empty payload must still set --regime-injected to suppress inline compute")
    }
}
```

- [ ] **Step 2: Run, verify FAIL.**
- [ ] **Step 3: Implement:**

```go
// appendInjectedRegimeArgs (#879) forwards the scheduler-computed regime payload
// to a check script. --regime-injected ALWAYS set (even on empty payload) so the
// check skips prepare_check_regime — an empty payload means fail-open, never
// "recompute inline".
func appendInjectedRegimeArgs(args []string, payload RegimePayload) []string {
    out := append(args, "--regime-injected")
    blob, err := json.Marshal(payload)
    if err != nil || string(blob) == "null" {
        blob = []byte(`""`)
    }
    return append(out, "--regime-payload-json", string(blob))
}
```

- [ ] **Step 4: Run, verify PASS.**
- [ ] **Step 5: Commit.** `feat(regime): injected-regime args builder (#879)`

---

### Task 7: `check_hyperliquid.py` accepts injection

**Files:**
- Modify: `shared_scripts/check_hyperliquid.py`
- Test: `shared_scripts/test_check_hyperliquid.py` (or new) — injected vs inline parity.

- [ ] **Step 1: Write failing test** — run the regime branch with injection and assert `strategy_params["regime"]`, `market_ctx["regime"]`, and `stdout_regime` equal the inline path for the same candles.

```python
def test_injected_regime_matches_inline(monkeypatch):
    # Build df + spec, compute inline payload, then feed it back via the injected
    # path; assert the three derived values match. (Exact harness mirrors the
    # check's existing test scaffolding — call the regime resolution block with
    # args.regime_injected=True and args.regime_payload_json=<inline payload>.)
    ...
```

- [ ] **Step 2: Run, verify FAIL.**
- [ ] **Step 3: Implement** — add argparse flags and branch the regime block (check_hyperliquid.py ~169-189):

```python
# argparse (near line 1640)
parser.add_argument("--regime-injected", action="store_true", default=False)
parser.add_argument("--regime-payload-json", default="")

# regime block (replacing the prepare_check_regime call ~169)
if args.regime_injected:
    injected = json.loads(args.regime_payload_json) if args.regime_payload_json.strip() else ""
    stdout_regime, live_regime, strategy_regime = resolve_injected_regime(
        injected,
        primary_key=_primary_window_key(regime_windows_spec),
        atr_window=regime_atr_window,
    )
else:
    stdout_regime, live_regime, strategy_regime = prepare_check_regime(
        df, regime_enabled=regime_enabled, windows_spec=regime_windows_spec, atr_window=regime_atr_window,
    )
strategy_params["regime"] = strategy_regime
```

Import `resolve_injected_regime` at the top (line 64) alongside the others. `_primary_window_key` = `"medium" if "medium" in spec else sorted(spec)[0]` (or empty when spec is None).

> **Critical:** when `--regime-injected` is set, the check MUST NOT call `prepare_check_regime` even on empty payload (that is the fail-open contract). Keep `--probe-only` working: injection flags must parse under probe argv.

- [ ] **Step 4: Run test, verify PASS.** Also `py_compile`.
- [ ] **Step 5: Commit.** `feat(regime): check_hyperliquid.py accepts injected regime (#879)`

---

### Task 8: Wire HL dispatch to store (Go)

**Files:**
- Modify: `scheduler/main.go` (build store before Phase 3; pass payload into `runHyperliquidCheck`)
- Test: covered by Task 9 integration parity + existing HL tests must stay green.

- [ ] **Step 1:** Add store build in the main loop before the Phase 3 dispatch block (after `dueStrategies` is finalized, near main.go:630/771). Store is a cycle-local var:

```go
regimeStore := buildRegimeStore(dueStrategies, cfg.Regime)
```

- [ ] **Step 2:** Thread `regimeStore` into `runHyperliquidCheck` (add a `payload RegimePayload` param or look it up inside). Inside, replace `args = appendRegimeArgs(...)`-fed inline compute by also appending injection:

```go
injected := regimeStore.payloadForStrategy(*sc, regime)
args = appendInjectedRegimeArgs(args, injected)
```

Keep `appendRegimeArgs`/`appendStrategyRegimeWindowArgs` (the script still needs the windows spec + atr-window selector to RESOLVE the injected payload). The check now echoes `injected` back as `result.Regime`, so the downstream `applyRegimeGate`/`syncStrategyRegimeState`/directional-policy code at main.go:1555-1560 is unchanged.

- [ ] **Step 3:** `go -C scheduler build .` then `go test ./scheduler/ -run 'HL|Hyperliquid|Regime' -v` — all green.
- [ ] **Step 4: Commit.** `feat(regime): HL dispatch injects store payload (#879)`

---

### Task 9: HL end-to-end parity test (Go + Python)

**Files:**
- Test: `scheduler/regime_calculator_test.go` (Go) + a Python parity assertion.

- [ ] **Step 1:** Add a Go test that stubs `runRegimeSubprocessFn` to return a known payload and asserts `runHyperliquidCheck`'s downstream gate/sync see that exact label. (Stub `RunHyperliquidCheck` too — see existing HL test scaffolding.)
- [ ] **Step 2:** Add an integration parity script/test: for a fixed candle fixture, assert `fetch_regime.py compute_payload` == `check_hyperliquid.py` inline `stdout_regime`. Run full pytest suite (registry/sys.path requirement).
- [ ] **Step 3: Commit.** `test(regime): HL injected-vs-inline parity (#879)`

---

### Task 10: Replicate injection to spot/okx/robinhood/topstep

**Files:**
- Modify: `shared_scripts/check_strategy.py`, `check_okx.py`, `check_robinhood.py`, `check_topstep.py`
- Modify: `scheduler/main.go` (`runSpotCheck`, `runOKXCheck`, `runRobinhoodCheck`, `runTopStepCheck`)
- Test: per-script `py_compile` + one parity test each (reuse Task 7 harness).

Repeat Task 7 + Task 8 mechanically for each platform. For each:
- [ ] Add `--regime-injected`/`--regime-payload-json`, import `resolve_injected_regime`, branch the regime block, echo payload back.
- [ ] In the Go `runXxxCheck`, append `appendInjectedRegimeArgs(args, regimeStore.payloadForStrategy(*sc, regime))`.
- [ ] `check_strategy.py` (spot/manual) uses positional argv + `--probe-only` short-circuit — confirm it currently has NO regime flags (per the explore map) and add them in the same argparse section; verify the manual path still works.
- [ ] Build + targeted tests green. Commit per platform: `feat(regime): <platform> dispatch injects store payload (#879)`.

> **Implementer:** these are argparse-strict scripts (HL/TopStep/RH/OKX) — the new flags MUST be added before they can be probed. Update `version_probe.go` in Task 15, but you can add the flags here and probe-test locally.

---

### Task 11: Options migration

**Files:**
- Modify: `scheduler/main.go` (~1515-1526, the `stratState.Regime = result.Regime` options branch)
- Modify: `shared_scripts/check_options.py` (regime at hardcoded `REGIME_TIMEFRAME="4h"`, `latest_regime` 3-state)
- Test: options regime parity.

- [ ] **Step 1:** Decide the options signature: `(underlying, "4h", adx-spec)` — options use a fixed 4h ADX regime independent of `regime.windows`. Add an options-specific signature so `buildRegimeStore` computes it (the representative options strategy contributes `(underlying, 4h)` with an ADX-14 spec). Confirm `check_options.py`'s underlying symbol maps to the adapter's `get_ohlcv` symbol.
- [ ] **Step 2:** Inject the 4h payload into `check_options.py` (add `--regime-injected`/`--regime-payload-json`; when set, skip `latest_regime`, use injected label). Go: `stratState.Regime = regimeStore.payloadForStrategy(sc, ...).PrimaryLabel(...)` instead of `result.Regime`.
- [ ] **Step 3:** Parity test: injected 4h-ADX label == inline `latest_regime` for the same candles. Build + test. Commit. `feat(regime): options reads regime store (#879)`

> **Decision to confirm with maintainer:** whether options should adopt the global `regime.windows` or keep its hardcoded 4h-ADX. Plan keeps 4h-ADX (no behavior change) and stores it under a distinct options signature.

---

### Task 12: Flat-`manual` reads store

**Files:**
- Modify: `scheduler/main.go` (manual dispatch ~1860-1879)
- Test: manual flat regime visibility test.

- [ ] **Step 1: Write failing test** — a flat manual strategy (no position) has `stratState.Regime` populated from the store after a cycle (today it is empty while flat because `runManualCloseEval` only computes when open).
- [ ] **Step 2:** In the manual dispatch, when flat (no open position so `runManualCloseEval` is skipped or returns empty), read `regimeStore.payloadForStrategy(sc, cfg.Regime)` and call `syncStrategyRegimeState(stratState, payload, cfg.Regime)`. When open, keep the existing `runManualCloseEval` path BUT prefer injecting the store payload there too (so the manual HL check echoes the store rather than recomputing). Preserve `stampPositionRegimeIfOpened` (frozen-at-open; #873 coordination — do not re-stamp).
- [ ] **Step 3:** Build + test. Commit. `feat(regime): flat manual reads regime store (#879)`

> **#873 coordination:** if #873 merged first, preserve its `manual-add` branch; the add path must thread the store payload like the open path and never re-stamp. If #879 merges first, #873 rebases its add branch onto the store read.

---

### Task 13: Manual close-eval injection

**Files:**
- Modify: `scheduler/main.go` (`runManualCloseEval`) + `check_strategy.py`/`check_hyperliquid.py` manual path.
- [ ] Inject store payload into the manual HL check (`runManualCloseEval` calls `runHyperliquidCheck`-style path) so `manualRegime` comes from the store, not an inline recompute. Build + test. Commit. `feat(regime): manual close-eval injects store payload (#879)`

---

### Task 14: Dashboard portfolio regime view

**Files:**
- Modify: `scheduler/ui_server.go` (+ `static/ui/*` if a new field is surfaced) and/or `/api/strategies/overview`.
- Test: `scheduler/ui_server_test.go`.

- [ ] **Step 1: Write failing test** for a new `/api/regime` (or field on overview) returning the per-signature store snapshot: `[{symbol, interval, adx3, comp7|windows}]`.
- [ ] **Step 2:** Persist the latest built store into `AppState` (cycle-local → store a snapshot under `mu` at end of build, like marks) so the HTTP handler (different goroutine) can read it. Expose a read-only JSON endpoint. Respect lock order `mu → strategiesMu`; loopback-only.
- [ ] **Step 3:** Build + test. Commit. `feat(regime): dashboard portfolio regime view (#879)`

> **Implementer:** the store is currently cycle-local. To serve the dashboard, store an immutable snapshot on `AppState` (e.g. `state.RegimeSnapshot`) under `mu.Lock` at the end of `buildRegimeStore`/end of cycle. Do NOT share the live map across goroutines without a copy.

---

### Task 15: Probe argv + same-SHA deploy guard

**Files:**
- Modify: `scheduler/version_probe.go`
- Test: `scheduler/version_probe_test.go`

- [ ] **Step 1:** Add `fetch_regime.py` probe argv + invocation:

```go
var fetchRegimeProbeArgv = []string{
    "--platform=hyperliquid", "--symbol=BTC", "--interval=1h",
    "--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
    "--ohlcv-limit=200", "--probe-only",
}
```
Invoke it once in `probeCheckScripts` when any strategy is configured (like `fetch_candles.py`).

- [ ] **Step 2:** Append `--regime-injected` + `--regime-payload-json` to `probeArgv` and `probeCompositeArgv` (and the composite variant) so a stale check script that doesn't accept the injection flags fails startup:

```go
// in probeArgv / probeCompositeArgv
"--regime-injected", "--regime-payload-json", `{"default":{"regime":"trending_up","score":0.4}}`,
```

- [ ] **Step 3:** Update `version_probe_test.go` expectations. Build + test. Commit. `feat(regime): probe fetch_regime + injection flags (#879)`

---

### Task 16: Backtest parity test + script-failure alert + final matrix

**Files:**
- Modify: `scheduler/script_failure_alerts.go` (regime subprocess failure → throttled alert)
- Test: `backtest/tests/test_regime_bundle_parity.py` (new), full Go + pytest suites, `--once` smoke.

- [ ] **Step 1:** Backtest parity test: assert `fetch_regime.compute_payload(df, spec)` per-window label == `ensure_regime_columns(df, ...)` / `compute_regime_composite` last-bar label for the same candles (ADX and composite, incl. `period>14` full-period ADX). Backtest code itself is unchanged.
- [ ] **Step 2:** Wire regime-subprocess failures through `scriptFailureTracker` keyed by signature (reuse `notifyScriptFailure` pattern at `scriptFailureAlertThreshold=3`), per the issue's failure policy. Test the throttle.
- [ ] **Step 3:** Full parity matrix test (per consumer: gate, sync, stamp, ATR-regime, directional) — default-param label unchanged pre/post for HL/spot/okx/rh/topstep/options/manual.
- [ ] **Step 4:** `go build` + `go test ./...` + `uv run --no-sync python -m pytest shared_strategies/ shared_tools/ platforms/ backtest/` + `shared_scripts/test_*.py` + `./go-trader --config scheduler/config.json --once` smoke.
- [ ] **Step 5: Commit.** `test(regime): backtest parity + failure alerts + full matrix (#879)`

---

## Latency analysis (issue requirement — do before merging)

The store build adds one serial phase before Phase 3: N distinct `(symbol, interval, spec)` regime subprocesses, run **concurrently** under the shared `pythonSemaphore=4` with `scriptTimeout=30s`. Each subprocess fetches OHLCV that lands in the #839 `/tmp` cache, so the subsequent check reads cache (no double network fetch). Net added wall-clock ≈ `ceil(N_distinct / 4) × (spawn + ADX compute)`, not `N_strategies`. Quantify on the real config (`--once` with timing) and record in the PR body. If unacceptable, the concurrent fan-out already bounds it; a further option is to skip the store build for cycles where no due strategy has `regime.enabled`.

## Self-review checklist (run after drafting)

- Spec items 1-7 mapped: store+subprocess (T1-4), load-time validation reused not duplicated (existing `validateStrategyRegimeVocabulary`, no change), raw+labels one pass via per-window classify (T4), strategy resolution from store (T8/T10), dashboard (T14), migrate every consumer incl. options/manual/backtest (T10-13,16), parity tests (T9,T16).
- Failure policy (b) implemented as present-but-empty store entry (T1,T3) + fail-open reads + throttled alert (T16).
- `pos.Regime` frozen-at-open untouched; #873 coordination noted (T12,T13).
- Same-SHA deploy: probe argv updated (T15).
- Type consistency: `RegimePayload`, `RegimeSignature`, `RegimeStore`, `resolve_injected_regime`, `appendInjectedRegimeArgs`, `buildRegimeStore`, `runRegimeSubprocessFn` used consistently across tasks.
