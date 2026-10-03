# Anchored VWAP Reversion (`anchored_vwap_reversion`) — Strategy Design

**Issue:** [#1170](https://github.com/richkuo/go-trader/issues/1170)
**Umbrella:** [#1017](https://github.com/richkuo/go-trader/issues/1017)
**Design parent:** [#1016](https://github.com/richkuo/go-trader/issues/1016) (`docs/superpowers/specs/2026-06-15-anchored-vwap-strategy-design.md`)
**Sibling:** [#1169](https://github.com/richkuo/go-trader/issues/1169) (`docs/superpowers/specs/2026-07-01-anchored-vwap-channel-strategy-design.md`)
**Date:** 2026-07-02
**Status:** Approved design — implemented alongside this spec.

## 1. Goal

Add the third AVWAP open strategy: mean-reversion **to** the single
pivot-anchored VWAP line. When price stretches at least `entry_atr_mult × ATR`
beyond the line and then snaps back inside that band without reclaiming the
line itself, enter toward the line: **long a downside stretch, short an upside
stretch**. The session-reset `vwap_reversion` trades the same idea against a
daily VWAP; this measures the stretch from a structural anchor (the most
recent confirmed swing pivot, #1016 §3) instead of a calendar reset.

## 2. Core design decisions (locked)

These are the decisions #1170 deferred to spec time, recorded per its
acceptance criteria:

| Decision | Choice |
|---|---|
| Band type | **ATR bands** — `entry_atr_mult × ATR` around the AVWAP, not `vwap_reversion`-style std-dev bands. Rationale: (1) family consistency — #1016 and #1169 both scale line distances by the inline ATR; (2) a std-dev band computed within the anchor window is degenerate right after a re-anchor (1–3 samples → zero/hair-trigger bands), while the rolling ATR is anchor-independent and always well-formed past warmup |
| Short leg | **Yes, symmetric** — a stretch `entry_atr_mult × ATR` ABOVE the line shorts back toward it. Joins `bidirectionalPerpsStrategies` and `LIVE_BIDIRECTIONAL_STRATEGIES` (`backtest/fee_audit.py`), same as both siblings |
| Trigger shape | **Stretch touch + buffered snap-back + zone hold** (not `vwap_reversion`'s raw band-cross): the trigger bar's extreme must reach the band, its close must recover back inside by `buffer_atr_mult × ATR`, and every window bar must close **between the band and the line** — the falling-knife guard the raw cross lacks, reusing #1016/#1169 clause vocabulary |
| Differentiation vs `anchored_vwap` (#1016) | Mutually exclusive per bar by construction: the flip strategy requires the close **beyond** the line (breach through); this strategy requires every window close strictly on the stretched side of the line (`close < avwap` for longs). A reclaim of the line during the hold kills the reversion fire — that regime belongs to the flip |
| Differentiation vs `anchored_vwap_channel` (#1169) | The channel fades touches of its **two** anchored lines (support/resistance = the AVWAPs themselves) from inside the channel; this fades an **ATR-measured stretch away from the one type-agnostic line**, entering beyond it. Same portfolio class (ranging mean reversion), disjoint trigger geometry; both ship pre-gated to composite ranging and operators tune overlap per asset |
| Regime gate default | **Pre-gated to composite ranging** `{ranging_quiet, ranging_volatile}` via `strategiesDefaultingToCompositeRangingGate` — stretch-fading is run over when the stretch is the start of a trend leg, the exact `atr_band_revert`/#1169 rationale; `ranging_directional` stays excluded |
| Exit | No custom close evaluator — SL/TP come from config close defaults; the opposite stretch signal flips/flattens (family posture, #1016 §2). The AVWAP-targeting close evaluator stays out of scope in #1017 |

## 3. Anchor and line

Identical to #1016 §3–4, reused verbatim: strict unique swing pivots
(`pivot_strength` bars each side), a pivot at *p* knowable only at `p + k`,
single type-agnostic anchor = most recent confirmed pivot, AVWAP via global
prefix sums (exact across re-anchors), zero-volume fallback to the bar's
typical price. Emitted columns `avwap`, `anchor_index`, `atr` match #1016.

## 4. Bands

```
lower_band[i] = avwap[i] − entry_atr_mult × ATR[i]
upper_band[i] = avwap[i] + entry_atr_mult × ATR[i]
```

Bands are per-bar (live AVWAP, live ATR) — not frozen at the trigger bar —
matching how #1169's hold clause tracks its live lines.

## 5. Trigger: stretch touch + buffered snap-back + zone hold

Notation mirrors #1016 §5: window `W = [b..n]` with `b = n − confirm_bars + 1`,
`buf = buffer_atr_mult × ATR[b]`.

**Validity at `b`:** anchor confirmed at `b` AND `b−1` (the freshness
reference needs a defined band too); `ATR[b]` not NaN.

**Long (+1) fires at bar `n`** iff all hold:
1. **Stretch touch:** `low[b] ≤ lower_band[b]` — the trigger bar reached at
   least `entry_atr_mult × ATR` below the line.
2. **Buffered snap-back:** `close[b] ≥ lower_band[b] + buf` — it closed
   decisively back inside the band.
3. **Zone hold:** `lower_band[k] ≤ close[k] < avwap[k]` for every `k ∈ W` —
   held inside the stretch zone: recovering, but the line (the target) not yet
   reached. A close back through the band (knife resumes) or above the line
   (move already over / flip territory) kills the fire.
4. **Fresh touch:** `low[b−1] > lower_band[b−1]` — the prior bar did not
   touch the band, so the fire is local and unique (#1016 §5 clause-3
   rationale: the signal column must be deterministic on its own, +1 only on
   the completing bar).

**Short (−1)** mirrors on the upper band: `high[b] ≥ upper_band[b]`,
`close[b] ≤ upper_band[b] − buf`, hold `avwap[k] < close[k] ≤ upper_band[k]`,
fresh `high[b−1] < upper_band[b−1]`. The two zone holds are disjoint
(`close < avwap` vs `close > avwap`), so a bar can never satisfy both; long is
evaluated first regardless — deterministic, documented.

NaN band values (ATR warmup inside the window) make every comparison false —
fail quiet, same posture as the siblings.

## 6. Parameters

| Param | Default | Range (optimizer) |
|---|---|---|
| `pivot_strength` | 5 | [3, 5, 8] |
| `entry_atr_mult` | 1.5 | [1.0, 1.5, 2.0] |
| `buffer_atr_mult` | 0.25 | [0.1, 0.25, 0.5] |
| `confirm_bars` | 2 | [1, 2, 3] |
| `atr_period` | 14 | (fixed; not swept) |

`entry_atr_mult` defaults to 1.5 by analogy with `vwap_reversion`'s
`entry_std = 1.5` (same "how stretched before fading" role, ATR-scaled).

## 7. Look-ahead safety

Identical guarantees to #1016 §7 (pivots knowable at `p + k`; prefix sums,
ATR, and every trigger clause read bars ≤ n; signal at N fills at N+1 open).
A dedicated `anchored_vwap_reversion` case joins
`backtest/tests/test_backtester_lookahead.py` with the same **non-vacuity**
(fixture must emit a +1 and a −1) and **sensitivity** (a forward-peeking
variant must fail the invariance assertion) checks the #1019 review demanded,
plus the #1169 full prefix sweep.

## 8. File-by-file wiring

1. **`shared_strategies/open/anchored_vwap_reversion.py`** (new) —
   `anchored_vwap_reversion_core(df, pivot_strength=5, entry_atr_mult=1.5,
   buffer_atr_mult=0.25, confirm_bars=2, atr_period=14)` returning the df plus
   `signal`, `avwap`, `anchor_index`, `atr`. ATR inlined byte-identical to
   `standard_atr` (same `shared_tools`-off-sys.path constraint as #1016).
2. **`shared_strategies/open/registry.py`** — import, `@register`
   (default platforms → spot + futures), and `PLATFORM_ORDER` entries directly
   after `anchored_vwap_channel` in both lists.
3. **`scheduler/init.go`** — `knownShortNames["anchored_vwap_reversion"] = "avwaprev"`;
   `bidirectionalPerpsStrategies` entry (shorts the upside stretch);
   `defaultSpotStrategies` / `defaultPerpsStrategies` / `defaultFuturesStrategies`
   entries after `anchored_vwap_channel`; `strategiesDefaultingToCompositeRangingGate`
   entry per §2.
4. **`backtest/optimizer.py`** — `DEFAULT_PARAM_RANGES["anchored_vwap_reversion"]` per §6.
5. **`backtest/fee_audit.py`** — `LIVE_BIDIRECTIONAL_STRATEGIES` entry (short leg).

## 9. Test plan

- **`shared_strategies/open/test_anchored_vwap_reversion.py`** (new),
  mirroring the sibling suites: empty/short-df guards; anchor confirmation
  indices; hand-computed AVWAP; zero-volume fallback; long stretch-snap-back
  fires exactly once on the completing bar; short mirrors; no signal before
  the first anchor; **no snap-back (close still beyond the band) → 0** (the
  falling-knife guard, non-vacuous); **line reclaimed during hold → 0** (the
  flip-territory guard, non-vacuous); buffer blocks a shallow snap-back;
  NaN-ATR warmup → 0; registry registration for spot + futures.
- **Look-ahead:** §7 case in `test_backtester_lookahead.py`.
- **Go:** `init_anchored_vwap_reversion_test.go` — wiring (short name,
  bidirectional membership, presence in all three default lists) and
  `generateConfig` composite-ranging-gate tests mirroring
  `init_anchored_vwap_channel_test.go`.
- **Registry parity:** `--list-json` snapshotted before the change for spot
  and futures; the post-change diff must be exactly the one new entry.

## 10. Backtest validation

Same two-leg protocol as #1016 §11 / #1169 §11 (the backtester measures one
direction per run; no `--config`, which would demand a close evaluator this
design doesn't have):

```
uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap_reversion --symbol BTC/USDT --timeframe 1h --mode single

uv run --no-sync python backtest/run_backtest.py \
  --strategy anchored_vwap_reversion --symbol BTC/USDT --timeframe 1h --mode single \
  --direction short
```

Sanity-check trade counts on each run (a stretch-fading strategy on trending
BTC should trade sparsely; zero trades across both legs would indicate a
wiring or gate bug, not selectivity).

## 11. Out of scope (still tracked in #1017)

- Optional momentum/regime gate on `anchored_vwap` itself.
- AVWAP-anchored close evaluator (`close/registry.py`) — including
  take-profit **at** the line, which this strategy approximates today via the
  opposite-signal flip and config close defaults.
