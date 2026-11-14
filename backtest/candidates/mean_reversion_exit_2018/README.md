# mean_reversion_exit_2018

Study for issue 2018: does a wider `ranging_volatile` take-profit ladder improve `mean_reversion` after current
execution costs and a 2x spread and slippage stress? Two frames are judged separately: the historical B2 tiered close
(`tiered_tp_atr_regime` with a 1.5 ATR stop, the 1152 `b2_rv_wider` hypothesis) and a defined trailing-ratchet close
(`trailing_tp_ratchet_regime` with the regime trail). The verdict per frame, group and direction and every number are
in `REPORT.md`. Everything here is suggest-only: no command sends an order, and no command writes an operational live
or paper config, a registry default, a quarantine status or `edge_status`.

## Verdict

| unit (frame/group/direction) | verdict | evidence |
|---|---|---|
| `b2/proxy/both` | positive | The x1.5 primary (0.75/1.5 ATR) improves the paired net return by +0.044% per position at base and +0.043% at stress cost (900 positions, decision p 0.0025, Holm threshold 0.0056), with no significant negative dataset or side, and passes the continuous gate. The x1.75 neighbour is positive too but misses Holm (p 0.021). |
| `ratchet/proxy/long` | negative | Both judged ladders are positive (x1.5 +0.0315%/+0.023%, x1.75 +0.040%/+0.036% per position over 731 positions), but neither passes Holm (decision p 0.0314 and 0.0083 against thresholds 0.0083 and 0.0063). |
| `ratchet/native/long` | negative | On 242 native Hyperliquid positions the x1.5 ladder is flat at stress cost (-0.0003%) and the x1.75 ladder is not significant (p 0.158); both fail the continuous gate. |

The suggested B2 configuration (`suggested_b2_proxy_config.json`) is an offline artifact. The complete strategy loses money
under both exits: over the continuous interval the suggested arm returns -75.5% of pooled capital at base cost and
-78.5% at stress cost (the incumbent -77.2% and -79.6%). The gain is relative to the incumbent exit and does not make
the strategy profitable; `mean_reversion` stays `no_edge`. No ratchet configuration is suggested. Fleet defaults,
`config.example.json`, registry defaults, quarantine status and `edge_status` are unchanged.

The committed suggested configuration has no `circuit_breaker` key, so the per-strategy circuit breaker stays at its
default (on). The arm files set `"circuit_breaker": false` only as a harness field: the backtester does not read it,
so no scored result depends on it, and the study measured nothing about the breaker. `verdict` copies the locked arm
template, so a rerun of `verdict` writes `"circuit_breaker": false` back into every strategy block of the suggested
file; remove it again before the file is used for anything.

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

## Known limitations

- The dirty-file guard in `lock_commit_status` (`run_study.py`) reads `git status --porcelain` through `git()`, which
  strips the whole output. The leading space of the first line (for example ` M`) is lost, and `line[3:]` then drops
  the first character of that path. When the first listed path is an allowed output (`scoring_log.json`,
  `checks_post.json`, `README.md`, `REPORT.md`, `render_report.py`, a scoring output or a `suggested_*` file), the
  guard reports it as blocking, and every scoring command and the `lock_committed` case of `checks-post` refuse. The
  guard fails closed: a modified or renamed locked file stays blocking at any position. `run_study.py` is pinned in
  `code_sha256` of the lock, so the code is left unchanged. Commit allowed outputs before a scoring command or
  `checks-post` runs.
