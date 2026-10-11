# Schaff Trend Cycle entries (#1651)

Research candidate `schaff_trend_cycle`: a futures-only, bidirectional entry
evaluator registered with `edge_status="no_edge"` (hidden from discovery). It
follows the current no-edge admission contract: exactly one explicit
`--mode=paper` runs without an acknowledgement; live mode or a missing mode
needs `allow_no_edge: true`. Invalid mode or acknowledgement input is refused,
for an effective open strategy and for an open-strategy close fallback. This
work grants no acknowledgement and makes no activation, promotion, deployment,
live-default or deployed-configuration change.

The settings below are research settings. They are not approved trading
settings. `candidate_seed.json` freezes the formula, the parameter domains, the
grid, the arms, the selection, sample, cost and verdict rules, and the
continuous-history comparison. Its protocol sha256
(`run_study.py --protocol-hash`) is written into `results.json` before any
score is computed. The seed, this file and the manifest were committed before
the held-out window was scored.

## Formula (frozen)

For every endpoint `t` the evaluator reads exactly 192 trailing closed bars
(rows `t-191 .. t`) and keeps no state between evaluations:

1. Fast and slow close EMAs restart at `close(t-191)`; then
   `ema(k) = alpha * close(k) + (1 - alpha) * ema(k-1)` with
   `alpha = 2 / (period + 1)`.
2. `macd(k) = fast_ema(k) - slow_ema(k)`. The first `slow_period - 1` samples
   of the replay are discarded.
3. Every complete, inclusive `cycle_length` window of the kept MACD samples is
   normalized: `100 * (macd(k) - min) / (max - min)`.
4. The first stage is smoothed recursively, seeded with its first value:
   `s1(k) = s1(k-1) + smoothing * (raw1(k) - s1(k-1))`.
5. Every complete `cycle_length` window of `s1` is normalized the same way
   and smoothed with the same rule, seeded with its first value. The value at
   row `t` is the oscillator.
6. A range that is not strictly positive in any normalization window of the
   replay invalidates the endpoint (`zero_range`). No carried value and no
   zero oscillator is substituted.

The previous endpoint `t-1` is computed independently from rows
`t-192 .. t-1`. A decision therefore needs 193 valid consecutive closed bars,
which fits the ordinary 200-row check window for every supported parameter
set; no fetch limit, feed requirement or runtime lookback changes.

- Long: `stc(t-1) <= lower_threshold`, `stc(t) > lower_threshold` and
  `macd(t) > macd(t-1)`.
- Short: `stc(t-1) >= upper_threshold`, `stc(t) < upper_threshold` and
  `macd(t) < macd(t-1)`.
- A crossing with a zero or wrong-signed MACD change holds
  (`slope_disagrees`). A sustained condition never emits a second entry.

Parameters (research seeds): `fast_period` 23, `slow_period` 50,
`cycle_length` 10, `smoothing` 0.5, `lower_threshold` 25, `upper_threshold`
75. Domains: integer-valued periods with fast in [2, 32], slow in [3, 64],
fast strictly below slow, cycle in [2, 16]; finite smoothing in [0.25, 1];
finite thresholds with `0 < lower < upper < 100`. Booleans, strings,
fractional periods and non-finite values are refused before evaluation (the
registry `param_validator` and constraints run in solo and batch preflight).

The thinkorswim Schaff Trend Cycle reference was not read for this work. The
formula above is the one implemented, and an independent scalar
re-implementation in `backtest/tests/test_schaff_trend_cycle_integration.py`
checks its intermediate and endpoint values exactly. No economic edge follows
from any reference. The input is the MACD line; `stoch_rsi` uses a different
input.

## Input validity and runtime freshness

Inside rows `t-192 .. t`: finite positive open, high, low and close, low <=
open and close <= high, finite volume >= 0 (zero volume is valid because the
formula does not divide by volume), a timestamp on every row, strictly
increasing timestamps spaced exactly at the frame's declared cadence
(`bar_interval_ms`), and no row that was received out of order. Any defect
holds the entry with a reason; the evaluator recovers once the defect leaves
the dependency span.

Only the Hyperliquid check marks a check-time frame (`decision_time` frame
attribute). A closed-bar decision frame carries the captured cutoff and the
selected bar boundary; its last row is fresh only when that boundary equals
the row's own boundary and `0 <= cutoff - boundary < interval`. A check-time
frame without timing (untimed checks, which include the forming bar) holds
with `decision_time_missing`, so runtime use needs `closed_bar_decisions`.
`check_hyperliquid.py` also records timestamps that arrive out of order before
its frame builder sorts the rows. Historical registry, simulator and parity
frames carry no attribute and use each row's own boundary. OKX and other
check scripts do not mark frames; this candidate is studied and wired for
Hyperliquid only.

Reason priority: frame defects; else the latest row or timestamp defect in the
dependency span; else `insufficient_history`; else `nonfinite_derived`; else
`zero_range`; else (last check-time row only) `decision_time_missing`,
`decision_boundary_mismatch`, `decision_bar_future`, `decision_bar_stale`;
else `entry_long`, `entry_short`, `slope_disagrees` or `no_cross`.

## Diagnostics

Every row carries `stc_eval_ts_ms`, `stc_value`, `stc_prev_value`,
`stc_macd`, `stc_prev_macd`, `stc_macd_slope`, `stc_stoch1_raw`,
`stc_stoch1`, `stc_stoch2_raw`, the thresholds and parameters, the replay and
required-history spans, validity flags, `stc_reason` and `stc_reason_code`;
check-time rows add the cutoff and boundary. `check_hyperliquid.py` keeps the
`stc_` values at full float precision (`FULL_PRECISION_INDICATOR_PREFIXES`)
and emits `stc_reason` as a text label, so an emitted entry reproduces from the
check output, threshold boundaries included.

## Close and protection

Entry logic owns no exit. Every study arm and the generated paper entry use one
close owner (`tiered_tp_pct` with its default ladder) and one stop owner
(`stop_loss_atr_mult` 1.0). The composer holds new entries while a position is
open; an invalid or stale entry frame holds the entry while the close owner
keeps closing long and short positions. No reversal, scale-in, hedge, paired or
quote lifecycle is added.

Paper add (`buildAddStrategyEntry`): explicit `--mode=paper`, `direction`
both, `close_strategy` `tiered_tp_pct`, `closed_bar_decisions: true`,
`stop_loss_atr_mult` 1.0 (so a root `default_stop_loss_atr_mult: 0` still
leaves exactly one stop owner) and no `allow_no_edge`.

## Study protocol (frozen)

- Data: `backtest/candidates/schaff_trend_cycle_1651_manifest.json` references
  the hash-pinned `connors_rsi_reversion_1645/data/` Hyperliquid 4h venue
  candles and funding in place (the same files as the #1646 manifest; offline
  manifests cannot reference files outside their own directory, so the manifest
  sits in `backtest/candidates/`). Windows: train [2024-09-01, 2025-09-01),
  held-out [2025-09-01, 2026-09-01), 300 warm-up bars. These windows were used
  by earlier studies, so the result is retrospective candidate evidence.
- Costs: the manifest execution specification (taker and maker fee, per-coin
  half spread, slippage, szDecimals lot flooring, $10 minimum notional) and its
  hourly funding coverage. Stress doubles half spread and slippage. No Binance
  US proxy leg and no cache-based `--funding` option is used.
- Coverage gate before selection: exact candle grid, valid rows, at least 193
  warm-up bars, complete left-closed funding coverage and a complete
  right-closed accrual span. A refused window makes the study inconclusive.
- Arms: `schaff_long` (matched long-only candidate), `schaff_both`
  (bidirectional candidate, reported separately), `macd` and `stoch_rsi`
  (registered long-only comparators). Same close owner, stop owner, datasets,
  windows, capital and costs inside every comparison.
- Selection on train only: complete coverage, every dataset scored, no
  liquidation, at least 10 independent positions; order max mean net Sharpe,
  then max mean drawdown-adjusted return, then earliest grid order.
- Held-out gates: at least 30 independent positions per selected arm; the
  bidirectional arm needs long and short positions; every finite-versus-
  continuous decision mismatch must be classified.
- Pass: both candidate arms net positive at base and stress cost, and the
  matched long-only arm's base mean Sharpe and mean drawdown-adjusted return
  both strictly above each selected comparator. A complete study that misses a
  pass condition fails. Gaps, failed sample gates or unclassified mismatches are
  inconclusive.
- A pass keeps `no_edge` with `edge_source` `unvalidated`; a fail records
  `study_fail` and an inconclusive result `study_inconclusive`, identically in
  `shared_strategies/open/registry.py` and `scheduler/edge_status.go`.

## Reproduce

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/schaff_trend_cycle_1651_manifest.json
uv run --no-sync python backtest/candidates/schaff_trend_cycle_1651/run_study.py --protocol-hash
uv run --no-sync python backtest/candidates/schaff_trend_cycle_1651/run_study.py --manifest backtest/candidates/schaff_trend_cycle_1651_manifest.json
uv run --no-sync python backtest/candidates/schaff_trend_cycle_1651/run_study.py --render-only
```

The study writes `results.json`, `REPORT.md`, `parity_results.json` and the
full-precision evidence files in `evidence/`. It reads only local manifest
inputs and needs no network access.

`discovery_before_{spot,futures}.json` and `discovery_after_{spot,futures}.json`
hold the `--list-json` output of the spot and futures shims before and after
this change; each pair is byte-identical.

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code
