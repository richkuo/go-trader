# analog_retrieval M1 baseline (#1138) — negative on defaults

Prototype of the LLM-free core of arXiv 2502.05878 ("Retrieval-augmented LLMs
for Financial Time Series Forecasting"): k-NN retrieval over a scale-free
per-bar state vector (return efficiency, ATR-normalized momentum, ATR%, vol
regime, trend), voting the retrieved strictly-prior windows' realized forward
returns into a t-stat- and ATR-edge-gated direction signal. Strategy is
registered **backtest-only** (`backtest_only=True`, discovery-hidden, refused
by every live check script) — see `shared_strategies/open/analog_retrieval.py`
for the walk-forward leakage invariant.

## M1 incumbent-relative result (registry defaults, spot harness, 2026-07-02)

Full protocol run — `eval_windows.py` default windows/datasets, incumbent-median
bar per (window, dataset):

| window | Sharpe | bar   | DDadj | bar   | verdict |
|--------|-------:|------:|------:|------:|---------|
| is     | -0.41  | -0.23 | -0.23 | -0.24 | FAIL    |
| oos    | -1.36  | -0.48 | -0.66 | -0.34 | FAIL    |
| 2023   |  0.64  |  1.28 |  0.98 |  2.69 | FAIL    |
| 2024   |  0.23  |  0.90 |  1.22 |  1.07 | FAIL    |
| 2025H1 | -0.56  | -0.42 | -0.35 | -0.37 | FAIL    |

**protocol OOS: FAIL; held-out windows passed: 0/3.** Per-dataset beats were
sparse and unstable (1–2 of 6 per held-out window; the occasional 4h SD beat —
BTC 4h 2024 Sharpe 1.95 vs bar 1.49, SOL 4h 2024 1.70 vs 0.78 — does not
survive the window means). The 1h legs are consistently the drag: ~120–150
trades/window with fee-heavy churn and deep max-DD (40–55%).

The #1054 `gross_edge_noise.py` adjudication is not triggered: it gates
`graduate_m1` verdicts before selectivity work, and there is no `graduate_m1`
verdict here — the baseline is a plain FAIL.

## Interpretation

- Raw distance retrieval on default params does not clear the incumbent bar —
  consistent with the paper's own finding that a *learned* matcher (FinSeer)
  is what beats distance similarity; the trained matcher is explicitly out of
  scope for this prototype (#1138 non-goal).
- The strategy trades far too often on 1h (gates too loose at defaults);
  the 4h legs are closer to the bar. Any follow-up should sweep
  `min_t_stat` / `min_edge_atr` / `horizon` (M1 step 6 plateau on the OOS
  window) before any conclusion about the retrieval idea itself, and run the
  #1054 noise gate on any leg that graduates.

## Reproduce

```
uv run --no-sync python backtest/eval_windows.py --strategy analog_retrieval \
  --json /tmp/m1-analog-retrieval.json

uv run --no-sync python backtest/run_backtest.py --strategy analog_retrieval \
  --symbol BTC/USDT --timeframe 1h --mode single
```

Verdict: strategy stays backtest-only research; no promotion candidate.
Promotion to live would additionally require explicit human sign-off (#1138).

---
Created with LLM: Fable 5 | high | Harness: Claude Code
