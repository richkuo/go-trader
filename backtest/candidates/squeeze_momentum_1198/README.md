# squeeze_momentum (spot) regime-gate re-run — entry frozen (#1198, M4)

Application of **M4 (regime gating / regime-profile allocation)** to the
registry's top-ranked entry (#956: +47.9pts vs B&H, -58.5% worst max DD on the
default open-signal-as-close stack), re-running the exact #1165 breakout
drivers against the strategy #983 named as the next candidate. #983 concluded
squeeze_momentum's **-58.5% worst DD could not be fixed by close-stack work**
(nothing beat baseline even in-sample) — the same "DD is regime exposure, not
exit quality" conclusion #984 reached for breakout. #1165 (PR #1194) then
showed the M4 entry-gate arm *works* on breakout: composite
`trending_up_clean` p21 cut worst DD -52.2% → -23.2% without amputating the
edge. This directory asks whether the same mechanism separates
squeeze_momentum's DD bleed from its edge — judged through the same M1 protocol
+ mandatory stitched audit headline + fee-drag gate. Entry logic, params, and
the close stack are frozen throughout (registry defaults, open-signal-as-close,
no SL/TP — the only protocol IS+OOS pass per #983/#984).

**Result: negative — the M4 gate does NOT separate squeeze_momentum's DD from
its edge the way #1165 did for breakout.** Two findings:

1. **The #1165 mechanism is breakout-specific.** The composite
   `trending_up_clean` gate that won for breakout *amputates* squeeze_momentum:
   **1 trade** at period 14, **0** at 21/28 across all six datasets.
   squeeze_momentum fires on volatility EXPANSION out of a coil (the squeeze
   releasing); the composite `clean` label (efficient, low-noise trend) rarely
   coincides with that release bar, so gating to clean-only removes ~every
   entry. The quality split (clean vs choppy) that isolated breakout's bleed
   does not exist for squeeze — a direct answer to the issue's step-3 question.
2. **squeeze's DD lever is the ADX bear-block, but it only shaves the tail.**
   The best gate (`adx_not_down` / `m4_bear_off`) cuts stitched worst DD
   **-58.5% → -51.6%** — a 6.8pp trim of a still-catastrophic tail, not
   breakout's -52.2% → -23.2% halving — while raising return (net +1.1% →
   +10.1%) by dropping net-losing bear entries. On the M1 protocol every gate
   *keeps* the IS+OOS pass and *matches* baseline's held-out record (1/3), but
   **none improves it** (breakout's p21 flipped a held-out window; no squeeze
   gate flips any). The bleed and the edge stay entangled.

The gate is a mild return/Sharpe improver, but the -58% DD #983 flagged is only
marginally reduced and no better-separated than the close stack #983 already
rejected. Per #995 step 4 a negative result is documented, not deleted, and no
live config changes.

**Structural notes (read before comparing rows).**

- **Registry is SPOT, not futures.** Unlike breakout (futures-only,
  `platforms=("futures",)`, why #1165 pinned the futures registry),
  squeeze_momentum is registered for BOTH spot and futures with no variant
  override, so its signal is identical in either registry. The drivers pin spot
  to keep the comparison valid: the #983 baseline this reproduces (-58.5% worst
  DD) and the M5 fee audit (`docs/research/fee-audit-m5.md`: gross -0.29%, net
  -3.69% on spot) were both run on spot. The issue's "futures registration"
  phrasing is inherited from the breakout template, where futures was the only
  option.
- The regime gate blocks **entries only** — closes always execute — so the
  frozen open-signal exit survives under every Arm A candidate. The #998 M4
  switch (Arm B) commits **only from flat** (confirm_bars 2), so a position
  opened under the "on" profile keeps its opening exit until it closes; the
  "off" profile (`kc_mult: 100.0`) zeroes new entries, not exits.
- **The "off" profile emits zero entries by construction.** squeeze_momentum
  fires when the Bollinger band, having been *inside* the Keltner channel (the
  squeeze), pops back *outside* it (the release). `kc_mult: 100.0` makes the
  Keltner channel so wide the Bollinger band is always inside — the squeeze is
  "on" on every bar and never transitions off, so no release ever fires
  (verified: 0 trades across all six IS datasets). The regime response is
  carried by the position (a flat-only switch), not the entry list — the analog
  of breakout's unreachable expansion multiple.
- **`m4_bear_selective`'s "off" set tightens the coil instead of zeroing it**
  (`kc_mult: 1.3`, `mom_lookback: 16` — a narrower Keltner channel demands a
  tighter squeeze, a longer momentum window demands more sustained momentum;
  standalone this keeps 41 of the 83 baseline IS entries, a genuine middle
  ground, not a near-off).

Every committed artifact regenerates from the repo root with the cache DB
present (`shared_tools/trading_bot.db`) via one of the four committed drivers
(`sweep_regime_gates.py`, `validate_shortlist.py`, `audit_headline.py`,
`fee_drag.py`) — each section below states its exact command. Candidate regime
state (`allowed_regimes` / `regime_windows_spec` / `profile_allocation`) is
threaded into every leg by `driver_common.candidate_leg_kwargs` (unit-tested in
`backtest/tests/test_squeeze_momentum_1198_drivers.py`); a driver that dropped
it would silently score the ungated entry. Protocol windows (`is`, `oos`,
held-out) are date-pinned; the continuous audit window ends at the latest cache
by default, so `audit_headline.py` records the effective per-dataset data range
in its artifact — diff that against the committed file before comparing numbers
regenerated under a fresher cache (committed `effective_range` pins last bars
2026-06-04 → 2026-06-12 per dataset, the same cache as `breakout_1165/`).

## Step 1 — M4 screen on IS (selection window only)

Both arms, one screen: Arm A gates entries by regime label (`allowed_regimes`;
legacy ADX and composite 9-state classifiers via `regime_windows_spec`,
#1058/#1124), Arm B switches the entry's param set by regime (#998 two-profile
allocation, ADX and composite switch windows).

```
uv run --no-sync python backtest/candidates/squeeze_momentum_1198/sweep_regime_gates.py \
    --window is --json backtest/candidates/squeeze_momentum_1198/sweep_is.json
```

| IS rank (mean DDadj) | DDadj | Sharpe | ret% | worst DD% | #T |
|---|---:|---:|---:|---:|---:|
| `m4_bear_selective` (ADX switch; off = tighter coil kc 1.3 / mom 16) | +0.442 | +0.14 | +4.30 | -39.6 | 70 |
| `m4_bear_off` (ADX switch; off = no entries) | +0.385 | +0.11 | +3.18 | -39.6 | 70 |
| `adx_not_down` (ADX gate `trending_up`+`ranging`) | +0.329 | +0.15 | +5.87 | -39.6 | 74 |
| `comp_up_family` (composite `trending_up_*`) | +0.276 | +0.12 | +1.97 | -40.0 | 77 |
| `comp_up_plus_dir_up` | +0.055 | -0.00 | -1.47 | -46.8 | 77 |
| baseline (ungated) / `comp_not_down` / `m4_comp_bear_off` | -0.005 | -0.07 | -3.14 | -46.8 | 83 |
| `comp_not_down_calm` | -0.048 | -0.14 | -5.88 | -46.8 | 79 |
| `comp_up_clean` (composite `trending_up_clean`) | -0.111 | -0.26 | -0.22 | -2.0 | **1** |
| `m4_trend_only` | -0.437 | -0.75 | -9.29 | -36.9 | 23 |
| `adx_up` | -0.445 | -0.77 | -10.59 | -36.9 | 32 |
| `adx_trend_only` | -0.613 | -0.90 | -18.88 | -43.3 | 47 |

The lever is the **ADX bear-block**: every candidate that turns off
`trending_down` ADX entries but keeps `ranging` (`adx_not_down`, `m4_bear_off`,
`m4_bear_selective`) lands at the same worst DD (-39.6%) on ~70 trades, ~3× the
baseline DDadj. This is the *opposite* of breakout, where "block down / not
down" sat at baseline (breakout's TR-expansion bars label as up/trending even
in bear tapes). The standout breakout gate, `comp_up_clean`, is the **worst
usable row here** — 1 trade — because squeeze's coil-release entries almost
never land on a `trending_up_clean` bar (see verdict). Gates that also drop
`ranging` (`adx_up`, `m4_trend_only`, `adx_trend_only`) go sharply negative:
squeeze needs its range-bound entries.

## Step 2 — plateau checks (M1 step 6, IS only)

```
uv run --no-sync python backtest/candidates/squeeze_momentum_1198/sweep_regime_gates.py \
    --window is --grid plateau-only \
    --plateau-allowed trending_up,ranging \
    --comp-plateau-allowed trending_up_clean \
    --json backtest/candidates/squeeze_momentum_1198/plateau_is.json
```

- **ADX gate threshold, `adx_not_down` (`trending_up`+`ranging`)**: t15 +0.174,
  **t20 +0.329**, t25 +0.300, t30 +0.058 — a genuine shelf around 20–25
  (default t20 is the peak), falling off at both ends, not a single-cell spike.
  The ADX arm carries.
- **Composite classifier period, `trending_up_clean`**: p10 -0.128 (11 trades),
  p14 -0.111 (1 trade), **p21 0.000 (0 trades), p28 0.000 (0 trades)** — the
  clean gate produces essentially no squeeze entries at any period. No period
  rescues it; the composite `clean` arm does not exist for this strategy.

## Step 3 — judge through the M1 protocol (#994)

```
uv run --no-sync python backtest/candidates/squeeze_momentum_1198/validate_shortlist.py \
    --json backtest/candidates/squeeze_momentum_1198/validation.json
```

(Same functions/harness as `eval_windows.py --candidate-json <c>.json
--registry spot`, one process so the incumbent bars compute once.
`comp_up_clean` is dropped from the shortlist — 1 trade is degenerate, nothing
to judge; its amputation is the step-1/step-2 finding.)

| Candidate | IS | OOS (judged) | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| baseline | PASS | **PASS** | PASS | FAIL | FAIL | 1/3 |
| `adx_not_down` | PASS | **PASS** | PASS | FAIL | FAIL | 1/3 |
| `m4_bear_off` | PASS | **PASS** | PASS | FAIL | FAIL | 1/3 |
| `m4_bear_selective` | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |
| `comp_up_family` | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |

- **Every candidate keeps the judged OOS pass** — and, unlike the #983 close
  stacks (which passed protocol yet collapsed on the stitched window), the ADX
  gates also *improve* the stitched frame (step 4). But **no gate improves
  baseline's held-out record**: `adx_not_down` / `m4_bear_off` merely *match* it
  (1/3, the same 2023 pass), and the composite / selective arms are *worse*
  (0/3, losing the 2023 pass). This is the crux of the negative: breakout's
  `comp_up_clean_p21` flipped a held-out window (0/3 → 1/3); no squeeze gate
  flips any.

## Step 4 — the mandatory stitched continuous-window headline

```
uv run --no-sync python backtest/candidates/squeeze_momentum_1198/audit_headline.py \
    --candidates baseline.json,adx_not_down.json,m4_bear_off.json,m4_bear_selective.json,comp_up_family.json \
    --json backtest/candidates/squeeze_momentum_1198/audit_window_headline.json
```

The #983/#984 lesson applied (the segmented protocol wins must survive the
stitch):

| | mean Sharpe | mean ret | vs B&H | worst DD | #T |
|---|---:|---:|---:|---:|---:|
| baseline | +0.07 | +1.1% | +45.9pts | **-58.5%** | 130 |
| `adx_not_down` | +0.21 | +10.1% | +54.9pts | -51.6% | 115 |
| `m4_bear_off` | +0.21 | +8.2% | +53.0pts | -51.6% | 109 |
| `m4_bear_selective` | +0.18 | +6.8% | +51.7pts | **-59.6%** | 113 |
| `comp_up_family` | +0.23 | +7.5% | +52.4pts | -54.0% | 119 |

Baseline reproduces #983's -58.5% worst DD almost exactly (-58.46%). The best
gate trims it to -51.6% — a 6.8pp cut of a tail that is still >50%, nowhere near
breakout's -52.2% → -23.2%. `m4_bear_selective` makes the tail *worse*
(-59.6%): tightening the bear-coil re-enters into the same drawdown it was
meant to skip. The #1165 bar — "worst DD *materially* better than -58.5% while
keeping the edge" — is not met; the edge is kept (and improved), but the DD is
not separated from it.

## Step 5 — fee-drag gate (squeeze is fee-negative per the M5 audit)

```
uv run --no-sync python backtest/candidates/squeeze_momentum_1198/fee_drag.py \
    --candidates baseline.json,adx_not_down.json,m4_bear_off.json,m4_bear_selective.json,comp_up_family.json \
    --json backtest/candidates/squeeze_momentum_1198/fee_drag_shortlist.json
```

| | gross ret | net ret | drag | #T | T/yr |
|---|---:|---:|---:|---:|---:|
| baseline | +8.9% | +1.1% | 7.8pp | 130 | 21.9 |
| `adx_not_down` | +17.6% | +10.1% | 7.5pp | 115 | 19.3 |
| `m4_bear_off` | +14.8% | +8.2% | 6.6pp | 109 | 18.3 |
| `m4_bear_selective` | +13.5% | +6.8% | 6.6pp | 113 | 19.0 |
| `comp_up_family` | +14.9% | +7.5% | 7.4pp | 119 | 20.0 |

Like #1165 and unlike the #983 close stacks, the gate *removes* entries, so drag
falls with trade count (7.8pp → 6.6pp) and gross rises (the removed trades were
net losers). Note these continuous-window figures (baseline net +1.1%) differ
from the M5 audit's per-leg means (gross -0.29%, net -3.69%): M5 averages
per-leg over the *segmented* is+oos windows, this driver compounds over the
*continuous* span — same divergence `breakout_1165/` records. The M5 finding
(squeeze is fee-negative per-leg) is the reason the gate matters; the gate
helps the fee side, but the fee side was never the -58% DD problem.

## Verdict

1. **The M4 regime gate does not separate squeeze_momentum's DD from its edge.**
   Best-case stitched worst DD -58.5% → -51.6% is a marginal trim of a
   still-catastrophic tail, no gate improves baseline's held-out record, and
   `m4_bear_selective` makes the DD worse. This confirms and extends #983: for
   squeeze the -58% DD is regime exposure that neither a close stack (#983) nor
   an entry gate (this study) can cleanly remove — the bleed and the edge are
   entangled, unlike breakout where #1165 pulled them apart.
2. **The #1165 mechanism is breakout-specific.** The composite
   `trending_up_clean` quality split amputates squeeze (1 trade at p14, 0 at
   21/28): squeeze's coil-release entries fire on volatility expansion, which
   the `clean` label excludes. The lever that *does* move for squeeze is the
   ADX bear-block (`adx_not_down` / `m4_bear_off`), which improves return/Sharpe
   by dropping net-losing bear entries but leaves the tail near baseline. The
   naive "block bear entries" idea helps here (opposite of breakout) — just not
   enough to fix the DD.
3. **Caveats, stated plainly**: selection was IS-only (M1 step 5 held — the
   judged OOS + held-out windows were looked at once per shortlist member); the
   ADX shelf is real (t20–t25) but the whole arm's DD gain is small; all
   evidence is one asset class (BTC/ETH/SOL × 1h/4h) on the audit frame; the
   `m4_bear_selective` "off" param set is one reasonable tightening, not an
   optimized one.
4. **This changes no live config.** A negative result ships as evidence, not a
   deployment (#995 step 4 symmetry): documented, not deleted. The squeeze
   deprecate signal from the M5 fee audit stands unchanged.
