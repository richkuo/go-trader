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

A swing high at bar *p* requires `high[p]` to be the **strict** maximum of the
window `high[p-strength : p+strength+1]` (strict `>` against every other bar).
Strictness is deliberate: existing pivot code uses `==`/`max()` (e.g.
`liquidity_sweeps.py:21`), which flags every bar of a flat-top plateau as a
pivot; strict `>` flags none on a plateau, keeping anchor selection
deterministic — we simply don't anchor on a flat top, the next clean pivot
anchors instead. A swing low mirrors on `low` with strict `<`. Because
confirmation needs `strength` bars **on each side**, a pivot at *p* is only
*knowable* at bar `p + strength` — this is the look-ahead guarantee.

- `pivot_strength` (default **5**) — bars required on each side.
- At any bar *n*, the **active anchor** = bar index of the most recent pivot
  whose confirmation bar `p + strength ≤ n`. Type (high vs low) does **not**
  affect logic — a single line is anchored from that bar regardless.
- The anchor **re-anchors forward** each time a newer pivot confirms.
- Before the first pivot confirms there is **no anchor → no signal** (signal 0).
  Do **not** fall back to a session/global VWAP.
- **Re-anchor frequency** is governed entirely by `pivot_strength`: a wider
  window confirms fewer, more significant pivots and re-anchors less often.
  Because the type-agnostic "most recent pivot" alternates high/low, the line
  can be jumpy at low `pivot_strength` — but this is a **tuning** concern owned
  by the swept `pivot_strength` range, **not** a separate `min_anchor_bars`
  param (which would be redundant). Measure re-anchor cadence during backtest.
- **Warmup / guards:** while a bar has no confirmed anchor, or `ATR[n]` is NaN
  (pre-`atr_period` warmup), emit `signal = 0`. Add an empty / short-df early
  return mirroring `vwap_rejection_st.py:91-98` — return the df with `signal=0`
  and helper columns populated where possible — so smoke tests that iterate
  every registered strategy don't index out of range.

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

The window length is `confirm_bars` (default 2, inclusive of the breach bar:
`confirm_bars=2` = breach bar + 1 confirming bar). Define, for the window
`W = [n-confirm_bars+1, n]` ending at the current bar `n`:

**Long (+1) fires at bar `n`** iff **all** hold:
1. **Hold (bare line, no buffer):** `close[k] ≥ AVWAP[k]` for every `k ∈ W`.
2. **Buffered breach on the window's first bar:** at `b = n-confirm_bars+1`,
   `close[b] ≥ AVWAP[b] + buffer[b]`. The buffer applies to the **breach bar
   only** — hold bars use the bare line.
3. **Fresh crossing:** the bar immediately before the window was below the line,
   `close[b-1] < AVWAP[b-1]`.

Clause 3 is the multi-bar analogue of `vwap_reversion`'s `shift(1)` cross idiom
(`registry.py:818`): it makes the fire **local and unique**, so the column is
`+1` only on the completing bar and `0` elsewhere — *without* relying on the
backtester/live position dedup downstream (that dedup exists, but the signal
column must be deterministic on its own, per §10's "0 elsewhere" assertion).
This is a transition condition, **not** a stateful "must return below after a
fire" rule.

**Short (−1)** mirrors with `≤`, `AVWAP − buffer`, and `close[b-1] > AVWAP[b-1]`.

**Reversal/exit:** the opposite flip emits the opposite signal. On bidirectional
perps this flips the position; on spot / long-only backtest the −1 flattens.

### Whipsaw / state notes
- A new anchor confirming inside a window: each bar `k` compares `close[k]` to
  *its own* live-anchor `AVWAP[k]`, so the window naturally re-references the new
  line, and a fresh crossing (clause 3) of the new line must then form. Intended
  — the level the market respects has moved.
- Where the window or its `b-1` reference extends before the first valid bar
  (anchor / ATR warmup), no signal (clauses can't be evaluated → 0).

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

The backtester measures **one direction per run**. On the default long/flat path
`signal=-1` only flattens a long — it **never opens a short**
(`backtester.py:1800-1806`). Shorts open only under `direction="short"`
(`:1063`, `:1813`). So validate the two legs with **two separate runs**, and do
**not** use `--config`: the live config maps `allow_shorts → direction="both"`
(`run_backtest.py:209-212`), which the plain path rejects (`backtester.py:1055-1062`),
and a `--config` run with no close evaluator (this design has none, §2)
hard-fails at `run_backtest.py:364-373`.

```
# Long leg (default direction):
uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap --symbol BTC/USDT --timeframe 1h --mode single

# Short leg (explicit):
uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap --symbol BTC/USDT --timeframe 1h --mode single \
  --direction short
```

This is backtestable (unlike `regime_directional_policy`): each run exercises one
leg's **signal generation**. The perps-specific live flip path is **not**
backtested — it is validated by unit tests + paper (§9). Entry ATR guard
(50%-of-AvgCost) applies. Sanity-check trade counts on each run.

## 12. Out of scope (tracked in #1017)

- Dual-anchor channel strategy (`anchored_vwap_channel`).
- Mean-reversion-to-line strategy (`anchored_vwap_reversion`).
- Optional momentum/regime gate on `anchored_vwap`.
- AVWAP-anchored close evaluator (`close/registry.py`).
