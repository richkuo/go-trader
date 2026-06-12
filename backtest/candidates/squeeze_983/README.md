# squeeze_momentum close-stack work — entry frozen (#983, M2 + M3)

Application of **M2 (close-stack co-optimization)** and **M3 (exit-quality
diagnostics)** to the registry's top-ranked entry (#956: +47.9pts vs B&H,
-58.5% worst max DD on the default open-signal-as-close stack). Entry logic
and params are frozen at registry defaults throughout; only the close stack
is swept.

**Result: negative, with one methodological finding.** No close stack in the
swept space cuts the tail drawdown while keeping the vs-B&H edge. The
strongest per-window candidate (the live default tiered-TP ladder) passes the
M1 protocol on every judged window yet *collapses on the continuous audit
window* — a window-segmentation blind spot worth keeping in the M1 playbook.
Per #995 step 4 a negative result is documented, not deleted.

Every number reproduces from the repo root with the cache DB present
(`shared_tools/trading_bot.db`).

## Baseline reproduction (M1 step 1)

`baseline.json` on the continuous audit window (2025-06-10 → latest cache,
six audit datasets) reproduces #956 almost exactly — mean Sharpe +0.07
(audit +0.03), vs B&H +45.9pts (+47.9), worst max DD **-58.46%** (-58.5%),
130 trades (131); residuals are five extra days of cache. Artifact:
`audit_window_headline.json`.

## Step 1 — diagnose (where the -58.5% DD accumulates)

```
uv run --no-sync python backtest/exit_diagnostics.py \
    --strategy squeeze_momentum --windows is,oos \
    --json backtest/candidates/squeeze_983/diagnostics.json
```

- **Late giveback dominates**: 57.8% of IS trades (48/83, net -113.9%) and
  50.0% of OOS trades (26/52, net -52.5%). Winners peak late
  (median `bars_to_MFE` up to 98 bars OOS) and surrender the move before the
  open-signal exit fires.
- **Early reversal** is secondary (IS 15.7% / OOS 21.2%); losers run deep —
  MAE p80 spans **4.6–18.6 ATR** across datasets, far beyond any sane stop.
- Fee churn is minor on the baseline (≤ 5% of trades) — but becomes the
  dominant failure mode once stops are added (below).
- The 12 IS clean wins net **+170.4%** against -174.7% from everything else:
  the entire edge is a fat right tail. Any exit that caps or clips that tail
  has no margin to give.

## Step 2 — M2 close-stack screen on IS (selection window only)

`sweep_close_stacks.py` expands the #996 default grid
(`DEFAULT_CLOSE_STACK_SPECS`, 25 stacks: baseline, fixed ATR stops, trailing
stops, three tiered-TP ladders × optional SL/trail) and scores each on the six
audit datasets over the protocol IS window with the entry frozen:

```
uv run --no-sync python backtest/candidates/squeeze_983/sweep_close_stacks.py \
    --window is --json backtest/candidates/squeeze_983/sweep_is.json
```

| IS rank (mean DDadj) | DDadj | Sharpe | ret% | worst DD% | #T |
|---|---:|---:|---:|---:|---:|
| tiered_tp_atr[1.5×:0.4, 3×:0.8, 5×:1] | +0.027 | -0.08 | -8.84 | -52.6 | 168 |
| baseline (open-signal-as-close) | -0.005 | -0.07 | -3.14 | -46.8 | 83 |
| tiered_tp_atr[1×:0.5, 2×:0.8, 3×:1] | -0.035 | -0.22 | -10.90 | -52.6 | 107 |
| sl_atr=1.5 | -0.200 | -0.64 | -5.13 | -31.9 | 114 |
| … every stop/trail/ladder+stop combo | < -0.26 | | | -14.2…-38.0 | 247–366 |

The DD-cutting stacks (worst DD -14% to -20%) **triple the trade count**:
squeeze_momentum's entry signal persists after a stop-out, so the long/flat
path re-enters the next bar and pays the round trip again — exactly the
fee-sensitivity the M5 screen flagged (gross edge -0.29%/leg, borderline
`deprecate`). Returns die before the DD benefit can matter.

Supplementary screens (same harness, artifacts committed):

- `sweep2_is.json` — ladder plateau, wide trails (4/5 ATR), time stops,
  ladder+wide-stop combos. Only `time_stop` produced a positive IS profile.
- `timestop_plateau_is.json` — `max_bars` ∈ {75…400}: positive region
  200–250 (DDadj +0.27…+0.53), falls off both sides.
- `ratchet_screen_is.json` — `trailing_tp_ratchet` (let-winners-run rungs,
  default + clean-group ladders × opening trail 3/4/5 ATR): all worse than
  baseline on IS (DDadj -0.30…-0.47); same re-entry churn.

## Step 3 — M2 walk-forward fold stability (frozen entry)

`walk_forward_optimize` with singleton open-param ranges and the 25-stack
grid, `dd_adjusted_return` selection, 5 folds over 2023-01-01 → 2026-01-01 on
BTC/ETH/SOL 1h (`walkforward_folds.json`): tiered-TP ladders win 10/15
train-folds (no single ladder dominates — a family plateau), baseline 3,
fixed SL 2, trailing stacks 0. Consistent with the IS screen: the ladder
family is the only competitive non-baseline stack per-window.

## Step 4 — judge through the M1 protocol (#994)

```
uv run --no-sync python backtest/eval_windows.py \
    --candidate-json backtest/candidates/squeeze_983/<candidate>.json
```

(`validate_shortlist.py` runs the same `eval_windows` functions for all
shortlist candidates in one process so the incumbent bars are computed once;
artifacts `validation.json`, `validation_timestop.json`.)

| Candidate | IS | OOS (judged) | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| baseline | PASS | **PASS** | PASS | FAIL | FAIL | 1/3 |
| `tp_default` (1.5×:0.4, 3×:0.8, 5×:1) | PASS | **PASS** | FAIL | PASS | PASS | 2/3 |
| `sl_atr_1.5` | FAIL | FAIL | FAIL | FAIL | FAIL | 0/3 |
| `trail_atr_3.0` | FAIL | FAIL | FAIL | FAIL | FAIL | 0/3 |
| `tp_runner_trail3` | FAIL | FAIL | FAIL | FAIL | FAIL | 0/3 |
| `time_stop_200/225/250` | PASS | **FAIL** | PASS | FAIL | FAIL | 1/3 |

- The `time_stop` IS plateau (the best absolute IS profile, +5.8% mean return)
  **does not survive the judged OOS window** — an in-sample mirage, rejected.
  Same lesson as ichimoku_997: time exits clip late-peaking winners.
- `tp_default` is the only stack that passes protocol IS + OOS and improves
  held-out consistency (2/3 vs baseline 1/3).

## Step 5 — the catch: continuous-window headline (why nothing ships)

Re-running baseline vs `tp_default` on the **continuous** audit window
(2025-06-10 → latest, the #956 frame; `audit_window_headline.json`):

| | mean Sharpe | mean ret | vs B&H | worst DD | #T |
|---|---:|---:|---:|---:|---:|
| baseline | **+0.07** | **+1.1%** | **+45.9pts** | -58.5% | 130 |
| `tp_default` | -0.51 | -23.8% | +21.1pts | **-72.4%** | 168 |

Segmented per-window scoring (equity resets at window boundaries, positions
truncated at window edges) hid this: `tp_default` passes IS and OOS *judged
separately* yet halves the vs-B&H edge and **deepens** the worst drawdown when
the windows are stitched. ETH 4h is the smoking gun: baseline +4.5% vs ladder
-56.3% — the ladder banks 40% at 1.5 ATR and is fully out by 5 ATR, amputating
the fat-tail trends that fund the strategy, while its lack of a signal exit
holds residual losers longer. **Do not deploy the ladder on this entry.**

Methodology note for #977: a candidate that materially changes *holding
structure* (caps winners / extends losers) must also be checked on a
continuous window before adoption — per-window protocol verdicts alone can
flip sign on the stitched frame.

## Verdict

1. **No close stack in the swept space meets the #983 target** (cut the
   -58.5% worst DD materially without giving up the +47.9pt vs-B&H edge).
   The space swept: 25-stack #996 default grid, ladder plateau, fixed stops
   1.5–4 ATR, trailing stops 2–5 ATR, time stops 75–400 bars,
   trailing-TP ratchets (default + wide rungs × 3 opening trails), and
   ladder×stop combos — all on a frozen entry.
2. Mechanism: the edge is a **fat right tail** (12 IS clean wins +170%
   against everything else -175%). Profit ladders amputate the tail; stops
   and ratchets convert tail-risk into re-entry fee churn (the persistent
   entry signal re-opens the next bar — 83 → 250–366 trades); time stops
   overfit the IS horizon and fail judged OOS.
3. The **entry stays validated**: baseline passes protocol OOS (only
   incumbent-relative pass in the shortlist with no caveat) — keep
   squeeze_momentum's default stack as-is.
4. The -58.5% DD is **regime exposure, not exit quality**: it accrues from
   staying long through bear windows the entry keeps re-entering. The right
   tool is M4 regime-profile allocation (#998) / composite-regime gating on
   this entry — close-stack work on a frozen entry is exhausted by this
   sweep.

A negative result is a valid M1 finding — documented, not deleted (#995 step 4).
