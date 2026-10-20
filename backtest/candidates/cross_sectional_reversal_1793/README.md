# cross_sectional_reversal_1793

Candidate directory of the no-edge `cross_sectional_reversal` basket evaluator. Issue 1793 froze the signal
definitions here; issue 1790 adds the study. The verdict and every number are in `REPORT.md`; this file lists what each
artifact is for and how to run it.

| file | role |
|---|---|
| `SIGNAL_SPEC.md` | Frozen signal specification from issue 1793. The study pins its sha256 and never changes it. |
| `discovery_*.json` | `--list-json` discovery snapshots from issue 1793. |
| `protocol.json` | Frozen study protocol: prerequisites and contracts, the reused data and the dated universe rule, windows and the disclosure that #1792 already inspected them, basket and member settings, the holding-horizon lifecycle, member close policy, costs, the 18-variant grid, training selection, both baselines with their decision-to-weight conversions, validity gates, cluster statistics, stability, concentration and the pass, fail and inconclusive mapping. |
| `data_reference.json` | Hashed data reference written by `run_study.py reference`: the #1792 data manifest (`../cross_sectional_momentum_1787/data/data_manifest.json`) and every candle, funding and lot file it lists, pinned by sha256. No data is copied. |
| `protocol.lock.json` | Written once by `run_study.py lock` before any replay: sha256 of the protocol, the signal specification and the data reference. `run` refuses a changed lock. |
| `run_study.py` | `reference`, `lock`, `run` (offline, writes `results.json`, `study_manifest.json`, `plans/`, `raw/`) and `render` (writes `REPORT.md`). |
| `plans/` | Portable `go-trader.basket-replay-plan` v1 files of every tried replay (training variants, held-out candidate at primary and stressed cost, both baselines). Data paths are relative to this directory and the state directory is a placeholder; `run` writes a materialized copy into its work directory. |
| `raw/` | Local only (git-ignored): gzipped `go-trader.basket-replay-result` v1 records as `backtest/basket_replay.py` wrote them. `results.json` pins the sha256 of every raw record and adapter log; a rerun of `run` regenerates them. |
| `results.json`, `study_manifest.json` | Study output: per-run summaries and validity gates, every tried variant, baseline conversions, coverage, source and binary pins, and the study verdict. |

## Usage

```
uv run --no-sync python backtest/candidates/cross_sectional_reversal_1793/run_study.py reference
uv run --no-sync python backtest/candidates/cross_sectional_reversal_1793/run_study.py lock
go -C scheduler build -o <bin>/go-trader .
uv run --no-sync python backtest/candidates/cross_sectional_reversal_1793/run_study.py run --binary <bin>/go-trader --work-dir <new empty dir> --jobs N
uv run --no-sync python backtest/candidates/cross_sectional_reversal_1793/run_study.py render
```

`reference` and `lock` never replace an existing file, and `run` never replaces an existing `results.json`; a new
experiment needs a new study directory. A replay of a recorded plan: `uv run --no-sync python backtest/basket_replay.py
--plan <materialized plan> --binary <bin>/go-trader --out <new file>`.

## How it runs

`run` checks the lock, the prerequisite commits, files and contracts (replay plan and result versions, adapter protocol
by a probe of the binary, ranker request version, registry metadata of the candidate and both baselines, baseline target
input, the `taker_all` / `inside_slippage` cost path and the `holding_horizon_seconds` lifecycle field), refuses
uncommitted source changes, verifies the data reference and every referenced hash, applies the dated universe rule again,
checks the full candle, funding and lot coverage of both windows (a missing or venue-absent funding hour refuses the
study; a zero rate is never synthesized), and only then builds plans. Every replay goes through `backtest/basket_replay.py`
and the one `go-trader basket-replay` adapter in its own new state directory, with no live route. Python produces frozen
deadline weights for the baselines only; Go owns targets, position lifetime, expiry, action ordering and recovery.

A run is valid only when the replay is complete with complete coverage and exit code 0, its steps equal the window
steps, the exported funding rows equal the booked rows, no step records a state save failure (a save failure that a
later save recovered still refuses the run), the applied cost policy equals the frozen one, every fill is on the frozen
lot grid, every funding row has the sign its quantity and rate imply, every baseline deadline the adapter did not hold
froze exactly the converted weights, and equity reconciles with booked close results, funding, fees and the endpoint
book within $0.01. An exit code of 0 alone never counts. Invalid runs stay unscored diagnostics. Selection uses
training runs only; the frozen sample, stability, concentration and economic rules then give the verdict.

All commands are research-only and suggest-only. Nothing here edits registry metadata, live defaults, deployed
configuration or acknowledgements, and live basket use stays refused for every outcome.

## Result

The recorded run gives `fail` (`REPORT.md`): every training variant lost after costs, and the training-selected variant
(return lookback 14, quantile 0.2, volatility lookback 28, one-day holding horizon) lost on the held-out window, with a
negative mean weekly return, a negative excess over both baselines and a negative stressed-cost return on 69 weekly
clusters. The `mean_reversion_pro` conversion gave no nonzero target at any held-out deadline (its daily entry signal
never fired on a deadline bar), so that arm is flat and its excess test equals the candidate net test; this is a
property of the frozen conversion, reported as is.

## Attempts

- Attempt 1 (stopped, discarded): started on the first locked commit and stopped after about 15 of 42 training
  deadlines, before any replay finished, because the new expiry parity tests found that the adapter's step records
  missed basket rows a durable save pruned within the same step. The adapter fix only adds those rows to the step
  records; it changes no order, booking or equity. Nothing from attempt 1 was read for selection or reporting, and the
  protocol, data reference and lock did not change.
- Attempt 2: the recorded run in `results.json`.

## Source pin and later main changes

The recorded run used commit `18705654` (base `70b9725d` plus the first study commit; `results.json` `source_hashes`
pins every prerequisite file by sha256). Before the pull request opened, the branch was rebased onto main `902ef897`
(#1665), so that commit is not in the merged history. Among the pinned paths, the rebase changed only
`shared_strategies/open/registry.py` (a new `volume_profile_reclaim` registration); outside them it raised the market
payload version to 2 with an additive volume-profile section (`shared_tools/market_payload.py`, the Go feed wire and
snapshot code). Checked after the rebase: the held-out candidate plan replayed with a binary built from the rebased
branch gives the same 484 step records, the same 10719 trade rows (position ids compared without their process nonce),
the same 831 venue events, the same run fingerprints and the same final basket equity as the recorded raw record.

After the recorded run, pull request review changed `run_study.py` (a pinned path) to refuse a run before its replays
when an existing `plans/` file differs from the plan that run builds, and to refuse before it writes the result when a
plan file is missing or changed at the end. The change adds checks only and builds no different plan, replay, selection
or verdict, so the current driver's sha256 differs from its `source_hashes` pin. Checked after the change: the current
driver rebuilds all 22 plans from the frozen inputs with the same bytes as the committed `plans/` files, and those files
equal both the `study_manifest.json` `plans` pins and every `portable_plan_sha256` in `results.json`.
