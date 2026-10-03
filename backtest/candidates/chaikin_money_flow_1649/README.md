# Chaikin Money Flow breakout confirmation (#1649)

Research-only candidate `chaikin_money_flow_breakout` (futures registry,
`backtest_only=True`, hidden from discovery). Every live check script refuses
it as an entry and as a close fallback. Promotion is a separate reviewed
decision. Nothing in this directory writes a live default, a config or a
deployment file.

The held-out result is in [`REPORT.md`](REPORT.md) (rendered from
[`results.json`](results.json)). This file is the frozen specification: the
signal contract, the truth table, the research settings, the selection rule and
the verdict rule. They were fixed before the held-out window was scored.

## Signal contract

Inputs: one closed-bar candle frame with `high`, `low`, `close`, `volume`
(base-asset units). The evaluator reads no other data, fetches nothing, and
owns only evaluation-local arrays. The scheduler keeps all trade, stop and
position ownership.

- Money-flow multiplier per bar: `(2 x close - high - low) / (high - low)`.
  A valid zero-range bar (`high == low`) contributes zero signed flow.
- Signed flow per bar: multiplier x volume.
- Money flow at bar N: sum of signed flow over the `flow_window` bars ending at
  N, divided by the sum of volume over the same bars. A window whose volume sum
  is zero is invalid (`zero_volume_window`), never zero flow.
- Prior upper boundary at bar N: highest high of the `breakout_window` bars
  before N (bar N excluded). Prior lower boundary: lowest low of the same bars.
- Price-breakout state at N: up when `close > upper`, down when
  `close < lower`, else none. Equality is no breakout.
- Long entry at N: state is up at N, the state at N-1 is valid and not up (a
  new episode), and money flow at N is strictly above `flow_threshold`.
- Short entry at N: the mirror, with money flow strictly below
  `-flow_threshold`.
- A flow change later in the same uninterrupted breakout episode never creates
  a fresh entry. An invalid state at N-1 never manufactures an episode start.
- Every window sum, maximum and minimum is computed from that window's own
  bars, so a decision depends only on the bars inside its window. The full
  series, every historical prefix and a bounded tail give the same decision for
  every bar that has enough history.
- Hard history need: `max(flow_window, breakout_window + 2)` consecutive valid
  bars, at most 102 bars with the bounds below. The live check window is 200
  bars, so no feed or argument change is needed.

This indicator estimates buying and selling pressure from candles. It is not
measured aggressor flow.

### Invalid input

Parameters are validated and invalid values raise `ValueError`:
`flow_window` and `breakout_window` are integers in [2, 100] (booleans and
fractional values are rejected); `flow_threshold` is finite and in [0, 1).

Market-data defects never raise. They produce a hold with a reason code in
`cmf_reason` (text) and `cmf_reason_code` (number): `missing_columns`,
`nonfinite_input`, `negative_volume`, `inconsistent_hlc` (high below low, or
close outside the range), `timestamp_order` (a timestamp not strictly after the
previous bar's, when the frame has a datetime index or a `timestamp` column),
`insufficient_history` and `zero_volume_window`. A bad bar invalidates every
decision whose windows include it. Gaps are never filled with invented prices
or volume.

### Explanation columns

Each row carries `cmf`, `cmf_upper`, `cmf_lower`, `cmf_breakout_state`,
`cmf_threshold`, `cmf_eval_ts_ms` (the evaluated bar's timestamp),
`cmf_valid`, `cmf_reason`, `cmf_reason_code` and `signal`, next to the input
columns. These values are enough to reproduce every emitted entry.

### Truth table

`U` = prior upper boundary, `L` = prior lower boundary, `T` = threshold,
`F` = money flow at N, `S(N-1)` = state at the previous bar.

| Case | Bar N | S(N-1) | F | Signal | Reason |
|---|---|---|---|---|---|
| 1 | close > U | none | F > T | +1 | entry_long |
| 2 | close > U | none | F = T | 0 | flow_below_threshold |
| 3 | close > U | none | F < T | 0 | flow_below_threshold |
| 4 | close = U | none | any | 0 | no_breakout |
| 5 | close > U | up | F > T | 0 | breakout_continuation |
| 6 | close > U (flow crossed T after the episode began) | up | F > T | 0 | breakout_continuation |
| 7 | close < L | none | F < -T | -1 | entry_short |
| 8 | close < L | none | F = -T | 0 | flow_below_threshold |
| 9 | close = L | none | any | 0 | no_breakout |
| 10 | close < L | down | F < -T | 0 | breakout_continuation |
| 11 | close > U | invalid | F > T | 0 | the defect's reason |
| 12 | any | any | volume sum 0 | 0 | zero_volume_window |
| 13 | fewer than `max(F_w, B_w + 2)` valid bars | - | - | 0 | insufficient_history |
| 14 | close > U, then flat, then close > U again | none | F > T | +1 | entry_long (a new episode) |

`shared_strategies/open/test_chaikin_money_flow.py` checks each row through the
real registry.

## Research settings (not trading settings)

- Seed: `flow_window=20`, `breakout_window=20`, `flow_threshold=0.05`.
- Optimizer grid (`DEFAULT_PARAM_RANGES`): windows 10, 20, 40, 80; threshold
  0, 0.05, 0.10, 0.20.
- Every arm (candidate and both comparators) uses direction `both`, the
  existing `time_stop` close owner with `max_bars=20`, the fixed entry-ATR
  stop with multiplier 1.0, $1,000 capital, and the same execution spec. One
  close owner and one stop owner; the composer blocks new entries while a
  position is open, so there is no reversal or scale-in.
- Comparators: `volume_weighted` and `breakout`, unchanged, each tuned on the
  training window over its own existing `DEFAULT_PARAM_RANGES` grid with the
  same selection rule. Their registry defaults are also reported.
- The existing M1 incumbent bar (median of the eight incumbents on the same
  frozen data) is reported per arm as a separate reference. It does not enter
  the verdict.

## Data and costs

[`study_manifest.json`](study_manifest.json) (schema
`offline_candle_manifest/v1`, read by `backtest/offline_manifest.py`) pins:

- Venue candles: Hyperliquid mainnet 4h candles for BTC, ETH and SOL from the
  public `candleSnapshot` endpoint, closed bars only, gzip CSV with sha256.
  The endpoint serves only the most recent 5,000 candles, so the files are
  committed; a later re-acquisition cannot reproduce the early bars. 1h data
  covers about seven months under the same cap and is not used.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out. Both have finite ends. 300 warm-up bars
  before each window feed the indicators and are never scored; a window with
  fewer warm-up bars fails closed.
- Funding: Hyperliquid hourly funding history, acquired through
  `shared_tools/funding_fetcher.py`, booked as a cost for every arm while a
  position is open. Coverage is measured per window; incomplete or missing
  funding is reported and makes the verdict inconclusive. An empty file is
  never treated as verified zero funding.
- Fees: base-tier perps taker 0.045% and maker 0.015% (pinned documentation
  quote with its sha256). Every fill in this study is a taker fill; the maker
  rate applies only to resting take-profit tiers, which this study does not
  use.
- Spread and slippage: a per-coin half spread from a pinned L2 snapshot
  (`data/venue_l2_snapshot.json`, one point in time, not a historical average)
  plus 5 bps of slippage per side. They are stored separately and combined
  once at execution. The stress run doubles both.
- Lot size: `szDecimals` from the pinned venue meta snapshot
  (`data/venue_meta.json`), checked against each dataset at load. Quantities
  are floored to the lot before any accounting.
- Minimum order: $10 (pinned documentation quote) with the live close gate's
  3% margin. An entry below it is rejected and recorded. A partial close below
  it is skipped and recorded, which matches the live close gate. A full close
  (stop, time stop, end of data) always closes the whole position.

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
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/chaikin_money_flow_1649/study_manifest.json
uv run --no-sync python backtest/candidates/chaikin_money_flow_1649/run_study.py
uv run --no-sync python backtest/eval_windows.py --registry futures --candidate-json backtest/candidates/chaikin_money_flow_1649/candidate_seed.json --manifest backtest/candidates/chaikin_money_flow_1649/study_manifest.json --sweep-window train --sweep flow_window=10,20,40,80
uv run --no-sync python backtest/run_backtest.py --mode single --registry futures --strategy chaikin_money_flow_breakout --close-strategy '{"name":"time_stop","params":{"max_bars":20}}' --stop-loss-atr-mult 1.0 --direction both --manifest backtest/candidates/chaikin_money_flow_1649/study_manifest.json --manifest-dataset "BTC 4h" --manifest-window test
```

Re-acquire (network, public endpoints only, refuses to run with
`HYPERLIQUID_SECRET_KEY` set; replaces the committed files and hashes):

```
uv run --no-sync python backtest/offline_manifest.py acquire --manifest backtest/candidates/chaikin_money_flow_1649/study_manifest.json --write-hashes
```

`discovery_before.json` / `discovery_after.json` hold the spot and futures
`--list-json` output before and after this change (identical), and
`research_registry_diff.txt` holds the only research-registry difference (the
new entry).
