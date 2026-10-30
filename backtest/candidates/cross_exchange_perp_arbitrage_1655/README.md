# cross_exchange_perp_arbitrage study

The spread policy is compared with no trade and a frozen two-leg schedule. This runner writes only an inconclusive verdict. `protocol.json` freezes the pass and fail rules for a later scored evaluation. This command does not apply them, and it does not replay `plans/experiments.json`. Missing real evidence stays inconclusive. A manifest whose classification is neither `evidence_missing` nor `mechanical_fixture` is refused.

`SIGNAL_SPEC.md` freezes the policy formulas. `protocol.json` freezes windows, the parameter grid, sample and coverage floors, and the verdict rules before any held-out read. `plans/experiments.json` names the candidate, both baselines, both orientations, the latency cases, and the venue failure cases. `data_manifest.json` records object hashes or the missing evidence. `protocol.lock.json` is written by `lock` and is not an input of itself.

## Commands

From the repository root, with a `go-trader` binary built from the same tree:

```
uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py validate
uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py lock --binary <go-trader>
uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py run --binary <go-trader> --work <fresh-dir>
uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py render
```

`run` checks the lock before replay. A changed source, binary, protocol, schedule, or data manifest refuses before a state directory is claimed. `--work` must sit outside this directory. A mechanical fixture is imported with `prepare --bundle <dir>` before `lock`; its verdict stays inconclusive.

Archive objects, when a manifest lists them, are read with `cache-check --manifest <manifest.json> --cache <dir>`, which uses `archive_cache.verified_object`.

## Limits

No sealed OKX recording and no continuous cross-venue depth are frozen in this directory. The committed verdict is inconclusive. A research receipt price is not a historical venue oracle price. `scenario.funding_policy` belongs only on a paper collection plan. Production refusal stays enabled.
