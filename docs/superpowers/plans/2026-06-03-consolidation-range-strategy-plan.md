# Consolidation Range Strategy — Implementation Plan

> **VALIDATED SCOPE:** Build the **BTC/USDT 4h breakout follower** (box 0.05/16, edge 0.2,
> `tp1_frac=0`/full trail, stop 0.75× ATR, trail 1.5× ATR, no filters). Config C (hybrid
> trail) is the close; configs A/B and the drift/regime filters are NOT built — backtesting
> showed they hurt. Phases below still apply; skip the drift filter and the geometry/ATR
> close variants.
>
> **IMPLEMENTED (2026-06-04).** Because the validated config is `tp1_frac=0` (no scale-out,
> trail the full position), the exit maps directly onto the EXISTING strategy-level
> `trailing_stop_atr_mult` — so **Phases 2 and 3 (box-bounds stamping + new geometry close
> evaluator) are NOT needed.** Done: open strategy `consolidation_range` (registry +
> `PLATFORM_ORDER` + `init.go` shortname/bidirectional + optimizer ranges), 5 Python tests,
> Go build + full Go tests + 344 open-strategy tests pass. `init --json` generates a valid
> perps config; `--once` paper smoke runs and reports
> `close=open-as-close sl=trailing_stop_atr_mult tp=none`. Validated config: BTC perps 4h,
> box 0.05/16 (registry defaults), `trailing_stop_atr_mult: 1.5`. Remaining: optional
> backtester trailing-stop parity check, then paper-trade.


**Spec:** `docs/superpowers/specs/2026-06-03-consolidation-range-strategy-design.md`
**Branch:** `consolidation-research` (or a fresh feature branch off main)
**Principle:** each phase is independently buildable + testable. Build + `go test ./...`
+ `pytest` + `py_compile` must pass at the end of every phase before moving on.

Verification commands (run from repo root):
- `go build -ldflags "-X main.Version=$(git describe --tags --always --dirty=-mod)" -o go-trader ./scheduler` (or `go -C scheduler build .`)
- `go test ./...`
- `uv run --no-sync python -m pytest shared_strategies/ shared_tools/ backtest/`
- `uv run --no-sync python -m py_compile <changed.py>`
- registry parity: snapshot `…/open/{spot,futures}/strategies.py --list-json` before/after.

---

## Phase 0 — Prep & guardrails

1. Snapshot registry output for byte-identical diffing later:
   `…/open/spot/strategies.py --list-json > /tmp/spot_before.json` (and futures).
2. Confirm target: HL perps, default 1h/4h. Decide single bidirectional instance vs
   separate long/short (spec open item) — default to one instance, `direction="both"`.

**Done when:** baselines captured; no code changed.

---

## Phase 1 — Open strategy: detection + edge entry (Python-first, backtestable)

Files:
- `shared_strategies/open/consolidation_range.py` — core detector + signal:
  - port `detect_range_containment` logic (trailing `min_bars` window within
    `box_width_pct` of mid); require a mature box.
  - signal: long when `close` in bottom `edge_entry_frac` of the live box, short in top
    `edge_entry_frac`, else hold. Direction from range position only.
  - expose the live box (top/bottom/mean) in the result for downstream stamping.
- `open/registry.py`: `@register("consolidation_range", …, platforms=("futures",) or perps)`,
  `default_params` (box_width_pct, min_bars, edge_entry_frac, atr_period), add to
  `PLATFORM_ORDER`.
- `init.go`: `knownShortNames` entry + defaults; `bidirectionalPerpsStrategies` add.
- `backtest/optimizer.py`: `DEFAULT_PARAM_RANGES` entry.

Tests:
- `shared_strategies/open/test_*` (or co-located): synthetic flat box → long signal at
  bottom edge, short at top edge, hold mid-box and when box immature.
- Registry: `--list-json` diff is byte-identical except the new entry.
- Smoke: `./go-trader init --json …` then `./go-trader --once` with the strategy.

**Done when:** strategy registers, signals correctly on fixtures, backtests open-as-close
(`run_backtest.py --strategy consolidation_range`), all suites green.

---

## Phase 2 — Persist box bounds on the Position (Go)

Files:
- `state.go` / position struct: add `ConsolidationTop`, `ConsolidationBottom`,
  `ConsolidationMean` (float64). DB migration if positions persist these (idempotent).
- `main.go`: stamp the box bounds when a position is opened by this strategy, mirroring
  `stampEntryATRIfOpened` — set on all relevant execute dispatches (perps live + paper).
  Source the bounds from the open strategy's emitted box (thread via the
  `--position-*`/signal output the same way EntryATR flows).

Tests:
- Go: opening a `consolidation_range` position stamps non-zero, self-consistent bounds
  (bottom < mean < top); other strategies leave them zero.

**Done when:** box bounds reliably present on the position post-open; build + tests green.

---

## Phase 3 — Geometry-anchored close evaluator (config A) — the novel piece

Files:
- `shared_strategies/close/consolidation_geometry_tp.py`: evaluator reading the position's
  box bounds + side:
  - TP1 = mean (close 50%), TP2 = opposite edge (close 100%).
  - partial-close decrements quantity, preserves InitialQuantity (existing partial rules).
- `close/registry.py`: register it; ensure import via `close_registry_loader`.
- Go close wiring:
  - `closeStrategyOwnedKeys` entry for the new evaluator.
  - SL = `stop_atr_mult × ATR` beyond the entry-side edge; integrate into the
    `EffectiveStopLossPct` priority chain as a strategy-level ATR owner (mutually exclusive
    with the seven existing stop fields). Validate sole ownership.
  - on-chain protection: this close places geometry TPs, not the ATR ladder — decide
    whether to suppress on-chain protection (it is NOT a `tiered_tp_atr` name, so it is not
    auto-suppressed; confirm the geometry TPs don't race the close evaluator, or place them
    on-chain explicitly). Add to the suppression list only if it places on-chain TPs.
  - hot-reload: gate box-bounds/SL shape changes while open (`validateHotReloadStateCompatible`).
- `backtest/backtester.py`: parity — geometry TP + ATR-beyond-edge SL, bar-level.

Tests:
- Python: long position with box {bottom,mean,top}; price to mean → 50% close; to top →
  full close; drop through bottom−ATR → SL.
- Go: SL arming computes AvgCost-relative trigger from box edge + ATR; skip-reason guards
  repeat conditions before spawning.
- Backtest look-ahead regression (`test_backtester_lookahead.py` analog).

**Done when:** config A fully works live-path-wise (paper) + backtests with parity.

---

## Phase 4 — Wire configs B & C + the hybrid (reuse, minimal code)

- **Config B (ATR-approx):** a sample close ref using `tiered_tp_atr_live` with mults
  approximating mean/opposite-edge. Config-only; no new code. Document the approximation.
- **Config C (regime ratchet):** a sample close ref using `trailing_tp_ratchet_regime`
  (#844) with regime-keyed tiers + strategy-level `trailing_stop_atr_mult`. Config-only.
- **Hybrid:** config A's TP1 (50% at mean) + ratchet-trail the runner. If A's evaluator
  can emit "scale 50% at mean, then hand the remainder to the trailing walker," implement
  as a flag on A; otherwise compose A (TP1 only) with the ratchet on the remaining qty.
  Confirm SL ownership is single (ratchet's `trailing_stop_atr_mult` vs A's ATR SL — pick
  one owner; for the hybrid the ratchet trail owns the runner's stop after TP1).

Tests:
- Config validation: each sample config loads, passes `validateConfig`, hot-reloads.
- Backtest each of B, C, hybrid on BTC 1h.

**Done when:** all four close configs load, validate, and backtest.

---

## Phase 5 — Regime gating

- Add `allowed_regimes` (ranging labels) to the shipped configs so entries are blocked in
  trending regimes (`regimeBlocksOpen`); closes always pass.
- Validate regime vocabulary matches the window classifier
  (`validateStrategyRegimeVocabulary`).

Tests: entry blocked when current regime ∉ allowed and flat; close unaffected.

**Done when:** regime gate verified on fixtures + a backtest with `--regime-*`.

---

## Phase 6 — Backtest bake-off (decision data, before any live)

- Run open + {A, B, C, hybrid} on BTC 1h & 4h, ETH 1h, SOL 4h, using research per-asset
  box params (`docs/research/consolidation_runs.csv`).
- Compare win rate, R:R, max drawdown, trade count. Record results into the research
  findings log (new section) for the issue.

**Done when:** comparison table produced; recommended default close config chosen.

---

## Phase 7 — Deploy plumbing & docs

- `version_probe.go`: append any new required CLI flags to `probeArgv` (and execute argv)
  — asymmetric deploy fails at startup otherwise.
- `init.go` wizard + `generateConfig` cover the new params.
- Update `CLAUDE.md` file-ownership map + `SKILL.md` if the post-update protocol needs it.
- Post-deploy smoke: `./go-trader --config scheduler/config.json --once`.

**Done when:** probe passes, smoke passes, docs updated.

---

## Risk register

- **Geometry TP race with on-chain protection** — resolve in Phase 3 (suppress or place
  explicitly). Highest-novelty risk.
- **SL double-ownership** in the hybrid (A's ATR SL vs ratchet trail) — enforce single
  owner per leg.
- **Registry byte-identical output** — diff `--list-json` every registry change.
- **Look-ahead leakage** in the geometry close backtest — covered by the regression test.

## Sequencing summary

Phase 1 (open) → 2 (box stamping) → 3 (geometry close A) are the critical path and the
only places with genuinely new code. 4–5 are reuse + config. 6 is the go/no-go bake-off.
7 is deploy hygiene. Each phase ends green before the next begins.
