# liquidity_sweeps short-leg screen (#1022)

Generated: 2026-06-17

## Reproduce

M1 short-leg protocol and held-out windows:

```bash
uv run --no-sync python backtest/eval_windows.py --strategy liquidity_sweeps --registry spot --direction short --json /tmp/liquidity_sweeps_1022_m1_short.json
```

## Baseline

The validation target in #1022 is the committed PR #1003 M5 row in
`docs/research/fee-audit-m5.md`: spot `liquidity_sweeps`, long leg only,
126 trades, 21.4/yr, -16.94% gross, -19.63% net, verdict
`unscreened_short`.

The short leg is the only remaining rescue path because `liquidity_sweeps`
emits bearish sweep fades as `signal=-1`, and the historical long/flat harness
dropped those entries.

## M1 Short-Leg Result

| direction | IS | OOS | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| short | PASS | **PASS** | FAIL | FAIL | PASS | 1/3 |

| window | Sharpe | bar | DDadj | bar | traded | liquidated |
|---|---:|---:|---:|---:|---:|---:|
| IS | -0.20 | -0.20 | -0.12 | -0.22 | 6/6 | 0 |
| OOS | 0.70 | -0.55 | 0.62 | -0.37 | 6/6 | 0 |
| 2023 | -34.14 | 1.21 | -0.75 | 2.68 | 6/6 | 2 |
| 2024 | -17.47 | 0.90 | -0.81 | 1.07 | 6/6 | 1 |
| 2025H1 | 0.72 | -0.42 | 0.45 | -0.37 | 6/6 | 0 |

Protocol OOS passes, but the held-out gate does not: the short leg fails the
2023 and 2024 windows and liquidates on SOL/USDT in both stress windows.

## Verdict

Move `liquidity_sweeps` to the deprecation list for the default strategy set.
The short leg has a current OOS bear-window edge, but it does not independently
clear the held-out windows required by #1022 and is not a clean rescue for the
-16.94% gross long leg. Do not open threshold-tuning or close-stack follow-ups.
