# Anchored VWAP (`anchored_vwap`) — Strategy Design

**Issue:** [#1016](https://github.com/richkuo/go-trader/issues/1016)
**Follow-ups (out of scope):** [#1017](https://github.com/richkuo/go-trader/issues/1017)
**Date:** 2026-06-15
**Status:** Approved design — ready for implementation plan.

## 1. Goal

Add a new open strategy that trades a **single Anchored VWAP (AVWAP)** line as a
dynamic support/resistance level. Unlike the two existing VWAP strategies
(`vwap_reversion`, `vwap_rejection_st`), which reset the cumulative
volume×price accumulation at each **calendar-day session boundary**, AVWAP
accumulates from a **meaningful market event** (a confirmed swing pivot) and
re-anchors only when a newer pivot confirms. The line represents the
volume-weighted average price since the last structural turn, which traders
read as support (anchored from a low) or resistance (anchored from a high).

## 2. Core design decisions (locked)

| Decision | Choice |
|---|---|
| Anchor event | Most recent **confirmed** swing pivot (high *or* low), type-agnostic |
| Anchor count | **Single** AVWAP line (not dual support/resistance) |
| Signal logic | **Support/resistance flip** — reclaim above → long, break below → short |
| Trigger filter | **ATR buffer + N-bar hold** (whipsaw guard) |
| Direction / platform | **Bidirectional perps live** + spot + backtest |
| Exit | Opposite flip is the exit / reversal — no custom close evaluator |

## 3. Anchor: confirmed swing pivot

A swing high at bar *p* requires `high[p] == max(high[p-strength : p+strength+1])`;
a swing low mirrors on `low`. Because confirmation needs `strength` bars **on
each side**, a pivot at *p* is only *knowable* at bar `p + strength` — this is
the look-ahead guarantee.

- `pivot_strength` (default **5**) — bars required on each side.
- At any bar *n*, the **active anchor** = bar index of the most recent pivot
  whose confirmation bar `p + strength ≤ n`. Type (high vs low) does **not**
  affect logic — a single line is anchored from that bar regardless.
- The anchor **re-anchors forward** each time a newer pivot confirms.
- Before the first pivot confirms there is **no anchor → no signal** (signal 0).
  Do **not** fall back to a session/global VWAP.

### Anchor selection (implementation note)
Compute boolean pivot-high / pivot-low masks, OR them into a single
`is_pivot` mask, then shift each pivot's *known-at* index to `p + strength`.
Forward-fill the pivot **bar index** along the known-at timeline to produce
`anchor_index[n]` for every bar. Ties (a high and a low confirming on the same
bar) resolve to the **later bar index**; if equal, prefer the pivot **low**
(documented, deterministic — only matters for the AVWAP start bar).

## 4. AVWAP computation

Anchored VWAP from anchor bar *a* to current bar *n*, using typical price
`tp = (high + low + close) / 3`:

```
AVWAP[n] = (Σ tp·vol [a..n]) / (Σ vol [a..n])
```

Implemented with **global prefix sums** so it is O(n) and exact across arbitrary
anchor resets:

```
AVWAP[n] = (prefix_tpvol[n] - prefix_tpvol[a-1]) / (prefix_vol[n] - prefix_vol[a-1])
```

- `a = anchor_index[n]`; when `a == 0`, the `[a-1]` term is 0.
- **Div-by-zero guard:** if the denominator (window volume) is 0, fall back to
  `tp[n]` (matches `vwap_rejection_st._session_vwap`'s `replace(0, NaN).fillna(typical)`).
- The anchor activates `strength` bars after the pivot bar but the accumulation
  **starts at the pivot bar** — the prefix-sum form handles the lag region
  naturally because it always sums `a..n`.

## 5. Trigger: ATR buffer + N-bar hold (symmetric)

ATR comes from `shared_tools/atr.py:standard_atr(df, period)` — **do not add a
5th inline ATR copy** (CLAUDE.md: ATR has 4 sites already).

```
buffer = buffer_atr_mult × ATR[n]          # price units
```

**Long (+1):** price was below the line, then `close ≥ AVWAP + buffer`, and
`close ≥ AVWAP` holds for `confirm_bars` consecutive bars. Fire **+1 only on the
bar that completes the hold** (the transition), not continuously while held —
mirrors `vwap_reversion`'s cross-once pattern (`buy_cross` = condition true now
& false on prior bar).

**Short (−1):** mirror — `close ≤ AVWAP − buffer` and `close ≤ AVWAP` held for
`confirm_bars`. Fire −1 on the completing transition.

**Reversal/exit:** the opposite flip emits the opposite signal. On bidirectional
perps this flips the position; on spot/backtest the −1 is an exit (or short per
backtester support).

### Whipsaw / state notes
- A new anchor confirming mid-trade resets the "was below/above" reference to
  the new line; the next qualifying buffered-and-held cross of the **new** line
  fires. This is intended (the level the market respects has moved).
- `confirm_bars` counts bars strictly **after** the buffered breach bar,
  inclusive of the breach bar itself = 1; default `confirm_bars=2` means breach
  bar + 1 confirming bar.

## 6. Parameters

| Param | Default | Range (optimizer) |
|---|---|---|
| `pivot_strength` | 5 | [3, 5, 8] |
| `buffer_atr_mult` | 0.25 | [0.1, 0.25, 0.5] |
| `confirm_bars` | 2 | [1, 2, 3] |
| `atr_period` | 14 | (fixed; not swept) |

## 7. Look-ahead safety

- Pivot confirmation, `anchor_index[n]`, AVWAP[n], and ATR[n] use **only bars ≤ n**
  (the `strength` bars after a pivot are themselves ≤ n at confirmation time).
- Signal at bar N fills at N+1 open (backtester invariant).
- Add an `anchored_vwap` case to `backtest/tests/test_backtester_lookahead.py`
  asserting that injecting future bars after N does not change `signal[≤N]`.

## 8. File-by-file wiring

1. **`shared_strategies/open/anchored_vwap.py`** (new) — `anchored_vwap_core(df, pivot_strength=5, buffer_atr_mult=0.25, confirm_bars=2, atr_period=14)` returning the df with added `signal`, `avwap`, `anchor_index`, `atr` columns. Logic non-trivial → its own module, imported like `vwap_rejection_st` (`from vwap_rejection_st import vwap_rejection_st_core`).
2. **`shared_strategies/open/registry.py`**:
   - `from anchored_vwap import anchored_vwap_core` (top imports, ~line 53).
   - `@register("anchored_vwap", "Anchored VWAP — single VWAP anchored to the last confirmed swing pivot as dynamic S/R; long on a buffered reclaim above, short on a buffered breakdown below", {"pivot_strength": 5, "buffer_atr_mult": 0.25, "confirm_bars": 2, "atr_period": 14})` wrapping `anchored_vwap_core`. Default `platforms=("spot","futures")`.
   - Add `"anchored_vwap"` to **both** `PLATFORM_ORDER["spot"]` and `["futures"]` (line 1254). Append near the other VWAP entries.
3. **`scheduler/init.go`**:
   - `knownShortNames["anchored_vwap"] = "avwap"` (line 37 map).
   - `bidirectionalPerpsStrategies["anchored_vwap"] = true` (line 89 map) — sets `AllowShorts=true` so `ExecutePerpsSignal` opens shorts from flat on −1 instead of skipping.
   - Add to `defaultSpotStrategies` / futures fallback lists for discovery-failure parity.
4. **`backtest/optimizer.py`** — `DEFAULT_PARAM_RANGES["anchored_vwap"]` (line 614 dict) per §6.

## 9. Bidirectional perps live — existing machinery to satisfy

Registering in `bidirectionalPerpsStrategies` flips on `AllowShorts`. The flip
path is **existing** code; the spec's obligation is to confirm the strategy
plays by its rules (CLAUDE.md "Bidirectional perps" + #1009):

- A flip (long→short / short→long) requires `closeFraction == 0`; `perpsLiveOrderSize`
  and its `runHyperliquidExecuteOrder` mirror must agree on `net_new_sz`.
- `perpsCloseActionSuppressesNewSL` gates arming a new reduce-only SL on the flip.
- `executePerpsSignalWithLeverage` caps `closeQty` at `pos.Quantity`.
- No custom close evaluator: SL/TP come from config defaults (manual/perps default
  `tiered_tp_atr_live` + SL@1.5×ATR). The strategy only emits entry signals.

These are **verification** items, not new code, unless a gap is found.

## 10. Test plan

- **`shared_strategies/open/test_anchored_vwap.py`** (new):
  - Construct a `DatetimeIndex` OHLCV fixture with a deliberate swing low, a
    decline below the resulting AVWAP, then a buffered reclaim held for
    `confirm_bars` → assert `signal == +1` on the exact completing bar and 0
    elsewhere. Mirror fixture for −1.
  - Assert **no signal before the first pivot confirms**.
  - Assert anchor **re-anchors** when a newer pivot forms (AVWAP value jumps to
    the new-anchor computation).
  - Div-by-zero: zero-volume window → AVWAP falls back to typical price, no crash.
  - Assert **actual** `avwap` values against a hand-computed prefix-sum (not just
    "signal nonzero").
- **Registry parity:** snapshot `open/{spot,futures}/strategies.py --list-json`
  **before** the change, diff **after** — Go `discoverStrategies` needs
  byte-identical output (run the FULL pytest suite, not isolated, per CLAUDE.md).
- **Look-ahead:** add the case to `test_backtester_lookahead.py` (§7).
- **Go:** `go -C scheduler test ./...` after `init.go` edits.

## 11. Backtest validation

```
uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap --symbol BTC/USDT --timeframe 1h --mode single
```

Vanilla bidirectional signal strategy → backtestable (unlike
`regime_directional_policy`). The perps-specific live flip path is **not**
backtested; signal generation (long+short) is. Entry ATR guard (50%-of-AvgCost)
applies. Sanity-check trade count and that both long and short legs fire.

## 12. Out of scope (tracked in #1017)

- Dual-anchor channel strategy (`anchored_vwap_channel`).
- Mean-reversion-to-line strategy (`anchored_vwap_reversion`).
- Optional momentum/regime gate on `anchored_vwap`.
- AVWAP-anchored close evaluator (`close/registry.py`).
