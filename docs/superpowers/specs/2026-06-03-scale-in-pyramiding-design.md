# Scale-in / Pyramiding — Design

**Issue:** [#873](https://github.com/richkuo/go-trader/issues/873) — Add scale-in / pyramiding: increase an open position's size (strategy + manual)
**Date:** 2026-06-03
**Status:** Approved (design); pending implementation plan

## Problem

There is no way to add to (scale into / pyramid) an open position. A position's size only ever decreases until flat. Same-direction re-entry is deliberately skipped everywhere (idempotency for strategies that re-emit a buy every cycle while the setup holds). This adds an **opt-in** way to increase an open position's size while leaving the default skip-on-same-direction behavior unchanged for every strategy that does not opt in.

## Settled decisions

### Scope (from brainstorming)
- **Entry paths:** BOTH a `manual-add` CLI subcommand AND an opt-in `allow_scale_in` strategy flag.
- **Add trigger (strategy flag):** configurable **signed** ATR-spacing. `add_spacing_atr > 0` = add-to-winners (price moved in-favor by ≥ N×EntryATR since last entry); `< 0` = average-down (price moved adverse by ≥ |N|×EntryATR); `0` = add on every same-direction signal up to the caps.
- **Platform/execution scope:** Hyperliquid perps + `manual`, both **live** (with on-chain protection re-size) and **paper** (blend only). Spot, futures, and backtest parity are explicitly **out of scope** for this cut.

### Blend semantics (frozen entry — from the issue)
A scale-in blends only **price and size** for PnL:
- `AvgCost = (oldQty·oldAvg + addQty·addPrice) / (oldQty + addQty)`
- `Quantity += addQty`
- `InitialQuantity += addQty`

Everything else is **frozen to the first entry**:
- `EntryATR` — unchanged (drives ATR stop distance and tiered-TP offsets).
- `Regime` / `RegimeWindows` — unchanged (regime-keyed SL/TP/ratchet resolve from the original label).
- TP tier **offsets** (trigger geometry) — unchanged. Only protection **sizing** is re-based.
- Tier watermarks `SLAdjustedTiersProcessed` / `TPArmedTiers` — unchanged (an add must not reset cleared tiers).

Rejected alternative (re-base around the new blended average on every add) — would shift the operator's original risk plan on each add and fight the `InitialQuantity` and tier-watermark invariants.

## Verified current behavior (grounding)

All file:line references verified against the worktree at design time.

- **Same-direction skip guards:** `PerpsOrderSkipReason(signal, posSide, direction)` (`scheduler/portfolio.go:594`) gates the live order before the subprocess spawns; `main.go:2684` calls it in `runHyperliquidExecuteOrder`. `ExecutePerpsSignal` skips same-direction at `portfolio.go:927` / `:1107`. Spot: `SpotOrderSkipReason` (`:773`), `FuturesOrderSkipReason` (`:800`), "Already long … skipping buy" (`:1318`). Manual: `manual.go:612` refuses when a position exists.
- **Position construction:** fresh `Position{AvgCost: execPrice, InitialQuantity: qty, Quantity: qty, …}` at `portfolio.go:1068` (perps long), `:1422` (spot), `:1682` (futures), `manual.go:615`. Position struct at `portfolio.go:10-49`.
- **`InitialQuantity` as high-water mark:** tier-fill detection `pos.Quantity+1e-9 < pos.InitialQuantity` (`discord.go:1135`); sl_after gate `pos.Quantity >= pos.InitialQuantity-1e-9 → return false` (`post_tp_sl.go:1134`) and `InitialQuantity<=0` guard (`:1123`); tier-split baseline `initQty := pos.InitialQuantity` in `hyperliquid_fills.go:472` and `hyperliquid_balance.go:1558`, feeding `hyperliquidTPTierIncrementalCloseQty(initQty, tiers, i)`.
- **Stamp-once:** `stampEntryATRIfOpened` (`main.go:2316`) returns early when `pos.EntryATR != 0`; `stampPositionRegimeFromPayload` (`regime_multi_window.go:515`) returns early when `pos.Regime != ""`.
- **On-chain protection:** `TPArmedTiers` (`portfolio.go:26-31`) and `SLAdjustedTiersProcessed` (`:41-44`) are the idempotency watermarks; live cancel+replace at `main.go:2742-2749` skips SL/TP cancel on partial close, cancels stale SL + all TP OIDs on flip/open.
- **Trailing stop:** `hlSLEffectiveQty(symbol, virtualQty, onChainQtyMap)` (`hyperliquid_trailing_stop.go:18`) returns `min(virtualQty, onChainQty)`; callers `main.go:1577/1618/1744`, `post_tp_sl.go:1199`.
- **Live order path:** leverage/margin-mode only from flat — `posQty == 0` gate at `main.go:2771`; `perpsLiveOrderSize(...)` (`portfolio.go:688`) has flat-open / flip / close branches only.
- **Trade stats:** `LifetimeTradeStatsAll` (`db.go:1484`) counts `is_close = 0` rows for `#T` and groups close legs by `(strategy_id, position_id)` for W/L. `Trade` struct (`portfolio.go:410-458`) has `IsClose`, `RealizedPnL`, `PositionID`, `TradeType`. `RecordTrade` (`state.go:41`) → `StateDB.InsertTrade` (`db.go:647`).
- **Partial-close mirror:** `pos.Quantity -= closeQty` preserving `InitialQuantity`/`AvgCost`/`EntryATR` at `portfolio.go:1003` (perps), `:1377` (spot), `:1628` (futures), `manual.go:697`. Partial-open mirrors this (increment instead of decrement; `IsClose:false`).

## Architecture (Approach A: shared pure core + thin entry adapters)

Rejected: (B) overloading the open path with an add-mode flag — entangles add logic into the hot open path, higher regression risk on the #298 guard; (C) close-to-flat then re-open — loses frozen entry, double fees.

### Components

**1. Pure blend core — `applyScaleIn(pos *Position, addQty, addPrice float64)`** (new, in `portfolio.go` or a new `scale_in.go`)
- Mutates: `AvgCost` (weighted blend), `Quantity += addQty`, `InitialQuantity += addQty`, `ScaleInCount++`, `LastAddPrice = addPrice`, `AddedNotionalUSD += addQty·addPrice`.
- Leaves frozen: `EntryATR`, `Regime`, `RegimeWindows`, `SLAdjustedTiersProcessed`, `TPArmedTiers`, `OpenedAt`, `TradePositionID`.
- Pure, subprocess-free, unit-testable.

**2. Add-decision helper — `perpsScaleInDecision(sc StrategyConfig, pos *Position, signal int, price float64) (shouldAdd bool, addQty float64, reason string)`** (new, pure)
- Computed **once** and consumed by BOTH `PerpsOrderSkipReason` (returns `""` to allow the order when `shouldAdd`) and the execute/dispatch path (sizes the add). This single-source decision keeps the on-chain order and the Trade record consistent — closes the #298 "fill lands, no Trade recorded" gap.
- Gates (all must hold):
  - `sc.AllowScaleIn` true.
  - Signal direction matches the open side (an add never flips; opposite signal stays a close/flip on the existing path).
  - `pos.ScaleInCount < max_adds`.
  - `pos.AddedNotionalUSD + addNotional ≤ max_added_notional_usd`.
  - ATR spacing: `add_spacing_atr == 0` → no spacing gate; `> 0` → in-favor move `(price − LastAddPrice)·dir ≥ add_spacing_atr·EntryATR`; `< 0` → adverse move `(LastAddPrice − price)·dir ≥ |add_spacing_atr|·EntryATR`. `dir = +1` long, `−1` short.
- `addQty` derived from `add_notional_usd / price`. Default when unset: the strategy's standard open notional (the same sizing a fresh open leg uses via `PerpsOpenNotional` / `margin_per_trade`), so the per-add size is unambiguous and matches the strategy's normal sizing.
- Returns a human-readable `reason` when it declines (logged as a skip, not an error).

**3. `perpsLiveOrderSize` add branch** (`portfolio.go:688`)
- New branch: when the dispatch has decided `shouldAdd`, return `size = addQty` (the increment), `ok = true`. Distinct from the flip branch (`posQty + new`).
- Leverage/margin-mode NOT re-sent (the `posQty == 0` gate at `main.go:2771` already prevents it on a non-flat add).

**4. HL-live protection re-size — reuse `runHyperliquidProtectionSync` forced replace**
- After `applyScaleIn` mutates the position (Quantity + InitialQuantity grown, watermark preserved), trigger a forced cancel+replace of:
  - **SL** sized to the full new `Quantity` at the frozen trigger (existing ATR/regime/trailing geometry from the original entry).
  - **Un-cleared TP tiers** sized from the grown `InitialQuantity` via the existing `hyperliquidTPTierIncrementalCloseQty` resolver, at frozen trigger prices. Cleared tiers (below the watermark) are NOT re-placed.
- `hlSLEffectiveQty = min(virtualQty, onChainQty)` naturally tracks the larger qty once the SL is re-placed.
- Paper: blend only, no on-chain calls.

**5. `manual-add` CLI** (`manual.go`) mirroring `manual-open`
- `manual-add --strategy <id> --symbol <sym> [--notional <usd> | --size <qty>] [--record-only]`. `--notional` and `--size` are mutually exclusive; when neither is given, fall back to `manual-open`'s notional default (`user_defaults.manual` → hardcoded `$50`).
- Direction inferred from the open position side; errors if the add would flip.
- Refuses when flat: "no open position for <id>/<sym>; open one first" (inverse of manual-open's `:612` guard).
- Kill-switch + circuit-breaker guards like manual-open.
- Calls `applyScaleIn`, books a `scale_in` Trade leg, and (live) re-sizes protection.

**6. Config surface**
- Strategy-level `allow_scale_in bool` + a `scale_in` sub-block:
  - `max_adds int` (e.g. 3)
  - `max_added_notional_usd float64` (absolute cap on cumulative added notional)
  - `add_spacing_atr float64` (signed; default 0)
  - `add_notional_usd float64` (per-add notional; default = initial entry notional)
- New `Position` fields: `ScaleInCount int`, `LastAddPrice float64`, `AddedNotionalUSD float64`, persisted via idempotent positions-table columns (matches existing migration pattern).
- If `manual-add` becomes a runtime-required argv on any check script, append it to `probeArgv` in `version_probe.go`. (Manual CLI is operator-invoked, not a check-script flag, so likely not required — confirm during implementation.)
- `config_version` bump only if the unknown-key guard requires it; new optional struct fields with json tags are additive (confirm during implementation).

**7. Trade records / stats**
- Add leg: `IsClose:false`, `TradeType:"scale_in"`, same `PositionID`, no `RealizedPnL`.
- `LifetimeTradeStatsAll` open-count query (`db.go:1484`) changes from `is_close = 0` to `is_close = 0 AND trade_type <> 'scale_in'` so `#T` still counts distinct positions opened.
- W/L grouping (close legs by `(strategy_id, position_id)`) is unaffected — the add leg is not a close.

## Data flow

### Strategy-flag add (HL perps live)
1. Cycle runs the signal script; same-direction signal returns for an open position.
2. Dispatch computes `perpsScaleInDecision` once.
3. `PerpsOrderSkipReason` consults the decision: `shouldAdd → ""` (allow); else the usual skip string.
4. `perpsLiveOrderSize` add branch returns `addQty`.
5. On-chain order placed for `addQty` (no `update_leverage`).
6. On fill: `applyScaleIn` blends the position; `scale_in` Trade recorded.
7. Forced protection sync re-sizes SL (full Quantity) + un-cleared TP tiers (grown InitialQuantity) at frozen triggers; watermark preserved.

### manual-add (live)
1. Operator runs `manual-add`. Guard: position must exist, side must match (no flip), kill-switch/CB clear.
2. On-chain add order for the requested notional/size.
3. On fill: `applyScaleIn`; `scale_in` Trade; forced protection re-size.

### Paper (perps or manual)
Same as above minus on-chain order and protection re-size — blend + Trade only.

## Error handling

- **Caps / spacing not met:** logged skip via the decision `reason`; not an error; the order is simply not placed.
- **Insufficient margin / leverage:** `perpsLiveOrderSize` returns `ok=false`; no state change.
- **Opposite-direction signal:** falls through to the existing close/flip path; never treated as an add.
- **Protection re-size failure (live):** the add fill is already booked — do NOT lose it. Emit an operator alert (reuse the live-exec failure notifier) and let the next-cycle protection sync retry the re-size. The position is correct; only the on-chain SL/TP temporarily under-covers.
- **manual-add when flat / would flip:** hard error, no order.

## Testing

- **Pure blend** (`applyScaleIn`): AvgCost/Quantity/InitialQuantity/AddedNotionalUSD across multiple adds; assert `EntryATR`, `Regime`, `SLAdjustedTiersProcessed`, `TPArmedTiers` unchanged.
- **Decision** (`perpsScaleInDecision`): caps (max_adds, max_added_notional_usd); spacing positive / negative / zero for long and short; direction-match (opposite signal → not an add).
- **Protection re-size:** table test that the tier resolver, after `InitialQuantity` grows and with a non-zero watermark, sizes the SL to the new total and places only un-cleared tiers; cleared tiers are not re-placed.
- **manual-add CLI:** record-only blend; refuse-when-flat; refuse-when-flip.
- **Stats:** a `scale_in` leg is excluded from `#T` open-count; W/L grouping unchanged.
- **Skip-reason ↔ execute consistency:** when the decision says add, both the guard allows and the execute path sizes it (no #298 gap); when it says skip, neither fires.

## Acceptance criteria (from the issue)

- Documented, opt-in increase of an open position's size (manual + strategy), gated so existing strategies are unaffected by default. ✓ (`allow_scale_in` default false; `manual-add` explicit)
- `AvgCost`/`Quantity` blend correctly; close PnL computed against the blended average. ✓ (`applyScaleIn`)
- `EntryATR`, regime label, TP tier offsets pinned to first entry. ✓ (frozen in `applyScaleIn`)
- On-chain TP/SL cover the new total at unchanged triggers without resetting the cleared-tier watermark. ✓ (forced sync, watermark preserved)
- `InitialQuantity` grows with the add so no false "partially closed" state. ✓
- Skip-reason guards allow the add without reopening the #298 gap. ✓ (single-source decision)
- Test coverage for blend math and protection re-sizing. ✓

## Out of scope

Spot/futures scale-in; backtest parity; re-basing stop/TP/regime around the blended average; averaging-down safety beyond the configured spacing sign (operator's responsibility).
