# Consolidation Research — Findings Log

Running log of consolidation-characterization runs. Newest first. Each entry records
the run config, the detector benchmark, key distributions, the shape→breakout signal,
and the takeaway. Snapshots live in per-run `--out-dir` directories so they don't
clobber each other.

Methodology is described in the local spec
(`docs/superpowers/specs/2026-06-03-consolidation-research-design.md`) — kept offline.

Column definitions for `consolidation_runs.csv` live in
`consolidation_runs_columns.csv` (a data dictionary: column, what it measures,
unit, notes).

---

## Run 001 — BTC/USDT 1h, default params

- **Date:** 2026-06-03
- **Command:**
  `consolidation_research.py --symbol BTC/USDT --timeframe 1h --since 2023-01-01 --exchange-id binanceus`
- **Params:** min_bars=8, box_width_pct=0.04, bandwidth_threshold=0.7,
  flatness_slope=0.0006, flatness_residual=0.02, escape_k=1.5, atr_period=14
- **Data:** 29,994 bars (2023-01-01 → 2026-06-04)
- **Out dir:** `backtest/consolidation_out/` (default — overwritten on next default run)

### Detector benchmark

| method | episodes | avg_bars | avg_width% | false_break | coverage% | score |
|---|---|---|---|---|---|---|
| range_containment (primary) | 264 | 104.9 | 5.6% | 0.15 | **0.92** | 212.1 |
| volatility_contraction | 372 | 20.9 | 2.0% | 0.56 | 0.26 | 159.8 |
| regression_flatness | 385 | 14.4 | 1.0% | 0.57 | 0.18 | 165.3 |

### Episode distributions (primary = range_containment)

- Duration (bars): median 50.5 (p25 20, p75 131.5) → ~2 days median.
- Width %: median 4.8% (p25 3.8%, p75 6.5%).
- Escape candle (× ATR): median 2.22 (p25 1.54, p75 3.22).

### Shape → breakout (Pearson, vs escape size × ATR)

- width_contraction: **−0.24** (strongest; tighter/contracting → bigger escape)
- bottom_edge_slope: 0.09, n_bars: 0.09, width_pct: −0.06, top_edge_slope: 0.01
- Grouped escape size by contraction tercile: tight 2.68, mid 2.06, wide 2.02 ×ATR.

### Takeaway

- Ranges and escape sizes are **measurable and stable**; escape ≈ 2.2× ATR is a usable
  stop-distance anchor.
- **Shape does not predict breakout direction** on 1h BTC; only a weak link between
  contraction and escape *size*.
- **Default box detector is too loose:** 92% coverage means it labels almost everything
  "in range" — not crisp tradeable boxes. Needs tightening (lower box_width_pct, higher
  min_bars) before trusting episode boundaries.

### Next

- [x] Sweep tighter range-containment params → see Phase 1 below.
- [ ] Try 4h timeframe where boxes are cleaner.

---

## Phase 1 — range_containment box-tightening (BTC/USDT 1h, since 2021-01-01, 47,508 bars)

- **Runner:** `consolidation_sweep.py --phase 1` (single fetch, 25 cells, runs 003–027).
- **Grid:** box_width_pct {0.015,0.02,0.025,0.03,0.04} × min_bars {8,12,16,24,36}.
- **Target:** coverage ~0.15–0.35 with low false-break = crisp tradeable boxes.

### Winner region

| run | box_width% | min_bars | coverage | false_break | episodes | esc ×ATR |
|---|---|---|---|---|---|---|
| **009** | 0.02 | 12 | 0.31 | **0.115** | 408 | 2.34 |
| **010** | 0.02 | 16 | 0.23 | 0.124 | 258 | 2.36 |
| 021 | 0.03 | 24 | 0.31 | 0.158 | 241 | 2.20 |
| 016 | 0.025 | 24 | 0.21 | 0.170 | 188 | 2.37 |

### Conclusions

- **`box_width_pct=0.02`, `min_bars=12–16` is the sweet spot.** Coverage falls from the
  loose default's 0.85 to 0.23–0.31 with the lowest false-break (~0.12).
- **Escape ≈ 2.2–2.4× ATR is invariant across all 25 cells** — independent of box
  definition. Strong reusable stop-distance anchor.
- **Carry-forward "best box" for Phase 2+:** box_width_pct=0.02, min_bars=12
  (run 009 — most episodes / lowest false-break); min_bars=16 (run 010) is the
  stricter alternative.

### Next

- [x] Phase 2: escape_k + atr_period on the best box → see Phase 2 below.
- [ ] Phase 3: timeframe mini-grid (15m/1h/4h/1d), re-tuning box width per TF.
- [ ] Optimization: skip recomputing unchanged detectors per cell (regression_flatness
      loop dominates sweep runtime).

---

## Phase 2 — escape-candle sensitivity (BTC/USDT 1h, best box = 0.02/12, 47,508 bars)

- **Runner:** `consolidation_sweep.py --phase 2` (runs 028–035).
- Box is fixed, so coverage=0.311 and episodes=408 are constant across all cells; only
  the escape metric and false-break vary.

### escape_k (breakout threshold strictness)

| escape_k | false_break |
|---|---|
| 1.0 | 0.022 |
| 1.25 | 0.069 |
| 1.5 | 0.115 |
| 2.0 | 0.199 |
| 2.5 | 0.265 |

### atr_period (escape size in ATR units)

| atr_period | esc ×ATR median | false_break |
|---|---|---|
| 7 | 2.01 | 0.152 |
| 14 | 2.34 | 0.115 |
| 21 | 2.49 | 0.093 |

### Conclusions

- false_break rises with escape_k partly by construction (stricter bar → more escapes fall
  short), but the substantive result: **even at escape_k=2.0, ~80% of consolidations end
  with a candle ≥ 2× ATR.** Escape candles are reliably large.
- **atr_period=14 (codebase standard) → 2.34× ATR median**; period choice shifts the
  absolute multiple 2.0→2.5 but not the conclusion.
- **Strategy implication:** a breakout-invalidation stop ~1.5–2.0× ATR(14) beyond the box
  edge is well-supported.

### Next

- [x] Phase 3: timeframe sweep → see Phase 3 below.

---

## Phase 3 — timeframe sweep (BTC/USDT, range_containment box mini-grid)

- **Runner:** `consolidation_sweep.py --phase 3` (with `--box-widths`/`--min-bars-list`
  overrides for the wider/tighter regimes). Detector cache added first, so the slow
  regression-flatness loop runs once per distinct min_bars instead of per cell.
- Box width must be re-tuned per timeframe — bars move more per bar at higher TFs.

### Best box per timeframe (coverage ~0.15–0.35, low false-break)

| timeframe | since | box_width_pct | min_bars | coverage | false_break | esc ×ATR | run |
|---|---|---|---|---|---|---|---|
| 1h  | 2021 | 0.02 | 12 | 0.31 | 0.115 | 2.34 | 009 |
| 4h  | 2021 | 0.05 | 16 | 0.28 | 0.148 | 2.19 | 049 |
| 1d  | 2021 | 0.10 | 12 | 0.17 | 0.062 | 1.88 | 073 |
| 15m | 2024 | 0.01 | 24 | 0.22 | 0.239 | 2.11 | 086 |

### Conclusions

- **Box width scales with timeframe: ~2% (1h) → ~5% (4h) → ~10% (1d).** Tight daily
  boxes (≤5%) find almost nothing — BTC daily rarely holds a 5% range for 12+ days.
- **Escape ≈ 2× ATR is timeframe-invariant** (1h 2.34, 4h 2.0–2.2, 1d 1.9–2.1). This is
  the single most robust result of the study and the strongest input to the strategy's
  stop sizing.
- **False-break falls monotonically as timeframe rises** (15m 0.24 → 1h 0.12 → 4h 0.15 →
  1d 0.06): higher timeframes give cleaner consolidations with fewer fake breakouts, at
  the cost of far fewer setups. 15m is the noisiest.

### Next

- [x] Phase 4: cross-asset robustness → see Phase 4 below.

---

## Phase 4 — cross-asset robustness (1h, box 0.02/12, since 2021-01-01)

- **Runner:** `consolidation_sweep.py --phase 4` (fixed best-box params, one fetch per
  symbol, range_containment forced). Runs 096–100.

| run | symbol | bars | episodes | coverage | false_break | dur_med (bars) | width_med | esc ×ATR | corr_dir |
|---|---|---|---|---|---|---|---|---|---|
| 096 | BTC/USDT | 47,509 | 408 | 0.311 | 0.115 | 26 | 0.0191 | 2.34 | −0.006 |
| 097 | ETH/USDT | 47,509 | 267 | 0.160 | 0.142 | 21 | 0.0184 | 2.44 | 0.056 |
| 098 | SOL/USDT | 47,509 | 87 | 0.043 | 0.092 | 21 | 0.0189 | 2.37 | −0.058 |
| 099 | BNB/USDT | 47,509 | 319 | 0.238 | 0.176 | 24 | 0.0191 | 2.21 | 0.004 |
| 100 | XRP/USDT | 25,637 | 166 | 0.193 | 0.139 | 27 | 0.0188 | 2.44 | −0.060 |

### Conclusions

- **Escape ≈ 2.2–2.4× ATR on every asset** — the most universal result in the study,
  holding across 5 assets and 4 timeframes.
- **Typical box duration 21–27 bars** and median width ~1.9% are consistent across assets
  at the 2% detector setting.
- **Coverage varies with asset volatility:** BTC 0.31 → BNB 0.24 → XRP 0.19 → ETH 0.16 →
  SOL 0.04. The 2%/12 box is BTC-tuned; more volatile alts (SOL especially) barely
  consolidate that tightly and would need a wider box. Box width should be set per asset,
  not globally.
- **Shape does not predict breakout direction on any asset** (corr_dir −0.06..+0.06,
  all ≈ 0). The negative shape→direction result from BTC is universal, not BTC-specific.

---

## Addendum — named chart patterns + volume profile (BTC/USDT 1h, box 0.02/12)

Two standard analyses added on top of the geometric metrics: the named-pattern taxonomy
(rectangle / triangles / wedges / broadening, classified from edge travel) and a
volume-profile / Market-Profile lens (Point of Control, Value Area).

### Named-pattern breakdown

| pattern | count | breakout up-rate | esc ×ATR |
|---|---|---|---|
| rectangle | 142 | 0.43 | 2.40 |
| rising_wedge | 117 | **0.76** | 2.31 |
| falling_wedge | 80 | **0.20** | 2.35 |
| rectangle_drift | 52 | 0.56 | 2.04 |
| ascending_triangle | 9 | 0.67 | 3.03 |
| descending_triangle | 8 | 0.25 | 2.83 |

### Volume profile

- POC-position vs breakout direction: Pearson −0.07 (no signal).
- POC-position vs escape size: ~0.

### Conclusions (nuances finding #5)

- **Rectangles are a coin flip (0.43 up)** — confirms direction is unpredictable for a
  symmetric box, as the continuous-slope correlation suggested.
- **But wedges carry a strong directional bias in the direction of their tilt:** rising
  wedge breaks up 76%, falling wedge breaks down 80% (n=117/80, robust counts). This is
  *continuation*, the opposite of textbook wedge-reversal lore.
- **Caveat — likely partly mechanical:** a "rising wedge" means both edges drifted up
  during the box, i.e. upward momentum was already present; the escape direction is
  measured from where the breakout close sits, so some of this is momentum
  autocorrelation, not a pure leading signal. Triangles are too rare (n=8–9) to trust.
- **Volume profile (POC/Value Area) adds no predictive power** on BTC 1h — POC location
  does not relate to breakout direction or size.
- **Revised takeaway:** symmetric-box direction is unpredictable, but **edge drift /
  momentum within the box biases the breakout the same way** — usable as a directional
  tilt for the strategy (favor longs in up-drifting boxes), with the autocorrelation
  caveat. Volume profile is not worth wiring in.

## Strategy simulation — verdict: NO ROBUST EDGE after costs

Ran the full strategy logic (look-ahead safe) via `consolidation_strategy_sim.py`:
trailing-window box → edge entry → TP1 at mean (50%) → geometry (TP2 at opposite edge)
**or** hybrid (ratchet-trail the runner) → ATR-beyond-edge stop. Drift and ADX-regime
filters toggle. Costs modeled at 5 bps/side (fee + slippage).

### Findings

- **Drift filter helps gross** (halves loss + drawdown on 1h) and the **hybrid (trailing
  runner) beats the geometry cap** — both confirmed gross of fees.
- **But the gross edge is tiny** (1h hybrid+drift: +0.05 R/trade, PF 1.09). A 1× ATR stop
  on 1h is a small risk distance, so 5 bps/side fees cost ~0.25 R/trade and turn every 1h
  variant **deeply negative** (PF ~0.6–0.7).
- **Higher timeframe reduces fee drag:** 4h hybrid ≈ breakeven (PF 0.98); **1d BTC hybrid
  looked strong (+49 R, PF 2.12, +0.87 R/trade)** — but that edge is breakout-capture from
  a few big trailing runners, not mean reversion.
- **The 1d edge does NOT generalize:** ETH PF 0.26, BNB PF 0.68, SOL only 5 trades. The BTC
  1d result was small-sample, BTC-specific luck.
- **The designed filters don't pay off:** the ADX regime gate consistently *removed*
  profitable trades; drift helps only gross on 1h and hurts on 4h/1d.

### Verdict

**No robust, generalizable (multi-asset) edge at default params.** Across 1h/4h/1d and 5
assets, after realistic costs there is no edge that holds across assets. The mean-reversion
thesis fails to fees; the only positive (BTC 1d breakout capture) does not replicate on other
assets and is small-sample. (Later revisited under a BTC-only scope — see the parameter sweep
and reconciliation sections below; the strategy is shipped as a tunable BTC baseline.)

### Re-test at 0.02% round-trip fees (low-cost assumption)

Even at a generous 0.02% round-trip cost the conclusion holds:
- 1h hybrid+drift: PF 1.01 (breakeven, not worth it).
- 4h hybrid baseline (no filters): BTC PF 1.10 / +0.068 R/trade over 395 trades — but
  **BTC-only**: ETH 0.93, SOL 0.81, BNB 0.89, XRP 0.88 (all negative).
- 1d hybrid baseline: BTC PF 2.18 but does not replicate (ETH/BNB negative).

**Signature across every test: BTC looks acceptable, all other assets lose.** That means the
apparent edge is BTC's specific 2021–2026 price path, not a generalizable consolidation
edge. Verdict unchanged — no robust edge even under optimistic fees.

### If revisited

- Reframe as a **pure higher-timeframe breakout follower** (the only thing that showed any
  promise), not a range-fade — and validate on far more assets/history before trusting it.
- The ~2× ATR escape and per-asset box-width findings remain valid and reusable; the
  *trading* of them at these stops/timeframes is what fails.

## Parameter sweep with in-sample / out-of-sample split (BTC 4h, 0.02% cost)

`consolidation_strategy_sweep.py` grid-searches exit/entry params and splits each
config's trades by date (IS = first 60%, OOS = rest) to catch overfitting.

### Result — a robust BTC config exists

- **112 of 360 configs are positive in BOTH IS and OOS.**
- **Best:** stop 0.75×ATR, trail 1.5×ATR, edge_entry 0.2, **tp1_frac = 0** (no
  scale-out), no drift/regime filters:
  - IS: 196 trades, PF 1.19, +0.14 R/trade
  - OOS: 256 trades, PF 1.34, +0.25 R/trade (OOS *better* than IS → not overfit)

### Key insight — it's a breakout follower, not a range fade

> **Superseded below** — see "Production backtest reconciliation" and the final verdict.
> On the production `run_backtest.py` engine this config loses; the shipped strategy is a
> range-edge mean-reversion entry with a trailing-ATR exit, kept here as the chronological
> research record.

Every top config has **tp1_frac = 0**: scaling out at the box mean *hurts*. The edge is
**entering near a consolidation edge with a tight stop and trailing the FULL position** to
ride breakouts. The original "buy floor / sell ceiling / TP at mean" mean-reversion thesis
is wrong; the related breakout-follow strategy is what carries the edge.

### Status

This is a **BTC-specific** robust config (user accepts BTC-only validity). Still a single
IS/OOS split, not full walk-forward; cost assumed 0.02% round-trip. Before building: confirm
with walk-forward across multiple splits and check fee sensitivity.

## Production backtest reconciliation — FAILED (the sim edge does not hold)

After implementing the strategy (open `consolidation_range` + `trailing_tp_ratchet` close +
`trailing_stop_atr_mult` 1.5) and running the REAL `backtest/run_backtest.py` engine on
BTC 4h since 2021:

- Default ratchet tiers: **−40%, PF 0.98, win 38.7%, 346 trades.**
- Explicit trail-only tier (closest to the sim's fixed full trail): **−47%, PF 0.94,
  win 35.5%, 327 trades.**

Both **contradict the research sim** (which showed PF 1.10–1.34). The gap is the engine
mechanics: the research sim was idealized (enter once per setup, clean fixed trail), while
the production engine enters on every edge bar and trails differently. The thin sim edge did
not survive the real engine. No automated config tweaking was done to chase a green backtest
(that would be overfitting) — the parameter space is left open for deliberate, market-aware
tuning.

**Verdict: a valid but losing strategy at the default params.** The implementation is wired,
builds, and runs (paper smoke OK); the default-param production backtest is a loser. It ships
as a **tunable baseline**, not a turnkey edge: the mechanics (range-edge entry, ATR trailing
exit, regime gating) are sound and selectable, and users can adjust box width, min_bars, stop,
trail, timeframe, and filters per market to search for a profitable configuration. Backtest
any candidate config with `run_backtest.py` before live use; do not run the defaults live as-is.

## Overall conclusions (Phases 1–4)

1. **Consolidations are real and measurable.** With a tuned range-containment detector
   (coverage ~15–35%), BTC 1h shows ~400 clean episodes over 5 years; the concept is
   sound.
2. **Stop sizing — the strongest, most reusable result:** the breakout/escape candle is
   **~2× ATR regardless of timeframe (15m–1d) or asset (BTC/ETH/SOL/BNB/XRP).** A
   breakout-invalidation stop ~1.5–2.0× ATR(14) beyond the box edge is well-grounded.
3. **Box width must be tuned per timeframe and per asset:** ~1% (15m) → 2% (1h) →
   5% (4h) → 10% (1d) for BTC; volatile alts need wider boxes than BTC at the same TF.
   There is no single global box width.
4. **Higher timeframe = cleaner setups, fewer of them:** false-break 0.24 (15m) → 0.06
   (1d). 1h is the sweet spot for setup count vs cleanliness; 1d for quality.
5. **Shape is NOT an edge.** Edge slopes and width-contraction do not predict breakout
   direction (corr ≈ 0 everywhere); width-contraction weakly relates to escape *size*
   only (≈ −0.2). The future strategy should not rely on shape to pick direction.

### Strategy implications (for the follow-up issue)

- Trade the **range geometry** (long near bottom, short near top, TP at mean / opposite
  edge) — supported.
- Size stops on **ATR (~1.5–2.0× ATR) beyond the box edge**, not on shape.
- Make **box width a per-asset, per-timeframe parameter**, not a constant.
- Do **not** use shape to choose long-vs-short; pick direction from range position alone.
- Prefer **1h or 4h**; 1d is cleanest but rare; 15m is too noisy (false-break ~0.24).
