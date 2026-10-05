# Commodity Channel Index trend entries (#1656)

Research-only candidate `commodity_channel_trend` (futures registry,
`backtest_only=True`, hidden from discovery). Every live check script refuses
it as an entry and as a close fallback. Promotion is a separate reviewed
decision. Nothing in this directory writes a live default, a config or a
deployment file.

The held-out result is in [`REPORT.md`](REPORT.md) (rendered from
[`results.json`](results.json)). This file is the frozen specification: the
signal contract, the truth table, the research settings, the selection rule and
the verdict rule. They were fixed before the held-out window was scored. The
study follows the #1649 pattern
([`../chaikin_money_flow_1649/`](../chaikin_money_flow_1649/README.md)) and
reuses its shared harness unchanged.

## Promotion record

- Proposed short name: `cct` (the derived name; no collision in the scheduler
  short-name map). The name is not in that map, so the Discord and status-page
  add-strategy command cannot add it.
- Direction: `both`. The name is in the scheduler and M5 bidirectional sets,
  which only choose `direction: both` for a name that already passed the
  add-strategy or wizard gate.
- Hard history need: `max(lookback + 1, trend_period)` consecutive valid bars,
  at most 150 bars with the bounds below. The live check window is 200 bars, so
  no feed, check-script or probe change is needed.

## Signal contract

Inputs: one closed-bar candle frame with `high`, `low`, `close`. The evaluator
reads no other data, fetches nothing, and owns only evaluation-local arrays.
The scheduler keeps all trade, stop and position ownership.

- Typical price per bar: `(high + low + close) / 3`.
- Mean at bar N: the simple mean of the typical prices of the `lookback` bars
  ending at N.
- Mean deviation at bar N: the mean of `|typical - mean(N)|` over the same
  bars, around that window's own mean.
- Index at bar N: `(typical(N) - mean(N)) / (0.015 x mean deviation(N))`. A
  window whose mean deviation is zero has no index (`zero_mean_deviation`).
- Trend average at bar N: the simple mean of the closes of the `trend_period`
  bars ending at N.
- Long entry at N: index(N-1) `<= threshold`, index(N) `> threshold`, and
  close(N) `>` trend average(N).
- Short entry at N: index(N-1) `>= -threshold`, index(N) `< -threshold`, and
  close(N) `<` trend average(N).
- A close equal to the trend average blocks both sides. An index equal to the
  threshold at N is not a cross; an index equal to the threshold at N-1 followed
  by a strictly larger index at N is a cross.
- Every mean, deviation and average is computed from that window's own bars
  (a sliding-window view), not from a running sum. The full series, every
  historical prefix and a bounded tail give bit-identical values for every bar
  that has enough history. The shared pandas `sma` helper is a running sum whose
  last bits depend on the frame start, so it is not used for the decision.

### Invalid input

Parameters are validated and invalid values raise `ValueError`: `lookback` is
an integer in [2, 100] and `trend_period` an integer in [2, 150] (booleans and
fractional values are rejected); `threshold` is finite and strictly positive.

Market-data defects never raise. They produce a hold with a reason code in
`cct_reason` (text) and `cct_reason_code` (number): `missing_columns`,
`nonfinite_input`, `inconsistent_hlc` (high below low, or close outside the
range), `timestamp_order` (a timestamp not strictly after the previous bar's,
when the frame has a datetime index or a `timestamp` column),
`insufficient_history` and `zero_mean_deviation`. A bad bar invalidates every
decision whose windows include it. Gaps are never filled with invented prices.

### Explanation columns

Each row carries `cct_typical`, `cct_mean`, `cct_mean_dev`, `cct_index`,
`cct_prev_index`, `cct_trend_sma`, `cct_threshold`, `cct_eval_ts_ms` (the
evaluated bar's timestamp), `cct_valid`, `cct_reason`, `cct_reason_code` and
`signal`, next to the input columns. These values are enough to reproduce every
emitted entry.

### Truth table

`T` = threshold, `I` = index, `A` = trend average.

| Case | I(N-1) | I(N) | close(N) vs A | Signal | Reason |
|---|---|---|---|---|---|
| 1 | <= T | > T | above | +1 | entry_long |
| 2 | <= T | > T | equal | 0 | trend_blocked |
| 3 | <= T | > T | below | 0 | trend_blocked |
| 4 | < T | = T | above | 0 | no_cross |
| 5 | = T | > T | above | +1 | entry_long |
| 6 | > T | > T | above | 0 | no_cross |
| 7 | >= -T | < -T | below | -1 | entry_short |
| 8 | >= -T | < -T | equal | 0 | trend_blocked |
| 9 | > -T | = -T | below | 0 | no_cross |
| 10 | any | mean deviation 0 | any | 0 | zero_mean_deviation |
| 11 | mean deviation 0 | any | any | 0 | zero_mean_deviation |
| 12 | fewer than `max(lookback + 1, trend_period)` valid bars | - | - | 0 | insufficient_history |
| 13 | a defect inside either window | - | - | 0 | the defect's reason |

`shared_strategies/open/test_commodity_channel.py` checks each row through the
real registry.

## Research settings (not trading settings)

- Seed: `lookback=20`, `threshold=100`, `trend_period=50`.
- Optimizer grid (`DEFAULT_PARAM_RANGES`): lookback 14, 20, 30, 50; threshold
  50, 100, 150, 200; trend period 20, 50, 100, 150 (64 combinations).
- Every arm (candidate and both comparators) uses direction `both`, the
  existing `time_stop` close owner with `max_bars=20`, the fixed entry-ATR
  stop with multiplier 1.0, $1,000 capital, and the same execution spec, the
  same close and stop protocol as #1649. One close owner and one stop owner;
  the composer blocks new entries while a position is open, so there is no
  reversal or scale-in.
- Comparators: `momentum_pro` and `adx_trend`, unchanged, each tuned on the
  training window over its own existing `DEFAULT_PARAM_RANGES` grid (243 and 9
  combinations) with the same selection rule. Their registry defaults are also
  reported.
- The existing M1 incumbent bar (median of the eight incumbents on the same
  frozen data; it includes `momentum_pro` at its defaults under the flat fee
  model) is reported per arm as a separate reference. It does not enter the
  verdict.

## Data and costs

[`study_manifest.json`](study_manifest.json) (schema
`offline_candle_manifest/v1`, read by `backtest/offline_manifest.py`) pins the
same inputs as #1649. Manifest paths must stay inside the manifest's own
directory, so `data/` holds byte-identical copies of the #1649 files (same
sha256). The Hyperliquid candle endpoint serves only the most recent 5,000
candles, so a fresh acquisition can no longer return the early bars of these
windows.

- Venue candles: Hyperliquid mainnet 4h candles for BTC, ETH and SOL, closed
  bars only, gzip CSV with sha256.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out. Both have finite ends. 300 warm-up bars
  before each window feed the indicators and are never scored; a window with
  fewer warm-up bars fails closed.
- Funding: Hyperliquid hourly funding history, booked as a cost for every arm
  while a position is open. Coverage is measured per window; incomplete or
  missing funding is reported and makes the verdict inconclusive. An empty file
  is never treated as verified zero funding.
- Fees: base-tier perps taker 0.045% and maker 0.015% (pinned documentation
  quote with its sha256). Every fill in this study is a taker fill.
- Spread and slippage: a per-coin half spread from the pinned L2 snapshot (one
  point in time, not a historical average) plus 5 bps of slippage per side,
  combined once at execution. The stress run doubles both.
- Lot size: `szDecimals` from the pinned venue meta snapshot, checked against
  each dataset at load. Quantities are floored to the lot before any
  accounting.
- Minimum order: $10 with the live close gate's 3% margin. An entry below it is
  rejected and recorded. A full close (stop, time stop, end of data) always
  closes the whole position.

A manifest replay verifies every hash first and fails closed on a mismatch or
a missing file. It never falls through to the network loader.

## Selection rule (train only)

Eligible combinations: all three datasets scored, no liquidated leg, and at
least 10 positions across datasets. Pick the highest mean Sharpe, then the
highest mean drawdown-adjusted return, then the earliest grid order. With no
eligible combination the registry defaults are used.

## Verdict rule (held-out only)

Inputs: the candidate's selected parameters and each comparator's selected
parameters, all at cost x1, plus the candidate at cost x2.

1. Inconclusive when any candle or funding coverage on the held-out window is
   incomplete.
2. Inconclusive when the candidate has fewer than 30 independent positions
   across datasets.
3. Pass only when all hold: candidate mean net return > 0 at cost x1 and at
   cost x2, and the candidate's mean Sharpe and mean drawdown-adjusted return
   are both above each comparator's.
4. Otherwise fail.

A pass would not change availability. The candidate stays `backtest_only`
until a separate reviewed promotion.

## Reproduce

Offline (no network; verifies hashes, then reruns everything):

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/commodity_channel_trend_1656/study_manifest.json
uv run --no-sync python backtest/candidates/commodity_channel_trend_1656/run_study.py
uv run --no-sync python backtest/eval_windows.py --registry futures --candidate-json backtest/candidates/commodity_channel_trend_1656/candidate_seed.json --manifest backtest/candidates/commodity_channel_trend_1656/study_manifest.json --sweep-window train --sweep lookback=14,20,30,50
uv run --no-sync python backtest/run_backtest.py --mode single --registry futures --strategy commodity_channel_trend --close-strategy '{"name":"time_stop","params":{"max_bars":20}}' --stop-loss-atr-mult 1.0 --direction both --manifest backtest/candidates/commodity_channel_trend_1656/study_manifest.json --manifest-dataset "BTC 4h" --manifest-window test --comparison-mode approximate
```

`discovery_before.json` / `discovery_after.json` hold the spot and futures
`--list-json` output before and after this change (identical), and
`research_registry_diff.txt` holds the only research-registry difference (the
new entry).
