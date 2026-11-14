# mean_reversion_exit_2018

Study for issue 2018: does a wider `ranging_volatile` take-profit ladder improve `mean_reversion` after current
execution costs and a 2x spread and slippage stress? Two frames are judged separately: the historical B2 tiered close
(`tiered_tp_atr_regime` with a 1.5 ATR stop, the 1152 `b2_rv_wider` hypothesis) and a defined trailing-ratchet close
(`trailing_tp_ratchet_regime` with the regime trail). The verdict per frame, group and direction and every number are
in `REPORT.md`. Everything here is suggest-only: no command sends an order, and no command writes an operational live
or paper config, a registry default, a quarantine status or `edge_status`.

## Verdict

VERDICT_TABLE

## Design in short

- Frozen protocol (`protocol.json`) and lock (`protocol_lock.json`) committed before any train, judge or continuous
  window was scored. Every scoring command checks the lock against the protocol, arm, input and code hashes, refuses
  uncommitted locked files, and records the lock commit and the first-scoring metadata in `scoring_log.json`.
- Inputs: byte copies of the `tema_cross_2016` frozen files (Binance US proxy candles, Hyperliquid funding, native
  Hyperliquid candles, venue meta and L2 snapshot), verified by sha256 (`study_manifest.json`). Proxy candles are read
  through study-owned offline manifests (`proxy_manifest_{30m,1h,4h}.json`); native candles through the copied venue
  manifests. Scoring blocks the network in every process and never reads the shared cache.
- Windows: train 2023-07-01 to 2025-06-10 (neighbour shortlist only), judge 2025-06-10 to 2026-11-10, continuous
  2023-07-01 to 2026-11-10 from a flat book; native tails 2026-08-07 (30m) and 2026-05-03 (1h) to 2026-11-10.
- Paired attribution: the incumbent runs free with entries gated to `ranging_volatile`; each of its positions is
  replayed for every arm and cost with one signal on the real `Backtester`. Opened positions (all close records of one
  entry) are the unit; net percent comes from unrounded engine trades.
- Arms: the primary widening x1.5 (B2 0.75/1.5 ATR; ratchet 1.5/3/4.5 ATR) plus two neighbours x1.25 and x1.75; one
  neighbour may be shortlisted on train. Holm over the full searched family (m = 9), synchronized 7-day block
  bootstrap, dataset harm checks, sample floors, base and stress costs, and a continuous complete-policy gate.

## Files

| file | role |
|---|---|
| `protocol.json`, `protocol_lock.json` | Frozen protocol and its lock (protocol, arm, input and code hashes). |
| `study_manifest.json`, `data/`, `proxy_manifest_*.json`, `venue_manifest_*.json` | Frozen inputs and their manifests. |
| `arms/b2/*.json`, `arms/ratchet/*.json` | Arm configs built by `build-arms` (`paired_*` carry the `ranging_volatile` gate; `cont_*` are the complete policies). |
| `arm_validation.json` | Real resolver and `go-trader inspect` checks: only `ranging_volatile` changes, every label keeps an exit, stop owners unchanged, unified B2 incumbent equals the historical defaults. |
| `coverage_report.json` | Warm-up, candle gaps and funding coverage per dataset and window. |
| `checks.json` | Pre-lock checks on the 2023-01-01 to 2023-07-01 parity window: runner parity with `eval_windows.run_leg`, replay fidelity, sliced-frame parity, identical-candidate zero delta, minimum-size cases, unified-versus-historical B2 equivalence, equity reconciliation, missing or changed inputs, stale lock and cache invalidation, verdict-rule cases, isolation. |
| `selection.json`, `results_train.json`, `samples_train.json.gz` | Training scores and the shortlist, written once before judgment. |
| `results_judge.json`, `samples_judge.json.gz` | Judged paired positions per arm and cost. |
| `continuous.json`, `continuous_equity.json.gz` | Continuous runs, reconciliation and the bar equity paths. |
| `verdict.json` | Statistics, Holm family and verdict per unit. |
| `checks_post.json` | Post-scoring checks: verdict repeatable, report deterministic, lock committed, runs reconciled, scoring isolated. |
| `run_study.py`, `render_report.py` | Driver and report renderer. |

## Reproduction

Only `copy-inputs` writes inputs (from committed files; no network). Every scoring command reads the frozen files,
blocks the network and writes checkpoints and label caches only under `--work-dir`.

```
S=backtest/candidates/mean_reversion_exit_2018/run_study.py
uv run --no-sync python $S verify-inputs
uv run --no-sync python $S build-manifests --check
uv run --no-sync python $S build-arms --check
go -C scheduler build -o <scratch>/go-trader . && uv run --no-sync python $S validate-arms --go-binary <scratch>/go-trader
uv run --no-sync python $S coverage
uv run --no-sync python $S checks --work-dir <dir>        # before the lock only
uv run --no-sync python $S lock                           # then commit the lock and every locked file
uv run --no-sync python $S verify-lock
uv run --no-sync python $S select --work-dir <dir> --workers <n>
uv run --no-sync python $S judge --work-dir <dir> --workers <n>
uv run --no-sync python $S continuous --work-dir <dir> --workers <n>
uv run --no-sync python $S verdict
uv run --no-sync python $S render
uv run --no-sync python $S checks-post
```

A rerun with the same `--work-dir` reuses every unit whose protocol, code, input, arm and selection hashes match.
Run the long stages with `OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1 VECLIB_MAXIMUM_THREADS=1`.
