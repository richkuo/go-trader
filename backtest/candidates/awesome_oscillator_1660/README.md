# Awesome Oscillator zero cross (#1660)

Research-only candidate `awesome_oscillator` (futures registry,
`backtest_only=True`, hidden from discovery). Every live check script refuses
it as an entry and as a close fallback. Promotion is a separate reviewed
decision. Nothing in this directory writes a live default, a config or a
deployment file.

The held-out result is in [`REPORT.md`](REPORT.md) (rendered from
[`results.json`](results.json)). This file is the frozen specification: the
signal contract, the truth table, the research settings, the selection rule,
the controls and the verdict rule. They were fixed before the held-out window
was scored.

## Signal contract

Inputs: one closed-bar candle frame with `high` and `low`. The evaluator reads
no other data, fetches nothing, and owns only evaluation-local arrays. The
scheduler keeps all trade, stop and position ownership.

- Midpoint per bar: `(high + low) / 2`.
- Oscillator at bar N: the mean midpoint of the `fast_period` bars ending at N
  minus the mean midpoint of the `slow_period` bars ending at N.
- Each mean is the window sum divided by the period, summed left to right over
  that window's own bars. The shared `sma` helper (a pandas rolling mean) is not
  used: its last-bit result for the same bars depends on where the frame
  starts, so an exact-zero or near-zero comparison could differ between the
  full series and a bounded window.
- Sign at N: +1 when the oscillator is above zero, -1 below, 0 when exactly
  zero.
- Predecessor at N: the most recent nonzero oscillator among bars N-1 back to
  N-8 (a fixed 8-bar horizon).
- Long entry at N: sign +1 at N and a predecessor with sign -1. Short entry:
  sign -1 at N and a predecessor with sign +1.
- An exact zero never emits. A run of up to 7 exact zeros between opposite
  values still crosses; a longer run never does (`zero_run_exceeded`).
- A decision at N reads exactly `slow_period + 8` bars and needs every one of
  them valid. A bad bar inside them holds the decision with that bar's reason
  (the invalid-history reset). The full series, every historical prefix and any
  bounded window of at least that length give the same decision.
- Hard history need: at most 108 bars (`slow_period` up to 100); the largest
  optimizer grid point needs 97. The live check window is 200 bars, so no feed
  or argument change is needed.

### Invalid input

Parameters are validated and invalid values raise `ValueError`:
`fast_period` and `slow_period` are integers in [1, 100] with
`fast_period < slow_period` (booleans and fractional values are rejected).

Market-data defects never raise. They produce a hold with a reason code in
`ao_reason` (text) and `ao_reason_code` (number): `missing_columns`,
`nonfinite_input`, `inconsistent_hl` (high below low), `timestamp_order` (a
timestamp not strictly after the previous bar's, when the frame has a datetime
index or a `timestamp` column) and `insufficient_history`. Gaps are never
filled with invented prices.

### Explanation columns

Each row carries `ao`, `ao_fast_mean`, `ao_slow_mean`, `ao_prev_nonzero` (the
predecessor's oscillator value), `ao_prev_lag` (its distance in bars),
`ao_eval_ts_ms` (the evaluated bar's timestamp), `ao_valid`, `ao_reason`,
`ao_reason_code` and `signal`, next to the input columns. These values are
enough to reproduce every emitted entry.

### Truth table

`P` = predecessor sign, `L` = predecessor lag.

| Case | Oscillator at N | P (L <= 8) | Signal | Reason |
|---|---|---|---|---|
| 1 | > 0 | -1 at L = 1 | +1 | entry_long |
| 2 | > 0 | -1 at L in 2..8 (zeros between) | +1 | entry_long |
| 3 | > 0 | +1 | 0 | no_cross |
| 4 | > 0 | none within 8 bars (zero run of 8 or more) | 0 | zero_run_exceeded |
| 5 | = 0 | any | 0 | zero_oscillator |
| 6 | < 0 | +1 at L in 1..8 | -1 | entry_short |
| 7 | < 0 | -1 | 0 | no_cross |
| 8 | any | a bad bar in the last `slow_period + 8` bars | 0 | the defect's reason |
| 9 | fewer than `slow_period + 8` bars | - | 0 | insufficient_history |

`shared_strategies/open/test_awesome_oscillator.py` checks each row through the
real registry.

## Research settings (not trading settings)

- Seed: `fast_period=5`, `slow_period=34`.
- Optimizer grid (`DEFAULT_PARAM_RANGES`): fast 3, 5, 8, 13; slow 21, 34, 55,
  89 (16 combinations).
- Every arm uses direction `both` (the short-side control excepted), the
  existing `time_stop` close owner with `max_bars=20`, the fixed entry-ATR stop
  with multiplier 1.0, $1,000 capital, and the same execution spec. One close
  owner and one stop owner; the composer blocks new entries while a position
  is open, so there is no reversal or scale-in.
- Comparators: `sma_crossover` and `macd`, unchanged, each tuned on the
  training window over its own existing `DEFAULT_PARAM_RANGES` grid with the
  same selection rule. Both are deprecated-edge names hidden from discovery
  and still loadable through the full futures registry, so a win over them
  alone is not evidence of an edge. Their registry defaults are also reported.
  With explicit closes and direction `both`, their negative crossover event is
  a short entry, so every arm is mirrored.
- Primary comparison: the existing M1 incumbent bar (median of the eight
  incumbents on the same frozen data and window). Incumbents run without a
  close strategy, so they keep the flat fee model at the manifest taker fee
  with spread plus slippage and no lot floor or venue minimum. Incumbent and
  explicit-close arms therefore do not share identical venue execution.

## Controls (reported, outside the verdict)

One same-period comparison cannot separate the price source, the short side
and the zero-tie rule, so each has its own control on the held-out window at
cost x1:

1. Midpoint source: `sma_crossover` on close at the candidate's selected
   periods, direction `both`, same close, stop and execution.
2. Short-side contribution: the candidate at its selected parameters with
   direction `long`.
3. Zero-tie rule: per dataset, the count of exact-zero oscillator bars, of
   `zero_run_exceeded` bars and of entries whose predecessor lag is above 1.
   When all are zero, the zero-tie rule had no effect on this data.

## Data and costs

[`study_manifest.json`](study_manifest.json) (schema
`offline_candle_manifest/v1`, read by `backtest/offline_manifest.py`) pins the
same frozen inputs as #1649. The files in `data/` are byte-identical copies of
`backtest/candidates/chaikin_money_flow_1649/data/` with the same sha256
values; they are copied because the manifest confines its paths to its own
directory.

- Venue candles: Hyperliquid mainnet 4h candles for BTC, ETH and SOL from the
  public `candleSnapshot` endpoint, closed bars only. The endpoint serves only
  the most recent 5,000 candles, so a re-acquisition cannot reproduce the early
  bars.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out. Both have finite ends. 300 warm-up bars
  before each window feed the indicators and are never scored.
- Funding: Hyperliquid hourly funding history, booked as a cost for every arm
  while a position is open. Coverage is measured per window; incomplete or
  missing funding makes the verdict inconclusive.
- Fees: base-tier perps taker 0.045% and maker 0.015% (pinned documentation
  quote with its sha256). Every fill in this study is a taker fill.
- Spread and slippage: a per-coin half spread from the pinned L2 snapshot (one
  point in time, not a historical average) plus 5 bps of slippage per side,
  stored separately and combined once at execution. The stress run doubles
  both.
- Lot size: `szDecimals` from the pinned venue meta snapshot, floored before
  any accounting.
- Minimum order: $10 with the live close gate's 3% margin. An entry below it is
  rejected and recorded; a partial close below it is skipped and recorded; a
  full close always closes the whole position.

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
   cost x2; the candidate's M1 verdict against the incumbent bar is `pass` at
   cost x1 (mean Sharpe and mean drawdown-adjusted return both above the bar's,
   not degenerate); and the candidate's mean Sharpe and mean drawdown-adjusted
   return are both above each comparator's.
4. Otherwise fail.

A pass would not change availability. The candidate stays `backtest_only`
until a separate reviewed promotion. The name is already in the bidirectional
set (`bidirectionalPerpsStrategies` in `scheduler/init.go`) and in
`LIVE_BIDIRECTIONAL_STRATEGIES` in `backtest/fee_audit.py`. The fee audit
screens every futures registry entry, so it must measure the short side of a
strategy that opens shorts or mark that side unmeasured. This membership does
not make the strategy live: it is not in `knownShortNames`, it stays hidden
from discovery, and every live check script still refuses it. Proposed for
that promotion: short name `ao` and direction `both`, added to
`knownShortNames`.

## Reproduce

Offline (no network; verifies hashes, then reruns everything):

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/awesome_oscillator_1660/study_manifest.json
uv run --no-sync python backtest/candidates/awesome_oscillator_1660/run_study.py
uv run --no-sync python backtest/eval_windows.py --registry futures --candidate-json backtest/candidates/awesome_oscillator_1660/candidate_seed.json --manifest backtest/candidates/awesome_oscillator_1660/study_manifest.json --sweep-window train --sweep fast_period=3,5,8,13
uv run --no-sync python backtest/run_backtest.py --mode single --registry futures --strategy awesome_oscillator --close-strategy '{"name":"time_stop","params":{"max_bars":20}}' --stop-loss-atr-mult 1.0 --direction both --manifest backtest/candidates/awesome_oscillator_1660/study_manifest.json --manifest-dataset "BTC 4h" --manifest-window test --comparison-mode approximate
```

Parity: `backtest/tests/test_awesome_oscillator_backtest.py` runs the
non-batched parity frame function on the frozen BTC frame at `window=200` and
with expanding windows (every historical prefix), and proves that batched
parity refuses the name. The parity command line reads only the local
Binance US candle cache and has no manifest input, so command-line runs are
supplementary evidence on proxy candles:

```
uv run --no-sync python backtest/parity_diff.py --strategy awesome_oscillator --registry futures --platform hyperliquid --symbol BTC/USDT --timeframe 4h --since 2026-01-01 --close 'time_stop:{"max_bars":20}' --window 200 --comparison-mode approximate
uv run --no-sync python backtest/parity_diff.py --strategy awesome_oscillator --registry futures --platform hyperliquid --symbol BTC/USDT --timeframe 4h --since 2026-01-01 --close 'time_stop:{"max_bars":20}' --window 0 --comparison-mode approximate
```

`discovery_before.json` / `discovery_after.json` hold the spot and futures
`--list-json` output before and after this change (identical), and
`research_registry_diff.txt` holds the only research-registry difference (the
new entry).
