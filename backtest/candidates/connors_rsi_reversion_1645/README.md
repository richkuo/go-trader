# Connors RSI pullback (#1645)

Research-only candidate `connors_rsi_reversion` (futures registry,
`backtest_only=True`, hidden from discovery). Every live check script refuses
it as an entry and as a close fallback. Promotion is a separate reviewed
decision. Nothing in this directory writes a live default, a config or a
deployment file.

The held-out result is in [`REPORT.md`](REPORT.md) (rendered from
[`results.json`](results.json)). This file is the frozen specification: the
signal contract, the bounded parity contract, the research settings, the
selection rule and the verdict rule. They were fixed before the held-out
window was scored.

## Signal contract

Input: one closed-bar candle frame with `close` (and its timestamps). The
evaluator reads no other data, fetches nothing, and owns only
evaluation-local arrays. The scheduler keeps all trade, stop and position
ownership.

- Price component at bar N: bounded Wilder relative strength of close changes
  over `price_period`.
- Streak component at bar N: bounded Wilder relative strength of the change in
  the signed streak over `streak_period`. The streak at bar j counts
  consecutive closes in the same direction ending at j (positive up, negative
  down), capped at 20. An unchanged close resets it to 0.
- Rank component at bar N: percentile rank of the latest one-bar return
  `close[N] / close[N-1] - 1` among the previous `rank_window` returns, with
  the mid-rank tie rule: `100 x (count below + 0.5 x count equal) / rank_window`.
- Composite `crsi` at bar N: the mean of the three components.
- Long entry at N: `crsi[N-1] < oversold` and `crsi[N] >= oversold`.
- Short entry at N: `crsi[N-1] > overbought` and `crsi[N] <= overbought`.
- Both bars must be valid. Equality on the current bar enters; equality on the
  previous bar does not.

### Bounded parity contract

Every decision depends only on a fixed trailing span ending at the evaluated
bar, so the full series, every historical prefix and the 200-bar check window
give the same decision for every bar that has enough history.

- Wilder initialization: each relative-strength component reads exactly the
  last 50 changes ending at bar N. The average gain and the average loss are
  seeded with the simple mean of the first `period` changes of that span, then
  smoothed with the Wilder recursion (weight `1/period`) over the other
  changes. This equals one fixed 50-weight sum, so no bar before the span is
  read. The shared `wilder_rsi` helper is not used, because it recurses from
  the first bar of the frame it receives.
- Relative strength is `100 x G / (G + L)`: 100 when L is 0 and G is
  positive, 0 when G is 0 and L is positive, 50 when both are 0.
- Streak truncation: the streak cap is 20 bars.
- History need: `max(50 + 20 + 1, rank_window + 2) + 1` consecutive valid bars
  (the last bar is the previous-bar composite). With the bounds below it is at
  most 153 bars, inside the 200-bar live check window, so no feed or argument
  change is needed.

### Invalid input

Parameters are validated and invalid values raise `ValueError`:
`price_period` and `streak_period` are integers in [2, 10]; `rank_window` is
an integer in [20, 150] (booleans and fractional values are rejected);
`oversold` and `overbought` are finite, in (0, 100), and
`oversold < overbought`.

Market-data defects never raise. They produce a hold with a reason code in
`crsi_reason` (text) and `crsi_reason_code` (number): `missing_columns`,
`nonfinite_input`, `nonpositive_close`, `timestamp_order` (a timestamp not
strictly after the previous bar's, when the frame has a datetime index or a
`timestamp` column) and `insufficient_history`. A bad bar invalidates every
decision whose span includes it. Gaps are never filled with invented prices.

### Explanation columns

Each row carries `crsi_price_rsi`, `crsi_streak`, `crsi_streak_rsi`,
`crsi_rank`, `crsi`, `crsi_prev`, `crsi_oversold`, `crsi_overbought`,
`crsi_eval_ts_ms` (the evaluated bar's timestamp), `crsi_valid`,
`crsi_reason`, `crsi_reason_code` and `signal`, next to the input columns.
`shared_strategies/open/test_connors_rsi.py` recomputes every value and entry
from raw closes with an independent per-bar implementation.

### Truth table

`P` = `crsi[N-1]`, `C` = `crsi[N]`, `O` = oversold, `B` = overbought.

| Case | Condition | Signal | Reason |
|---|---|---|---|
| 1 | P < O and C >= O | +1 | entry_long |
| 2 | P = O and C > O | 0 | no_cross |
| 3 | C < O | 0 | below_oversold |
| 4 | P > B and C <= B | -1 | entry_short |
| 5 | P = B and C < B | 0 | no_cross |
| 6 | C > B | 0 | above_overbought |
| 7 | O <= C <= B, no cross | 0 | no_cross |
| 8 | a defect inside the span | 0 | the defect's reason |
| 9 | fewer than the history need | 0 | insufficient_history |

## Research settings (not trading settings)

- Seed: `price_period=3`, `streak_period=2`, `rank_window=100`,
  `oversold=10`, `overbought=90`.
- Optimizer grid (`DEFAULT_PARAM_RANGES`): `price_period` 2, 3, 5;
  `streak_period` 2, 3; `rank_window` 50, 100, 150; `oversold` 5, 10, 20;
  `overbought` 80, 90, 95 (162 combinations).
- Every arm (candidate and the three comparators) uses direction `both`, the
  existing `time_stop` close owner with `max_bars=20`, the fixed entry-ATR
  stop with multiplier 1.0, $1,000 capital, and the same execution spec. This
  is the #1649 protocol unchanged, so no exit setting was chosen for this
  candidate. One close owner and one stop owner; the composer blocks new
  entries while a position is open, so there is no reversal or scale-in.
- Comparators: `rsi`, `stoch_rsi` and `mean_reversion_pro`, unchanged, each
  tuned on the training window over its own existing `DEFAULT_PARAM_RANGES`
  grid with the same selection rule. Their registry defaults are also
  reported.
- The existing M1 incumbent bar (median of the eight incumbents on the same
  frozen data) is reported per arm as a separate reference. It does not enter
  the verdict.
- Promotion record (not applied): short name `crsi`, direction `both`. The
  name is in the Go bidirectional set and the M5 live bidirectional set, and
  it is not in `knownShortNames`, so no operator command can add it.

## Data and costs

[`study_manifest.json`](study_manifest.json) (schema
`offline_candle_manifest/v1`, read by `backtest/offline_manifest.py`) pins the
same frozen inputs as the #1649 study, byte for byte (the same sha256 and the
same git blobs). The manifest loader refuses paths outside the manifest
directory, so the study carries its own copies in `data/`.

- Venue candles: Hyperliquid mainnet 4h candles for BTC, ETH and SOL from the
  public `candleSnapshot` endpoint, closed bars only. The endpoint serves only
  the most recent 5,000 candles, so the files are committed.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out. Both have finite ends. 300 warm-up bars
  before each window feed the indicators and are never scored.
- Funding: Hyperliquid hourly funding history, booked as a cost for every arm
  while a position is open. Coverage is measured per window; incomplete or
  missing funding makes the verdict inconclusive.
- Fees: base-tier perps taker 0.045% and maker 0.015% (pinned documentation
  quote). Every fill in this study is a taker fill.
- Spread and slippage: a per-coin half spread from a pinned L2 snapshot (one
  point in time) plus 5 bps of slippage per side, combined once at execution.
  The stress run doubles both.
- Lot size: `szDecimals` from the pinned venue meta snapshot. Quantities are
  floored to the lot before any accounting.
- Minimum order: $10 with the live close gate's 3% margin. An entry below it
  is rejected and recorded. A partial close below it is skipped and recorded.
  A full close always closes the whole position.

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

## Reproduce

Offline (no network; verifies hashes, then reruns everything):

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/connors_rsi_reversion_1645/study_manifest.json
uv run --no-sync python backtest/candidates/connors_rsi_reversion_1645/run_study.py
uv run --no-sync python backtest/eval_windows.py --registry futures --candidate-json backtest/candidates/connors_rsi_reversion_1645/candidate_seed.json --manifest backtest/candidates/connors_rsi_reversion_1645/study_manifest.json --sweep-window train --sweep rank_window=50,100,150
uv run --no-sync python backtest/run_backtest.py --mode single --registry futures --strategy connors_rsi_reversion --close-strategy '{"name":"time_stop","params":{"max_bars":20}}' --stop-loss-atr-mult 1.0 --direction both --manifest backtest/candidates/connors_rsi_reversion_1645/study_manifest.json --manifest-dataset "BTC 4h" --manifest-window test
```

`discovery_before.json` / `discovery_after.json` hold the spot and futures
`--list-json` output before and after this change (identical), and
`research_registry_diff.txt` holds the only research-registry difference (the
new entry).
