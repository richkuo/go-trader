# Anchored VWAP (`anchored_vwap`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a new open strategy `anchored_vwap` that anchors a single VWAP to the most recent confirmed swing pivot and trades it as a buffered support/resistance flip (bidirectional, perps-live + spot + backtest).

**Architecture:** Pure pandas core in `shared_strategies/open/anchored_vwap.py` (`anchored_vwap_core`), registered through `open/registry.py` and surfaced to Go via `--list-json`. Go wiring (`scheduler/init.go`) makes it discoverable and bidirectional-perps capable. ATR is inlined (open-strategy convention). No custom close evaluator — the opposite flip is the exit/reversal.

**Tech Stack:** Python 3 / pandas / numpy (strategy + pytest), Go (`scheduler/`), uv for env.

**Spec:** `docs/superpowers/specs/2026-06-15-anchored-vwap-strategy-design.md`. Read it before starting.

**Conventions:**
- Run all commands from the worktree root: `/Users/richardkuo/Work/go-trader/.claude/worktrees/anchored-vwap`.
- Python: `uv run --no-sync python ...`. Go: `/opt/homebrew/bin/go -C scheduler ...`.
- Pytest for `shared_strategies/open/` runs from that dir's import context; the repo convention is `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -v`.
- `gofmt -w <file>.go` after every Go edit.

---

## File Structure

- **Create** `shared_strategies/open/anchored_vwap.py` — `anchored_vwap_core(df, pivot_strength, buffer_atr_mult, confirm_bars, atr_period)` + private `_inline_atr`. Sole owner of the AVWAP math, pivot detection, and trigger.
- **Create** `shared_strategies/open/test_anchored_vwap.py` — unit tests for the core.
- **Modify** `shared_strategies/open/registry.py` — import + `@register` wrapper + `PLATFORM_ORDER` (spot & futures).
- **Modify** `scheduler/init.go` — `knownShortNames`, `bidirectionalPerpsStrategies`, and the three default fallback lists.
- **Modify** `backtest/optimizer.py` — `DEFAULT_PARAM_RANGES["anchored_vwap"]`.
- **Modify** `backtest/tests/test_backtester_lookahead.py` — add an `anchored_vwap` look-ahead case.

---

## Task 1: Core module scaffold + guards

**Files:**
- Create: `shared_strategies/open/anchored_vwap.py`
- Test: `shared_strategies/open/test_anchored_vwap.py`

- [ ] **Step 1: Write the failing test**

```python
"""Tests for anchored_vwap.py — Anchored VWAP S/R-flip strategy."""

import numpy as np
import pandas as pd

from anchored_vwap import anchored_vwap_core


def _hourly_index(n, start="2026-01-01 00:00:00"):
    return pd.date_range(start, periods=n, freq="1h")


def _ohlcv(closes, highs=None, lows=None, opens=None, volume=100.0):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    highs = closes + 0.5 if highs is None else np.asarray(highs, dtype=float)
    lows = closes - 0.5 if lows is None else np.asarray(lows, dtype=float)
    opens = closes if opens is None else np.asarray(opens, dtype=float)
    vol = np.full(n, float(volume)) if np.isscalar(volume) else np.asarray(volume, dtype=float)
    return pd.DataFrame(
        {"open": opens, "high": highs, "low": lows, "close": closes, "volume": vol},
        index=_hourly_index(n),
    )


def test_empty_and_short_df_return_zero_signal():
    empty = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = anchored_vwap_core(empty)
    assert list(out["signal"]) == []
    assert "avwap" in out.columns and "anchor_index" in out.columns and "atr" in out.columns

    short = _ohlcv(np.linspace(100, 101, 6))  # < 2*5+1+2
    out = anchored_vwap_core(short)
    assert (out["signal"] == 0).all()
    assert (out["anchor_index"] == -1).all()
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'anchored_vwap'`.

- [ ] **Step 3: Write minimal implementation**

Create `shared_strategies/open/anchored_vwap.py`:

```python
"""Anchored VWAP (AVWAP) — single VWAP anchored to the most recent *confirmed*
swing pivot, traded as a buffered support/resistance flip.

Design: docs/superpowers/specs/2026-06-15-anchored-vwap-strategy-design.md

ATR is inlined (rolling-mean True Range, integer-round only when atr >= 100)
to match standard_atr without importing shared_tools — open strategies cannot
assume shared_tools is on sys.path at module load (the registry parity test
loads registry.py via importlib without it).
"""

from __future__ import annotations

import numpy as np
import pandas as pd


def _inline_atr(df: pd.DataFrame, period: int) -> pd.Series:
    high = df["high"].astype(float)
    low = df["low"].astype(float)
    prev_close = df["close"].astype(float).shift(1)
    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)
    atr = tr.rolling(window=period).mean()
    return atr.where(atr < 100, atr.round(0))


def anchored_vwap_core(
    df: pd.DataFrame,
    pivot_strength: int = 5,
    buffer_atr_mult: float = 0.25,
    confirm_bars: int = 2,
    atr_period: int = 14,
) -> pd.DataFrame:
    result = df.copy()
    n = len(result)
    result["signal"] = 0
    result["avwap"] = np.nan
    result["anchor_index"] = -1
    result["atr"] = _inline_atr(result, atr_period)
    if n < 2 * pivot_strength + 1 + confirm_bars:
        return result
    return result  # full logic added in later tasks
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add shared_strategies/open/anchored_vwap.py shared_strategies/open/test_anchored_vwap.py
git commit -m "feat(#1016): anchored_vwap core scaffold + guards

LLM: Opus 4.8 | xhigh | Harness: Claude Code"
```

---

## Task 2: Pivot detection + anchor_index

**Files:**
- Modify: `shared_strategies/open/anchored_vwap.py`
- Test: `shared_strategies/open/test_anchored_vwap.py`

- [ ] **Step 1: Write the failing test**

Append to the test file:

```python
def test_strict_pivot_and_confirmed_anchor_index():
    # pivot_strength=2: a strict swing LOW at index 5, with 2 bars each side.
    # Bars: descend to a unique trough at 5, then ascend.
    closes = [110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 112]
    df = _ohlcv(closes, highs=np.array(closes) + 0.5, lows=np.array(closes) - 0.5)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    anchor = out["anchor_index"].to_numpy()
    # Trough index 5 is a strict low; confirmed at 5+2=7. Bars 0..6 have no anchor.
    assert (anchor[:7] == -1).all()
    assert (anchor[7:] == 5).all()


def test_flat_top_plateau_is_not_a_pivot():
    # Two equal highs at the top (indices 4,5) -> plateau -> no pivot there.
    closes = [100, 102, 104, 106, 108, 108, 106, 104, 102, 100, 98, 96]
    highs = np.array(closes) + 0.2
    highs[4] = highs[5] = 110.0  # equal plateau highs
    df = _ohlcv(closes, highs=highs, lows=np.array(closes) - 0.5)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    # No bar's anchor should ever equal 4 or 5 (plateau bars not pivots).
    assert not np.isin(out["anchor_index"].to_numpy(), [4, 5]).any()
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py::test_strict_pivot_and_confirmed_anchor_index -v`
Expected: FAIL — anchor stays -1 (logic not implemented).

- [ ] **Step 3: Write minimal implementation**

Replace the trailing `return result  # full logic added in later tasks` with:

```python
    high = result["high"].astype(float).to_numpy()
    low = result["low"].astype(float).to_numpy()

    # --- strict swing pivots (unique max high / unique min low in window) ---
    k = pivot_strength
    is_pivot = np.zeros(n, dtype=bool)
    for i in range(k, n - k):
        wh = high[i - k:i + k + 1]
        wl = low[i - k:i + k + 1]
        wmax = wh.max()
        wmin = wl.min()
        is_high = high[i] == wmax and int((wh == wmax).sum()) == 1
        is_low = low[i] == wmin and int((wl == wmin).sum()) == 1
        if is_high or is_low:
            is_pivot[i] = True

    # --- anchor in effect at each bar: most recent pivot confirmed by then.
    # A pivot at p becomes knowable at bar p + k.
    anchor = np.full(n, -1, dtype=int)
    last = -1
    for b in range(n):
        p = b - k
        if p >= 0 and is_pivot[p]:
            last = p
        anchor[b] = last
    result["anchor_index"] = anchor

    return result
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add shared_strategies/open/anchored_vwap.py shared_strategies/open/test_anchored_vwap.py
git commit -m "feat(#1016): anchored_vwap strict pivot detection + anchor_index

LLM: Opus 4.8 | xhigh | Harness: Claude Code"
```

---

## Task 3: AVWAP prefix-sum computation

**Files:**
- Modify: `shared_strategies/open/anchored_vwap.py`
- Test: `shared_strategies/open/test_anchored_vwap.py`

- [ ] **Step 1: Write the failing test**

Append:

```python
def test_avwap_matches_hand_computed_prefix_sum():
    closes = [110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 112]
    highs = np.array(closes) + 0.0   # tp == close when high==low==close
    lows = np.array(closes) + 0.0
    df = _ohlcv(closes, highs=highs, lows=lows, volume=10.0)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    # Anchor = index 5 from bar 7 on. With tp==close and constant volume,
    # AVWAP[n] = mean(close[5..n]).
    avwap = out["avwap"].to_numpy()
    for nbar in (7, 8, 9, 10, 11):
        expected = np.mean(closes[5:nbar + 1])
        assert abs(avwap[nbar] - expected) < 1e-9, (nbar, avwap[nbar], expected)
    assert np.isnan(avwap[:7]).all()  # no anchor yet


def test_avwap_zero_volume_window_falls_back_to_typical():
    closes = [110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 112]
    df = _ohlcv(closes, volume=0.0)  # zero volume everywhere
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    avwap = out["avwap"].to_numpy()
    tp = (df["high"] + df["low"] + df["close"]).to_numpy() / 3.0
    for nbar in range(7, 12):
        assert abs(avwap[nbar] - tp[nbar]) < 1e-9
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py::test_avwap_matches_hand_computed_prefix_sum -v`
Expected: FAIL — `avwap` is all NaN.

- [ ] **Step 3: Write minimal implementation**

Immediately before the final `return result` (after the anchor block), insert:

```python
    # --- AVWAP via global prefix sums (exact across re-anchors) ---
    tp = ((result["high"] + result["low"] + result["close"]) / 3.0).to_numpy()
    vol = result["volume"].astype(float).to_numpy()
    pref_tpvol = np.concatenate([[0.0], np.cumsum(tp * vol)])  # pref[i] = sum first i
    pref_vol = np.concatenate([[0.0], np.cumsum(vol)])
    avwap = np.full(n, np.nan)
    for b in range(n):
        a = anchor[b]
        if a < 0:
            continue
        num = pref_tpvol[b + 1] - pref_tpvol[a]
        den = pref_vol[b + 1] - pref_vol[a]
        avwap[b] = tp[b] if den <= 0 else num / den
    result["avwap"] = avwap
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add shared_strategies/open/anchored_vwap.py shared_strategies/open/test_anchored_vwap.py
git commit -m "feat(#1016): anchored_vwap prefix-sum AVWAP + zero-volume fallback

LLM: Opus 4.8 | xhigh | Harness: Claude Code"
```

---

## Task 4: Trigger — buffered S/R flip (fire-once, symmetric)

**Files:**
- Modify: `shared_strategies/open/anchored_vwap.py`
- Test: `shared_strategies/open/test_anchored_vwap.py`

- [ ] **Step 1: Write the failing test**

Append:

```python
def _long_reclaim_df():
    # Descend to a strict swing low (idx 5), drift below the AVWAP, then
    # decisively reclaim and hold above it. pivot_strength=2, confirm_bars=2.
    closes = [110, 108, 106, 104, 102, 100,   # trough at idx 5
              100.5, 100.2, 99.8, 99.5,        # chop below the rising AVWAP
              103.5, 104.0, 104.5, 105.0]      # buffered reclaim + hold
    return _ohlcv(closes, volume=10.0)


def test_long_signal_fires_once_on_completing_bar():
    df = _long_reclaim_df()
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2)
    sig = out["signal"].to_numpy()
    longs = np.where(sig == 1)[0]
    assert len(longs) == 1, longs               # fire-once
    b = longs[0]
    # the bar before the window must have been below the line (fresh crossing)
    win_start = b - 2 + 1
    assert out["close"].to_numpy()[win_start - 1] < out["avwap"].to_numpy()[win_start - 1]


def test_no_signal_before_first_anchor():
    df = _long_reclaim_df()
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2)
    # bars before anchor confirmation (idx < 7) never signal
    assert (out["signal"].to_numpy()[:7] == 0).all()


def test_short_signal_mirrors():
    # Ascend to a strict swing high (idx 5), drift above AVWAP, then break below.
    closes = [90, 92, 94, 96, 98, 100,
              99.5, 99.8, 100.2, 100.5,
              96.5, 96.0, 95.5, 95.0]
    df = _ohlcv(closes, volume=10.0)
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2)
    sig = out["signal"].to_numpy()
    assert (sig == -1).sum() == 1
    assert (sig == 1).sum() == 0


def test_nan_atr_warmup_yields_no_signal():
    df = _long_reclaim_df()
    # atr_period longer than the series -> ATR all NaN -> buffer NaN -> no fire
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.25, confirm_bars=2, atr_period=99)
    assert (out["signal"] == 0).all()
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -k "signal or anchor or warmup" -v`
Expected: FAIL — no signals emitted yet.

> Note (executor): the `_long_reclaim_df` / short fixtures are hand-built. If the exact `signal==1` bar count differs, adjust the close path so there is exactly one fresh buffered reclaim held for `confirm_bars` (the invariant under test), not the literal prices. Keep `buffer_atr_mult=0.0` for the basic crossing tests to isolate the flip from ATR scale.

- [ ] **Step 3: Write minimal implementation**

Before the final `return result`, insert:

```python
    # --- trigger: buffered S/R flip, fire-once via fresh-crossing clause ---
    close = result["close"].astype(float).to_numpy()
    atr_arr = result["atr"].to_numpy()
    cb = int(confirm_bars)
    sig = np.zeros(n, dtype=int)
    for nbar in range(n):
        b = nbar - cb + 1                       # window start
        if b - 1 < 0 or anchor[b] < 0:
            continue
        if np.isnan(avwap[b - 1]) or np.isnan(atr_arr[b]):
            continue
        buf = buffer_atr_mult * atr_arr[b]
        win_c = close[b:nbar + 1]
        win_v = avwap[b:nbar + 1]
        # LONG: held above, buffered breach on window-start, prior bar below.
        if (np.all(win_c >= win_v)
                and close[b] >= avwap[b] + buf
                and close[b - 1] < avwap[b - 1]):
            sig[nbar] = 1
            continue
        # SHORT: mirror.
        if (np.all(win_c <= win_v)
                and close[b] <= avwap[b] - buf
                and close[b - 1] > avwap[b - 1]):
            sig[nbar] = -1
    result["signal"] = sig
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -v`
Expected: PASS (all tests). Fix fixtures per the Step-2 note if a count assertion is off.

- [ ] **Step 5: Commit**

```bash
git add shared_strategies/open/anchored_vwap.py shared_strategies/open/test_anchored_vwap.py
git commit -m "feat(#1016): anchored_vwap buffered S/R-flip trigger (fire-once)

LLM: Opus 4.8 | xhigh | Harness: Claude Code"
```

---

## Task 5: Register in the open registry

**Files:**
- Modify: `shared_strategies/open/registry.py` (import ~line 53; `@register` near the other VWAP blocks; `PLATFORM_ORDER` line 1254)
- Test: `shared_strategies/open/test_anchored_vwap.py`

- [ ] **Step 1: Write the failing test**

Append:

```python
import importlib.util
import os


def _load_registry():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location(
        "_reg_under_test_avwap", os.path.join(here, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_registered_for_spot_and_futures():
    reg = _load_registry()
    for platform in ("spot", "futures"):
        names = [s["name"] for s in reg.list_strategies(platform)]
        assert "anchored_vwap" in names, (platform, names)


def test_registered_fn_applies_via_registry():
    reg = _load_registry()
    entry = reg.STRATEGIES["anchored_vwap"]
    df = _long_reclaim_df()
    out = entry["fn"](df, **entry["default_params"])
    assert "signal" in out.columns
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -k registered -v`
Expected: FAIL — `KeyError: 'anchored_vwap'` / not in list.

- [ ] **Step 3: Write minimal implementation**

In `registry.py`, add near the other module imports (~line 53, after `from vwap_rejection_st import vwap_rejection_st_core`):

```python
from anchored_vwap import anchored_vwap_core
```

Add a registration block beside the `vwap_rejection_st` block (~line 1099):

```python
@register(
    "anchored_vwap",
    "Anchored VWAP — single VWAP anchored to the last confirmed swing pivot as dynamic S/R; long on a buffered reclaim above, short on a buffered breakdown below",
    {
        "pivot_strength": 5,
        "buffer_atr_mult": 0.25,
        "confirm_bars": 2,
        "atr_period": 14,
    },
)
def anchored_vwap_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return anchored_vwap_core(df, **params)
```

In `PLATFORM_ORDER` (line 1254), append `"anchored_vwap"` to **both** lists. Spot list — add after `"vwap_reversion", "chart_pattern",`:

```python
        "heikin_ashi_ema", "order_blocks", "vwap_reversion", "anchored_vwap", "chart_pattern",
```

Futures list — same insertion after `"vwap_reversion", "chart_pattern",`:

```python
        "heikin_ashi_ema", "order_blocks", "vwap_reversion", "anchored_vwap", "chart_pattern",
```

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
uv run --no-sync python -m pytest shared_strategies/open/test_anchored_vwap.py -v
uv run --no-sync python -m pytest shared_strategies/ -q
```
Expected: PASS — the new tests and the full `shared_strategies/` suite (parity tests included).

- [ ] **Step 5: Commit**

```bash
git add shared_strategies/open/registry.py shared_strategies/open/test_anchored_vwap.py
git commit -m "feat(#1016): register anchored_vwap (spot + futures)

LLM: Opus 4.8 | xhigh | Harness: Claude Code"
```

---

## Task 6: Go wiring — discovery + bidirectional perps

**Files:**
- Modify: `scheduler/init.go` (`knownShortNames` ~line 37; `bidirectionalPerpsStrategies` ~line 89; `defaultSpotStrategies` ~line 128; `defaultPerpsStrategies` ~line 166; `defaultFuturesStrategies` ~line 187)

- [ ] **Step 1: Write the failing test**

Create/append to `scheduler/init_anchored_vwap_test.go`:

```go
package main

import "testing"

func TestAnchoredVWAPWiring(t *testing.T) {
	if got := deriveShortName("anchored_vwap"); got != "avwap" {
		t.Fatalf("deriveShortName(anchored_vwap) = %q, want avwap", got)
	}
	if !isBidirectionalPerpsStrategy("anchored_vwap") {
		t.Fatal("anchored_vwap must be a bidirectional perps strategy")
	}
	for _, list := range [][]stratDef{defaultSpotStrategies, defaultPerpsStrategies, defaultFuturesStrategies} {
		found := false
		for _, s := range list {
			if s.ID == "anchored_vwap" {
				found = true
			}
		}
		if !found {
			t.Fatal("anchored_vwap missing from a default fallback list")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `/opt/homebrew/bin/go -C scheduler test -run TestAnchoredVWAPWiring ./...`
Expected: FAIL — short name `"av"` (first-letters fallback), not bidirectional, missing from lists.

- [ ] **Step 3: Write minimal implementation**

In `scheduler/init.go`:

- `knownShortNames` (after the `"vwap_reversion": "vwap",` line):
```go
	"anchored_vwap":         "avwap",
```
- `bidirectionalPerpsStrategies` (add an entry):
```go
	"anchored_vwap":       true, // single-AVWAP S/R flip; emits short on a buffered breakdown below the line (#1016)
```
- `defaultSpotStrategies` (after the `vwap_reversion` entry):
```go
	{ID: "anchored_vwap", ShortName: "avwap"},
```
- `defaultPerpsStrategies` (append before the regime entries):
```go
	{ID: "anchored_vwap", ShortName: "avwap"},
```
- `defaultFuturesStrategies` (after the `vwap_reversion` entry):
```go
	{ID: "anchored_vwap", ShortName: "avwap"},
```

Then: `gofmt -w scheduler/init.go`.

- [ ] **Step 4: Run tests + build to verify they pass**

Run:
```bash
/opt/homebrew/bin/go -C scheduler test -run TestAnchoredVWAPWiring ./...
/opt/homebrew/bin/go -C scheduler build -ldflags "-X main.Version=$(git describe --tags --always --dirty=-mod)" -o /tmp/gt-avwap .
/opt/homebrew/bin/go -C scheduler test ./...
```
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add scheduler/init.go scheduler/init_anchored_vwap_test.go
git commit -m "feat(#1016): wire anchored_vwap into Go discovery + bidirectional perps

LLM: Opus 4.8 | xhigh | Harness: Claude Code"
```

---

## Task 7: Optimizer ranges + look-ahead regression

**Files:**
- Modify: `backtest/optimizer.py` (`DEFAULT_PARAM_RANGES` ~line 614)
- Modify: `backtest/tests/test_backtester_lookahead.py`

- [ ] **Step 1: Write the failing test**

First inspect the existing look-ahead test to match its harness/fixture style:

Run: `sed -n '1,80p' backtest/tests/test_backtester_lookahead.py`

Then append a parametrization/case for `anchored_vwap` mirroring the existing pattern (the existing test asserts signals at bars `≤ N` are unchanged when future bars are appended). Use the file's existing helper(s); a representative shape:

```python
def test_anchored_vwap_no_lookahead():
    from anchored_vwap import anchored_vwap_core  # path set up by this test module
    import numpy as np, pandas as pd
    closes = list(np.linspace(120, 100, 30)) + list(np.linspace(100, 115, 20))
    idx = pd.date_range("2026-01-01", periods=len(closes), freq="1h")
    df = pd.DataFrame({"open": closes, "high": np.array(closes) + 0.5,
                       "low": np.array(closes) - 0.5, "close": closes,
                       "volume": np.full(len(closes), 10.0)}, index=idx)
    full = anchored_vwap_core(df, pivot_strength=3, confirm_bars=2)["signal"].to_numpy()
    cut = 40
    trunc = anchored_vwap_core(df.iloc[:cut], pivot_strength=3, confirm_bars=2)["signal"].to_numpy()
    assert np.array_equal(full[:cut], trunc), "signals <= cut must not depend on future bars"
```

(If `test_backtester_lookahead.py` uses an `importlib`/sys.path harness to import open strategies, follow that mechanism instead of the bare import.)

- [ ] **Step 2: Run test to verify it fails or passes**

Run: `uv run --no-sync python -m pytest backtest/tests/test_backtester_lookahead.py -k anchored -v`
Expected: PASS (the core is already look-ahead safe by construction). If it FAILS, that is a real look-ahead bug in the core — fix the core, not the test.

- [ ] **Step 3: Add optimizer ranges**

In `backtest/optimizer.py`, add to `DEFAULT_PARAM_RANGES` (beside `"vwap_reversion"`):

```python
    "anchored_vwap": {
        "pivot_strength": [3, 5, 8],
        "buffer_atr_mult": [0.1, 0.25, 0.5],
        "confirm_bars": [1, 2, 3],
    },
```

- [ ] **Step 4: Verify**

Run:
```bash
uv run --no-sync python -m py_compile backtest/optimizer.py
uv run --no-sync python -m pytest backtest/tests/test_backtester_lookahead.py -k anchored -v
```
Expected: compile clean; look-ahead test PASS.

- [ ] **Step 5: Commit**

```bash
git add backtest/optimizer.py backtest/tests/test_backtester_lookahead.py
git commit -m "feat(#1016): anchored_vwap optimizer ranges + look-ahead regression

LLM: Opus 4.8 | xhigh | Harness: Claude Code"
```

---

## Task 8: Integration verification — list-json parity, full suite, backtest

**Files:** none (verification only; record outputs in the PR).

- [ ] **Step 1: `--list-json` byte-identical parity**

```bash
uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json > /tmp/spot_after.json
uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json > /tmp/fut_after.json
python -c "import json,sys; json.load(open('/tmp/spot_after.json')); json.load(open('/tmp/fut_after.json')); print('valid json')"
```
Expected: valid JSON; both lists include `anchored_vwap`. (Go `discoverStrategies` consumes this.)

- [ ] **Step 2: Full Python suite (registry/sys.path coverage)**

```bash
uv run --no-sync python -m pytest shared_strategies/ shared_tools/ backtest/ -q
```
Expected: all PASS (isolated runs mask open-vs-close `registry.py` import-order conflicts — run the full suite).

- [ ] **Step 3: Full Go suite + build**

```bash
/opt/homebrew/bin/go -C scheduler test ./...
/opt/homebrew/bin/go -C scheduler build -ldflags "-X main.Version=$(git describe --tags --always --dirty=-mod)" -o /tmp/gt-avwap .
```
Expected: PASS + clean build.

- [ ] **Step 4: Backtest validation — two legs (per spec §11)**

```bash
# Long leg (default direction):
uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap --symbol BTC/USDT --timeframe 1h --mode single

# Short leg (explicit):
uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap --symbol BTC/USDT --timeframe 1h --mode single \
  --direction short
```
Expected: both runs complete; non-trivial trade counts. Do **not** use `--config` (hard-fails with no close evaluator). Record metrics in the PR.

- [ ] **Step 5: Smoke the binary discovers the strategy**

```bash
./go-trader init --json '{"platform":"hyperliquid","strategy":"anchored_vwap","symbol":"BTC","capital":1000}' --output /tmp/avwap_init.json 2>&1 | tail -5 || true
```
(Adjust init JSON to the wizard's required keys; goal is to confirm `anchored_vwap` is an accepted strategy ID end-to-end.)

- [ ] **Step 6: Open the PR**

```bash
git push -u origin cc/anchored-vwap
gh pr create --repo richkuo/go-trader --title "feat(#1016): Anchored VWAP strategy" \
  --body "Closes #1016. Single-AVWAP S/R-flip strategy anchored to the most recent confirmed swing pivot; bidirectional perps + spot + backtest. Design: docs/superpowers/specs/2026-06-15-anchored-vwap-strategy-design.md. Follow-ups: #1017.

LLM: Opus 4.8 | xhigh | Harness: Claude Code"
```

---

## Self-Review notes (resolved)

- **Spec coverage:** §3 pivot/anchor → Task 2; §4 AVWAP → Task 3; §5 trigger → Task 4; §6 params → Tasks 1/7; §7 look-ahead → Task 7; §8 wiring → Tasks 5/6; §9 perps obligations → verified via Go test + backtest (Tasks 6/8); §10 tests → Tasks 1-5; §11 backtest → Task 8.
- **Inline ATR** decision (not `import standard_atr`) is reflected in Task 1 and the corrected spec §5.
- **Naming consistency:** `anchored_vwap_core` / `anchored_vwap_strategy` / short name `avwap` / param names `pivot_strength,buffer_atr_mult,confirm_bars,atr_period` are identical across all tasks.
- **Fixture caveat:** Task 4's hand-built price fixtures assert the *invariant* (exactly one fresh buffered-and-held flip), and the Step-2 note tells the executor to tune the price path, not the invariant, if a count is off.
