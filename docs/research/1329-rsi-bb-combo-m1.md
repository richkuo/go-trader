# #1329 rsi_bb_combo — M1 incumbent-relative validation (negative result)

**Verdict: FAIL — not a promotion candidate.** `rsi_bb_combo` fails the M1
protocol in both its plain and designed (regime-gated) shapes, and is
dominated head-to-head by the incumbent `mean_reversion_pro`. The strategy
ships registered-but-unproven as a tunable baseline (the
`consolidation_range` precedent): loud loss warning in the registry
description, no config surface recommends it, wizard wires the composite
ranging gate by default. Per the #1054 convention the `gross_edge_noise`
step-2 gate only adjudicates `graduate_m1` verdicts — nothing graduated, so
it was not run.

## Runs (eval_windows.py, default six audit datasets, `ohlc_walk`)

All defaults: `bb_period=20, bb_std=2.0, rsi_period=14, oversold=30,
overbought=70, confirm_window=3`.

### 1. `rsi_bb_combo` plain (long/flat harness)

| window | Sharpe | bar | DDadj | bar | verdict |
|---|---|---|---|---|---|
| is | -0.06 | -0.05 | -0.17 | -0.10 | FAIL |
| oos | -1.33 | -0.41 | -0.69 | -0.32 | FAIL |
| 2023 | 1.28 | 1.56 | 2.31 | 4.09 | FAIL |
| 2024 | 0.18 | 0.96 | -0.00 | 1.22 | FAIL |
| 2025H1 | 0.42 | -0.33 | 0.26 | -0.31 | PASS |

Protocol OOS: **FAIL**; held-out windows passed: **1/3**.

### 2. `rsi_bb_combo` designed shape (composite ranging gate + tiered TP + 2×ATR SL, direction=both)

Candidate: `allowed_regimes=["ranging_quiet","ranging_volatile"]`,
`regime_windows_spec={"medium":{"classifier":"composite","period":30}}`,
`close_strategies=[tiered_tp_atr 2.0×/0.5 → 3.0×/1.0]`,
`stop_loss_atr_mult=2.0`.

| window | Sharpe | bar | DDadj | bar | verdict |
|---|---|---|---|---|---|
| is | 0.08 | -0.05 | 0.26 | -0.10 | PASS |
| oos | -0.69 | -0.41 | -0.19 | -0.32 | FAIL |
| 2023 | 0.09 | 1.56 | 0.60 | 4.09 | FAIL |
| 2024 | -0.06 | 0.96 | 0.46 | 1.22 | FAIL |
| 2025H1 | 0.74 | -0.33 | 1.53 | -0.31 | PASS |

Protocol OOS: **FAIL**; held-out windows passed: **1/3**.

### 3. Incumbent head-to-head: `mean_reversion_pro` (long/flat harness, defaults)

| window | Sharpe | bar | DDadj | bar | verdict |
|---|---|---|---|---|---|
| is | -0.14 | -0.05 | -0.11 | -0.10 | FAIL |
| oos | 0.68 | -0.41 | 0.78 | -0.32 | PASS |
| 2023 | 1.17 | 1.56 | 6.12 | 4.09 | FAIL |
| 2024 | 1.23 | 0.96 | 2.25 | 1.22 | PASS |
| 2025H1 | 0.08 | -0.33 | 0.08 | -0.31 | PASS |

Protocol OOS: **PASS**; held-out windows passed: **2/3**.

## Interpretation

- The issue's design question ("what does `rsi_bb_combo` do that
  `mean_reversion_pro` cannot?") gets an empirical answer: on the audit
  protocol, nothing — dropping the inline ADX gate in favor of the external
  composite regime gate does not recover the incumbent's OOS edge. The gate
  does improve risk-adjusted quality within the candidate (single-dataset
  BTC 4h: max drawdown -36% → -16%, profit factor 1.14 → 1.42; see PR 1330),
  but not enough to pass M1.
- The one consistent bright spot (2025H1 passes in both shapes, with the
  gated shape's DDadj 1.53 vs the incumbent's 0.08) is a single half-year
  window — exactly the sample-noise territory the #1054 gate exists for, and
  not actionable without a `graduate_m1` verdict.
- Operators who still want the BB-native parameterization must treat default
  params as a starting point for per-market tuning behind the wizard-wired
  composite ranging gate; any future promotion attempt re-runs this M1
  protocol plus `gross_edge_noise` on the tuned shape.

## Reproduce

```
uv run --no-sync python backtest/eval_windows.py --strategy rsi_bb_combo \
  --json /tmp/m1-rsibb-plain.json

# designed shape (candidate JSON as specced above)
uv run --no-sync python backtest/eval_windows.py \
  --candidate-json /tmp/rsibb-m1-gated.json --json /tmp/m1-rsibb-gated.json

uv run --no-sync python backtest/eval_windows.py --strategy mean_reversion_pro \
  --json /tmp/m1-mrpro.json
```

---
Created with LLM: Fable 5 | high | Harness: Claude Code
