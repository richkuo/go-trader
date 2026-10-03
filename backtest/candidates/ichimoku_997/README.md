# M3 exit-quality reference application — `ichimoku_cloud` (#997 / #986)

The reference application for **Methodology M3** (holding-time / excursion
diagnostics driving time/ATR/z-target exits). It proves the methodology
end-to-end on the cleanest audit case and records a **negative result**: no
exit-quality knob rescues `ichimoku_cloud`'s out-of-sample edge.

Reproduce any number below from the repo root.

## Step 1 — diagnose (where the PnL dies)

```
uv run --no-sync python backtest/exit_diagnostics.py \
    --strategy ichimoku_cloud --windows is,oos \
    --json backtest/candidates/ichimoku_997/diagnostics.json
```

Findings (six audit datasets, IS + OOS):

- **Late giveback dominates** — 50–100% of trades on most datasets. Winners
  reach a large favourable excursion (median MFE 4–15%) but peak *late*
  (`bars_to_mfe` 32–91 bars) and give back 2.5–11% before the open signal
  exits. The 51+ bar holding bucket carries most trades and is a net loser on
  several datasets (e.g. SOL 1h 51+: −42%).
- **Early reversal** is the secondary sink — net losers that run straight
  against the entry, MAE p80 ≈ 5–6 ATR.
- Fee churn is minor (< 9% of trades).

Mechanism mapping (M1 step 4 — each an independent default-off knob):

| Bleed mode      | Mechanism        | Candidate(s) |
|-----------------|------------------|--------------|
| late giveback   | `time_stop`      | `time_stop_50` |
| early reversal  | `atr_stop`       | `atr_stop_3.0`, `atr_stop_4.0` |
| fade/stretch    | `zscore_target`  | `zscore_2.0` |
| combined        | atr + time       | `combo_atr3_time50` |

## Step 2 — score through the M1 harness (#994)

```
uv run --no-sync python backtest/eval_windows.py \
    --candidate-json backtest/candidates/ichimoku_997/<candidate>.json \
    --windows is,oos
```

Every candidate JSON hard-codes `"direction": "long"`. This is mandatory: with
`close_strategies` present the engine uses the open/close path where
`signal == -1` *opens a short*; only an explicit `direction: long` keeps the
candidate long-only and comparable to the long-leg incumbent bar. The close
ref then *replaces* the strategy's own signal exit (live-parity: a single
`CloseStrategy` owns the exit).

### Protocol verdict (incumbent-median bar)

| Candidate            | IS Sharpe (bar) | OOS Sharpe (bar) | Protocol OOS |
|----------------------|-----------------|------------------|--------------|
| baseline (no exits)  | +0.16 (−0.12) PASS | −1.39 (−0.71) | **FAIL** |
| `time_stop_50`       | −0.89 (−0.12)   | −1.13 (−0.71)    | **FAIL** |
| `zscore_2.0`         | −0.68 (−0.12)   | −1.14 (−0.71)    | **FAIL** |
| `atr_stop_3.0`       | −0.09 (−0.12)   | −1.74 (−0.71)    | **FAIL** |
| `atr_stop_4.0`       | −0.17 (−0.12)   | −1.59 (−0.71)    | **FAIL** |
| `combo_atr3_time50`  | −0.46 (−0.12)   | −1.73 (−0.71)    | **FAIL** |

### Held-out windows (M1 step 7), best knob vs baseline

| Window | baseline | `time_stop_50` |
|--------|----------|----------------|
| 2023   | FAIL     | FAIL           |
| 2024   | **PASS** | FAIL           |
| 2025H1 | PASS     | PASS           |

## Verdict

`ichimoku_cloud`'s edge is window-dependent: it passes in-sample and in the
trending 2024 / 2025H1 held-out windows but **fails the judged protocol OOS**,
exactly as the #956 audit flagged ("OOS edge evaporates"). The baseline
already fails OOS by a wide margin (−1.39 vs −0.71 bar); the exit-quality knobs
narrow the gap at best (`time_stop_50` −1.13, `zscore_2.0` −1.14) but none
clears the bar, and `time_stop` *degrades* the one held-out window (2024) the
baseline passed by cutting late-peaking winners short.

**Exit-quality refinement does not rescue `ichimoku_cloud`.** Per the M1
protocol it moves to the deprecation list (#986). Its window-disjoint behaviour
(passes trends, fails the protocol OOS chop) is a candidate for M4
regime-profile allocation (#998), not M3.

A negative result is a valid M1 finding — documented, not deleted (#995 step 4).
