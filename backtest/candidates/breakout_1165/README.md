# breakout (futures) regime-gate work — entry frozen (#1165, M4)

Application of **M4 (regime gating / regime-profile allocation)** to the
registry's second-ranked entry (#956: +47.5pts vs B&H, -52.2% worst max DD on
the default open-signal-as-close stack), the follow-on the #984 verdict named:
the DD is regime exposure, not exit quality, so the sweep is over **when the
entry may fire**, not how it exits. Entry logic, params, and the close stack
are frozen throughout (registry defaults, open-signal-as-close, no SL/TP —
the only protocol IS+OOS pass per #984); same two load-bearing deltas as
`breakout_984/`: every driver loads the **futures registry** and pins
**`direction="long"`** (#996).

**Result: positive — the first arm in the #983/#984/#985/#1165 series to
clear the bar.** The composite `trending_up_clean` gate at classifier period
21 keeps the protocol IS+OOS pass (the #984 failure point: every close stack
lost the judged OOS window), flips 2025H1 to PASS (held-out 1/3 vs baseline
0/3), and on the mandatory stitched audit frame cuts worst DD **-52.2% →
-23.2%** while *raising* vs-B&H **+43.7 → +56.3pts** — with fee drag down
14.3pp → 3.8pp on 67 trades vs 260. The #984 inversion (bull-year fixes
amputate the crash edge) does not bite: the gate skips the bear-tape
re-entries instead of banking trends early.

**Structural notes (read before comparing rows).**

- The regime gate blocks **entries only** — closes always execute — so the
  frozen breakdown exit survives under every Arm A candidate
  (`backtester.py` regime-gate block; the #984 `atr_stop` control showed
  replacing that exit is strictly harmful).
- The #998 M4 switch (Arm B) commits **only from flat** (confirm_bars 2), so
  a position opened under the "on" profile keeps its opening profile's
  breakdown exit until it closes; the "off" profile
  (`atr_multiplier: 100.0`) zeroes new entries, not exits.
- **The naive "block bear entries" hypothesis is refuted** (the caution the
  issue flagged, in the opposite direction): `adx_not_down` /
  `comp_not_down` sit at baseline (breakout's TR-expansion entry bars label
  as *trending/up* even in bear tapes — dead-cat rallies — so "not down"
  sets block almost nothing; `comp_up_family` blocks literally nothing at
  period 14, 148/148 trades). What separates the bleed from the edge is
  trend **quality**: the `clean` vs `choppy` efficiency split, not up vs
  down direction.

Every committed artifact regenerates from the repo root with the cache DB
present (`shared_tools/trading_bot.db`) via one of the four committed drivers
(`sweep_regime_gates.py`, `validate_shortlist.py`, `audit_headline.py`,
`fee_drag.py`) — each section below states its exact command. Candidate
regime state (`allowed_regimes` / `regime_windows_spec` /
`profile_allocation`) is threaded into every leg by
`driver_common.candidate_leg_kwargs` (unit-tested in
`backtest/tests/test_breakout_1165_drivers.py`) — unlike the #984 twins, a
driver here that dropped it would silently score the ungated entry. Protocol
windows (`is`, `oos`, held-out) are date-pinned; the continuous audit window
ends at the latest cache by default, so `audit_headline.py` records the
effective per-dataset data range in its artifact — diff that against the
committed file before comparing numbers regenerated under a fresher cache
(committed `effective_range` pins last bars 2026-06-04 → 2026-06-12 per
dataset, the same cache as `breakout_984/`).

## Step 1 — M4 screen on IS (selection window only)

Both arms, one screen: Arm A gates entries by regime label
(`allowed_regimes`; legacy ADX and composite 9-state classifiers via
`regime_windows_spec`, #1058/#1124 — the `comp_not_down` sets use the bare
`ranging_directional` per bare-covers-subs), Arm B switches the entry's
param set by regime (#998 two-profile allocation, ADX and composite switch
windows).

```
uv run --no-sync python backtest/candidates/breakout_1165/sweep_regime_gates.py \
    --window is --json backtest/candidates/breakout_1165/sweep_is.json
```

| IS rank (mean DDadj) | DDadj | Sharpe | ret% | worst DD% | #T |
|---|---:|---:|---:|---:|---:|
| `comp_up_clean` (composite p14, `trending_up_clean`) | +0.658 | +0.56 | +11.08 | -32.7 | 45 |
| `m4_bear_selective` (ADX switch; off = lookback 55 / mult 2.5) | +0.617 | +0.35 | +9.41 | -39.6 | 118 |
| `m4_bear_off` (ADX switch; off = no entries) | +0.552 | +0.25 | +7.25 | -39.6 | 125 |
| `m4_trend_only` | +0.391 | +0.41 | +8.99 | -29.1 | 79 |
| `m4_comp_bear_off` | +0.361 | -0.04 | +2.58 | -52.2 | 147 |
| `adx_not_down` | +0.335 | +0.04 | +3.61 | -46.0 | 144 |
| `adx_up` | +0.317 | +0.13 | +5.41 | -37.0 | 112 |
| `adx_trend_only` | +0.315 | +0.03 | +4.11 | -44.2 | 117 |
| baseline (ungated) | +0.308 | -0.06 | +1.94 | -52.2 | 148 |
| `comp_not_down` | +0.308 | -0.06 | +1.94 | -52.2 | 148 |
| `comp_up_family` / `comp_not_down_calm` / `comp_up_plus_dir_up` | +0.280 | -0.10 | +1.14 | -52.2 | 148 |

The composite `clean`-only gate is the standout screen (3× baseline DDadj on
30% of the trades); the ADX-window M4 switches beat baseline but keep most of
the churn; every "block down / not-down" variant is baseline-flat (see
structural note).

## Step 2 — plateau checks (M1 step 6, IS only)

```
uv run --no-sync python backtest/candidates/breakout_1165/sweep_regime_gates.py \
    --window is --grid plateau-only \
    --comp-plateau-allowed trending_up_clean --plateau-allowed trending_up \
    --json backtest/candidates/breakout_1165/plateau_is.json
```

- **Composite classifier period, `trending_up_clean` gate**: p10 +0.272,
  p14 +0.658, **p21 +1.172**, p28 +0.765 — a 14–28 shelf all above
  baseline's +0.308 (p10 falls off), not a single-cell spike; p21 is the
  peak (Sharpe +0.69, ret +13.76%, worst DD -23.2%, 36 trades).
- **ADX gate threshold, `trending_up`**: t15 +0.259, t20 +0.317, t25 +0.414,
  t30 -0.084 — weak and non-monotonic everywhere; the ADX arm is not
  carried past the screen.

## Step 3 — judge through the M1 protocol (#994)

```
uv run --no-sync python backtest/candidates/breakout_1165/validate_shortlist.py \
    --json backtest/candidates/breakout_1165/validation.json
```

(Same functions/harness as `eval_windows.py --candidate-json <c>.json
--registry futures`, one process so the incumbent bars compute once.)

| Candidate | IS | OOS (judged) | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| baseline | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |
| `comp_up_clean` | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |
| `comp_up_clean_p21` | PASS | **PASS** | FAIL | FAIL | **PASS** | 1/3 |
| `m4_bear_off` | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |
| `m4_bear_selective` | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |

- **Every shortlist member keeps the judged OOS pass** — the exact window
  where all five #984 close stacks died. The gates trade the 2026 bear tape
  30 times (p21) vs baseline's 117, skipping the dead-cat re-entries instead
  of amputating the trend-holds (OOS mean Sharpe: p21 -0.40, m4s -0.03, all
  vs bar -0.75).
- `comp_up_clean_p21` flips 2025H1 (+1.1% mean return vs baseline's -23.4%)
  without the #984 inversion: 2023/2024 stay FAIL against bull-year bars
  (+74.9% / +18.2% mean return vs baseline's +141.5% / +28.7% — the gate
  gives up bull-year upside against a long-biased bar, it does not go
  negative). No liquidated legs anywhere.

## Step 4 — the mandatory stitched continuous-window headline

```
uv run --no-sync python backtest/candidates/breakout_1165/audit_headline.py \
    --candidates baseline.json,comp_up_clean.json,comp_up_clean_p21.json,m4_bear_off.json,m4_bear_selective.json \
    --json backtest/candidates/breakout_1165/audit_window_headline.json
```

The #983/#984 lesson applied (every prior IS story evaporated here — this
one doesn't):

| | mean Sharpe | mean ret | vs B&H | worst DD | #T |
|---|---:|---:|---:|---:|---:|
| baseline | +0.02 | -1.1% | +43.7pts | -52.2% | 260 |
| `comp_up_clean` | +0.16 | +3.6% | +48.4pts | -41.2% | 91 |
| **`comp_up_clean_p21`** | **+0.45** | **+11.4%** | **+56.3pts** | **-23.2%** | **67** |
| `m4_bear_off` | +0.27 | +7.7% | +52.5pts | -39.6% | 218 |
| `m4_bear_selective` | +0.32 | +10.1% | +54.9pts | -39.6% | 211 |

The #1165 validation bar was "worst DD materially better than -52.2% with
vs-B&H within a few points of +47.5" — `comp_up_clean_p21` beats it on both
sides simultaneously.

## Step 5 — fee-drag gate (breakout is fee-marginal)

```
uv run --no-sync python backtest/candidates/breakout_1165/fee_drag.py \
    --candidates baseline.json,comp_up_clean.json,comp_up_clean_p21.json,m4_bear_off.json,m4_bear_selective.json \
    --json backtest/candidates/breakout_1165/fee_drag_shortlist.json
```

| | gross ret | net ret | drag | #T | T/yr |
|---|---:|---:|---:|---:|---:|
| baseline | +13.3% | -1.1% | 14.3pp | 260 | 43.7 |
| `comp_up_clean` | +8.7% | +3.6% | 5.1pp | 91 | 15.3 |
| **`comp_up_clean_p21`** | **+15.2%** | **+11.4%** | **3.8pp** | **67** | **11.3** |
| `m4_bear_off` | +20.5% | +7.7% | 12.8pp | 218 | 36.6 |
| `m4_bear_selective` | +22.9% | +10.1% | 12.9pp | 211 | 35.5 |

Opposite of the #984 close stacks (which all *added* legs on a fee-marginal
entry): the gate removes entries, so drag falls with trade count — and p21's
gross return is *above* baseline's (+15.2% vs +13.3%): the removed trades
were net losers before fees, not just fee victims.

## Step 6 — live-label fidelity gate (#1197 wiring evidence)

Everything above scored `trending_up_clean` labels computed over the **full
cached history**; the live gate reads them from `check_regime.py`, which
recomputes the composite over a **bounded 200-bar fetch** each cycle — the
ADX sub-recursion seeds differently, so the same calendar bar can label
differently live vs in this evidence (#1082). Before wiring, this step
measures that drift with the same hand-rule arm the #1074 promotion gate
uses (`regime_bounded_window_validate.validate`, `model=None` — no fitted
model, so `gate_verdict`/provenance don't apply), applied fail-closed
(#1082 bar: agreement ≥ 0.95 on ≥ 30 comparable bars, per dataset × window;
a short/missing row blocks, never vacuously passes):

```
uv run --no-sync python backtest/candidates/breakout_1165/live_label_fidelity.py \
    --json backtest/candidates/breakout_1165/live_label_fidelity.json
```

**Result: PASS on all 12 rows** (6 datasets × is/oos). Worst all-label
agreement 0.98678 (BTC/4h is); worst gate-membership agreement — the exact
`trending_up_clean` bit `allowed_regimes` consumes — 0.99338 (ETH/4h oos,
6 flips on 907 bars). 8 of 12 rows have zero gate flips; the bounded window
the live scheduler sees reproduces the labels this evidence scored.

| | 1h is | 1h oos | 4h is | 4h oos |
|---|---:|---:|---:|---:|
| BTC/USDT | 0.99735 / 0 | 0.99837 / 2 | 0.98678 / 0 | 0.99890 / 1 |
| ETH/USDT | 0.99796 / 0 | 0.99897 / 0 | 0.99504 / 0 | 0.99228 / 6 |
| SOL/USDT | 1.00000 / 0 | 0.99820 / 0 | 0.99835 / 0 | 0.99338 / 2 |

(all-label agreement / gate-membership flips; n per row: 1h is 4900,
1h oos 3676–3887, 4h is 1210, 4h oos 907)

### The wiring itself (operator config, out-of-tree per #1056)

The live config lives at `/var/lib/go-trader[/<instance>]/config.json`, not
in the repo, so the deployment is an operator edit. The exact shape — pinned
by `scheduler/regime_comp_up_clean_gate_test.go` so it can never silently
drift from what was validated:

```jsonc
"regime": {
  "enabled": true,
  "windows": {
    "comp_p21": { "classifier": "composite", "period": 21 }
    // …existing windows (e.g. an ADX "medium") stay as they are
  }
},
// on the breakout futures strategy:
"regime_gate_window": "comp_p21",
"allowed_regimes": ["trending_up_clean"]
```

Deployment constraints (all enforced by config validation / hot-reload
guards, tested in the same file):

- **Set `regime_gate_window` explicitly.** Without it the gate reads the
  PRIMARY window ("medium" when present) — if that's an existing ADX window,
  its 3-label vocabulary lacks `trending_up_clean` and config load rejects;
  if the composite window itself happens to be named "medium" it works, but
  only by deployment-order luck. The explicit key makes the wiring
  independent of what other windows exist.
- Composite thresholds stay the shared defaults (this evidence ran them);
  the label pairing is validated against the gate window's classifier
  (`validateStrategyRegimeVocabulary`).
- **Apply while the strategy is flat**: SIGHUP hot-reload blocks
  `regime_gate_window`/classifier changes on a referenced window while a
  position is open (`config_reload.go`).
- Gate semantics live: entries blocked when the medium-window label ≠
  `trending_up_clean`, **closes and position management always execute** —
  the same asymmetry every backtest row above relied on.

## Verdict

1. **The -52.2% DD is regime exposure, and a label set that separates the
   bleed from the edge exists**: composite `trending_up_clean` (period 21,
   medium window) — the quality split (clean vs choppy trend efficiency),
   NOT direction. The #984-conjectured naive bear-block fails exactly as
   cautioned, but by blocking nothing rather than amputating the edge:
   breakout's entry bars label directionally "up" even in bear tapes.
2. `comp_up_clean_p21` meets the full #1165 bar: protocol IS+OOS PASS,
   held-out 1/3 (2025H1 flipped; bull-years still lose to long-biased bars
   but stay positive), stitched worst DD -23.2% (vs -52.2%) with vs-B&H
   +56.3pts (vs +43.7), fee drag 3.8pp on 67 trades. `m4_bear_selective` is
   the runner-up (stitched +54.9pts / -39.6% DD) if the position-carried
   response is ever preferred over entry gating.
3. **Caveats, stated plainly**: period 21 was selected on the IS plateau
   (the shelf is broad — p14/p21/p28 all clear baseline — but 21 is still
   the IS peak); the judged OOS window was looked at once per shortlist
   member (M1 step 5 discipline held: selection was IS-only); all evidence
   is one asset class (BTC/ETH/SOL × 1h/4h) on the audit frame.
4. **Steps 1–5 changed no live config.** The wiring follow-on is **#1197**
   (Step 6 above): the live-label fidelity bar passed on all 12 rows, the
   operator config shape + gate semantics are pinned by
   `scheduler/regime_comp_up_clean_gate_test.go`, and the config edit itself
   is operator-side (out-of-tree, #1056). Remaining follow-on (own issue):
   re-run this directory against `squeeze_momentum` (#983, same DD
   conclusion, -58.5%): the drivers are strategy-parameterized
   (`--strategy/--registry/--direction`; `driver_common.py` holds the
   breakout-specific M4 param sets).

A positive result still ships as evidence, not a config change (#995 step 4
symmetry: documented, not deployed).
