# cross_sectional_momentum_1787

Candidate directory of the no-edge `cross_sectional_momentum` basket evaluator. Issue 1787 froze the signal
definitions here; issue 1792 adds the study. The verdict and every number are in `REPORT.md`; this file lists what each
artifact is for.

| file | role |
|---|---|
| `SIGNAL_SPEC.md` | Frozen signal specification from issue 1787. The study pins its sha256 and never changes it. |
| `discovery_*.json` | `--list-json` discovery snapshots from issue 1787. |
| `protocol.json` | Frozen study protocol: prerequisites and contracts, data sources, universe rule, windows, basket and member settings, member close policy, costs, grid, training selection, baselines, cluster statistics, sample sufficiency and the pass, fail and inconclusive mapping. |
| `protocol.lock.json` | Written once by `run_study.py lock` before any replay: sha256 of the protocol, the signal specification and the data manifest. `run` refuses a changed lock. |
| `data/` | Frozen Hyperliquid daily candles, hourly funding and lot metadata, with `data_manifest.json` (hashes, listing evidence, coverage, venue-absent hours, excluded candidates). |
| `run_study.py` | `prepare` (network, writes `data/`), `lock`, `run` (offline, writes `results.json`, `study_manifest.json`, `plans/`, `raw/`), `render` (writes `REPORT.md`). |
| `plans/` | Portable `go-trader.basket-replay-plan` v1 files of every replay (the state directory is a placeholder; `run` writes a materialized copy into its work directory). |
| `raw/` | Local only (git-ignored; 0.6 to 6.6 MB per replay, 26 MB for the recorded run): gzipped `go-trader.basket-replay-result` v1 records as `backtest/basket_replay.py` wrote them. `results.json` pins the sha256 of every raw record and adapter log; a rerun of `run` regenerates them. |
| `results.json`, `study_manifest.json` | Study output: per-run summaries, every tried variant, baseline targets, coverage, source and binary pins, and the study verdict. |

How it runs: `run` checks the lock, the prerequisite commits, files and contracts (replay plan and result versions,
adapter protocol by a probe of the binary, ranker request version, registry metadata, baseline target input and the
`taker_all` / `inside_slippage` cost path), refuses uncommitted source changes, verifies every data hash and the full
candle, funding and lot coverage of both windows, and only then builds plans. Every replay goes through
`backtest/basket_replay.py` and the one `go-trader basket-replay` adapter in its own new state directory. The study reads
each replay's status and coverage; an adapter exit code 0 alone never counts. A complete replay is scored by the replay
driver for accounting only; the study then applies its own frozen sample and economic rules, and an insufficient
sample stays an unscored study diagnostic.

All commands are research-only and suggest-only. Nothing here edits registry metadata, live defaults, deployed
configuration or acknowledgements, and live basket use stays refused.

## Attempts

- Attempt 1 (stopped, discarded): the first `run` used a binary whose `basket-replay` trade export returned only the
  newest 500 stored rows per member (the storage read caps one page at 500), so the output trades, fees and funding
  rows were incomplete although equity was right. It was stopped after two training replays, before any held-out
  replay ran; its outputs were not used for selection or reporting. The adapter now pages through every stored row,
  and the study refuses a replay whose exported funding rows differ from the rows its steps booked. The protocol and
  lock did not change.
- Attempt 2: the recorded run in `results.json`.

## Source pin and later main changes

The recorded run used commit `1b11cf55` (base `7e879656` plus the first two study commits; `results.json`
`source_hashes` pins every prerequisite file by sha256). Before the pull request opened, the branch was rebased onto a
later main, so that commit is not in the merged history. The rebase changed these pinned paths:

- `shared_strategies/open/cross_sectional_momentum.py` and `basket_allocation.py` (#1793): the return, volatility,
  validation and preflight helpers moved into `basket_allocation.py` behind wrappers with the same names. Checked
  after the rebase: `cross_sectional_momentum_core` from the old and the new source gives byte-identical output on
  every training and held-out deadline of this study for every grid variant (672 evaluations on the frozen
  candles), and all 117 recorded rebalance targets of the held-out candidate replay equal the new source's weights.
- `shared_strategies/open/registry.py` (#1793, #1640, #1654): new registrations only; this study's entry changes
  only its `edge_source` and `edge_ref`.
- `scheduler/basket_replay.go` (#1654): the adapter also refuses macro calendar inputs, which this study never uses.
- `shared_scripts/check_hyperliquid.py` (#1654): macro inputs for `macro_event_breakout` only.

After the recorded run, issue 1810 changed `run_study.py` (a pinned path) to refuse a batch before its replays when an
existing `plans/` file differs from the plan that batch builds, and to refuse before it writes the result when a plan
file is missing or changed at the end. Each refusal records `plan_file_mismatch` at the selected output path. The
change adds checks only and builds no different plan, replay, selection or verdict, so the current driver's sha256
differs from its `source_hashes` pin. Checked after the change, with no replay: the current driver rebuilds all 11
plans from the frozen inputs (the 6 training specs, and the 5 held-out specs from the recorded `selected_params` and
the recorded baseline positions divided by the member count) with the same bytes as the committed `plans/` files, and
those files equal both the `study_manifest.json` `plans` pins and every `portable_plan_sha256` in `results.json`.
