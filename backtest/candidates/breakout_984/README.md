# breakout (futures) close-stack work — entry frozen (#984, M2 + M3)

Application of **M2 (close-stack co-optimization)** and **M3 (exit-quality
diagnostics)** to the registry's second-ranked entry (#956: +47.5pts vs B&H,
-52.2% worst max DD on the default open-signal-as-close stack, 260 trades).
Entry logic and params are frozen at futures-registry defaults throughout;
only the close stack is swept. Same treatment as `squeeze_983/` (the #983
negative-result precedent), with two breakout-specific deltas: every driver
loads the **futures registry** (`breakout` is absent from spot) and pins
**`direction="long"`** (`breakout` emits `signal=-1` on breakdowns but is not
in `bidirectionalPerpsStrategies`; with close refs the open/close engine path
would open shorts on raw `-1`, #996).

**Result: negative — same verdict as #983, reached by a different route.**
Unlike squeeze_momentum (where nothing beat baseline even in-sample), breakout
had genuine IS winners and a walk-forward-stable ladder family; every one of
them then failed the judged protocol OOS window and/or collapsed on the
continuous audit frame. The candidates that DO fix the held-out bull years
(`tp_tight` 2/3, `time_stop_250` 3/3) are precisely the ones that amputate
the 2026 crash-window outperformance the keep verdict rests on. Per #995
step 4 a negative result is documented, not deleted.

**Structural note (read before comparing rows).** The baseline exit *is* the
breakdown signal (`signal=-1` closes the long on the plain long/flat path).
Under the pinned `direction="long"`, stacks split into two families:
stop-only / trail-only stacks (no close refs) stay on the plain path and
**keep** the breakdown exit, adding a stop on top; any stack with close refs
engages the open/close engine, which masks `-1` to 0 — those stacks **replace**
the breakdown exit entirely. The `atr_stop` screen (worst DD -61…-65% across
the whole plateau) is the controlled experiment: swapping the breakdown exit
for a pure stop is strictly harmful on this entry.

Every committed artifact regenerates from the repo root with the cache DB
present (`shared_tools/trading_bot.db`) via one of the six committed drivers
(`sweep_close_stacks.py`, `sweep_supplementary.py`, `walkforward.py`,
`validate_shortlist.py`, `audit_headline.py`, `fee_drag.py`) or the documented
`eval_windows.py` / `exit_diagnostics.py` command — each section below states
its exact command (all with `--registry futures` where the harness takes it).
Protocol windows (`is`, `oos`, held-out) are date-pinned; the continuous audit
window ends at the latest cache by default, so `audit_headline.py` records the
effective per-dataset data range in its artifact — diff that against the
committed file before comparing numbers regenerated under a fresher cache.

## Baseline reproduction (M1 step 1)

```
uv run --no-sync python backtest/candidates/breakout_984/audit_headline.py \
    --candidates baseline.json,trail_atr_3.0.json,tp_tight.json,tp_tight_trail3.json,time_stop_250.json \
    --json backtest/candidates/breakout_984/audit_window_headline.json
```

`baseline.json` on the continuous audit window (2025-06-10 → latest cache,
six audit datasets) reproduces #956 almost exactly — mean Sharpe +0.02
(audit +0.01), mean return -1.08% (-1.1%), worst max DD **-52.17%** (-52.2%),
**260 trades (260)**; vs B&H +43.7pts vs the audit's +47.5 — the residual is
cache drift (B&H moves with the extra bars; the committed
`effective_range` block pins last bars 2026-06-04 → 2026-06-12 per dataset).

## Step 1 — diagnose (where the -52.2% DD accumulates)

```
uv run --no-sync python backtest/exit_diagnostics.py \
    --strategy breakout --registry futures --windows is,oos \
    --json backtest/candidates/breakout_984/diagnostics.json
```

- **Winners are slow trend-holds.** IS hold buckets: 51+ bars = 68/148 trades,
  65% win rate, net **+283.8%**; every bucket under 51 bars is net negative
  (21–50: 18% win, -154.3%; 6–20: 0% win, -102.2%). OOS repeats it
  (51+ bucket: 82% win, +121.8%).
- **Late giveback dominates the bleed**: 56.8% of IS trades (84/148,
  net -113.7%) and 62.4% OOS — the breakdown exit surrenders a chunk of the
  move before firing. Early reversal is secondary (17.6% IS, net -83.8%).
- **Stops have no safe room**: aggregate median MAE is already 2.3–4.5 ATR
  across datasets (p80 4.2–6.4 ATR); winners-only median MAE is ~0.8–1.4 ATR
  but with a p80 of ~5.5 ATR — a stop tight enough to cut the losers'
  drawdown sits inside the adverse excursion of a fat slice of eventual
  winners.
- Fee churn is minor on the baseline (≤4% of trades) but breakout's edge is
  already fee-marginal (M5: gross +5.28%/leg vs net -1.54%/leg, 6.82pp drag
  over these same 260 trades — `docs/research/fee-audit-m5.md`), so any stack
  that adds legs starts in debt.

## Step 2 — M2 close-stack screen on IS (selection window only)

`sweep_close_stacks.py` expands the #996 default grid
(`DEFAULT_CLOSE_STACK_SPECS`, 25 stacks: baseline, fixed ATR stops, trailing
stops, three tiered-TP ladders × optional SL/trail) and scores each on the six
audit datasets over the protocol IS window with the entry frozen:

```
uv run --no-sync python backtest/candidates/breakout_984/sweep_close_stacks.py \
    --window is --json backtest/candidates/breakout_984/sweep_is.json
```

| IS rank (mean DDadj) | DDadj | Sharpe | ret% | worst DD% | #T |
|---|---:|---:|---:|---:|---:|
| tiered_tp_atr[1×:0.5, 2×:0.8, 3×:1] trail_atr=3 | +0.836 | -0.01 | +3.02 | -35.3 | 558 |
| trail_atr=3 | +0.453 | +0.09 | +6.01 | -39.8 | 202 |
| tiered_tp_atr[1×:0.5, 2×:0.8, 3×:1] | +0.371 | +0.34 | +4.20 | -45.3 | 148 |
| baseline (open-signal-as-close) | +0.308 | -0.06 | +1.94 | -52.2 | 148 |
| … every other ladder/stop/trail combo | ≤ +0.229 | | | -27.1…-53.4 | 123–670 |

Unlike squeeze_983, real IS winners exist — but the top stack already shows
the fee red flag (558 trades, 3.8× baseline: each ladder tier-out is followed
by a fresh breakout re-entry). `trail_atr=3` is the cleanest single mechanism
(keeps the breakdown exit, +6% return, -39.8% worst DD, modest 202 trades).

Supplementary screens (same harness; generator `sweep_supplementary.py`,
every artifact row embeds its full stack spec):

```
uv run --no-sync python backtest/candidates/breakout_984/sweep_supplementary.py \
    --screen <sweep2|timestop|ratchet|atrstop|zscore> \
    --json backtest/candidates/breakout_984/<artifact>.json
```

- `sweep2_is.json` — ladder plateau, wide trails (4/5 ATR), time stops,
  ladder+wide-stop combos: `tp_tight` (+0.371) and `trail_atr_4` (+0.253)
  lead; the trail plateau is one-sided (t2 -0.230, t2.5 -0.301, t3 +0.453,
  t4 +0.253, t5 -0.013 — a t3/t4 shelf, not a broad plateau).
- `timestop_plateau_is.json` — `max_bars` ∈ {75…400}: only 250 is positive
  (+0.094) with **both neighbors negative** (225 -0.239, 300 -0.267) — a
  single-cell spike, not a plateau; carried to judgment only to document it.
- `ratchet_screen_is.json` — `trailing_tp_ratchet` (default + wide rungs ×
  opening trails; wide rungs start at 3.0 ATR so wide is screened at trails
  4/5 only): best +0.049 (`ratchet_def_t3`), rest negative — the ratchet
  replaces the breakdown exit and pays the same churn.
- `atrstop_plateau_is.json` — `atr_stop` evaluator 2–5 ATR × entry/live:
  **uniformly bad** (DDadj -0.24…-0.37, worst DD -61…-65%) — the
  exit-replacement control described above.
- `zscore_screen_is.json` — stretch exits z 1.5–3 × lookback 20/50: best
  +0.143 with Sharpe -0.82 (return -10.4%); rejected at screen.

## Step 3 — M2 walk-forward fold stability (frozen entry)

```
uv run --no-sync python backtest/candidates/breakout_984/walkforward.py \
    --json backtest/candidates/breakout_984/walkforward_folds.json
```

`walk_forward_optimize` with singleton open-param ranges (futures-registry
defaults), the 25-stack grid, `dd_adjusted_return` selection,
`direction="long"`, 5 folds over 2023-01-01 → 2026-01-01 on BTC/ETH/SOL 1h
(`walkforward_folds.json`): the tiered-TP ladder family wins 13/15
train-folds — the tight ladder `[1×:0.5, 2×:0.8, 3×:1]` alone takes 7 —
baseline 2, trailing/fixed stops 0. Consistent with the IS screen: the ladder
family is the stable per-window pick. (Fold test legs are mixed — e.g. the
tight ladder's three BTC test folds span +22.9% to -21.0%.)

## Step 4 — judge through the M1 protocol (#994)

```
uv run --no-sync python backtest/eval_windows.py --registry futures \
    --candidate-json backtest/candidates/breakout_984/<candidate>.json
```

(`validate_shortlist.py` runs the same `eval_windows` functions for a list of
candidates in one process so the incumbent bars are computed once. The
default shortlist emits `validation.json`; `validation_timestop.json` is
`--candidates time_stop_250.json`.)

| Candidate | IS | OOS (judged) | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| baseline | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |
| `trail_atr_3.0` | PASS | **FAIL** | FAIL | FAIL | FAIL | 0/3 |
| `tp_tight` (1×:0.5, 2×:0.8, 3×:1) | PASS | **FAIL** | FAIL | PASS | PASS | 2/3 |
| `tp_tight_trail3` | PASS | **FAIL** | FAIL | FAIL | FAIL | 0/3 |
| `time_stop_250` | PASS | **FAIL** | PASS | PASS | PASS | 3/3 |

- **Every non-baseline candidate fails the judged OOS window** — the 2026
  bear window is where breakout's +23.6pt OOS keep verdict (#956) lives, and
  it is exactly what the exits give away: baseline OOS Sharpe -0.21 vs bar
  -0.75; `trail_atr_3.0` -1.26, `tp_tight` -1.63, `time_stop_250` -1.81.
- The inversion is the finding: `tp_tight` and `time_stop_250` improve the
  bull-year held-outs (2/3, 3/3 vs baseline 0/3) by banking trends early —
  and that same behavior amputates the crash-window outperformance. One close
  stack cannot serve both regimes on this entry.
- `time_stop_250` was already flagged a single-cell IS spike; its 3/3
  held-out run does not rehabilitate it (OOS Sharpe -1.81 is the shortlist's
  worst; stitched frame below −68% DD).

## Step 5 — the catch, repeated: continuous-window headline

Re-running the shortlist on the **continuous** audit window (2025-06-10 →
latest, the #956 frame; `audit_window_headline.json`, generated by the
step-1 command):

| | mean Sharpe | mean ret | vs B&H | worst DD | #T |
|---|---:|---:|---:|---:|---:|
| baseline | **+0.02** | **-1.1%** | **+43.7pts** | -52.2% | 260 |
| `trail_atr_3.0` | -0.33 | -9.4% | +35.5pts | **-52.3%** | 364 |
| `tp_tight` | -0.26 | -10.5% | +34.4pts | **-69.0%** | 148 |
| `tp_tight_trail3` | -0.57 | -11.9% | +32.9pts | -48.6% | 961 |
| `time_stop_250` | -0.39 | -25.3% | +19.6pts | **-68.4%** | 109 |

The IS story evaporates stitched. `trail_atr_3.0` — the best-behaved IS
candidate — ends with the **same** worst DD as baseline (-52.3% vs -52.2%)
while surrendering 8pts of edge to 40% more trades. `tp_tight` *deepens* the
worst DD to -69.0% (the squeeze_983 `tp_default` collapse, same mechanism:
the ladder is fully out by 3 ATR, amputating the fat-tail trends, while its
lack of a breakdown exit holds residual losers longer). `tp_tight_trail3`
runs 961 trades — 3.7× baseline. Nothing here ships.

## Step 6 — fee-drag gate (breakout-specific)

```
uv run --no-sync python backtest/candidates/breakout_984/fee_drag.py \
    --candidates baseline.json,trail_atr_3.0.json,tp_tight.json,tp_tight_trail3.json \
    --json backtest/candidates/breakout_984/fee_drag_shortlist.json
```

Each candidate re-run twice per dataset on the continuous window — default
friction vs zero friction (the #999 overrides `commission_pct=0.0,
slippage_pct=0.0`). Numbers are means of per-dataset **total returns** (not
the M5 per-leg means, so they are not directly comparable to the 6.82pp M5
row — the direction is what matters):

| | gross ret | net ret | drag | #T | T/yr |
|---|---:|---:|---:|---:|---:|
| baseline | +13.3% | -1.1% | 14.3pp | 260 | 43.7 |
| `trail_atr_3.0` | +8.4% | -9.4% | 17.7pp | 364 | 61.2 |
| `tp_tight` | +3.1% | -10.5% | 13.6pp | 148 | 24.9 |
| `tp_tight_trail3` | +12.9% | -11.9% | 24.8pp | 961 | 161.5 |

Confirms the M5 shape (gross edge real, friction eats it) and the gate: the
trail adds 3.4pp of drag for zero stitched DD benefit; the trail+ladder combo
burns 24.8pp; `tp_tight` cuts trades but cuts gross return even faster.

## Verdict

1. **No close stack in the swept space meets the #984 target** (cut the
   -52.2% worst DD materially without giving up the +47.5pt vs-B&H edge).
   The space swept, all on the frozen entry: 25-stack #996 default grid,
   ladder plateau, fixed stops 1.5–4 ATR, trailing stops 2–5 ATR, `atr_stop`
   evaluator 2–5 ATR × entry/live, time stops 75–400 bars, trailing-TP
   ratchets (default + wide rungs × opening trails 3/4/5), z-score stretch
   exits (z 1.5–3 × lookback 20/50), and ladder×stop/trail combos.
2. Mechanism: breakout's PnL is **slow trend-holds** (51+ bar trades carry
   the entire edge; everything shorter is net negative) and its keep verdict
   is **crash-window outperformance** (protocol OOS +23.6pts in a -30% B&H
   window). Exits that bank earlier fix the bull-year held-outs and destroy
   the OOS edge — the two failure modes are the same trades. Stops/trails
   add re-entry churn on a fee-marginal entry (M5: 6.82pp drag at 260
   trades); replacing the breakdown exit with any pure stop is strictly
   harmful (`atr_stop` control).
3. The **entry stays validated**: baseline is the only shortlist member that
   passes protocol IS + OOS. Keep breakout's default stack
   (open-signal-as-close, no SL/TP) as-is. Note baseline's held-out record is
   0/3 — its value is concentrated in bear/crash windows, which is regime
   information, not exit information.
4. The -52.2% DD is **regime exposure, not exit quality** — it accrues from
   staying long through bear windows the entry keeps re-entering (same
   conclusion as #983, reached with better in-sample candidates). The right
   tool is M4 regime-profile allocation (#998) / composite-regime gating on
   this entry; close-stack work on the frozen entry is exhausted by this
   sweep.

A negative result is a valid M1 finding — documented, not deleted (#995 step 4).

## Addendum 2026-07-05 — re-run under the corrected simulation geometry (#1243)

Re-ran the headline drivers (`audit_headline.py` continuous-window;
`validate_shortlist.py` M1 protocol; `fee_drag.py`) on current `main` (the
#1238 audit fixes plus #1250 fee-net-per-trade and #1251 canonical Sortino /
`None` profit-factor / half-open windows), identical cache snapshot. **The
keep-baseline verdict holds** — but one candidate, `tp_tight`, shifted
materially, so read the detail before reusing the step-5 numbers.

**Verdict unchanged, and why.** The decision rests on the M1 protocol: baseline
is the only shortlist member passing judged IS+OOS, and every non-baseline
candidate still FAILS the judged OOS (2026 crash) window — the window
breakout's keep verdict lives in. That table is unchanged: baseline OOS PASS
(held-out 0/3); `trail_atr_3.0` FAIL 0/3; `tp_tight` FAIL 2/3;
`tp_tight_trail3` FAIL 0/3; `time_stop_250` FAIL 3/3. `tp_tight`'s OOS mean
Sharpe is if anything slightly worse (-1.67 vs the documented -1.63), so the
decisive crash-window failure is intact. **No promotion guidance changes.**

**But the step-5 continuous-collapse claim for `tp_tight` does NOT reproduce.**
Under the corrected closed-bar EntryATR geometry, the tiered-TP ladder's fills
move and the stitched-frame collapse the doc headlines is gone:

| run | metric | documented | re-run | status |
|---|---|---:|---:|---|
| baseline | Sharpe / ret / vsB&H / worstDD / #T | +0.02 / -1.1% / +43.7 / -52.2% / 260 | +0.02 / -1.08% / +43.74 / -52.17% / 260 | identical |
| `trail_atr_3.0` | " | -0.33 / -9.4% / +35.5 / -52.3% / 364 | -0.495 / -14.86% / +29.97 / -51.62% / 371 | shifted worse |
| `tp_tight` | " | -0.26 / -10.5% / +34.4 / **-69.0%** / 148 | **+0.150 / +0.86% / +45.69 / -34.28%** / 148 | **shifted much better** |
| `tp_tight_trail3` | " | -0.57 / -11.9% / +32.9 / -48.6% / 961 | -0.720 / -15.62% / +29.21 / -47.89% / 963 | shifted worse |
| `time_stop_250` | " | -0.39 / -25.3% / +19.6 / -68.4% / 109 | -0.386 / -25.27% / +19.56 / -68.36% / 109 | identical |

`tp_tight` keeps the same 148 entries (the entry set is frozen), so the whole
move is exit-geometry: on the stitched continuous frame it goes from a -69.0%
worst-DD, -10.5%-return collapse to a **-34.28% worst DD, +0.86% return, +45.69
vs-B&H** profile essentially level with baseline. `fee_drag.py` corroborates —
its net flips from a documented -10.5% to +0.86% with drag collapsing 13.6pp →
1.5pp — while `trail_atr_3.0`'s gross edge instead erodes (8.4% → 1.47%). So the
specific documented narrative "`tp_tight` deepens the worst DD to -69.0%,
amputating the fat-tail trends" is **refuted under corrected geometry**; the
generic step-5 methodology lesson (per-window PASS can hide a stitched-frame
problem) is unaffected — it still holds for other candidates and studies.

**Net:** the promotion decision (keep baseline; do not ship `tp_tight`) is
unchanged because it is driven by the judged-OOS crash-window failure, not by
the stitched frame. `tp_tight` is now the candidate to revisit **if and only
if** breakout's keep rationale is ever reweighted away from crash-window
outperformance — but that is a hypothetical, not a flip today, so no follow-up
decision issue is filed. The corrected `tp_tight` continuous numbers above
supersede the step-5 table for any future comparison.

## Addendum 2026-07-10 — re-run under intra-bar stop resolution + corrected HL fees (#1294)

Re-ran `audit_headline.py` (baseline; `tp_tight` + `trail_atr_3.0`) and
`validate_shortlist.py` (default shortlist) on current `main` — #1271
`ohlc_walk` intrabar default and the #1320 audit fee-model switch — under BOTH
`--intrabar-resolution` modes (the drivers now thread the flag, #1294),
identical cache snapshot (last bars 2026-06-04 → 2026-06-12, zero drift).
**The keep-baseline verdict holds.**

M1 protocol table (default shortlist): baseline OOS PASS (held-out 0/3),
`trail_atr_3.0` FAIL (0/3), `tp_tight` FAIL (2/3), `tp_tight_trail3` FAIL (0/3)
— **identical to the documented table, in both modes.** The decisive
judged-OOS crash-window failure of every non-baseline candidate is intact, so
no promotion guidance changes.

Two shortlist candidates arm engine-tracked stops and are therefore
#1271-reached: `trail_atr_3.0` (bare trailing stop) and `tp_tight_trail3`
(tiered TP + `trailing_stop_atr_mult` 3.0, the analog of squeeze_983's
`tp_runner_trail3`). Both have mode-different numbers; `baseline` and
`tp_tight` (evaluator ladders only) reproduce byte-identically across the
mode pair. Continuous audit window:

| run | metric | documented (2026-07-05) | `bar_close` re-run | `ohlc_walk` re-run | attribution |
|---|---|---:|---:|---:|---|
| baseline | Sharpe / ret / vsB&H / worstDD / #T | +0.02 / -1.08% / +43.74 / -52.17% / 260 | +0.145 / +3.87% / +48.70 / -51.80% / 260 | identical to bar_close | fee model only |
| `tp_tight` | " | +0.150 / +0.86% / +45.69 / -34.28% / 148 | +0.203 / +1.96% / +46.79 / -34.22% / 148 | identical to bar_close | fee model only (evaluator ladder, no engine stop) |
| `trail_atr_3.0` | " | -0.495 / -14.86% / +29.97 / -51.62% / 371 | -0.288 / -9.34% / +35.49 / -50.13% / 371 | **-0.433 / -10.54% / +34.29 / -47.50% / 412** | fee model (bar_close column) + intra-bar walk (delta between the two re-run columns) |

The `trail_atr_3.0` mode-pair isolates #1271 cleanly: the intra-bar walk
converts stop touches into same-bar trigger-price exits (#T 371 → 412), trims
the worst DD (-50.13% → -47.50%) and gives back return/Sharpe — the expected
"strictly more conservative for tight stops" direction. It stays a deep M1
FAIL either way. `tp_tight_trail3`'s mode pair (M1 pooled windows, Sharpe /
DDadj): IS +0.09 / +0.70 under `bar_close` → **-0.39 / +0.58** under
`ohlc_walk`; OOS -1.11 / -0.62 → -0.98 / -0.58. Its pooled-IS window label
flips PASS → FAIL under the intra-bar walk, but the protocol verdict is
unchanged in both modes (judged-OOS FAIL, held-out 0/3, matching the
documented table). The #1243 finding that `tp_tight`'s documented continuous
collapse was refuted stands (it is intrabar-unreached; only fees moved it).

---
Updated with LLM: Opus 4.8 | high | Harness: Claude Code
Updated with LLM: Fable 5 | high | Harness: Claude Code
