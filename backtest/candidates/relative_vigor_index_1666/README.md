# Relative Vigor Index crossover (#1666)

Research-only candidate `relative_vigor_index` (futures registry,
`backtest_only=True`, hidden from discovery). Every live check script refuses
it as an entry and as a close fallback. Promotion is a separate reviewed
decision. Nothing in this directory writes a live default, a config or a
deployment file.

The held-out result is in [`REPORT.md`](REPORT.md) (rendered from
[`results.json`](results.json)). This file is the frozen specification: the
signal contract, the truth table, the research settings, the selection rule and
the verdict rule. They were fixed before the held-out window was scored.

## Signal contract

Inputs: one closed-bar candle frame with `open`, `high`, `low`, `close` and
timestamps (a datetime index or a `timestamp` column). The evaluator reads no
other data, fetches nothing, and owns only evaluation-local arrays. The
scheduler keeps all trade, stop and position ownership.

- Smoothed numerator at bar t: `(x[t] + 2 x[t-1] + 2 x[t-2] + x[t-3]) / 6` with
  `x = close - open`. Smoothed denominator: the same weights on
  `high - low`.
- Index (RVI) at t: the mean of the smoothed numerator over the `period` bars
  ending at t, divided by the mean of the smoothed denominator over the same
  bars. A denominator mean of zero is invalid (`zero_range_window`), never an
  index of zero.
- Signal line at t: the same 1-2-2-1 weights applied to the index at t..t-3.
- Difference at t: index minus signal line.
- Long entry at t: the difference at t-1 is less than or equal to zero, the
  difference at t is strictly above zero, and (with `zero_line_filter` on) the
  index at t is strictly above zero.
- Short entry at t: the mirror (previous difference at or above zero,
  current difference strictly below zero, index strictly below zero).
- An already-crossed value (both differences on the same side) never emits.
  An exact zero difference at t never emits.
- Candle support: one index value needs `period + 3` candles, one signal-line
  value needs `period + 6`, and a decision (index and signal line at t and
  t-1) needs `period + 7`. With `period` at most 100 this is at most 107
  candles. The live check window is 200 candles, so no feed, argument or probe
  change is needed.
- Every term is a finite trailing window with no recursive state, and every
  window sum is computed from its own bars. A decision depends only on the
  candles inside its window, so the full series, every historical prefix and a
  bounded tail give the same decision for every bar with enough history.

### Invalid input and cadence

Parameters are validated and invalid values raise `ValueError`: `period` is an
integer in [2, 100] (booleans and fractional values are rejected; an
integral float such as `10.0` is accepted); `zero_line_filter` is a boolean.

Market-data defects never raise. They produce a hold with a reason in
`rvi_reason` (text) and `rvi_reason_code` (number):

- `missing_columns:<names>` and `missing_timestamps` hold the whole frame.
- `nonfinite_input`: a non-finite open, high, low or close.
- `inconsistent_ohlc`: high below low, or open or close outside the range.
- `timestamp_order`: a missing timestamp, or one not strictly after the
  previous candle's (duplicate or reversed).
- `cadence_gap`: the timestamp steps between consecutive candles inside the
  decision window are not all equal. This rule reads only the window's own
  candles, so a gap invalidates exactly the windows that contain it, and
  appending candles never reclassifies an earlier window. The frame-wide
  smallest step is never used. A fixed interval parameter is not used either:
  the live slot evaluator passes only the frame and strategy parameters, and a
  second copy of the timeframe could disagree with the configured feed.
- `zero_range_window`: a denominator mean of zero inside the decision window.
- `insufficient_history`: fewer than `period + 7` candles.

A bad candle invalidates every decision whose window includes it. A zero-range
candle (`high == low`) is valid inside a window with a positive aggregate
range. Gaps are never filled with invented prices.

### Explanation columns

Each row carries `rvi`, `rvi_signal`, `rvi_diff`, `rvi_prev_diff`,
`rvi_num_mean`, `rvi_den_mean`, `rvi_zero_line_filter`, `rvi_eval_ts_ms` (the
evaluated bar's timestamp), `rvi_valid`, `rvi_reason`, `rvi_reason_code` and
`signal`, next to the input columns. These values are enough to reproduce every
emitted entry.

### Numeric tolerance

The integration fixture recomputes the index and signal line in exact rational
arithmetic. Indicator values agree within an absolute 1e-12. Decisions agree
exactly on every bar where no compared quantity (the two differences and the
index) lies within 1e-12 of zero without being exactly zero.

### Truth table

`D(t)` = index minus signal line at t; `R(t)` = index at t; filter on unless
stated.

| Case | D(t-1) | D(t) | R(t) | Signal | Reason |
|---|---|---|---|---|---|
| 1 | < 0 | > 0 | > 0 | +1 | entry_long |
| 2 | = 0 | > 0 | > 0 | +1 | entry_long |
| 3 | < 0 | > 0 | <= 0 | 0 | zero_line_filter |
| 4 | < 0 | > 0 | <= 0 (filter off) | +1 | entry_long |
| 5 | > 0 | > 0 | any | 0 | no_cross |
| 6 | any | = 0 | any | 0 | no_cross |
| 7 | > 0 | < 0 | < 0 | -1 | entry_short |
| 8 | = 0 | < 0 | < 0 | -1 | entry_short |
| 9 | > 0 | < 0 | >= 0 | 0 | zero_line_filter |
| 10 | < 0 | < 0 | any | 0 | no_cross |
| 11 | window has a bad candle | - | - | 0 | the defect's reason |
| 12 | window has unequal steps | - | - | 0 | cadence_gap |
| 13 | denominator mean zero in window | - | - | 0 | zero_range_window |
| 14 | fewer than `period + 7` candles | - | - | 0 | insufficient_history |

`shared_strategies/open/test_relative_vigor_index.py` checks these cases
through the real registry and composer.

## Promotion facts (recorded, not applied)

- Proposed scheduler short name: `rvi` (the derived name; no collision in the
  short-name map). Direction: `both`.
- Required lookback: `period + 7` candles (at most 107).
- The name is in the scheduler bidirectional set and the fee-audit
  bidirectional set (the fee-audit test requires the two sets to match). The
  strategy opens shorts, so the fee audit marks its short side as unmeasured.
  It does not treat the strategy as long-only.
- This issue adds no short-name or default-list entry. The Discord add command
  and the status-page add action refuse the name. Discovery hides it, and every
  live check script refuses it.

## Research settings (not trading settings)

- Seed: `period=10`, `zero_line_filter=true`.
- Optimizer grid (`DEFAULT_PARAM_RANGES`): `period` 4, 6, 8, 10, 14, 20, 30;
  `zero_line_filter` held on.
- Every arm (candidate, ablation and the three comparators) uses direction
  `both`, the existing `time_stop` close owner with `max_bars=20`, the fixed
  entry-ATR stop with multiplier 1.0, $1,000 capital, and the same execution
  spec. One close owner and one stop owner; the composer blocks new entries
  while a position is open, so there is no reversal or scale-in.
- Comparators: `momentum_pro`, `sma_crossover` and `ema_crossover`, unchanged,
  each tuned on the training window over its own existing
  `DEFAULT_PARAM_RANGES` grid with the same selection rule. Their registry
  defaults are also reported.
- Ablation: the candidate's selected parameters with `zero_line_filter=false`,
  every other setting equal, scored on both windows. It does not enter the
  verdict.
- The existing M1 incumbent bar (median of the eight incumbents on the same
  frozen data) is reported per arm as a separate reference. Incumbents receive
  funding but keep the legacy flat execution model, so the reference does not
  enter the verdict.

## Data and costs

[`study_manifest.json`](study_manifest.json) (schema
`offline_candle_manifest/v1`, read by `backtest/offline_manifest.py`) pins the
same frozen inputs as the #1649 study. Every file under `data/` is a
byte-identical copy of `backtest/candidates/chaikin_money_flow_1649/data/` with
the same sha256, because manifest paths cannot leave the manifest directory and
the endpoint serves only the most recent 5,000 candles.

- Venue candles: Hyperliquid mainnet 4h candles for BTC, ETH and SOL, closed
  bars only.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out. 300 warm-up bars before each window feed
  the indicators and are never scored. The held-out window was also the #1649
  held-out window; no #1666 parameter was chosen from it.
- Funding: Hyperliquid hourly funding history, booked as a cost for every arm
  while a position is open. Incomplete or missing funding is reported and makes
  the verdict inconclusive. An empty file is never treated as verified zero.
- Fees: base-tier perps taker 0.045% and maker 0.015% (pinned documentation
  quote). Every fill in this study is a taker fill.
- Spread and slippage: a per-coin half spread from a pinned L2 snapshot (one
  point in time, not a historical average) plus 5 bps of slippage per side,
  stored separately and combined once at execution. The stress run doubles
  both.
- Lot size: `szDecimals` from the pinned venue meta, floored before any
  accounting. Minimum order: $10 with the live close gate's 3% margin.

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
   across datasets (a partially closed position counts once).
3. Pass only when all hold: candidate mean net return > 0 at cost x1 and at
   cost x2, and the candidate's mean Sharpe and mean drawdown-adjusted return
   are both above each comparator's.
4. Otherwise fail.

A pass would not change availability. The candidate stays `backtest_only`
until a separate reviewed promotion.

## Frozen-input parity

`run_study.py` calls `parity_diff.compute_parity_frame` (non-batched, futures
registry, the study close owner, direction `both`) on each dataset's
hash-verified held-out frame (warm-up included) with the selected parameters,
at `window=None` (every historical prefix) and `window=200` (the live candle
count), and records the compared bars and mismatches. The parity command line
reads cached exchange data and has no manifest input, so the driver calls the
function directly.

## Reproduce

Offline (no network; verifies hashes, then reruns everything):

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/relative_vigor_index_1666/study_manifest.json
uv run --no-sync python backtest/candidates/relative_vigor_index_1666/run_study.py
uv run --no-sync python backtest/eval_windows.py --registry futures --candidate-json backtest/candidates/relative_vigor_index_1666/candidate_seed.json --manifest backtest/candidates/relative_vigor_index_1666/study_manifest.json --sweep-window train --sweep period=4,6,8,10,14,20,30
uv run --no-sync python backtest/run_backtest.py --mode single --registry futures --strategy relative_vigor_index --close-strategy '{"name":"time_stop","params":{"max_bars":20}}' --stop-loss-atr-mult 1.0 --direction both --manifest backtest/candidates/relative_vigor_index_1666/study_manifest.json --manifest-dataset "BTC 4h" --manifest-window test
```

`run_study.py --render-only` re-renders `REPORT.md` from the committed
`results.json`.

`discovery_before.json` / `discovery_after.json` hold the spot and futures
`--list-json` output before and after this change (identical), and
`research_registry_diff.txt` holds the only research-registry difference (the
new entry).
