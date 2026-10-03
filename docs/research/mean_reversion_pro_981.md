# mean_reversion_pro trade frequency M1 application (#981)

Generated: 2026-07-01

Part of the M1 program (#977/#978); audit source #956/#975. The audit ranked
`mean_reversion_pro` best among strategies that actually traded below the two
keepers (-0.20 mean Sharpe, +35.8pts vs B&H) but on a starvation sample: 17
trades in-sample, 2 OOS ("weak (2 trades)"). The regime filter that separates
it from its deprecated parent (`mean_reversion`, -1.46 / deprecate) is also
what starves it. This application raises trade frequency **without weakening
the regime gate**, per the #981 focus: mechanisms as independent default-off
knobs (M1 step 4), degenerate passes rejected (step 5), plateau-checked
(step 6).

## Mechanisms (implemented, default-off)

`shared_strategies/open/mean_reversion_pro.py` — two new params (registry
defaults keep both off; `--list-json` byte-identical, both registries
verified):

- `touch_entry` (default **0** = off): adds a **band-touch** trigger — the
  bar the z-score first pierces ±`entry_std` (previous bar inside the band)
  while RSI shows the extreme.
- `turn_entry` (default **0** = off): adds a **stretched-turn** trigger — the
  z-score still beyond ±`entry_std` but turning back toward the mean vs the
  prior bar, while RSI shows the extreme. Fires earlier than the base
  reversion cross and also catches stretches whose RSI confirmation window
  has expired by the time z crosses back.

Both triggers are OR'd with the base reversion-cross trigger (they only ever
add signal bars — regression-tested) and both sit behind the **same ADX
no-trend gate and RSI-extreme evidence** — frequency from more setups inside
the allowed regime, not weaker filtering. RSI evidence for the extra
triggers = extreme on the current bar OR within the base trigger's
`confirm_window`. Defaults 0/0 are bit-identical to the pre-#981 strategy
(regression-tested).

The third mechanism — a **looser ADX gate** — needs no new code:
`adx_max` is an existing registered param; it is swept as mechanism 3 below.

## Baseline reproduction (M1 step 1)

Audit row (#956, in `scheduler/ui_reports.go`): in-sample mean Sharpe -0.20,
+35.8pts vs B&H, 17 trades over 12 months × 6 datasets; OOS +11.8pts on 2
trades. On the M1 harness (IS = 2025-06-10→2026-01-01, ~7 months, so lower
counts are expected; cache populated through 2026-07-01):

| Window | mean Sharpe / bar | mean DDadj / bar | trades (Σ 6 legs) | verdict |
|--------|-------------------|------------------|--------------------|---------|
| IS (2025-06-10→2026-01-01) | -0.14 / -0.12 | -0.11 / -0.14 | 9  | FAIL |
| OOS (2026-01-01→latest)    |  0.41 / -0.63 |  0.45 / -0.46 | 13 | PASS |
| 2023                        |  1.17 /  1.46 |  6.10 /  3.67 | 20 | FAIL |
| 2024                        |  1.23 /  0.90 |  2.24 /  1.07 | 23 | PASS |
| 2025H1                      |  0.07 / -0.42 |  0.06 / -0.37 | 16 | PASS |

Protocol OOS PASS, held-out 2/3 — consistent with the audit's "top tier of
strategies that traded, sample too thin to trust". The starvation is visible
per-leg: IS has two zero-trade legs (BTC 4h, ETH 4h; 2025H1 adds SOL 4h),
and no baseline leg in any window reaches double-digit trades. IS is a
near-miss FAIL (-0.14 vs bar -0.12) driven by the SOL 4h leg (1 trade,
-32.3%).

**Diagnosis (M1 step 3):** unlike the M5 fee-churn class, every failure here
is a *sample-size* failure — the strategy is fee-light (9-23 trades per
window across 6 legs vs incumbents' hundreds) but one bad leg dominates any
window mean. More setups inside the same regime is the right lever;
loosening the regime filter is the wrong one (see mechanism 3).

## Mechanism sweeps (M1 steps 4-6; tuned on IS + held-outs only, per step 5)

### Mechanism 3 — looser ADX gate (`adx_max` sweep, existing param)

Mean candidate Sharpe per tuning window (bar in header; PASS = beats bar on
Sharpe AND DDadj means; degenerate zero-trade passes auto-rejected):

| adx_max | IS (bar -0.12) | 2023 (bar 1.46) | 2024 (bar 0.90) | 2025H1 (bar -0.42) |
|---------|----------------|------------------|------------------|---------------------|
| 20      | 0.18 PASS (3/6 traded) | 0.67 FAIL (3/6) | 0.15 FAIL (3/6) | 0.45 PASS (4/6) |
| 25 (default) | -0.14 FAIL | 1.17 FAIL | **1.23 PASS** | 0.07 PASS |
| 30      | -0.78 FAIL | 1.30 FAIL | **1.27 PASS** | -0.11 PASS |
| 35      | -0.34 FAIL | 1.24 FAIL | 0.79 FAIL | 0.63 PASS |
| 40      | -0.17 FAIL | 1.16 FAIL | 0.56 FAIL | 0.68 PASS |
| 50      | 0.01 PASS | 1.42 FAIL | 0.40 FAIL | 0.34 PASS |
| 100 (≈ no gate) | 0.09 PASS | 1.48 FAIL | 0.31 FAIL | 0.37 PASS |

**Negative result — the gate is not the frequency lever.** No value dominates
the default across tuning windows: loosening to 35+ bleeds the bull year
(2024 PASS→FAIL, Sharpe 1.23→0.79→0.31 as the ceiling rises — exactly the
falling-knife fades the gate exists to block), while tightening to 20 starves
half the legs to zero trades. The `adx_max=50/100` IS "passes" are the
audit's core finding replayed in reverse: without the gate the strategy
converges toward its deprecated parent (2024 at 100 ≈ unfiltered
mean_reversion behavior). The default `adx_max=25` stays; the issue's
premise ("the plateau check guards against trading the edge away") is
confirmed — there is no plateau to move to.

### Mechanisms 1+2 — additional entry triggers (isolated and combined)

Mean candidate Sharpe / DDadj per tuning window; Σ trades across the 6 legs:

| config | IS (bar -0.12/-0.14) | 2023 (bar 1.46/3.67) | 2024 (bar 0.90/1.07) | 2025H1 (bar -0.42/-0.37) |
|--------|----------------------|-----------------------|-----------------------|---------------------------|
| base (off/off) | -0.14 / -0.11 · 9 FAIL | 1.17 / 6.10 · 20 FAIL | 1.23 / 2.24 · 23 PASS | 0.07 / 0.06 · 16 PASS |
| **turn only** | **0.14 / 0.30 · 22 PASS** | 1.32 / 4.09 · 50 FAIL | 0.79 / 1.15 · 54 FAIL | 0.12 / 0.16 · 27 PASS |
| touch only | -0.59 / -0.48 · 17 FAIL | 0.90 / 3.08 · 46 FAIL | 1.08 / 1.70 · 40 PASS | -0.07 / -0.11 · 23 PASS |
| both | -0.47 / -0.40 · 25 FAIL | 1.34 / 3.71 · 61 FAIL | 0.61 / 0.59 · 56 FAIL | -0.21 / -0.12 · 31 PASS |

- **`turn_entry=1` is the frequency mechanism that works**: trades 2-2.5×
  everywhere (9→22, 20→50, 23→54, 16→27), every zero-trade leg fills
  (6/6 traded in all four windows), and IS flips FAIL→PASS — the ETH 4h leg
  goes 0 trades → +16.7% (Sharpe 1.88 vs bar 0.30); BTC 1h goes 2→6 trades,
  Sharpe 0.03 vs bar -1.02.
- **`touch_entry=1` is a negative result**: it adds nearly as many trades
  but catches knives (IS Sharpe -0.14→-0.59); stacking it onto turn only
  dilutes (IS -0.47 vs turn's +0.14). Documented, stays available, not
  recommended.
- **2024 is a real regression under turn** (PASS→FAIL, 1.23→0.79 vs bar
  0.90): in a steady bull the extra fades enter earlier into pullbacks that
  keep falling; the base cross-back trigger's extra patience was worth more
  than the extra sample. Same trade-bull-upside-for-chop-coverage shape as
  #982's HTF gate.

### Plateau checks for the chosen config (`turn_entry=1`, M1 step 6)

- **`adx_max` axis (turn on):** IS passes at 22 and 25 (0.12/0.14), fails at
  20 (-0.51, 4/6 traded) and 30+ (trend bleed, -0.29/-0.30) — the default 25
  sits on the two-value plateau, not a cliff edge. 2023 shows a lone spike
  (adx_max=22 → 2.22 PASS flanked by 1.40/1.32 FAILs) — rejected as an
  overfit tell per step 6, not chased.
- **`confirm_window` axis (turn on):** inert — 2,3,4,5 produce identical
  means on IS and 2024 (the turn trigger's RSI evidence is dominated by the
  current-bar extreme while stretched). No cliff.
- **`entry_std` axis (turn on, IS):** flat plateau — 1.75/2.0/2.25/2.5 all
  PASS (Sharpe 0.11-0.15, DDadj 0.30-0.33, 6/6 traded). The IS pass is not
  an `entry_std` artifact.

## Final protocol measurement (one OOS look, chosen config `{"turn_entry": 1}`)

| Window | baseline verdict | turn verdict | turn Sharpe / bar | turn DDadj / bar |
|--------|------------------|--------------|--------------------|-------------------|
| IS     | FAIL | **PASS** | 0.14 / -0.12 | 0.30 / -0.14 |
| OOS    | PASS | **FAIL** | -1.32 / -0.63 | -0.68 / -0.46 |
| 2023   | FAIL | FAIL | 1.32 / 1.46 | 4.09 / 3.67 |
| 2024   | PASS | **FAIL** | 0.79 / 0.90 | 1.15 / 1.07 |
| 2025H1 | PASS | PASS | 0.12 / -0.42 | 0.16 / -0.37 |

Windows passed: **2/5 vs baseline 3/5**; protocol OOS **FAIL** (baseline
PASS); held-out 1/3 vs baseline 2/3. The OOS failure is broad, not one bad
leg: 1/6 legs beat the bar (baseline: 5-6/6). On the 2026 crash tape the
stretched-turn trigger enters *before* the reversion is confirmed and
catches the continuation — SOL 1h flips from +25.3% (baseline, 4 trades) to
-43.1% (turn, 4 trades); BTC 4h from +9.2% to -19.9%. Slow persistent bleeds
keep ADX below the ceiling, so the regime gate — correctly — does not save
an entry trigger that fires too early inside the allowed regime.

## Honest caveats

1. **The frequency mechanism works; the chosen trigger's edge does not
   survive the protocol window.** Turn delivers exactly the sample the issue
   asked for (2-2.5× trades, zero-trade legs eliminated, IS FAIL→PASS on a
   plateau across `adx_max`/`confirm_window`/`entry_std`) but gives back the
   baseline's OOS pass and the 2024 held-out — the base trigger's patience
   (wait for the z-score to cross back) *is* the edge the audit measured,
   not just a sampling handicap.
2. **The value is regime-conditional** — which is M4's territory (#977 maps
   mean_reversion_pro to M4 regime-profile allocation): switching
   `turn_entry` 0↔1 on a slow regime signal (fade harder in grind windows,
   stand down in trends/crashes) is the natural follow-on the issue itself
   names. Out of scope here; the default-off knob is the surface M4 needs.
3. The audit's 2-trade OOS sample is superseded by the current-cache OOS
   window (6 months, 13 baseline trades) — the baseline's standing is now
   measured on a real sample, which strengthens, not weakens, the
   keep-the-default verdict.

## Verdict

**Negative result for any default change — documented, not deleted (M1
step 4).** All three mechanisms were isolated, swept, and rejected for
promotion on the M1 bar:

- `adx_max` (mechanism 3): no plateau beats the default across tuning
  windows; loosening replays the deprecated parent's failure mode.
- `touch_entry` (mechanism 1): knife-catcher — degrades IS outright.
- `turn_entry` (mechanism 2): the real candidate — passes IS on a broad
  plateau with the frequency the issue wanted, but fails the one-look
  protocol OOS and the bull-year held-outs.

**Registry defaults stay exactly as before** (`--list-json` byte-identical
on both registries, verified); both new knobs ship **default-off** as
validated research surface for per-config opt-in and for the M4 follow-on.
The #981 premise is answered: the regime filter is not what starves the
strategy — the *reversion-confirmation patience* is, and that patience is
load-bearing. Live behavior is unchanged.

## Reproduce

```bash
# Baseline, all 5 windows (M1 steps 1-2)
uv run --no-sync python backtest/eval_windows.py --strategy mean_reversion_pro

# Mechanism 3 — ADX ceiling sweep per tuning window W ∈ {is,2023,2024,2025H1}
uv run --no-sync python backtest/eval_windows.py --strategy mean_reversion_pro \
  --windows W --sweep adx_max=20,25,30,35,40,50,100 --sweep-window W

# Mechanisms 1+2 — trigger isolation/combination per tuning window W
uv run --no-sync python backtest/eval_windows.py --strategy mean_reversion_pro \
  --windows W --sweep touch_entry=0,1 --sweep turn_entry=0,1 --sweep-window W

# Plateau axes for the chosen config (IS shown; adx also run on 2023/2024/2025H1)
uv run --no-sync python backtest/eval_windows.py --strategy mean_reversion_pro \
  --params '{"turn_entry": 1}' --windows is --sweep adx_max=20,22,25,30,35 --sweep-window is
uv run --no-sync python backtest/eval_windows.py --strategy mean_reversion_pro \
  --params '{"turn_entry": 1}' --windows is --sweep confirm_window=2,3,4,5 --sweep-window is
uv run --no-sync python backtest/eval_windows.py --strategy mean_reversion_pro \
  --params '{"turn_entry": 1}' --windows is --sweep entry_std=1.75,2.0,2.25,2.5 --sweep-window is

# Final protocol measurement (single OOS look, chosen config)
uv run --no-sync python backtest/eval_windows.py --strategy mean_reversion_pro \
  --params '{"turn_entry": 1}'
```

Runs executed 2026-07-01 on the audit datasets (BTC/ETH/SOL × 1h/4h,
binanceus fee model, 5 bps slippage, long-leg open-as-close harness);
`--registry spot` (mean_reversion_pro is byte-identical in both registries).
Caveat (same class as #980's): a run against a cold or partially-populated
cache is not reproducible — populate 2023-01-01→now first; with a warm cache
back-to-back runs reproduce to the digit.

---
Created with LLM: Fable 5 | high | Harness: Claude Code
