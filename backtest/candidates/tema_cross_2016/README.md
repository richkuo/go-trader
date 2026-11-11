# tema_cross_2016

Study for issue 2016: does an entry filter keep the `tema_cross` gross edge after fees, on the spot long frame and on
the two Hyperliquid perps shapes that run today? The verdict per frame and every number are in `REPORT.md`.
Everything here is suggest-only: no command writes a live or paper config, a registry default or `edge_status`, and
none sends an order.

## Verdict

| frame | verdict | why |
|---|---|---|
| `spot` | negative | Every judged arm (`bar_4h`, `p_13_34_89`, `gate_cm_up`, the `gate_cm_up` + `p_13_34_89` combination) keeps a positive net return per position at base and stress cost on the pooled `oos`, `2023H2`, `2024` and `2025H1` windows (best: `bar_4h` +0.475%, stress +0.375%, 246 positions), but none passes the Benjamini-Hochberg correction over 36 arms (lowest decision p-value 0.143). |
| `paper` | negative | The baseline has a measurable gross edge (+0.075% per position, p 0.0025), but fees and slippage remove it: the judged arms `gate_cm_up` and `bar_1h` lose 0.16% to 0.18% per position after costs. |
| `live` | inconclusive | The live-rule check of the higher-timeframe trend (60 fetched buckets against the frozen series) agrees on 94.7% to 94.9% of bucket closes in `2024` and `2025H1`, under the frozen 0.95 bar, so ETH leaves the statistics and no dataset remains. Before that exclusion every judged live arm loses money after costs (`live_long` -0.139%, `bar_1h` -0.126% per position). |

No perps frame is positive, so no suggested config is written. The frames disagree (spot net-positive but not significant; perps net-negative); the perps frames decide. `gate_adx_up` and `gate_p21_up_clean` opened no position on `is` in any frame, so both are degenerate gates for this entry. Details and every number: `REPORT.md`.

## Frames

| frame | shape | datasets | baselines |
|---|---|---|---|
| `spot` | the M1 audit frame: registry close (the strategy's own exit signal), long only, Hyperliquid fees, funding charged; its own regime block (`composite_medium` 350, ADX `medium` 350, `comp_p21`) | Binance US BTC, ETH, SOL at 1h and 4h | `spot_control` |
| `paper` | the ten deployed paper strategies: `direction: long`, `trailing_tp_ratchet_regime` with `use_defaults`, a 2.5 ATR regime trail, composite period 350 windows, `atr_method: wilder`; the uncertified directional policy stays off | Binance US proxy BNB, BTC, ETH, SOL at 30m and 1h (HYPE has too little history and is venue-only) | `control` |
| `live` | the one deployed live strategy: ETH 30m, `direction: both`, `htf_filter: true`, the explicit three-tier ladder with `close_fraction` 0, a 2.5 ATR regime trail, isolated margin at leverage 20 with a fixed margin per trade, its own regime block (composite period 336, largest period 1440) | Binance US proxy ETH 30m (ETH 1h for the bar-size arm) | `live_both` (as deployed), `live_long` |

Filter arms, each scored alone on `is` first: four regime entry gates (`gate_cm_up`, `gate_cm_not_down`,
`gate_adx_up`, `gate_p21_up_clean`), the higher-timeframe filter (`htf`; in the live frame the filter is already on,
so the slot is the direction arm `live_long`), five slower EMA period sets (`p_8_21_55` to `p_55_144_377`, one scale
axis with a plateau check), the bar size (`bar_1h` in the perps frames, `bar_4h` in the spot frame) and one
combination of the family picks. Family size 36 (12 per frame). In the live frame every filter arm overlays
`live_long`; `live_both_nohtf` and `live_long_nohtf` are reference arms outside the family.

## Files

| file | role |
|---|---|
| `deployed_tema_cross_sanitized.json` | Sanitized copy of the deployed entries and both regime blocks, read 2026-11-11: generic ids, no routing, storage, account or host field. `run_study.py sanitize-check` enforces a key allowlist, the generic id pattern and the absence of addresses, IPv4 addresses, host paths, URLs and secret words. |
| `protocol.json`, `protocol_lock.json` | The frozen protocol (frames, arms, windows, selection, statistics, verdict, venue and fidelity rules, prior reads, disclosures) and its lock, written before the first score. |
| `protocol_amendment.json` | One amendment written after `select` and before any judged window: every per-trade statistic counts opened positions (all close records of one entry), not close records, because the ratchet ladders close positions in parts. |
| `arms/paper/*.json`, `arms/live/*.json` | Config arms: pure builds of the sanitized copy (`build-arms`), read through `load_strategy_config` and `build_candidate`, with `htf_filter` from the strategy entry and `atr_method` from the resolution. `combo.json` is written by `select` from the `is` picks. |
| `arms/spot/*.json` | Spot candidates built from the frame block; gate arms carry the plan from `resolve_feature_windows`. |
| `study_manifest.json`, `data/` | Frozen inputs: the `breakout_2014` proxy candles and funding, venue files and manifests copied byte for byte (source paths and sha256 recorded), and the Binance US 4h candles for BTC, ETH and SOL acquired once. |
| `venue_manifest_*.json`, `bridge_manifest_*.json` | The four `breakout_2014` offline manifests, copied byte for byte (their `study` field still names `breakout_2014`). |
| `coverage_report.json` | Read-only coverage per frame and dataset: warm-up prefix, priced funding, the 50-bucket higher-timeframe rule. |
| `smoke.json` | Every arm on synthetic candles in a throwaway cache with the network blocked: the engine accepts each shape, the entry ATR is the Wilder ATR, `live_both` opens shorts. |
| `htf_parity.json` | The frozen higher-timeframe input against the existing `--htf-filter` path: signals (before the lock) and one scored leg (after it). |
| `cache_parity.json`, `timing.json` | Label cache parity and the measured run-time projection. |
| `selection.json` | The `is` scores, shortlist and picks per frame, written once before any judged window. |
| `noise_gate.json` | The gross-edge noise gate per baseline, written before any filter arm's judged leg is read. |
| `results.json`, `samples.json.gz`, `run_log.json` | Judged proxy, venue and bridge windows; pooled per-trade samples; one log entry per `judge` start and run. |
| `fidelity.json` | Live-label fidelity rows, the engine label check and the higher-timeframe live-rule rows. |
| `verdict.json` | The statistics, the BH correction and one verdict per frame; `suggested_<frame>_config.json` exists only for a positive perps frame. |
| `run_study.py`, `render_report.py` | The driver and the report renderer. |

## Reproduction

Commands in order. Only `acquire-4h` reads the network (public Binance US candles, with `HYPERLIQUID_SECRET_KEY`
unset); it ran once and refuses to overwrite. Every scoring command reads only the frozen files, builds a throwaway
cache at a new path (never `shared_tools/trading_bot.db`) and blocks the network.

```
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py sanitize-check
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py copy-frozen
env -u HYPERLIQUID_SECRET_KEY uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py acquire-4h --cache-db <new path>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py verify-frozen
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py build-arms --check
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py smoke --work-dir <dir>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py coverage
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py htf-parity --signals --cache-db <new path>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py lock
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py htf-parity --leg --cache-db <new path> --work-dir <dir>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py cache-parity --cache-db <new path> --work-dir <dir>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py time-leg --cache-db <new path> --work-dir <dir>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py select --cache-db <new path> --work-dir <dir> --workers <cpus>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py noise-gate --cache-db <new path> --work-dir <dir> --workers <cpus>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py judge --cache-db <new path> --work-dir <dir> --workers <cpus>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py fidelity --work-dir <dir> --workers <cpus>
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py suggest
uv run --no-sync python backtest/candidates/tema_cross_2016/run_study.py render
go -C scheduler build -o <scratch>/go-trader . && <scratch>/go-trader inspect --config backtest/candidates/tema_cross_2016/arms/paper/control.json --all
```

`--work-dir` holds the label cache (under `labels/<labeller hash>`) and one checkpoint per leg; a rerun with the same
`--work-dir` reuses every leg whose protocol, engine-file, labeller-file and arm-file hashes match, so a stopped
`select`, `noise-gate`, `judge` or `fidelity` resumes where it stopped. Run the long stages with
`OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1 VECLIB_MAXIMUM_THREADS=1` and one worker per CPU.

## Live config and `go-trader inspect`

Every paper arm file passes `go-trader inspect --config <file> --all`. The live arm files do not: the deployed live
entry runs in `--mode=live` (inspect then needs `HYPERLIQUID_SECRET_KEY`) and holds `capital: 0`, which is valid
only beside the other live strategies of its shared wallet pool, and the sanitized copy keeps only this entry. The
shape is kept as deployed; a diagnostic copy of each live arm with `--mode=paper` and `capital: 1000` passes
`inspect` (not committed).
