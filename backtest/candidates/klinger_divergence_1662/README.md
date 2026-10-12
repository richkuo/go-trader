# Klinger Oscillator divergence entries (#1662)

Research candidate `klinger_divergence` (short name `klingerdiv`, tracking
group `hyperliquid-strategy-candidates-2026-10-02`): a futures-only,
bidirectional entry evaluator registered with `edge_status="no_edge"` (hidden
from discovery). It follows the current no-edge admission contract: exactly
one explicit `--mode=paper` runs without an acknowledgement; live mode or a
missing mode needs `allow_no_edge: true`, and the existing valid live
acknowledgement stays supported. Invalid mode or acknowledgement input is
refused, for an effective open strategy and for an open-strategy close
fallback. This work grants no acknowledgement and makes no activation,
promotion, deployment, live-default or deployed-configuration change.

The settings below are research settings. They are not approved trading
settings. `candidate_seed.json` freezes the formula, the seed rules, the event
boundaries, the parameter domains, the grid, the arms, the selection, sample,
cost and verdict rules, the continuous-history comparison and the parity
checks. Its protocol sha256 (`run_study.py --protocol-hash`) is written into
`results.json` before any score is computed. The seed, this file, the manifest
and `run_study.py` were committed before the held-out window was scored.

## Formula (frozen)

The volume-force recurrence follows the TradingView Klinger Oscillator
calculation description named in the issue, implemented as written in the
issue. The 13-period signal average is an exponential moving average; the
reference does not state its type. This is a chosen finite-replay research
variant. No equivalence with any platform's built-in implementation is
claimed.

For every endpoint `t` the evaluator replays exactly `replay_bars + 1`
trailing closed bars (rows `t-replay_bars .. t`) and keeps no state between
endpoints or between calls:

1. Seed row `t-replay_bars`: trend `-1` (no earlier price is read),
   `dm = high - low`, `cm = dm`, and the seed volume force from the formula
   below (zero when `dm > 0`). Both EMAs start at the seed force; the signal
   line starts at the seed oscillator (`0`).
2. Later rows: trend `+1` when `high + low + close` exceeds the previous sum,
   else `-1` (an equal sum gives `-1`). `cm` adds `dm` while the trend
   repeats and restarts at `previous dm + dm` on a change.
3. `volume force = volume * 2 * (dm / cm - 1) * trend * 100`. Because
   `cm >= dm`, the factor `dm / cm - 1` is never positive, so the force has
   the opposite sign of the trend except when `dm == cm`. This is the issue's
   formula as written; the report states the property and the study does not
   change it.
4. Oscillator = fast EMA minus slow EMA of the force
   (`alpha = 2 / (period + 1)`); signal line = `signal_period` EMA of the same
   replay's oscillator values. Every recurrence updates inside one replay; no
   average is fed from values with a longer history.
5. A non-positive or non-finite `cm` anywhere in a replay invalidates that
   endpoint (`zero_cm`). Zero volume is valid (zero force when `cm > 0`); a
   zero-range bar with positive `cm` is valid.

Events, all on confirmed bars:

- Strict price pivots: a low pivot at `q` is strictly below every low in
  `q-left_span .. q-1` and `q+1 .. q+right_span`; highs mirror. A pivot is
  confirmed and usable at `c = q + right_span`.
- Pairing: each newly confirmed pivot pairs with the nearest earlier
  same-side pivot `p` with `min_separation <= q - p <= max_separation` (the
  largest pivot `<= q - min_separation`; no pair when that one is older than
  `q - max_separation`). No farther pivot is searched.
- Divergence: long needs a strictly lower low and a strictly higher
  oscillator at the pivot bars; short needs a strictly higher high and a
  strictly lower oscillator. Equal prices or oscillator values fail.
- Entry: an armed setup enters on the first cross of the oscillator through
  its signal line in its direction (up: `osc(t-1) <= sig(t-1)` and
  `osc(t) > sig(t)`; down mirrors) on bars `c+1 .. c+setup_expiry`; the
  confirmation bar cannot enter. The first eligible cross consumes the setup,
  whether or not that bar's entry was held.
- Replacement: every newly confirmed same-side pivot replaces the previous
  same-side setup on its confirmation bar, also when the new pair fails.
- Opposite sides are independent; simultaneous long and short entries hold
  and consume both (unreachable, because one bar cannot cross up and down).
- `divergence_test = false` is the signal-cross-only ablation: it keeps
  pivots, pairing, replacement, expiry, crossing and consumption and drops only
  the two strict comparisons. No ablation strategy is registered.

Parameters (research seeds): `fast_period` 34, `slow_period` 55,
`signal_period` 13, `replay_bars` 112, `left_span` 3, `right_span` 3,
`min_separation` 5, `max_separation` 40, `setup_expiry` 12,
`divergence_test` true. Domains: integer-valued fast in [2, 120], slow in
[3, 150], signal in [2, 150], replay in [4, 190] with fast < slow < replay and
signal < replay; spans in [1, 10]; separations in [1, 100] with min <= max;
expiry in [1, 50]; a boolean `divergence_test`; and
`replay_bars + left_span + max_separation + right_span + setup_expiry + 2 <= 200`.
Booleans, strings, fractional and non-finite values are refused before
evaluation: the registry constraints and `param_validator` run in solo and
batch preflight, and the evaluator validates again.

## Dependency span

A decision at bar `k` reads exactly
`S = replay_bars + left_span + max_separation + right_span + setup_expiry + 2`
rows `k-S+1 .. k` (default 172, grid maximum 188, at most 200 by validation).
An entry at `k` needs `c >= k - setup_expiry`, so the second pivot
`q >= k - setup_expiry - right_span` and the first pivot
`p >= q - max_separation`. The earliest read is the first pivot's left span
or its oscillator replay, `p - max(left_span, replay_bars)`, which is at least
`k - S + 2`. Every cross in `c+1 .. k` reads endpoints at or after `c`, and
every pivot that could replace the setup confirms inside `c .. k`. The bound
fits the ordinary 200-row check window, so no feed lookback, fetch limit or
startup-probe change is needed. `backtest/tests/test_klinger_divergence_integration.py`
proves it by perturbing the row just outside the span (no change) and the
first row inside it (hold).

## Input validity and runtime freshness

Inside the span: finite positive open, high, low and close, low <= open and
close <= high, finite volume >= 0, a timestamp on every row, strictly
increasing timestamps spaced exactly at the declared cadence
(`bar_interval_ms`), no row received out of order, and a valid oscillator
endpoint for every endpoint whose replay fits in the span. Any defect holds
the entry with a reason; parameter errors stay preflight errors. A defect
outside the span has no effect.

Only the Hyperliquid check marks a check-time frame (`decision_time`). Its
last row is fresh only when the selected closed bar boundary equals the row's
boundary and `0 <= cutoff - boundary < interval`; an untimed check-time frame
holds (`decision_time_missing`), so runtime use needs `closed_bar_decisions`.
`LoadConfig` and `LoadConfigForProbe` refuse every `klinger_divergence`
reference (positional open, `open_strategy`, `close_strategy` fallback) unless
the strategy is Hyperliquid perps with `closed_bar_decisions: true`
(`closedBarRequiredOpenStrategies`). Forming-bar volume can therefore never
confirm a pivot or trigger a cross. Historical frames carry no attribute and
use each row's own boundary.

Reason priority: frame defects; else the latest row or timestamp defect in the
span; else `insufficient_history`; else `zero_cm`; else `nonfinite_derived`;
else (last check-time row only) `decision_time_missing`,
`decision_boundary_mismatch`, `decision_bar_future`, `decision_bar_stale`;
else `opposite_events`, `entry_long`, `entry_short`, `setup_armed`,
`awaiting_cross`, `setup_consumed`, `no_divergence` or `no_pivot_pair` (long
side first), `no_setup`.

## Diagnostics

Every row carries bar-open timestamps (`klinger_eval_ts_ms`, replay and
support starts), the endpoint trend, `dm`, `cm`, volume force, both EMAs,
oscillator, signal line, previous values and cross, the endpoint and decision
validity flags, both setups (state, armed flag, both pivot timestamps, prices
and oscillator values, confirmation and expiry), the parameters, the support
span, `klinger_reason` and `klinger_reason_code`; check-time rows add the
cutoff and boundary. `check_hyperliquid.py` keeps the `klinger_` values at
full float precision and emits `klinger_reason` as a text label, so an entry
reproduces from the check output.

## Close and protection

Entry logic owns no exit. Every study arm and the generated paper entry use one
close owner (`tiered_tp_pct` with one 2% full-close tier) and one stop owner
(`stop_loss_atr_mult` 1.0). The composer holds new entries while a position is
open; an invalid or stale entry frame holds the entry while the close owner
keeps closing long and short positions. No reversal, scale-in, hedge or paired
lifecycle is added.

Paper add (`buildAddStrategyEntry`, also used by the status page): explicit
`--mode=paper`, `direction` both, `close_strategy` `tiered_tp_pct` with the
2% full-close tier, `closed_bar_decisions: true`, `stop_loss_atr_mult` 1.0 and
no `allow_no_edge`. The frozen v20 migration never stamps this name.

## Study protocol (frozen)

- Data: `backtest/candidates/klinger_divergence_1662_manifest.json` references
  the hash-pinned `on_balance_volume_divergence_1657/data/` Hyperliquid 4h
  venue candles, actual venue volume, funding, venue metadata and spread
  snapshot in place (offline manifests cannot reference files outside their
  own directory, so the manifest sits in `backtest/candidates/`). No data is
  acquired. Windows: train [2024-09-01, 2025-09-01), held-out
  [2025-09-01, 2026-09-01), 300 warm-up bars. Earlier studies used these
  windows, so the result is retrospective candidate evidence.
- Costs: the manifest execution specification (taker and maker fee, per-coin
  half spread from one stored snapshot, slippage, szDecimals lot flooring, $10
  minimum notional) and its hourly funding. Stress doubles half spread and
  slippage; fees stay unchanged. No Binance US proxy leg and no cache-based
  `--funding` option is used.
- Coverage gate before selection: exact candle grid, valid rows, warm-up at
  least the largest candidate span, complete left-closed funding coverage and
  a complete right-closed accrual span. A refused window makes the study
  inconclusive.
- Arms: `klinger_long` and `klinger_both` (primary candidates, own grid);
  `ablation_long` and `ablation_both` (inherit the selected candidate
  parameters, `divergence_test` false); `obv_long` and `obv_both`
  (`on_balance_volume_divergence`, its existing grid); `volume_weighted`
  (long only, its existing grid). Comparisons: `klinger_long` against
  `volume_weighted`, `obv_long` and `ablation_long`; `klinger_both` against
  `obv_both` and `ablation_both`. Same close owner, stop owner, datasets,
  windows, capital, funding and costs in every comparison.
- Selection on train only: complete coverage, every dataset scored, no
  liquidation or incomplete funding, at least 10 independent position clusters
  (overlapping position intervals merged across datasets); order max mean net
  Sharpe, then max mean drawdown-adjusted return, then earliest grid order.
- Held-out gates: at least 30 independent clusters in each primary arm; the
  bidirectional arm needs long and short positions; every finite-versus-
  continuous decision mismatch must be classified; decision and indicator
  parity must be clean.
- Pass: both primary arms net positive at base and stress cost, and at base
  cost each primary arm has a strictly higher mean net return and a worst
  drawdown no larger than each of its comparators. A complete study that
  misses a pass condition fails. Coverage gaps, funding gaps, failed sample
  gates, a missing selection, unclassified mismatches or parity failures are
  inconclusive.
- Every outcome keeps `no_edge`. A pass keeps `edge_source` `unvalidated`; a
  fail records `study_fail` and an inconclusive result `study_inconclusive`,
  identically in `shared_strategies/open/registry.py` and
  `scheduler/edge_status.go`. A pass would still need separate approval
  before any edge classification changes. Results are suggest-only.
- The M1 incumbents are reported separately under the long-only common
  contract; the bidirectional arm has no matched incumbent.

## Reproduce

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/klinger_divergence_1662_manifest.json
uv run --no-sync python backtest/candidates/klinger_divergence_1662/run_study.py --protocol-hash
uv run --no-sync python backtest/candidates/klinger_divergence_1662/run_study.py --manifest backtest/candidates/klinger_divergence_1662_manifest.json
uv run --no-sync python backtest/candidates/klinger_divergence_1662/run_study.py --render-only
```

The study writes `results.json`, `REPORT.md`, `parity_results.json` and the
full-precision evidence files in `evidence/`. It reads only local manifest
inputs and needs no network access.

`discovery_before_{spot,futures}.json` and `discovery_after_{spot,futures}.json`
hold the `--list-json` output of the spot and futures shims before and after
this change; each pair is byte-identical.

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code
