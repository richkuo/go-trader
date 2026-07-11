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

Every committed artifact regenerates from the repo root with the cache DB
present (`shared_tools/trading_bot.db`) via one of the four committed drivers
(`sweep_close_stacks.py`, `sweep_supplementary.py`, `validate_shortlist.py`,
`audit_headline.py`) or the documented `eval_windows.py` command — each
section below states its exact command. Protocol windows (`is`, `oos`,
held-out) are date-pinned; the continuous audit window ends at the latest
cache by default, so `audit_headline.py` records the effective per-dataset
data range in its artifact — diff that against the committed file before
comparing numbers regenerated under a fresher cache.

## Baseline reproduction (M1 step 1)

```
uv run --no-sync python backtest/candidates/squeeze_983/audit_headline.py \
    --json backtest/candidates/squeeze_983/audit_window_headline.json
```

`baseline.json` on the continuous audit window (2025-06-10 → latest cache,
six audit datasets) reproduces #956 almost exactly — mean Sharpe +0.07
(audit +0.03), vs B&H +45.9pts (+47.9), worst max DD **-58.46%** (-58.5%),
130 trades (131); residuals are five extra days of cache. Artifact:
`audit_window_headline.json` (also carries the step-5 `tp_default` run; the
committed file's `effective_range` block pins the per-dataset data range the
numbers were computed from — last bars 2026-06-04 → 2026-06-12 depending on
dataset).

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

Supplementary screens (same harness; generator `sweep_supplementary.py`,
every artifact row embeds its full stack spec):

```
uv run --no-sync python backtest/candidates/squeeze_983/sweep_supplementary.py \
    --screen <sweep2|timestop|ratchet> \
    --json backtest/candidates/squeeze_983/<sweep2_is|timestop_plateau_is|ratchet_screen_is>.json
```

- `sweep2_is.json` — ladder plateau, wide trails (4/5 ATR), time stops,
  ladder+wide-stop combos. Only `time_stop` produced a positive IS profile.
- `timestop_plateau_is.json` — `max_bars` ∈ {75…400}: positive region
  200–250 (DDadj +0.27…+0.53), falls off both sides.
- `ratchet_screen_is.json` — `trailing_tp_ratchet` (let-winners-run rungs,
  default + clean-group ladders × opening trail 3/4/5 ATR for the default
  rungs, 4/5 for the wide rungs — wide's first rung sits at 3.0 ATR, so an
  opening trail of 3 never fires a rung before the trail itself): all worse
  than baseline on IS (DDadj -0.30…-0.47); same re-entry churn.

## Step 3 — M2 walk-forward fold stability (frozen entry)

```
uv run --no-sync python backtest/candidates/squeeze_983/walkforward.py \
    --json backtest/candidates/squeeze_983/walkforward_folds.json
```

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

(`validate_shortlist.py` runs the same `eval_windows` functions for a list of
candidates in one process so the incumbent bars are computed once. The
default shortlist emits `validation.json`; `validation_timestop.json` is
`--candidates time_stop_200.json,time_stop_225.json,time_stop_250.json`.)

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
(2025-06-10 → latest, the #956 frame; `audit_window_headline.json`, generated
by the `audit_headline.py` command in step 1):

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

## Addendum 2026-07-05 — re-run under the corrected simulation geometry (#1243)

Re-ran the headline drivers (`audit_headline.py` continuous-window;
`validate_shortlist.py` M1 protocol, default shortlist + time-stop shortlist)
on current `main` (the #1238 audit fixes plus #1250 fee-net-per-trade and #1251
canonical Sortino / `None` profit-factor / half-open windows), identical cache
snapshot. **Verdict holds: no close stack ships; keep the baseline stack.** The
baseline (open-signal-as-close, no ATR geometry) is byte-identical; the
ATR-geometry candidate (`tp_default` tiered-TP ladder) shifted slightly, all in
the conservative direction, and stays a collapse.

Step-5 continuous audit window (2025-06-10 → latest):

| run | metric | documented | re-run | status |
|---|---|---:|---:|---|
| baseline | mean Sharpe / ret / vsB&H / worstDD / #T | +0.07 / +1.1% / +45.9 / -58.5% / 130 | +0.07 / +1.07% / +45.90 / -58.46% / 130 | identical |
| `tp_default` | mean Sharpe / ret / vsB&H / worstDD / #T | -0.51 / -23.8% / +21.1 / -72.4% / 168 | **-0.532 / -24.07% / +20.76 / -72.38% / 158** | shifted, still collapses |

`tp_default` runs 158 trades vs the documented 168 — the #1238 closed-bar
EntryATR moved a handful of tiered-TP fills — and its stitched worst DD stays
-72% with a negative return, so the step-5 conclusion ("do not deploy the
ladder on this entry") is unchanged.

M1 protocol PASS/FAIL table — **every verdict identical to the documented run**
(baseline OOS PASS, held-out 1/3; `tp_default` OOS PASS, held-out 2/3;
`sl_atr_1.5` / `trail_atr_3.0` / `tp_runner_trail3` FAIL 0/3;
`time_stop_200/225/250` OOS FAIL, held-out 1/3). No PASS↔FAIL flipped despite
the #1251 DDadj-floor and half-open-window changes. The keep-baseline verdict
and the "-58.5% DD is regime exposure, not exit quality → M4" disposition stand.

## Addendum 2026-07-10 — re-run under intra-bar stop resolution + corrected HL fees (#1294)

Re-ran `audit_headline.py` (baseline + `tp_default`) and `validate_shortlist.py`
(default shortlist) on current `main` — #1271 `ohlc_walk` intrabar default and
the #1320 audit fee-model switch (binanceus → hyperliquid) — under BOTH
`--intrabar-resolution` modes (the drivers now thread the flag, #1294),
identical cache snapshot (last bars 2026-06-04 → 2026-06-12, zero drift).
**Verdict holds: no close stack ships; keep the baseline stack.**

**#1271 reach is split across the shortlist.** `baseline` and `tp_default`
arm no engine-tracked stop (evaluator ladders/tiers only) and reproduce
**byte-identically** across the mode pair — for those two rows all movement
vs the 2026-07-05 addendum is the #1320 fee model alone. The other three
shortlist candidates (`sl_atr_1.5`, `trail_atr_3.0`, `tp_runner_trail3`) DO
arm engine-tracked `stop_loss_atr_mult`/`trailing_stop_atr_mult`, so #1271
moves their per-window numbers (table below); none of their M1 verdicts
changes with the mode.

| run | metric | documented (2026-07-05) | re-run (both modes) | status |
|---|---|---:|---:|---|
| baseline | Sharpe / ret / vsB&H / worstDD / #T | +0.07 / +1.07% / +45.90 / -58.46% / 130 | +0.136 / +3.84% / +48.67 / -57.58% / 130 | fee model only (same 130 entries; byte-identical across modes) |
| `tp_default` | " | -0.532 / -24.07% / +20.76 / -72.38% / 158 | -0.506 / -23.55% / +21.27 / -72.38% / **94** | fee model only; still collapses (byte-identical across modes) |

`tp_default`'s trade count drops 158 → 94 with the entry set frozen — the
cheaper hyperliquid fees change the equity path enough to move which ladder
tiers fill — but the stitched frame stays a -72% worst-DD, negative-return
collapse, so the step-5 conclusion is unchanged.

The engine-stop candidates' pooled M1 windows under each mode (Sharpe / DDadj,
audit bar in the full run artifacts):

| run | window | `bar_close` | `ohlc_walk` (#1271 delta) | verdict (both modes) |
|---|---|---:|---:|---|
| `sl_atr_1.5` | is | -0.52 / -0.06 | -0.23 / -0.02 | FAIL |
| `sl_atr_1.5` | oos | -1.38 / -0.48 | -1.18 / -0.42 | FAIL |
| `trail_atr_3.0` | is | -0.19 / -0.06 | -0.18 / -0.09 | FAIL |
| `trail_atr_3.0` | oos | -1.12 / -0.47 | -0.94 / -0.30 | FAIL |
| `tp_runner_trail3` | is | -0.21 / -0.13 | -0.24 / -0.25 | FAIL |
| `tp_runner_trail3` | oos | -0.67 / -0.34 | -0.50 / -0.18 | PASS |

M1 protocol verdicts: baseline judged-OOS PASS (held-out 1/3), `tp_default`
PASS (2/3), `sl_atr_1.5` / `trail_atr_3.0` FAIL (0/3) — identical to the
documented table under both modes. One label moved vs documented:
**`tp_runner_trail3` judged-OOS FAIL → PASS** (held-out still 0/3). The flip
is already present under the `bar_close` control, so the *label* change is
fee-model-driven; #1271 additionally shifts that candidate's numbers (its
trailing stop is engine-tracked) without moving any verdict. With 0/3
held-out windows it remains a non-shipper, so the keep-baseline verdict is
unaffected.

---
Updated with LLM: Opus 4.8 | high | Harness: Claude Code
Updated with LLM: Fable 5 | high | Harness: Claude Code
