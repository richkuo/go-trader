# calendar_conditioned_entry_1653

Study for the no-edge `calendar_conditioned_entry` entry evaluator (issue 1653, tracking group
`hyperliquid-strategy-candidates-2026-10-02`). The verdict and every number are in `REPORT.md`; this file lists
what each artifact is for and how to reproduce the result. Everything here is suggest-only: no command writes
deployed configuration, live defaults or acknowledgements, and none sends an order.

## Mechanism

- Signal timing: a decision on closed bar N is for entry at bar N+1 open. The next bar's UTC weekday/hour cell
  comes from the decision bar's opening time plus `expected_interval_ms`, never from a median spacing of the
  frame. Naive indexes are UTC, aware indexes are converted to UTC, and a native-close index is converted to the
  bar open before the cell is taken.
- Labels: `open[e + holding_bars] / open[e] - 1`, the same nominal entry and exit opens as
  `time_stop(max_bars=holding_bars)` in the simulator (it counts the entry bar in `bars_held` and fills the
  scheduled close at the next open). A label enters a table only if its exit open is at or before the refit bar.
- Refits: on closed-bar boundaries that are multiples of `refit_bars x expected_interval_ms` after
  1970-01-05T00:00:00Z. Every evaluation reconstructs the last scheduled table from the frame; nothing is cached
  across evaluations and nothing is written.
- Statistics: weekly cluster means per cell, a Newey-West error over the weekly series, a conservative effective
  sample, a cost hurdle applied after the sign, and a one-sided Bonferroni threshold over 168 cells x 2
  directions. `protocol.json` freezes every rule.
- Required history: `training_bars + refit_bars - 1` clean consecutive bars ending at the decision bar
  (`required_history_bars`). The walk-forward optimizer and both study paths use that span.
- Hold reasons (`cce_reason`): `insufficient_history`; row defects (`missing_timestamp`, `grid_misaligned`,
  `received_out_of_order`, `nonfinite_price`, `nonpositive_price`, `inconsistent_candle`); edge defects
  (`duplicate_timestamp`, `descending_timestamp`, `cadence_gap`); the same defects inside the required history as
  `history_<reason>`; cell holds (`below_threshold`, `empty_cell`, `insufficient_effective_obs`,
  `no_uncertainty_estimate`, `nonfinite_statistic`, `direction_tie`); frame holds (`missing_columns`,
  `missing_timestamps`, `undeclared_timing`, `timing_contradiction`); check-time holds (`decision_time_missing`,
  `decision_bar_stale`, `decision_bar_future`, `decision_boundary_mismatch`). A hold returns a zero-signal frame,
  so close and protective management continue.
- Registration: futures only, `short_entries=True`, `edge_status="no_edge"`, hidden from discovery; Go short
  name `cce`, futures platform row, bidirectional row, no-edge evidence, fixed-interval default 3600000 and the
  Hyperliquid closed-bar requirement. Explicit `--mode=paper` runs it; live use needs `allow_no_edge: true`. It
  is not in the frozen v20 acknowledgement roster.

## Artifacts

| file | role |
|---|---|
| `protocol.json` | Frozen protocol: parameters, the variant grid, statistics, contract, baselines, warm-up, windows, the sample floor, the economic correction, parity and the pass/fail/inconclusive rules. |
| `protocol_lock.json` | Written by `run_study.py lock` before the first score; `run` and `parity` refuse a `protocol.json` whose sha256 differs. |
| `study_manifest.json` | The `dual_thrust_1642` frozen Binance US candles and Hyperliquid funding (proxy evidence), read in place with every sha256 checked against this file and the pinned source manifest. |
| `../calendar_conditioned_entry_1653_manifest.json` | `offline_candle_manifest/v1` venue manifest: the `dual_thrust_1642` Hyperliquid 4h candles, funding, venue meta and L2 snapshot, referenced in place, with a 1133-bar warm-up and the `test` window only. |
| `paper_parity_1h.json`, `paper_parity_4h.json` | Explicit-paper go-trader configurations (BTC, the frozen parameters, one `time_stop` close, `stop_loss_atr_mult: 3.0`, `direction: both`, `closed_bar_decisions: true`); the Go loaders accept them as unacknowledged paper. |
| `discovery_{before,after}_{futures,spot}.json` | `--list-json` output captured before and after registration; byte-identical, because the candidate is hidden from discovery. |
| `run_study.py` | `lock`, `hash`, `run` (offline, network blocked, study-local unconditional and randomized baselines installed into the research registry instance only), `parity`, `render`. |
| `results.json`, `parity_results.json` | Full outputs: every arm and window, the variant grid, per-entry audit, start-of-window holds, refit-table summaries, verdict guards and parity runs. |
| `evidence/refit_tables_*.json.gz` | Every per-cell refit table used by the primary windows (sha256 of the uncompressed JSON in `results.json`). |

## Reproduction

```
uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py lock
uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py run --cache-db /tmp/cce1653_run.db
uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py parity --cache-db /tmp/cce1653_parity.db
uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py render
```

`lock` only confirms the recorded lock once it exists. `run` and `parity` refuse an existing cache file, so pass
a new path each time. The parity cache also stores the proxy candles under the bare coin symbol so the
`parity_diff.py` CLI, which reads the configuration's `BTC` symbol, finds them offline.

## Proposed future table artifact

A live loader would need a frozen, versioned table per market: venue, coin and perpetual identity, universe,
timeframe and `expected_interval_ms`, the UTC basis and the 1970-01-05 epoch, the training cutoff (last
completed label exit open), a generation counter, an expiry at the next scheduled refit, the full per-cell
statistics and a sha256. The loader must hold on a stale, missing, corrupt or wrong-market table and may replace
a table only while flat. This study implements neither that loader nor a live holding-horizon close.
