# Vortex trend crossover (#1647)

Research-only candidate `vortex_trend` (futures registry, `backtest_only=True`,
hidden from discovery). Every live check script refuses it as an entry and as a
close fallback. Promotion is a separate reviewed decision. Nothing in this
directory writes a live default, a config or a deployment file.

The held-out result is in [`REPORT.md`](REPORT.md) (rendered from
[`results.json`](results.json)). This file is the frozen specification: the
signal contract, the truth table, the research settings, the selection rule and
the verdict rule. They were fixed before the held-out window was scored. The
study reuses the #1649 harness (`backtest/offline_manifest.py`, the
`eval_windows.py --manifest` leg path and the backtester execution spec)
without changes, and follows the `chaikin_money_flow_1649` study layout.

Formula reference: [Vortex Indicator](https://www.tradingview.com/support/solutions/43000591352-vortex-indicator/).
The reference motivates the implementation. It is not evidence of an edge on
Hyperliquid.

## Signal contract

Inputs: one closed-bar candle frame with `high`, `low`, `close`. The evaluator
reads no other data, fetches nothing, and owns only evaluation-local arrays. The
scheduler keeps all trade, stop and position ownership.

- Per bar N, with both bar N and bar N-1 valid:
  `VM+ = |high(N) - low(N-1)|`, `VM- = |low(N) - high(N-1)|`, and
  `TR = max(high(N) - low(N), |high(N) - close(N-1)|, |low(N) - close(N-1)|)`
  from the shared `indicators_core.true_range_series`.
- The shared true range returns `high - low` when the previous close is
  missing. The evaluator does not accept that value: the first bar, and every
  bar whose previous bar is invalid, has no `VM+`, `VM-` or `TR`.
- `VI+(N) = sum(VM+) / sum(TR)` and `VI-(N) = sum(VM-) / sum(TR)` over the
  `period` bars ending at N. A window whose `TR` sum is zero is invalid
  (`zero_true_range`), never a zero reading.
- `D(N) = VI+(N) - VI-(N)`. `s` = `min_separation`.
- Long entry at N: `D(N) > s` and `D(N-1) <= s`. The positive line crosses
  above the negative line by more than the separation.
- Short entry at N: `D(N) < -s` and `D(N-1) >= -s`.
- Equality is no entry. With `s = 0` this is the plain crossover with strict
  comparisons: `VI+ > VI-` now and `VI+ <= VI-` on the previous bar.
- One crossing of the separation line gives one event. When `s > 0` and `D`
  falls back to `s` or below and then rises above `s` again, that is a new
  crossing and a new event, even if `VI+` stayed above `VI-`. The composer
  blocks new entries while a position is open, so this never adds to a
  position.
- Missing candles (a time gap with strictly increasing timestamps) use the
  previous row, as the shared true range does. Gaps are never filled with
  invented prices.
- Every window sum is computed from that window's own bars. A decision depends
  only on bars N-period-1 to N. The full series, every historical prefix and a
  bounded tail give the same decision for every bar with enough history.
- Hard history need: `period + 2` consecutive valid bars (one indicator value
  needs `period + 1`, and the crossover also needs the value on bar N-1). With
  the seed period 14 that is 16 bars, and at most 102 bars with the bounds
  below. The live check window is 200 bars, so no feed or argument change is
  needed.

### Invalid input

Parameters are validated and invalid values raise `ValueError`: `period` is an
integer in [2, 100] (booleans and fractional values are rejected);
`min_separation` is finite and in [0, 1).

Market-data defects never raise. They produce a hold with a reason code in
`vortex_reason` (text) and `vortex_reason_code` (number): `missing_columns`,
`nonfinite_input`, `inconsistent_hlc` (high below low, or close outside the
range), `timestamp_order` (a timestamp not strictly after the previous bar's,
when the frame has a datetime index or a `timestamp` column),
`insufficient_history` and `zero_true_range`. A bad bar invalidates every
decision whose windows include it, including the bar after it (its missing
previous-bar prices).

### Explanation columns

Each row carries `vortex_vi_plus`, `vortex_vi_minus`, `vortex_diff` (D at N),
`vortex_prev_diff` (D at N-1, set when the decision is valid),
`vortex_tr_sum`, `vortex_separation`, `vortex_eval_ts_ms` (the evaluated bar's
timestamp), `vortex_valid`, `vortex_reason`, `vortex_reason_code` and
`signal`, next to the input columns. These values are enough to reproduce every
emitted entry.

### Truth table

| Case | D(N-1) | D(N) | Signal | Reason |
|---|---|---|---|---|
| 1 | <= s | > s | +1 | entry_long |
| 2 | > s | > s | 0 | no_cross |
| 3 | <= 0 | in (0, s] | 0 | separation_not_met |
| 4 | >= -s | < -s | -1 | entry_short |
| 5 | < -s | < -s | 0 | no_cross |
| 6 | >= 0 | in [-s, 0) | 0 | separation_not_met |
| 7 | <= s | = s | 0 | no_cross, or separation_not_met when D(N-1) <= 0 < s |
| 8 | > s | < -s | -1 | entry_short (a one-bar flip) |
| 9 | 0, with s = 0 | > 0 | +1 | entry_long |
| 10 | invalid | any | 0 | the defect's reason |
| 11 | any | TR sum 0 | 0 | zero_true_range |
| 12 | fewer than `period + 2` valid bars | - | 0 | insufficient_history |
| 13 | > s, then <= s, then > s | - | +1 on the second rise | entry_long (a new crossing) |

`shared_strategies/open/test_vortex_trend.py` checks these rows through the
real registry.

## Research settings (not trading settings)

- Seed: `period=14`, `min_separation=0.0` (the plain crossover).
- Optimizer grid (`DEFAULT_PARAM_RANGES`): period 7, 14, 21, 28; separation 0,
  0.02, 0.05, 0.10.
- Every arm (candidate and both comparators) uses direction `both`, the
  existing `time_stop` close owner with `max_bars=20`, the fixed entry-ATR
  stop with multiplier 1.0, $1,000 capital, and the same execution spec. These
  are the `chaikin_money_flow_1649` settings, kept so the two studies compare
  directly. One close owner and one stop owner; the composer blocks new entries
  while a position is open, so there is no reversal or scale-in.
- Comparators: `adx_trend` and `ema_crossover`, unchanged, each tuned on the
  training window over its own existing `DEFAULT_PARAM_RANGES` grid with the
  same selection rule. Their registry defaults are also reported. Both emit
  +1 and -1 crossover events, so direction `both` trades both sides for them
  too.
- The existing M1 incumbent bar (median of the eight incumbents on the same
  frozen data) is reported per arm as a separate reference. It does not enter
  the verdict.
- Proposed later promotion metadata: short name `vt`, direction `both`. This
  issue adds no short name.

## Data and costs

[`study_manifest.json`](study_manifest.json) (schema
`offline_candle_manifest/v1`) pins byte-identical copies of the frozen
`chaikin_money_flow_1649` inputs, with the same sha256 values. The manifest
loader refuses paths outside the manifest's own directory, so the files are
copied. A new acquisition cannot reproduce them, because the venue serves only
the most recent 5,000 candles.

- Venue candles: Hyperliquid mainnet 4h candles for BTC, ETH and SOL, closed
  bars only.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out. 300 warm-up bars before each window feed
  the indicators and are never scored.
- Funding: Hyperliquid hourly funding history, booked as a cost for every arm
  while a position is open. Coverage is measured per window. Incomplete or
  missing funding makes the verdict inconclusive.
- Fees: base-tier perps taker 0.045% and maker 0.015% (pinned documentation
  quote). Every fill in this study is a taker fill.
- Spread and slippage: a per-coin half spread from the pinned L2 snapshot plus
  5 bps of slippage per side, combined once at execution. The stress run
  doubles both.
- Lot size: `szDecimals` from the pinned venue meta. Quantities are floored to
  the lot before any accounting.
- Minimum order: $10 with the live close gate's 3% margin. An entry below it is
  rejected and recorded. A full close always closes the whole position.

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
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/vortex_trend_1647/study_manifest.json
uv run --no-sync python backtest/candidates/vortex_trend_1647/run_study.py
uv run --no-sync python backtest/eval_windows.py --registry futures --candidate-json backtest/candidates/vortex_trend_1647/candidate_seed.json --manifest backtest/candidates/vortex_trend_1647/study_manifest.json --sweep-window train --sweep period=7,14,21,28
uv run --no-sync python backtest/run_backtest.py --mode single --registry futures --strategy vortex_trend --close-strategy '{"name":"time_stop","params":{"max_bars":20}}' --stop-loss-atr-mult 1.0 --direction both --manifest backtest/candidates/vortex_trend_1647/study_manifest.json --manifest-dataset "BTC 4h" --manifest-window test
```

`discovery_before.json` / `discovery_after.json` hold the spot and futures
`--list-json` output before and after this change (identical), and
`research_registry_diff.txt` holds the only research-registry difference (the
new entry).
