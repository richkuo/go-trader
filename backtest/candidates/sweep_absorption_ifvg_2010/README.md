# sweep_absorption_ifvg_2010

Frozen study for the production-refused research candidate `sweep_absorption_ifvg` (issue 2010).

## Files

- `candidate_spec.json`: the signal contract, support horizon, source contract, arm matrix, windows, grid,
  selection rule, coverage rule and verdict rules. It was committed before any study window was read.
  `study_manifest.json` pins its sha256; `run_study.py` refuses to run when the file changes.
- `study_manifest.json`: the required input roster (recording, side evidence, sealed payload capture, one
  `offline_candle_manifest/v7` per interval) with the pinned hashes.
- `run_study.py`: `run` preflights every required input. A missing or refused input becomes a named gate and
  the verdict is INCONCLUSIVE without economic scoring. With every gate present it scores the frozen protocol.
  `check` reruns into a scratch directory and compares `results.json` and `REPORT.md` byte for byte.
- `arms.py`: the study-local execution simulator for the research arms (CE limit entry, structural stop,
  liquidity-pool target). Its reference arm must reproduce the real Backtester's entry schedule before any
  research arm is compared.
- `scenario.py`: deterministic synthetic sessions (long and mirrored short setups, absorption, gap and
  displacement variants) that the integration tests share. It is never study evidence.
- `discovery_{before,after}_{spot,futures}.json` and `evidence/catalog_{before,after}_{spot,futures}.json`:
  ordinary `--list-json` and full `strategy_catalog.py` captures around the registration.

## Acquiring the inputs (source B)

1. Run `go-trader record-observations --kinds trade_flow --coins BTC,ETH,HYPE --out-dir <empty dir>` from
   before the train window start through the test window end, then copy the closed run into `data/recording/`
   and pin the `run.manifest.json` sha256 in `study_manifest.json`.
2. Pin taker-fill side evidence for every coin into `data/side_evidence/side_evidence.json`.
3. Capture sealed market payloads of an explicit-paper shared-feed run of the candidate over part of the
   recording into `data/sealed_payloads/` for the sealed-versus-replayed decision proof.
4. Write `data/offline_5m.json` and `data/offline_15m.json` (schema v7): candles, funding, the trade-flow series
   replayed from the recording by `backtest/trade_flow_replay.py`, and pinned one-hour `htf_candles`, with the
   specification windows and `warmup_bars` at least the support horizon.
5. Run `uv run --no-sync python backtest/candidates/sweep_absorption_ifvg_2010/run_study.py run`.

The study is suggest-only. No verdict writes live defaults, config or pull requests, and live use stays refused.
