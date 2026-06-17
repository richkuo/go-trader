# range_scalper sample and gross-edge screen (#987)

Generated: 2026-06-17

## Reproduce

M1 protocol and held-out windows:

```bash
uv run --no-sync python backtest/eval_windows.py --strategy range_scalper --registry spot --json /tmp/range_scalper_987_m1.json
```

Focused M5 net-vs-gross screen:

```bash
uv run --no-sync python backtest/fee_audit.py --registry spot --strategies range_scalper --json /tmp/range_scalper_987_m5.json --markdown /tmp/range_scalper_987_m5.md
```

## Baseline

The original strategy-audit row left `range_scalper` at watch: -0.38 mean
Sharpe, -14.2% mean return, +34.4pts vs buy-and-hold, and only 9 trades across
the six BTC/ETH/SOL x 1h/4h audit runs. The OOS comparison was already worse
than holding: -38.3% OOS return, -7.9pts vs B&H.

The current-cache rerun keeps the same shape but makes the decision simpler:
the strategy is degenerate in every M1 window and remains gross-negative in M5.

## M1 Result

| window | Sharpe | bar | DDadj | bar | traded | verdict |
|---|---:|---:|---:|---:|---:|---|
| IS | -0.19 | -0.12 | -0.15 | -0.14 | 2/6 | DEGENERATE |
| OOS | -0.66 | -0.55 | -0.28 | -0.37 | 2/6 | DEGENERATE |
| 2023 | 0.47 | 1.21 | 0.74 | 2.68 | 2/6 | DEGENERATE |
| 2024 | 0.20 | 0.90 | 0.22 | 1.07 | 2/6 | DEGENERATE |
| 2025H1 | 0.12 | -0.42 | 0.08 | -0.37 | 1/6 | DEGENERATE |

Protocol OOS is degenerate and the held-out gate passes 0/3 windows. The few
profitable 2023/2024 BTC/ETH 1h legs do not form a M1-valid sample because at
least four of six datasets are zero-trade in every window.

## M5 Result

| strategy | reg | trades | trades/yr | gross %/leg | net %/leg | fee drag | verdict |
|---|---|---:|---:|---:|---:|---:|---|
| range_scalper | spot | 7 | 0.9 | -7.46 | -10.36 | 2.89pp | `deprecate` |

One comparable leg was skipped by M5 because the zero-friction run changed the
trade count (`BTC/USDT 1h`, IS: 8 vs 4 trades). The remaining comparable legs
are still enough for the decision gate: gross edge is negative, so fee-drag
tuning cannot rescue the default entry.

## Condition Funnel

Default params: `bb_period=14`, `bb_std=1.5`, `bw_threshold=0.008`,
`vol_ratio=0.8`, `rsi_period=7`, `rsi_ob=70`, `rsi_os=30`.

| window | bars | bandwidth pass | low-volume range | band cross in range | RSI-confirmed signals | closed trades | traded datasets |
|---|---:|---:|---:|---:|---:|---:|---:|
| IS | 18,456 | 1,599 | 1,034 | 119 | 17 | 5 | 2/6 |
| OOS | 15,045 | 1,345 | 881 | 106 | 20 | 6 | 2/6 |
| 2023 | 32,832 | 5,406 | 2,828 | 204 | 24 | 7 | 2/6 |
| 2024 | 32,946 | 1,959 | 1,216 | 143 | 16 | 4 | 2/6 |
| 2025H1 | 16,296 | 885 | 569 | 66 | 7 | 2 | 1/6 |

The bottleneck is not just one predicate. On 4h, the 0.8% Bollinger bandwidth
gate is almost always closed. On 1h, the range/cross stages still create some
opportunities, but RSI confirmation collapses them into a small number of
signals, and the long/flat harness converts only paired buy/sell sequences into
closed trades. The strategy source also says it was designed for 1m-5m, which
does not match the audited 1h/4h production surface.

## Verdict

Move `range_scalper` to the deprecation list for the default strategy-audit
surface. Do not spend M3 or composite-regime-gate work on the current default:
the sample is M1-degenerate in every window and the M5 gross edge is already
negative. A future lower-timeframe experiment should be a new scoped issue with
data support and a separate validation bar, not a rescue of this default row.
