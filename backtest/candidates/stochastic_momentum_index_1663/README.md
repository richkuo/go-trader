# Stochastic Momentum Index pullback entries (#1663)

Research candidate `stochastic_momentum_index` (short name `smi`): a
futures-only, bidirectional entry evaluator registered with
`edge_status="no_edge"` (hidden from discovery). It follows the current no-edge
admission contract: exactly one explicit `--mode=paper` runs without an
acknowledgement; live mode or a missing mode needs `allow_no_edge: true`.
Malformed mode or acknowledgement input is refused, for an effective open
strategy and for an open-strategy close fallback. This work grants no
acknowledgement and makes no activation, promotion, deployment, live-default or
deployed-configuration change.

The settings below are research settings. They are not approved trading
settings. `candidate_seed.json` freezes the formula, the parameter domains, the
grid, the arms, the selection, sample, cost, stability and verdict rules, and
the continuously seeded diagnostic. Its protocol sha256
(`run_study.py --protocol-hash`) is written into `results.json`. The seed,
this file, the manifest and both drivers were committed before any window was
scored.

## Formula (frozen)

For bar `i`, `HH(i)` and `LL(i)` are the highest high and lowest low of the
`range_period` bars ending at `i`; `range(i) = HH(i) - LL(i)` and
`relative(i) = close(i) - (HH(i) + LL(i)) / 2`.

For every endpoint `t` the evaluator keeps no state between evaluations. With
`L = recursion_length` and seed bar `s = t - L`:

1. `n1(s) = n2(s) = relative(s)`; then for `k = s+1 .. t`:
   `n1(k) = a * relative(k) + (1 - a) * n1(k-1)` and
   `n2(k) = a * n1(k) + (1 - a) * n2(k-1)`, with `a = 2 / (smoothing_period + 1)`.
2. `d1` and `d2` run the same two recursions on `range`, seeded with `range(s)`.
3. `smi(k) = 200 * n2(k) / d2(k)`.
4. The signal line is seeded with `smi(s)` and updated with
   `b = 2 / (signal_period + 1)` from the oscillator sequence of the same
   endpoint replay.
5. Exactly `L` updates run after the seed. The endpoint values are `smi(t)`,
   `line(t)` and `diff(t) = smi(t) - line(t)`.

Endpoint `t - 1` is replayed independently from its own seed `t - 1 - L`; the
penultimate value of the `t` replay is never used. An endpoint needs `L + K`
input bars (`K = range_period`) and a crossing needs `L + K + 1`.

- Long: `smi(t-1) <= oversold`, `smi(t) > oversold`, `diff(t-1) <= 0` and
  `diff(t) > 0` on the same bar.
- Short: `smi(t-1) >= overbought`, `smi(t) < overbought`, `diff(t-1) >= 0` and
  `diff(t) < 0` on the same bar.
- Otherwise no entry (`level_cross_only`, `signal_cross_only` or `no_cross`).

Parameters (research seeds): `range_period` 10, `smoothing_period` 3,
`signal_period` 3, `recursion_length` 60, `oversold` -40, `overbought` 40.
Periods and `recursion_length` are positive integers (integer-valued floats are
accepted; booleans, strings, fractions and non-finite values are refused).
Thresholds are finite with `-100 < oversold < overbought < 100`.
`recursion_length` must be at least 8 times the larger of the smoothing and
signal periods, and `recursion_length + range_period` at most 198, so a
crossing fits in 199 closed rows of a 200-row check input that also holds a
forming row. The registry `param_validator` and the evaluator share one
validation function; the binary constraint grammar cannot express the compound
rules. Every optimizer combination passes it.

The finite replay is the candidate definition. It guarantees the same decision
for identical valid trailing inputs whatever the history length; it does not
claim equality with a continuously seeded indicator. No numeric seed-error
figure is claimed; the continuously seeded comparison in `REPORT.md` names its
script, inputs, parameters and hashes.

## Input validity and runtime freshness

Inside the dependency span (rows `t-L-K .. t`): finite positive open, high, low
and close, `low <= open, close <= high`, a timestamp on every row, strictly
increasing timestamps spaced exactly at the frame's declared cadence
(`bar_interval_ms`), and no row that was received out of order. Volume is not
an input. A smoothed range that is not strictly positive anywhere in either
replay (both signal-line seeds included) holds with `zero_range`; a non-finite
derived value holds with `nonfinite_derived`. Any defect holds the entry with a
reason and zero signal; the evaluator recovers once the defect leaves the span.
It never raises on bad data, so the explicit close owner keeps evaluating an
open position.

Only the Hyperliquid check marks a check-time frame (`decision_time` frame
attribute). Its last row is fresh only when the selected closed-bar boundary
equals the row's own boundary and `0 <= cutoff - boundary < interval`; an
untimed check-time frame holds with `decision_time_missing`. `LoadConfig` and
`LoadConfigForProbe` refuse every `stochastic_momentum_index` reference
(positional open, `open_strategy`, `close_strategy` fallback) unless the
strategy is Hyperliquid perps with `closed_bar_decisions: true`
(`closedBarRequiredOpenStrategies`). The candidate evaluates every row it is
given and removes none; the closed-bar selector owns the forming-row cut.
Historical registry, simulator and parity frames carry no attribute and use
each row's own boundary.

## Diagnostics

Every row carries `smi_eval_ts_ms`, `smi_value`, `smi_prev_value`,
`smi_signal_line`, `smi_prev_signal_line`, `smi_diff`, `smi_prev_diff`, the
smoothed relative value and range, both seed bars' relative value and range,
thresholds and parameters, `smi_endpoint_bars`, `smi_required_history`,
validity flags, `smi_reason` and `smi_reason_code`; check-time rows add the
cutoff and boundary. `check_hyperliquid.py` keeps the `smi_` values at full
float precision (`FULL_PRECISION_INDICATOR_PREFIXES`) and emits `smi_reason` as
a text label, so an entry reproduces from the check output, threshold
boundaries included.

## Close and protection

Entry logic owns no exit. Every study arm and the generated paper entry use one
close owner (`tiered_tp_pct` with one tier at +3% closing the whole position)
and one stop owner (`stop_loss_atr_mult` 1.0). The composer holds new entries
while a position is open; an invalid or stale entry frame holds the entry while
the close owner keeps closing long and short positions. No reversal, scale-in,
regime classifier, state owner, feed requirement or probe argument is added.

Paper add (`buildAddStrategyEntry`): explicit `--mode=paper`, `direction`
both, `close_strategy` `tiered_tp_pct` with the one-tier ladder,
`closed_bar_decisions: true`, `stop_loss_atr_mult` 1.0 and no `allow_no_edge`.

## Study protocol (frozen)

- Venue (primary): `backtest/candidates/stochastic_momentum_index_1663_manifest.json`
  references the hash-pinned `connors_rsi_reversion_1645/data/` Hyperliquid 4h
  candles and funding in place (offline manifests cannot reference files
  outside their own directory, so the manifest sits in `backtest/candidates/`).
  Costs come from its execution specification (fees, per-coin half spread,
  slippage, szDecimals lot flooring, the $10 minimum) and its hourly funding;
  stress doubles half spread and slippage.
- Proxy (secondary): the hash-pinned Binance US 4h candles and Hyperliquid
  funding of `ultimate_oscillator_1664/data/`, referenced in place and verified
  against its inventory, loaded into a private scratch SQLite cache
  (`GO_TRADER_OHLCV_CACHE_DB`) with network fetches disabled. Hyperliquid fees,
  the default slippage standing in for spread, `--funding charge`, no lot or
  minimum rule; stress doubles slippage.
- BTC, ETH and SOL at 4h. Train [2024-09-01, 2025-09-01), held-out
  [2025-09-01, 2026-09-01), 300 warm-up bars. Earlier studies used these
  windows, so the evidence is retrospective.
- Coverage gates run before selection (venue: exact grid, valid rows, funding
  hours and the right-closed accrual span; proxy: window edges, consecutive
  warm-up, at most 0.5% missing bars, every gap listed). A funding-incomplete
  leg is excluded from selection and from every economic conclusion.
- Arms: `smi_long` against `stoch_rsi` (both long only; `stoch_rsi` registers no
  short entries, so its -1 never becomes a short) and `smi_both` against
  `mean_reversion_pro` (both directions). Same close, stop, datasets, windows,
  capital and costs inside each matched comparison. The comparator grids are
  their unchanged optimizer rows. The M1 incumbent aggregate runs long only
  under the same contract and is descriptive.
- Selection on venue train only: every dataset scored with complete coverage,
  no liquidation, at least 30 independent position clusters; max mean net
  Sharpe, then max mean drawdown-adjusted return, then grid order. The proxy
  evidence reuses the venue selections.
- Held-out gates: at least 30 independent position clusters per arm, at least
  10 positions per direction for the bidirectional arms, and at least one
  evaluable one-step grid neighbour per candidate arm.
- Pass (per provenance): no liquidation, both candidate arms net positive at
  base and stress cost, each candidate arm strictly above its matched baseline
  on base mean Sharpe and mean drawdown-adjusted return, and at least two
  thirds of the evaluable one-step neighbours net positive. A complete study
  that misses a condition fails; a coverage, sample or evaluability gap is
  inconclusive. Overall: the venue verdict when it is fail or inconclusive; a
  venue pass becomes the overall pass only when the proxy also passes,
  otherwise inconclusive.
- A pass keeps `no_edge` with `edge_source` `unvalidated`; a fail records
  `study_fail` and an inconclusive result `study_inconclusive`, identically in
  `shared_strategies/open/registry.py` and `scheduler/edge_status.go`.

## Reproduce

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/stochastic_momentum_index_1663_manifest.json
uv run --no-sync python backtest/candidates/stochastic_momentum_index_1663/run_study.py --protocol-hash
uv run --no-sync python backtest/candidates/stochastic_momentum_index_1663/run_study.py
uv run --no-sync python backtest/candidates/stochastic_momentum_index_1663/run_parity.py
uv run --no-sync python backtest/candidates/stochastic_momentum_index_1663/run_study.py --render-only
```

`run_study.py` writes `results.json`, `REPORT.md` and the full-precision
evidence files in `evidence/`; `run_parity.py` writes `parity_results.json`.
Both read only local frozen inputs and need no network access.

`discovery_before_{spot,futures}.json` and `discovery_after_{spot,futures}.json`
hold the `--list-json` output of the spot and futures shims before and after
this change; each pair is byte-identical.

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code
