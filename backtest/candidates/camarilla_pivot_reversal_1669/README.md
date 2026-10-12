# camarilla_pivot_reversal_1669

Study for the no-edge `camarilla_pivot_reversal` entry evaluator (issue 1669, tracking group
`hyperliquid-strategy-candidates-2026-10-02`). The verdict and every number are in `REPORT.md`; this file lists
what each artifact is for and how to reproduce the result. Everything here is suggest-only: no command writes
deployed configuration, live defaults or acknowledgements, and none sends an order.

| file | role |
|---|---|
| `protocol.json` | Frozen protocol: the fixed formula (nothing is tuned), the matched contract, the three comparators, the windows, the 300-bar warm-up policy, the sample floor, the funding rule and the pass/fail/inconclusive rules. |
| `protocol_lock.json` | Written by `run_study.py lock` before the first score; `run` and `parity` refuse a `protocol.json` whose sha256 differs. |
| `study_manifest.json` | The `dual_thrust_1642` frozen Binance US 1h/4h candles and Hyperliquid funding (proxy evidence), read in place under `backtest/candidates/dual_thrust_1642/data` with every sha256 checked against this file and the pinned source manifest. |
| `../camarilla_pivot_reversal_1669_manifest.json` | `offline_candle_manifest/v1` venue manifest: the `dual_thrust_1642` Hyperliquid 4h candles, funding, venue meta and L2 snapshot, referenced in place through manifest-relative paths (venue evidence with the execution specification). |
| `paper_parity_<interval>.json` | Explicit-paper go-trader configurations for every supported interval (15m, 30m, 1h, 2h, 4h, 8h, 12h): BTC, the matching `expected_interval_ms`, `tiered_tp_pct` +3%, `stop_loss_atr_mult: 1.0`, `direction: both`, `closed_bar_decisions: true`. The Go loaders accept them as unacknowledged paper. |
| `discovery_{before,after}_{futures,spot}.json` | `--list-json` output captured before and after registration; byte-identical, because the candidate is hidden from discovery. |
| `run_study.py` | `lock`, `hash`, `run` (offline: rebuilds a fresh cache from the frozen files, verifies every hash, disables network acquisition, installs the study-local reclaim comparator into the research registry instance for the run only, scores the three groups through the shared M1 and manifest functions), `parity`, `render`. |
| `results.json`, `parity_results.json` | Full outputs: every arm and window, held-out diagnostics, the entry audit, start-of-window holds, verdict guards and parity runs. |

## Reproduction

```
uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py lock
uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py run --cache-db /tmp/cpr1669_run.db
uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py parity --cache-db /tmp/cpr1669_parity.db
uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py render
```

`lock` only confirms the recorded lock once it exists. `run` and `parity` refuse an existing cache file, so pass
a new path each time. Every arm of a window loads the same 300 verified bars before the window start; the 2023
proxy window has no prehistory and runs with no warm-up, so its first complete prior day holds with
`insufficient_history`. Hyperliquid funding starts in May 2023, so proxy 2023 legs that hold positions before the
first rate are funding_incomplete and that window can never support a pass.
