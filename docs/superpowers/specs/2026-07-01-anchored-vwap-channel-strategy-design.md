# Anchored VWAP Channel (`anchored_vwap_channel`) — Strategy Design

**Issue:** [#1169](https://github.com/richkuo/go-trader/issues/1169)
**Umbrella:** [#1017](https://github.com/richkuo/go-trader/issues/1017)
**Design parent:** [#1016](https://github.com/richkuo/go-trader/issues/1016) (`docs/superpowers/specs/2026-06-15-anchored-vwap-strategy-design.md`)
**Date:** 2026-07-01
**Status:** Approved design — implemented alongside this spec.

## 1. Goal

Add a second AVWAP open strategy that maintains **two** anchored VWAPs — one
anchored at the most recent **confirmed swing LOW** (the support line), one at
the most recent **confirmed swing HIGH** (the resistance line) — and trades the
channel between them: **long a buffered bounce off the lower line, short a
buffered rejection off the upper line**. This is range-edge mean reversion on
volume-weighted structure, deliberately complementary to `anchored_vwap`
(#1016), which trades a **breach through** its single line: the channel
strategy only ever fades a touch that *holds*; a close beyond either line
simply produces no signal here (that regime belongs to the flip strategy).

## 2. Core design decisions (locked)

These are the decisions #1169 deferred to spec time, recorded per its
acceptance criteria:

| Decision | Choice |
|---|---|
| Line trigger | **Touch + buffered reclaim** — the trigger bar's extreme (low/high) must touch or penetrate the line, and its close must recover past the line by `buffer_atr_mult × ATR`; then an N-bar bare-line hold (reusing `buffer_atr_mult` and `confirm_bars` semantics from #1016, re-aimed at bounces instead of breaches) |
| Fire-once clause | **Fresh touch** — the bar before the trigger bar must NOT have touched the line (the multi-bar analogue of #1016's fresh-crossing clause, keyed on the touch extreme rather than the close) |
| Line inversion | **No-trade** — when `support ≥ resistance` (the low-anchored line has climbed above the high-anchored line) the channel is structurally meaningless; emit 0. No re-labeling, no swap: fail quiet until pivots re-establish order |
| Minimum channel width | **ATR-mult gate** — require `(resistance − support) ≥ min_width_atr_mult × ATR` on the trigger bar (default **1.5**); thinner channels leave no room between entry and the opposite edge |
| Channel-validity evaluation bar | The **trigger bar** `b` (window start). Hold bars only re-check their own line |
| Direction / platform | **Bidirectional perps live** + spot + backtest (same as #1016); the short leg joins `bidirectionalPerpsStrategies` |
| Exit | No custom close evaluator — SL/TP come from config close defaults, and the opposite edge's signal flips/flattens (same posture as #1016 §2) |
| Regime gate default | **Pre-gated to composite ranging** `{ranging_quiet, ranging_volatile}` via `strategiesDefaultingToCompositeRangingGate` — see §7 |

## 3. Anchors: typed swing pivots

`anchored_vwap` tracks the most recent confirmed pivot **type-agnostically**
(§3 of the parent spec). The channel variant tracks the two types
**separately**, with identical strictness and the identical look-ahead
guarantee:

- A swing high at bar *p* requires `high[p]` to be the **strict, unique**
  maximum of `high[p−k : p+k+1]`; a swing low mirrors on `low` (strict, unique
  minimum). Flat-top/flat-bottom plateaus confirm nothing (parent §3).
- A pivot at *p* is knowable only at bar `p + k` (`k = pivot_strength`).
- `anchor_high_index[n]` = bar index of the most recent swing HIGH whose
  confirmation bar ≤ n; `anchor_low_index[n]` mirrors for swing LOWs. Each is
  −1 until its first pivot confirms.
- **Both** anchors must exist before any signal can fire. Until then: 0.

## 4. The two AVWAPs

Same global-prefix-sum computation as parent §4, evaluated twice:

```
support[n]    = (Σ tp·vol [anchor_low..n])  / (Σ vol [anchor_low..n])
resistance[n] = (Σ tp·vol [anchor_high..n]) / (Σ vol [anchor_high..n])
```

with `tp = (high + low + close) / 3` and the same zero-volume fallback to
`tp[n]`. Emitted as `avwap_support` / `avwap_resistance` columns (NaN before
the respective anchor confirms), alongside `anchor_low_index`,
`anchor_high_index`, and `atr`.

## 5. Trigger: touch + buffered reclaim + N-bar hold

All notation mirrors parent §5: window `W = [b..n]` with `b = n − confirm_bars + 1`,
`buf = buffer_atr_mult × ATR[b]`.

**Channel validity at `b`** (both signals require it):
1. Both anchors confirmed at `b` and at `b−1` (the freshness reference needs
   defined lines too).
2. `ATR[b]` is not NaN.
3. Not inverted: `support[b] < resistance[b]`.
4. Wide enough: `resistance[b] − support[b] ≥ min_width_atr_mult × ATR[b]`.

**Long (+1) fires at bar `n`** iff all hold:
1. **Touch:** `low[b] ≤ support[b]` — the trigger bar dipped to/through the
   support line.
2. **Buffered reclaim:** `close[b] ≥ support[b] + buf` — it closed decisively
   back above.
3. **Hold (bare line):** `close[k] ≥ support[k]` for every `k ∈ W`.
4. **Fresh touch:** `low[b−1] > support[b−1]` — the prior bar did not touch,
   so the fire is local and unique (parent §5 clause-3 rationale: the signal
   column must be deterministic on its own, +1 only on the completing bar).

**Short (−1)** mirrors on the resistance line: `high[b] ≥ resistance[b]`,
`close[b] ≤ resistance[b] − buf`, hold `close[k] ≤ resistance[k]`, fresh
`high[b−1] < resistance[b−1]`. Long is evaluated first; a bar satisfying both
(possible only in a degenerate wide-bar case that the width gate makes rare)
resolves long — deterministic, documented.

Notes:
- A close **through** a line fails the hold clause here by construction —
  breaches are `anchored_vwap`'s trade, not this one's (§1).
- Re-anchors inside a window behave exactly as parent §5: each bar compares
  against its own live lines.

## 6. Parameters

| Param | Default | Range (optimizer) |
|---|---|---|
| `pivot_strength` | 5 | [3, 5, 8] |
| `buffer_atr_mult` | 0.25 | [0.1, 0.25, 0.5] |
| `confirm_bars` | 2 | [1, 2, 3] |
| `min_width_atr_mult` | 1.5 | [1.0, 1.5, 2.5] |
| `atr_period` | 14 | (fixed; not swept) |

## 7. Regime gate default (composite ranging)

The channel trade is definitionally ranging-structure: it fades both edges and
is run over when the range resolves into a trend. That is the exact rationale
`strategiesDefaultingToCompositeRangingGate` encodes for `atr_band_revert`
(`scheduler/init.go:115`), so `anchored_vwap_channel` joins that map with the
same labels `{ranging_quiet, ranging_volatile}` — `ranging_directional` stays
excluded (directional pressure inside a range is the breakout precursor,
mean reversion's worst case). This is a **wizard default** only: it stamps
`allowed_regimes` on generated configs (plus the global composite "medium"
window when absent); operators widen/narrow post-init. `anchored_vwap` itself
stays ungated — its S/R-flip leg is breakout-following, not range-fading.

## 8. Look-ahead safety

Identical guarantees to parent §7 (pivots knowable at `p + k`; prefix sums,
ATR, and every trigger clause read bars ≤ n; signal at N fills at N+1 open).
A dedicated `anchored_vwap_channel` case joins
`backtest/tests/test_backtester_lookahead.py` with the same **non-vacuity**
(fixture must emit a +1 and a −1) and **sensitivity** (a forward-peeking
variant must fail the invariance assertion) checks the #1019 review demanded
of the parent's test.

## 9. File-by-file wiring

1. **`shared_strategies/open/anchored_vwap_channel.py`** (new) —
   `anchored_vwap_channel_core(df, pivot_strength=5, buffer_atr_mult=0.25,
   confirm_bars=2, min_width_atr_mult=1.5, atr_period=14)` returning the df
   plus `signal`, `avwap_support`, `avwap_resistance`, `anchor_low_index`,
   `anchor_high_index`, `atr`. ATR inlined byte-identical to `standard_atr`
   (same `shared_tools`-off-sys.path constraint as parent §5).
2. **`shared_strategies/open/registry.py`** — import, `@register`
   (default platforms → spot + futures), and `PLATFORM_ORDER` entries directly
   after `anchored_vwap` in both lists.
3. **`scheduler/init.go`** — `knownShortNames["anchored_vwap_channel"] = "avwapch"`;
   `bidirectionalPerpsStrategies` entry (shorts the upper line);
   `defaultSpotStrategies` / `defaultPerpsStrategies` / `defaultFuturesStrategies`
   entries after `anchored_vwap`; `strategiesDefaultingToCompositeRangingGate`
   entry per §7.
4. **`backtest/optimizer.py`** — `DEFAULT_PARAM_RANGES["anchored_vwap_channel"]` per §6.

## 10. Test plan

- **`shared_strategies/open/test_anchored_vwap_channel.py`** (new), mirroring
  the parent suite: empty/short-df guards; typed-anchor confirmation indices;
  hand-computed support AND resistance AVWAPs; zero-volume fallback; long
  bounce fires exactly once on the completing bar; short rejection mirrors;
  no signal before both anchors; **inversion → 0** (with a non-vacuous
  would-be touch inside the inverted region); **width gate → 0**; registry
  registration for spot + futures.
- **Look-ahead:** §8 case in `test_backtester_lookahead.py`.
- **Go:** `init_anchored_vwap_channel_test.go` — wiring (short name,
  bidirectional membership, presence in all three default lists) and
  `generateConfig` composite-ranging-gate tests mirroring
  `init_atr_band_revert_test.go`.
- **Registry parity:** `--list-json` snapshotted before the change for spot
  and futures; the post-change diff must be exactly the one new entry.

## 11. Backtest validation

Same two-leg protocol as parent §11 (the backtester measures one direction per
run; no `--config`, which would demand a close evaluator this design doesn't
have):

```
uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap_channel --symbol BTC/USDT --timeframe 1h --mode single

uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap_channel --symbol BTC/USDT --timeframe 1h --mode single \
  --direction short
```

Sanity-check trade counts on each run (a range-edge strategy on trending BTC
should trade sparsely; zero trades across both legs would indicate a wiring or
gate bug, not selectivity).

## 12. Out of scope (still tracked in #1017)

- Mean-reversion-to-line strategy (`anchored_vwap_reversion`).
- Optional momentum/regime gate on `anchored_vwap` itself.
- AVWAP-anchored close evaluator (`close/registry.py`).
- Channel-interior targets (e.g. take-profit at mid-channel) — close-evaluator
  territory, not open-signal territory.
