# Consolidation Range Strategy — Design

> **VALIDATED CONFIG (supersedes the mean-reversion design below).** Backtesting flipped
> the thesis: this is a **BTC-only breakout follower**, not a range fade.
> - Scope: **BTC/USDT 4h only**. Detector box **0.05 / 16** (box_width_pct / min_bars).
> - Entry: near a consolidation edge (`edge_entry_frac` 0.2).
> - Exit: **`tp1_frac = 0` (NO scale-out at mean)** — trail the FULL position; initial stop
>   **0.75× ATR**, trailing stop **1.5× ATR** (hybrid ratchet).
> - **No drift filter, no regime/ADX gate** (both reduced performance).
> - Research sim looked positive (OOS PF 1.34; walk-forward 4/5 folds) BUT the production
>   `run_backtest.py` engine LOSES at these defaults (~-40% to -47%, PF <1) — the sim was
>   idealized. Shipped as a **tunable baseline**, not a turnkey edge: tune box/stop/trail
>   per market and backtest before live use. Sections below are the original mean-reversion
>   thesis, kept for context.

**Date:** 2026-06-03
**Status:** Implemented as a tunable baseline (loses at default params on the production
backtester); original mean-reversion design rejected
**Depends on:** consolidation research (Phases 1–4, `docs/research/consolidation-findings.md`)
**Platform:** Hyperliquid perps (needs native shorts); paper + backtest parity required.

## Premise (from the research)

1. Consolidations are real and measurable; a tuned range-containment detector finds
   ~400 clean BTC 1h episodes over 5 years.
2. **Escape candle ≈ 2× ATR, invariant across timeframe (15m–1d) and asset
   (BTC/ETH/SOL/BNB/XRP).** → ATR-sized breakout-invalidation stop.
3. **Box width must be per-asset, per-timeframe** (~1%/2%/5%/10% for 15m/1h/4h/1d on
   BTC; wider for volatile alts). No global constant.
4. Higher timeframe = cleaner but rarer (false-break 0.24→0.06). 1h/4h are the sweet spot.
5. **Shape does NOT predict breakout direction** (corr ≈ 0). Direction comes from range
   position only.

## Architecture

Uses the codebase's open/close split. ONE open strategy; the exit is pluggable, so the
strategy ships with three supported close configurations plus a recommended hybrid.

### Open strategy — `consolidation_range`

`shared_strategies/open/consolidation_range.py`, registered in `open/registry.py`.

- Live range detection: the trailing `min_bars` candles' high-low span stays within
  `box_width_pct` of mid (the range-containment detector ported from the research).
- Require a **mature** box (held the full window) — never trade a forming range.
- **Edge entry only:** long when price is in the bottom `edge_entry_frac` (default 0.20)
  of the box; short in the top `edge_entry_frac`. Entering near the edge is what makes the
  reward-to-risk work.
- **Drift-tilt filter (from the pattern/wedge finding):** compute the box's net drift as
  the sum of the top- and bottom-edge travel (the `top_edge_travel` + `bottom_edge_travel`
  metrics from the research, each a fraction of box height). The research showed wedges
  break in the direction of their drift (rising-wedge 76% up, falling-wedge 80% down),
  while flat rectangles are a coin flip. So:
  - **Up-drifting box** (net drift > `drift_threshold`, default 0.5): take longs at the
    bottom; **suppress shorts at the top** (high odds of an upward break-through that would
    stop the short).
  - **Down-drifting box** (net drift < −`drift_threshold`): take shorts at the top;
    suppress longs at the bottom.
  - **Flat box** (|net drift| ≤ `drift_threshold`): trade both edges (pure mean reversion).
  The filter only *vetoes* fading a strongly-tilted box; it never forces an entry. Treated
  as a veto (not a strong predictor) because the wedge bias is partly momentum
  autocorrelation, not a clean leading signal. `drift_filter` (default on) toggles it.
- One position at a time per asset. Direction = range position, gated by the drift tilt.
- Bidirectional: add to `bidirectionalPerpsStrategies` (init.go).

### Box bounds + regime stamped on the Position at open

New `Position` fields (set like `stampEntryATRIfOpened`): `ConsolidationTop`,
`ConsolidationBottom`, `ConsolidationMean`. The geometry close evaluator reads these.
`pos.Regime` is already stamped on all execute dispatches.

### Close configurations

**A. Geometry-anchored TP (new close evaluator `consolidation_geometry_tp`)**
- TP1 = box mean → scale out 50%.
- TP2 = opposite edge → close remaining 100%.
- SL = `stop_atr_mult` × ATR beyond the entry-side edge (default 1.0× ATR below box
  bottom for a long). Per finding #2 the real escape is ~2× ATR, so a 1× ATR buffer sits
  inside range noise but is taken out decisively by a true breakout.
- Reads the stamped box bounds. Faithful to the concept and the data.
- Strategy-level SL owner; new evaluator added to `closeStrategyOwnedKeys`, Go SL arming,
  Python evaluator, and backtest parity.

**B. ATR-approximated TP (reuse existing `tiered_tp_atr_live` / `_regime`)**
- Approximate mean/opposite-edge as ATR multiples off entry. No new close code.
- Less accurate (box-width-to-ATR isn't constant per finding #3) but a zero-new-code
  fallback and a useful backtest baseline to compare against A.

**C. Regime ratchet (reuse existing `trailing_tp_ratchet_regime`, #844)**
- Tiers cleared by ATR profit distance; each cleared tier monotonically tightens the
  trailing ATR stop and optionally scales out. Places no on-chain TP.
- Regime-keyed tiers (ranging vs trending labels via `regime_atr_window`).
- Captures the case a pure range trade forgoes: when price breaks OUT in your favor after
  an edge entry, the ratchet lets it run instead of capping at the opposite edge.

**Recommended hybrid (best technical solution):** scale out 50% at the **box mean**
(config A's high-probability mean-reversion leg), then **ratchet-trail the remaining 50%**
(config C) instead of hard-capping at the opposite edge. Banks the reliable mean-reversion
profit and still rides the occasional decisive breakout. Implemented as config A's TP1 plus
a ratchet-trailed runner; both mechanisms already exist once A is built.

### Regime gating

Counter-trend by nature, so gate entries with `allowed_regimes` (ranging only) — skip
strong trends. The ratchet variant additionally keys its tier table to regime.

## Parameters (per-asset, per-timeframe)

`box_width_pct`, `min_bars`, `edge_entry_frac` (0.20), `stop_atr_mult` (1.0),
`atr_period` (14), `drift_filter` (on), `drift_threshold` (0.5). Research-backed
defaults: BTC 1h → 0.02 / 12; 4h → 0.05 / 16;
1d → 0.10 / 12; volatile alts wider. Ships per-asset in config, not a global value.

## Codebase wiring (checklist)

1. Open strategy module + `open/registry.py` + `PLATFORM_ORDER`; `knownShortNames` +
   defaults in `init.go`; `DEFAULT_PARAM_RANGES` in `backtest/optimizer.py`;
   `bidirectionalPerpsStrategies`.
2. `Position` box-bounds fields + stamping at open (all relevant execute dispatches).
3. New close evaluator `consolidation_geometry_tp`: Go SL arming, `closeStrategyOwnedKeys`,
   Python evaluator (`shared_strategies/close/`), close registry, on-chain-protection
   suppression handling, hot-reload gating, backtest parity.
4. Wire configs B and C as supported close refs (existing evaluators, no new code).
5. Probe argv additions if any new required CLI flags (`version_probe.go`).
6. Tests: Go (skip-reason, SL arming, box stamping) + Python (evaluator) + backtest
   look-ahead regression.

## Scope and risks

- **Novel work:** geometry-anchored TP evaluator + box-bounds persistence. Configs B and C
  reuse existing machinery.
- **Main failure mode:** entering a short at the top exactly as price breaks up. Mitigated
  by the maturity requirement, the regime gate, and the 1× ATR invalidation stop (decisive
  per the data). The hybrid's ratchet runner also turns an adverse "I was on the wrong side
  of a breakout" into a smaller loss on the correct side over time.
- **Counter-trend:** pair with the regime gate; do not run in trending regimes.

## Backtest plan (before live)

- Backtest open + each close config (A/B/C/hybrid) on BTC 1h and 4h, plus ETH/SOL, using
  the research's per-asset box params.
- Compare hybrid vs pure-geometry vs ratchet on win rate, R:R, and drawdown.
- Confirm look-ahead invariants (signal N → fill N+1, regime reads N−1).

## Follow-up / open items

- Decide whether box bounds should also re-anchor on SIGHUP while open (likely frozen at
  open like `pos.Regime` for #844).
- Whether to support a `direction="both"` vs separate long/short instances.
