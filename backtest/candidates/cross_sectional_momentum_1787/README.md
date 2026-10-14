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
| `raw/` | Gzipped `go-trader.basket-replay-result` v1 records of every replay, as `backtest/basket_replay.py` wrote them. |
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
