# Consolidation Characterization Study — Design

**Date:** 2026-06-03
**Status:** Approved (design)
**Scope:** Offline research artifact only. The live consolidation trading strategy is a
separate, follow-up issue informed by this research's findings.

## Problem

We want to trade the geometry of an asset's consolidation (range) periods: enter long near
the bottom of a range and short near the top, take 50% profit around the range mean and the
remaining 100% at the opposite edge, and invalidate the trade when a candle large enough to
signal a breakout appears. To do that responsibly, we first need to *characterize* how a
given asset actually consolidates on a given timeframe:

- How long do consolidation periods last?
- How wide are they (max / min / average price range)?
- What shapes do they take (flat box vs. contracting triangle vs. drifting), and does shape
  predict anything useful?
- How big is the candle that "escapes" a consolidation, so a future strategy knows where to
  place a breakout-invalidation stop *before* entering?

This issue delivers that characterization as a reproducible, offline analysis. It does **not**
build the live strategy.

## Non-Goals (deferred to the follow-up strategy issue)

- The live open/close strategy (range entries, geometry-anchored take-profits at 50% mean /
  100% opposite edge, breakout-sized stops).
- Any runtime / real-time consolidation detection.
- Any Go changes: no `scheduler/` code, no strategy registry entries, no backtester wiring,
  no config fields.
- Live-trading integration of any kind.

## Deliverable

A self-contained Python research script that, for one `--symbol` / `--timeframe`, scans
historical candles, segments consolidation episodes, measures each one, correlates shape
against breakout behavior, and writes a per-episode table, an aggregate summary, and charts.

- **Code:** `backtest/consolidation_research.py`
- **Tests:** `backtest/test_consolidation_research.py`
- **New dependency:** `matplotlib` (charts). Add to `pyproject.toml`. This is the only new
  dependency; pandas is already present.

Placement alongside the existing `backtest_*.py` analysis scripts keeps it next to its peers
(`backtest_options.py`, `backtest_theta.py`, `backtest_pairs.py`) and lets it reuse
`shared_tools/data_fetcher`.

## CLI

Mirrors `run_backtest.py` parameterization so the research runs on the same data the
backtester pulls:

```
uv run --no-sync python backtest/consolidation_research.py \
  --symbol BTC/USDT \
  --timeframe 1h \
  --since 2023-01-01 \
  --exchange-id binanceus \
  --out-dir <dir>
```

Plus detection-tuning flags (with sensible defaults), e.g.:

- `--min-bars` — minimum episode length in bars (default e.g. 8)
- `--box-width-pct` / `--box-width-atr` — range-containment band width
- `--bandwidth-threshold` — volatility-contraction squeeze threshold (relative to its own
  recent average)
- `--flatness-slope` / `--flatness-residual` — regression-flatness thresholds
- `--escape-k` — multiplier(s) for the escape-candle definitions
- `--atr-period` — ATR lookback (default 14)

Defaults must let the script run with only `--symbol` and `--timeframe` supplied.

## Pipeline

### 1. Fetch

Pull OHLCV via `shared_tools/data_fetcher.fetch_full_history` (paginated complete history) or
`fetch_ohlcv` (bounded). Offline-friendly; reuses the existing SQLite-backed fetch path.

### 2. Detect (three methods, each a pure function)

Each detector takes `(df, params) -> list[Episode]`, where `Episode` is a dataclass holding
`start_idx`, `end_idx` (and derived start/end timestamps). Detectors are pure and independently
testable.

- **Range-containment (rolling box):** a run of `min_bars`+ consecutive bars whose entire
  high-low span stays within a band of width `<= box_width_pct` (or `<= K x ATR`). Yields the
  box top / bottom / mean and duration directly. Most literal match to "trade the top/bottom
  of the range."
- **Volatility contraction (ATR / Bollinger bandwidth):** episodes where ATR or Bollinger
  bandwidth drops below a threshold relative to its own recent average (a squeeze).
- **Regression flatness:** rolling linear regression where consolidation = near-zero slope
  **and** low residual scatter. Distinguishes flat ranges from drifting / triangular ones.

### 3. Benchmark detectors

Score each detector on a cleanliness metric — internal range tightness, episode stability
(robustness to small parameter perturbation), and false-break rate (episodes that immediately
re-enter range after a flagged escape). Designate a **primary** method but report results for
all three so the comparison is visible.

### 4. Measure (per episode)

For each detected episode compute:

- **Duration:** in bars and wall-clock time.
- **Box geometry:** top (max high), bottom (min low), mean (midpoint and/or average close),
  width in both percent and ATR multiples.
- **Price range:** max, min, average.
- **Shape metrics (quantitative, no named patterns):**
  - top-edge slope (linear fit of rolling highs)
  - bottom-edge slope (linear fit of rolling lows)
  - width-contraction ratio (end width / start width) — distinguishes triangles/wedges from
    rectangles
  - time-in-zone skew — fraction of bars spent in the top / middle / bottom third of the box
- **Escape candle, under all three definitions:**
  - `K x` median in-range true range (self-normalizing per episode)
  - `K x` ATR (consistent with the codebase's ATR-multiple stop sizing)
  - edge-penetration (close beyond a box edge by more than a margin)
  For each definition, record the escape candle's size in price terms and the triggering `K`.
- **Breakout direction:** up or down.

### 5. Correlate shape vs. breakout

Relate shape metrics to breakout behavior — does a rising bottom edge predict an upward break?
Does a larger width-contraction predict a larger escape candle? Report Spearman/Pearson
correlations plus simple grouped means (e.g., median escape size bucketed by contraction
ratio). This is the part that informs the future strategy's edge.

### 6. Output

Written to `--out-dir`:

- **Per-episode table** (CSV + JSON): start, end, duration, top, bottom, mean, range
  max/min/avg, shape metrics, escape-candle size per definition + triggering K, breakout
  direction.
- **Aggregate summary** (text + JSON): distributions and medians of duration / width / escape
  size, the detector benchmark scores, and the shape-vs-breakout correlations.
- **Charts** (matplotlib PNGs): annotated price series with detected boxes and breakout
  markers; histograms of duration / width / escape size; shape-vs-breakout scatter.

## Units (each independently testable)

- `Episode` dataclass.
- `detect_range_containment`, `detect_volatility_contraction`, `detect_regression_flatness`
  — pure `df+params -> list[Episode]`.
- `measure_box`, `measure_shape`, `measure_escape_candle` (the three definitions),
  `classify_breakout`.
- `benchmark_detectors` — scores the three detectors.
- `correlate_shape_breakout` — shape metrics ↔ breakout direction / escape size.
- `render_report` — writes CSV / JSON / charts.
- `main` — CLI wiring.

## Testing

Pytest on synthetic candle fixtures, no network:

- A hand-built **flat box** → detectors find the box; box top/bottom/mean correct; top/bottom
  edge slopes near zero.
- A hand-built **contracting triangle** → width-contraction ratio < 1; edge slopes carry the
  expected signs (converging).
- A hand-built **clean breakout** → escape candle flagged at the correct bar under each
  definition; breakout direction correct.
- Data acquisition is either fed a synthetic DataFrame directly or `fetch_*` is monkeypatched,
  so tests never hit the network.

Follows the repo testing rules: new functionality includes tests; `uv run --no-sync python -m
py_compile` and the pytest suite must pass.

## Risks / Open Questions

- **Detection-parameter sensitivity:** consolidation boundaries are inherently fuzzy. The
  benchmark step and the stability score are the mitigation; defaults will need tuning per
  asset/timeframe, which is expected for a research artifact.
- **Escape-candle false positives:** the three-definition comparison and the false-break rate
  in the benchmark exist specifically to surface this.
- **matplotlib dependency:** accepted. Charts are worth the dependency for eyeballing whether
  detection is sane.

## Follow-up

A second issue will build the live strategy using this research's outputs (typical range
width, escape-candle size for stop placement, shape→breakout signal) to drive range entries,
50%-at-mean / 100%-at-opposite-edge take-profits, and breakout-sized stops, wired through the
codebase's open/close strategy split.
