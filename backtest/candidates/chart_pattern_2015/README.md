# chart_pattern_2015

Study for issue 2015: does `htf_gate_factor=4` improve net results against `htf_gate_factor=0` for `chart_pattern`, on
the historical spot long audit reference and on the requested Hyperliquid perps shape, per direction? The verdict
for every contract and group and every number are in `REPORT.md`. Everything here is offline and suggest-only: no
command writes a live or paper config, a registry default, a shared trading implementation or `edge_status`, and
none sends an order. The registry default stays `htf_gate_factor=0`.

## Verdict

Every perps contract and group is **negative** for factor 4 and for the selected additional factor; no suggested
parameter line is written. The gate cuts losses: the paired daily improvement over factor 0 survives the
Benjamini-Hochberg correction for `ratchet_both` (30m and 1h), `ratchet_long` 30m and `ratchet_short` 1h. But every
gated arm still loses money after costs on the proxy groups over `oos`, `2024` and `2025H1t`, so the opt-in rule fails
for every member. On the Hyperliquid venue tail, only `ref_long` 1h has a gated arm that is positive and ahead of
factor 0. The deployment verdict is
**inconclusive**: no `chart_pattern` deployment snapshot exists, and `research_config.json` is a research assumption.

| contract | group | factor | outcome | paired mean daily (pct) | BH q | gated net USD | venue gated / f0 net USD |
|---|---|---|---|---|---|---|---|
| `ratchet_both` | `proxy_1h` | f4 | negative | +0.0551 | 0.0440 | -1418.37 | -526.45 / -1229.45 |
| `ratchet_both` | `proxy_30m` | f4 | negative | +0.0807 | 0.0020 | -3802.48 | -376.58 / -728.83 |
| `ratchet_long` | `proxy_1h` | f4 | negative | +0.0166 | 0.4017 | -741.71 | -133.67 / -523.69 |
| `ratchet_long` | `proxy_1h` | f2 | negative | +0.0194 | 0.2806 | -615.78 | -190.20 / -523.69 |
| `ratchet_long` | `proxy_30m` | f4 | negative | +0.0664 | 0.0020 | -2493.81 | -12.63 / -247.21 |
| `ratchet_short` | `proxy_1h` | f4 | negative | +0.0365 | 0.0440 | -752.46 | -408.25 / -808.71 |
| `ratchet_short` | `proxy_30m` | f4 | negative | +0.0151 | 0.4465 | -1760.47 | -351.81 / -478.19 |
| `ratchet_short` | `proxy_30m` | f2 | negative | +0.0237 | 0.1948 | -1444.83 | -392.92 / -478.19 |
| `ref_long` | `proxy_1h` | f4 | negative | -0.0427 | 1.0000 | -1077.36 | +331.20 / +97.37 |
| `ref_long` | `proxy_30m` | f4 | negative | +0.0201 | 0.6457 | -147.23 | +579.36 / +632.05 |
| `ref_short` | `proxy_1h` | f4 | negative | -0.0438 | 1.0000 | -3136.67 | -404.39 / -575.96 |
| `ref_short` | `proxy_30m` | f4 | negative | +0.0244 | 0.6426 | -2731.84 | -275.50 / -256.67 |

In the live-shaped 200-candle frame, factors 5 and 6 never warm their 40-bucket EMA (200 / 5 = 40 buckets), so they
trade exactly as factor 0; factor 4 has 50 buckets per frame, so its EMAs start from the first bucket of each frame. The `is` selection picked factor 2
for `ratchet_long` 1h and `ratchet_short` 30m; both are negative.

The spot long audit reference keeps the issue 982 and issue 1282 factor 4 verdicts on `is`, `oos`, `2024` and `2025H1`
(PASS, PASS, FAIL, PASS) under current defaults; `2023` is `funding_incomplete` under charged funding (Hyperliquid
funding starts 2023-05-12) and PASS in both labeled diagnostics. The issue 982 cost diagnostic reproduces the issue 982
2023 Sharpe means within 0.01 (bar 1.46). Factor 0 `oos` flips to fail and factors 5 and 6 `oos` flip to pass; the diagnostics place both
flips on the four months of new `oos` data. The paired factor 4 minus factor 0 daily difference on the audit datasets
is +0.0154 percent (p 0.3287). The audit conclusion is **inconclusive**: one of 540 sampled
full-frame signals changes when the frame is cut at its decision bar, so the historical full-frame path is not causal,
and the audit contract cannot support a perps opt-in in any case.

## Design

| part | what it is |
|---|---|
| audit reference | The issue 982 frame: spot registry, long, open-as-close, BTC/ETH/SOL at 1h and 4h, the five M1 windows, the full-frame `eval_windows` path and the median of the 8 incumbents. Three incumbent memo instances: `current` (current defaults: Hyperliquid fee, perps accounting, charged funding), and two labeled diagnostics, `current_funding_off` and `legacy_cost` (the issue 982 cost model, plus `oos_982` for the dates issue 982 could read). The documented issue 982 and issue 1282 verdicts sit beside the rerun. |
| perps contracts | `ref_long` and `ref_short` (open-as-close references, no engine stop, scored as separate books) and `ratchet_long`, `ratchet_short` and `ratchet_both` (the research configuration with `trailing_tp_ratchet_regime`). `ref_both` is refused: the harness has no bidirectional open-as-close book, and a sum of the long and short references is never presented as one. |
| datasets | `proxy_30m` and `proxy_1h` (Binance US candles for BNB, BTC, ETH, HYPE and SOL with Hyperliquid fees and charged funding, labeled proxy), `venue_30m` and `venue_1h` (the Hyperliquid venue manifests of `breakout_2014`), and `bridge_30m` and `bridge_1h` (proxy candles on the venue dates). |
| signal path | The perps contracts use a finite-frame replay: for every decision bar the canonical `chart_pattern_core` runs on the last 200 closed candles, the frame the scheduler fetches for this configuration, and only the last row's decision is kept. `replay-check` proves the replay against direct `chart_pattern_core` calls and against `check_hyperliquid.evaluate_signal_slot` on the same frames, and checks future invariance, higher-timeframe bucket visibility and next-bar fills in the real backtester. |
| selection | Factors 2, 3, 4, 5 and 6 are scored on `is` only. Factor 4 is always judged. At most one additional factor per contract and group is selected by the frozen rule and is reported separately from factor 4. |
| statistics | Paired seven-day block bootstrap of daily equity returns (gated minus factor 0) over `oos`, `2024` and `2025H1t`, 10,000 resamples, seed 2015, Benjamini-Hochberg at 0.05 over 20 predeclared members. |

## Files

| file | role |
|---|---|
| `protocol.json`, `protocol_lock.json` | The frozen protocol (windows, contracts, groups, frame, selection, statistics, verdict rule, historical evidence, disclosures) and its lock, written before any selection leg. The lock records the digests of the protocol, inputs, coverage, research configuration, arm files, validation and every signal, engine, close, configuration, cost, funding and label file, plus `uv.lock`. |
| `research_config.json` | The explicit research configuration. No sanitized `chart_pattern` deployment snapshot was available, so this file is an assumption, not a deployment: every field is the issue's request or a scheduler default (`use_defaults` ladders and trailing table, ADX period 14 regime, simple ATR, leverage 1, paper mode), plus `closed_bar_decisions: true`. |
| `arms/` | One file per contract and factor, a pure build of `protocol.json` and `research_config.json` (`build-arms --check`). `arms/resolution.json` holds the settings the configuration reader and candidate builder resolve, the per-label tiers and trailing multiples, the settings the study carries and the refused contract. |
| `study_manifest.json` | The input freeze: every proxy candle and funding file and the four offline manifests, read in place from `breakout_2014` and `tema_cross_2016`, with source study, source manifest digest, source revision and sha256. No data is acquired or copied. |
| `coverage_report.json` | Coverage per group, dataset and window: warm-up prefix, candles, funding hours priced or proven venue-absent, and the reason for every exclusion. |
| `validation.json` | The replay and causality checks, including the full-frame causality sample on the historical audit path and a full-frame against finite-frame signal count. |
| `selection.json` | The `is` scores and the frozen pick per contract and group, written once before any judged leg. |
| `results.json`, `samples.json.gz`, `run_log.json` | Every judged leg (effective scored range, settings source, costs, funding completeness), the position lists and daily equity behind the statistics, and the run log with the network probe. |
| `verdict.json` | The statistics, the correction, one outcome per contract, group and factor slot, the audit reference, the deployment verdict and any suggested parameter line. |
| `REPORT.md` | Rendered from the artifacts by `render`; `render --check` refuses a stale report. |
| `run_study.py`, `render_report.py` | The driver and the renderer. |

## Reproduction

Commands in order, from the repository root. No command reads the network: the scoring commands build a new
throwaway cache at `--cache-db` (never `shared_tools/trading_bot.db`), block sockets, name resolution and the data
fetchers, and record a network probe. `--work-dir` holds the signal tables, the label cache and one checkpoint per
leg; `select` and `judge` must share one `--work-dir`, and a rerun with the same `--work-dir` reuses every leg whose
protocol, dependency, arm and table fingerprints match. Run the long stages with `OMP_NUM_THREADS=1`.

```
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py freeze-inputs
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py verify-inputs
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py build-arms --check
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py coverage
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py tables --work-dir <dir> --workers <n>
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py replay-check --cache-db <new path> --work-dir <dir> --workers <n>
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py lock
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py select --cache-db <new path> --work-dir <dir> --workers <n>
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py judge --cache-db <new path> --work-dir <dir> --workers <n>
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py verdict
uv run --no-sync python backtest/candidates/chart_pattern_2015/run_study.py render --check
uv run --no-sync python -m pytest backtest/tests/test_chart_pattern_2015_integration.py -q
go -C scheduler build -o <scratch>/go-trader . && <scratch>/go-trader inspect --config backtest/candidates/chart_pattern_2015/arms/ratchet_both/f4.json --all
```

`freeze-inputs` and `lock` run once and refuse to overwrite. `replay-check` runs only before the lock, `select`
refuses an existing `selection.json`, and `judge` and `verdict` refuse results written under another lock or
selection.

## Driver change after the lock

One fix landed in `run_study.py` after `protocol_lock.json` and after `judge`, before the final `verdict.json`:
`index_results` keyed result rows by coin only, so the 1h and 4h audit legs of one coin overwrote each other in the
audit scoring. It now keys rows by coin and interval and refuses a duplicate. No leg was rescored (leg fingerprints do
not include the driver), `results.json` is unchanged, and `verdict` and `render` were rerun. The `driver_sha256`
recorded in `protocol_lock.json` is the driver before this fix.
