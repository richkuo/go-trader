# dual_thrust_1642

Study for the no-edge `dual_thrust` entry evaluator (issue 1642, tracking group
`hyperliquid-strategy-candidates-2026-10-02`). The verdict and every number are in `REPORT.md`; this file lists
what each artifact is for and how to reproduce the result. Everything here is suggest-only: no command writes
deployed configuration, live defaults or acknowledgements, and none sends an order.

| file | role |
|---|---|
| `protocol.json` | Frozen protocol: parameter bounds, the 80-point grid, the matched contract, windows, selection rule, sample floors, funding rule and pass/fail/inconclusive rules. |
| `protocol_lock.json` | Written by `run_study.py lock` before the first score; `run` and `parity` refuse a `protocol.json` whose sha256 differs. |
| `spec_1h.json`, `spec_4h.json` | Seed candidate specs for `eval_windows.py --registry futures --candidate-json` (direction `both`, one explicit `tiered_tp_pct` close, one 1.0 ATR stop, the group's `expected_interval_ms`). |
| `study_manifest.json`, `data/proxy/` | Byte-identical copies of the `chande_momentum_1661` frozen Binance US 1h/4h candles and Hyperliquid funding (proxy evidence), with their sha256 values and copied-from provenance. |
| `venue_manifest.json`, `data/venue/` | Byte-identical copies of the `aroon_trend_1643` Hyperliquid 4h candles, funding, venue meta and L2 snapshot in the `offline_candle_manifest/v1` schema (venue evidence with the execution specification). |
| `paper_parity_1h.json`, `paper_parity_4h.json` | Explicit-paper go-trader configurations (BTC, `tiered_tp_pct` +3%, `stop_loss_atr_mult: 1.0`, `direction: both`, `closed_bar_decisions: true`) used for decision parity; they load in the Go loader as unacknowledged paper. The Go loaders refuse any `dual_thrust` reference without `closed_bar_decisions: true`. |
| `discovery_{before,after}_{futures,spot}.json` | `--list-json` output captured before and after registration; identical, because the candidate is hidden from discovery. |
| `run_study.py` | `lock`, `hash`, `run` (offline: rebuilds a fresh cache from `data/proxy/`, verifies every hash and both copied-from manifests, disables network acquisition, scores the three groups through the shared M1 and manifest functions), `parity`, `render`. |
| `results.json`, `parity_results.json` | Full outputs: every training variant and exclusion, held-out arms, diagnostics for every variant, entry audit, start-of-window holds, parity runs. |

## Reproduction

```
uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py lock
uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py run --cache-db /tmp/dt1642_run.db
uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py parity --cache-db /tmp/dt1642_parity.db
uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py render
```

`lock` only confirms the recorded lock once it exists. `run` and `parity` refuse an existing cache file, so pass
a new path each time. Proxy windows load only their own candles, so the first `lookback_sessions + 1` sessions of
each window hold with `insufficient_history`; `results.json` records those holds per leg. Hyperliquid funding starts
in May 2023, so proxy 2023 legs that hold positions before the first rate are funding_incomplete and that window
can never support a pass.
