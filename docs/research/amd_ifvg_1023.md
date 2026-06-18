# amd_ifvg — DST/session-timing fix, then rebaseline (#1023)

Generated: 2026-06-18

Part of #977 (Methodology M1) / #978. Source verdict under test: the PR #1003 M5
fee-audit row (`docs/research/fee-audit-m5.md`), spot `amd_ifvg`, long leg,
85 trades (14.5/yr), **-8.50% gross / -10.18% net**, verdict `deprecate`.

The issue's thesis: that baseline may not reflect the concept at all, because
the session windows were UTC-hour fixed / DST-unaware **and** the audit ran on
1h/4h while `amd_ifvg` is a 15m strategy. Fix the timing, rebaseline on the
designed timeframe, then apply the decision gate.

## The defect (confirmed in code)

- `amd_ifvg_core` read `result.index.hour` and applied fixed UTC-hour masks
  (old defaults Asian `0–8`, London `8–12` UTC). No timezone/DST handling — the
  intended ICT civil sessions drift by one hour against UTC across DST, so the
  strategy detected Asian ranges / sweeps / IFVG entries in the wrong window for
  ~half the year.
- The committed M1/M5 harness datasets are BTC/ETH/SOL on **1h/4h**
  (`backtest/eval_windows.py`), but `amd_ifvg` is documented/configured as a
  **15m** strategy. On 4h the London window holds <3 bars so the strategy
  **never fires** (0 trades, all windows below); on 1h a "3-candle IFVG" spans
  three hours — a degenerate gap. The -8.50% M5 baseline was a broken form of
  the concept.

## The fix

`shared_strategies/open/amd_ifvg.py` now derives sessions in **civil
(DST-aware) time** anchored to `session_tz` (default `America/New_York`, the
canonical ICT reference):

- New defaults are the canonical ICT killzones: **Asian range 20:00–00:00 ET**
  (accumulation, prior evening), **London open killzone 02:00–05:00 ET**
  (manipulation).
- The UTC index (tz-aware from the live cache, tz-naive UTC from the backtest
  loader) is converted to the civil zone via `tz_convert`, so the same civil
  session maps to a UTC hour that shifts one hour across DST.
- Because the Asian range forms the evening *before* the London/NY session it
  manipulates, the logical "session day" is anchored at the Asian open so
  accumulation→manipulation→distribution share a grouping key across civil
  midnight.
- Hours and `session_tz` remain overridable; pass `session_tz="UTC"` with the
  old hours to recover the legacy fixed-UTC behaviour.
- The #732 look-ahead-safe entry logic (bar-by-bar IFVG selection, no
  day-final-close peek) is unchanged and its regression tests still pass.

`--list-json` stays byte-identical to main for the params/logic change
(discovery emits only id + description); the only discovery change is the
intentional deprecation below.

## Reproduce

```bash
# Designed timeframe (15m): fetch data first (binanceus, 2023→latest)
uv run --no-sync python - <<'PY'
import sys; sys.path.insert(0, 'shared_tools')
from data_fetcher import fetch_full_history
for s in ("BTC/USDT","ETH/USDT","SOL/USDT"):
    fetch_full_history(s, "15m", since="2023-01-01", exchange_id="binanceus", store=True)
PY

# M1 protocol — designed 15m timeframe
uv run --no-sync python backtest/eval_windows.py --strategy amd_ifvg --registry spot \
    --datasets BTC/USDT:15m,ETH/USDT:15m,SOL/USDT:15m --json /tmp/amd_15m.json

# M1 protocol — standard 1h/4h audit datasets
uv run --no-sync python backtest/eval_windows.py --strategy amd_ifvg --registry spot \
    --json /tmp/amd_1h4h.json
```

Long/flat leg (matches the M5 long-leg verdict), capital 1000, binanceus fee
model. Incumbent bar = median of the eight #963/#976 incumbents, recomputed per
(window, dataset) on the identical harness.

## Results — corrected baseline (mean per-leg, candidate vs incumbent bar)

### 15m (designed timeframe)

| window | cand Sharpe | bar Sharpe | cand DDadj | bar DDadj | traded | verdict |
|---|---:|---:|---:|---:|:--:|---|
| is     | -1.50 | -1.12 | -0.74 | -0.70 | 3/3 | FAIL |
| oos    | -1.50 | -1.65 | -0.72 | -0.79 | 3/3 | **PASS** |
| 2023   |  0.76 |  0.53 |  1.00 |  1.02 | 3/3 | FAIL |
| 2024   | -0.18 |  0.10 | -0.30 | -0.11 | 3/3 | FAIL |
| 2025H1 | -0.26 | -0.45 | -0.24 | -0.40 | 3/3 | **PASS** |

### 1h/4h (standard M1/M5 protocol)

| window | cand Sharpe | bar Sharpe | cand DDadj | bar DDadj | traded | verdict |
|---|---:|---:|---:|---:|:--:|---|
| is     | -0.10 | -0.12 | -0.11 | -0.14 | 3/6 | PASS |
| oos    | -0.30 | -0.75 |  0.03 | -0.49 | 3/6 | **PASS** |
| 2023   |  0.64 |  1.46 |  1.22 |  3.67 | 3/6 | FAIL |
| 2024   | -0.01 |  0.90 |  0.06 |  1.07 | 3/6 | FAIL |
| 2025H1 |  0.34 | -0.42 |  0.42 | -0.37 | 3/6 | **PASS** |

`traded 3/6` on 1h/4h is the three 1h datasets only — **4h never fires** (0
trades every window), confirming the timeframe degeneracy. On 15m the strategy
fires actively (~27–38 trades per window/dataset; ~120 signals/symbol on OOS).

## What the fix changed

The correction was **not** cosmetic. On the original M5 surface (1h/4h, long
leg) the verdict was `deprecate`; the corrected baseline now **passes protocol
OOS on both timeframes** and passes 2025H1. So the original concern was real —
the broken timing/timeframe understated the concept. `amd_ifvg` is genuinely
competitive with the incumbent median in chop/range conditions (OOS, 2025H1).

But it does not hold up across regimes: in the 2023 and 2024 trending bull years
it lags the incumbent-median bar on both timeframes (2024 1h/4h: 0/6 datasets
beat the bar; mean Sharpe -0.01 vs bar 0.90). The edge exists only when the
broad market is sideways and evaporates in trends.

## Decision gate

> If the corrected-timing baseline still does not clear the incumbent-median bar
> on protocol OOS **and** held-out windows, move `amd_ifvg` to the deprecation
> list — the concept is then disproven on a sound implementation.

| timeframe | oos | 2023 | 2024 | 2025H1 | held-out | gate |
|---|:--:|:--:|:--:|:--:|:--:|---|
| 15m   | PASS | FAIL | FAIL | PASS | 1/3 | **not cleared** |
| 1h/4h | PASS | FAIL | FAIL | PASS | 1/3 | **not cleared** |

Both timeframes clear OOS but fail two of three held-out years. The gate is not
cleared on either. **Verdict: deprecate** — the concept is disproven on a sound
(DST-corrected, designed-timeframe) implementation; no further iteration is
warranted. (Per the issue, magic-number sweeps of the IFVG-gap / sweep
threshold were deliberately not run — tuning a concept that fails held-out
robustness on its corrected baseline is noise.)

## Deprecation (implemented)

`amd_ifvg` is hidden from discovery but kept loadable for any existing
config/backtest:

- `shared_strategies/open/registry.py`: added to `DISCOVERY_HIDDEN_STRATEGIES`
  (drops from `--list-json` on spot and futures; default_params updated to the
  NY-anchored ICT killzones for explicit loads).
- `scheduler/ui_reports.go`: audit verdict `watch` → `deprecate`, plus a
  Deprecations entry.
- Tests: `test_registry_parity.py::test_deprecated_amd_ifvg_hidden_but_loadable`
  (hidden-but-loadable on both shims + asserts the NY-canon defaults);
  `ui_reports_test.go` deprecation count 16 → 17; `test_amd_ifvg.py` rewritten
  with DST-boundary, session-day-wrap, and explicit-hour-override coverage while
  preserving the #732 look-ahead regression.

Worth revisiting only if a genuine trend/regime filter is added that flattens
`amd_ifvg` during sustained uptrends (so it stops bleeding in the bull-year
held-outs while keeping the chop-window edge) — out of scope here.

Status: M1 protocol complete (corrected baseline, both 15m and 1h/4h, all 5
windows). Verdict: deprecate — **implemented** (hidden from discovery, kept
loadable).

---
Created with LLM: Opus 4.8 | xhigh | Harness: Claude Code + live M1 runs
