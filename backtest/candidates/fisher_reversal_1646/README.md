# Fisher Transform reversals (#1646)

Candidate `fisher_reversal` (futures registry only, `short_entries=True`,
`edge_status="no_edge"`, hidden from discovery). It follows the current no-edge
availability rule: exactly one explicit `--mode=paper` runs it without an
acknowledgement; `--mode=live` or a missing `--mode` needs `"allow_no_edge": true`.
No study outcome in this directory changes that rule, grants an acknowledgement,
promotes the strategy or writes a live default, a config or a deployment file.

This file and [`candidate_seed.json`](candidate_seed.json) are the frozen
specification. The formula, parameter domains, optimizer grid, close and stop
owners, selection rule and verdict gates were fixed before any held-out result
was read. Protocol hash: `candidate_seed.json` sha256
`d212f51880bf3b969fbf89346c04b567e8a697964a52bc6180fc3486ffcb6375`.
`run_study.py` reads every protocol value from that file and records its hash in
`results.json`. The held-out result is in [`REPORT.md`](REPORT.md).

## Registration record

- Short name `fisher` (unique in the scheduler short-name map), direction `both`.
  The Discord and status-page add-strategy actions create an explicit paper
  entry `hl-fisher-<asset>` with `--mode=paper`, `direction: both` and no
  acknowledgement.
- Go mirrors: `knownShortNames`, `registeredOpenStrategyPlatforms` (`futures`)
  and `bidirectionalPerpsStrategies` in `scheduler/init.go`; `noEdgeStrategies`
  in `scheduler/edge_status.go` (same `edge_source` and `edge_ref` as Python).
- Evidence metadata: registered first with `edge_source="unvalidated"`; after
  the held-out run both owners record the study outcome (`study_fail` or
  `study_inconclusive`; a pass keeps `unvalidated`) with
  [`REPORT.md`](REPORT.md) as `edge_ref`.
- Not in any starter or default strategy list. The hidden discovery set is
  derived from the no-edge metadata.

## Signal contract

Inputs: one closed-bar frame with `open`, `high`, `low`, `close`, `volume` and
timestamps (a datetime index or a `timestamp` column). The evaluator fetches
nothing, writes no book, keeps no state between calls and returns the supplied
rows, columns and index unchanged with added columns.

For a timestamp `t` and lookback `L`:

1. Midpoint `m(t) = (high(t) + low(t)) / 2`.
2. Range: `lo(t)` and `hi(t)` are the minimum and maximum midpoint over the
   inclusive bars `t-L+1 .. t`. `hi(t) == lo(t)` is a `flat_range` hold.
3. Normalized `n(t) = 2 * (m(t) - lo(t)) / (hi(t) - lo(t)) - 1`, in [-1, 1].
4. Fisher at `t`: start `x = 0` and `F = 0`, then process exactly the 64
   normalized observations `n(t-63) .. n(t)` in timestamp order. For each one:
   `x = 0.33 * n + 0.67 * x`; clamp `x` to `[-1 + 0.000001, 1 - 0.000001]`;
   `F = 0.5 * ln((1 + x) / (1 - x)) + 0.5 * F`. `F(t)` is the final `F`.
   Every timestamp has its own fixed initialization, so a value never depends
   on where the supplied frame starts.
5. Trigger at `t` is `F(t-1)`, computed independently in the same way.
6. Long entry at `t`: `F(t) > F(t-1)` and `F(t-1) <= F(t-2)` and
   `F(t-1) < -extreme`.
7. Short entry at `t`: `F(t) < F(t-1)` and `F(t-1) >= F(t-2)` and
   `F(t-1) > extreme`.

Equality qualifies only on the previous crossing comparison (`<=` or `>=`
between `F(t-1)` and `F(t-2)`). Equality at `t` or at the extreme threshold is
not an entry. Because `extreme > 0`, one bar can never be both a long and a
short entry.

### History bound

`F(t)` reads `n(t-63) .. n(t)`; each `n(k)` reads midpoints `k-L+1 .. k`; so
`F(t)` reads the `L + 63` rows `t-L-62 .. t`. A decision reads `F(t)`,
`F(t-1)` and `F(t-2)`, so it reads the `L + 65` rows `t-L-64 .. t`. At the
largest permitted lookback (32) that is 97 rows, inside the 200-row live check
frame. No feed key, check lookback, probe flag or private fetch changes.

### Parameters

Invalid parameters raise `ValueError` before evaluation (a configuration
error, never a hold). `lookback` is an integer in [2, 32]; booleans, fractional
and non-finite values are refused, an integral float such as `9.0` is accepted.
`extreme` is a finite number (not a boolean) in [0.5, 3.0]. The registry
constraints repeat the same bounds.

### Invalid market data

Market-data defects never raise. They give `signal = 0` with a reason in
`fisher_reason` (text) and `fisher_reason_code` (number). Reason codes:
`entry_long` 1, `entry_short` -1, `no_cross` 0, `insufficient_history` 10,
`missing_columns` 11, `missing_timestamps` 12, `nonfinite_input` 13,
`nonpositive_price` 14, `invalid_candle_geometry` 15 (high below low, or open or
close outside the range), `negative_volume` 16, `timestamp_order` 17 (a
timestamp not strictly after the previous one), `irregular_cadence` 18 (unequal
spacing between consecutive timestamps) and `flat_range` 19.

Validity is checked only inside each decision's own `L + 65`-row span: every
row valid, timestamps strictly increasing with one equal spacing, and no flat
range among the 66 normalizations that `F(t-2)`, `F(t-1)` and `F(t)` read. A
defect outside the span cannot change a decision, and a decision recovers once
the defect leaves the span. Venue prices are tick-rounded, so two equal
midpoints can fill a short lookback; that flat range holds every decision whose
span reads it (66 consecutive decisions). The nominal interval itself is owned by the existing
seal timing (live) and the manifest grid check (study). Candle gaps are never
filled. A reasoned hold keeps the composer running, so the close and stop
evaluators still manage an open position when the entry inputs fail.

### Explanation columns

All numeric and always finite (so the check script exports them in
`indicators`): `fisher_value`, `fisher_trigger` (`F(t-1)`),
`fisher_prev_trigger` (`F(t-2)`), `fisher_normalized`, `fisher_smoothed`,
`fisher_midpoint`, `fisher_range_low`, `fisher_range_high`,
`fisher_eval_ts_ms`, the validity flags `fisher_value_valid`,
`fisher_trigger_valid`, `fisher_prev_trigger_valid`,
`fisher_normalized_valid` and `fisher_valid` (1 or 0), `fisher_lookback`,
`fisher_extreme`, `fisher_horizon`, `fisher_required_history` and
`fisher_reason_code`. An unavailable value is the placeholder 0 with its
validity flag 0. `fisher_reason` is the text form of the reason.

### Truth table

`E` = extreme, `A = F(t)`, `B = F(t-1)`, `C = F(t-2)`, all valid.

| Case | A vs B | B vs C | B vs threshold | Signal | Reason |
|---|---|---|---|---|---|
| 1 | A > B | B < C | B < -E | +1 | entry_long |
| 2 | A > B | B = C | B < -E | +1 | entry_long |
| 3 | A = B | B < C | B < -E | 0 | no_cross |
| 4 | A > B | B > C | B < -E | 0 | no_cross |
| 5 | A > B | B < C | B = -E | 0 | no_cross |
| 6 | A < B | B > C | B > E | -1 | entry_short |
| 7 | A < B | B = C | B > E | -1 | entry_short |
| 8 | A = B | B > C | B > E | 0 | no_cross |
| 9 | A < B | B < C | B > E | 0 | no_cross |
| 10 | A < B | B > C | B = E | 0 | no_cross |
| 11 | fewer than `L + 65` rows | - | - | 0 | insufficient_history |
| 12 | a defect inside the span | - | - | 0 | the defect's reason |

`backtest/tests/test_fisher_reversal_integration.py` checks these cases through
the real registry, composer and simulator.

## Research settings (not trading settings)

- Seed `lookback=9`, `extreme=1.5`. These are proposed research settings with no
  established trading edge.
- Optimizer grid (`DEFAULT_PARAM_RANGES`, 16 combinations): lookback 5, 9, 13,
  21; extreme 0.75, 1.0, 1.5, 2.0. The domain endpoints (lookback 2 and 32,
  extreme 0.5 and 3.0) are tested for parity separately and are not study arms.
- Close owner: the existing `tiered_tp_pct` with its current default ladder
  (1/2/3/4 percent from average cost closing a cumulative 25/50/75/100 percent),
  strict comparison mode and the default `ohlc_walk` intrabar walk. Stop owner:
  the fixed entry-ATR stop at 1 ATR. No regime gate, no higher timeframe.
- One close owner and one stop owner; the composer gives closes priority and
  holds position-increasing signals while a position is open, so there is no
  reversal or scale-in.
- Arms: `fisher_reversal` and `mean_reversion_pro` with direction `both`; `rsi`
  long-only because it registers `short_entries=False`. Each comparator is
  unchanged and tuned on the training window over its existing grid (27 and 972
  combinations) with the same rule and the same close and stop owners.
- $1,000 capital per dataset leg; a signal on bar N fills at the bar N+1 open.

## Data and costs

[`../fisher_reversal_1646_manifest.json`](../fisher_reversal_1646_manifest.json)
(schema `offline_candle_manifest/v1`) sits in `backtest/candidates/`, the common
parent of the earlier studies, so its relative paths reference the hash-pinned
files in `connors_rsi_reversion_1645/data/` without copies or a loader change.
Every hash is verified on load.

- Hyperliquid mainnet 4h candles for BTC, ETH and SOL, closed bars only.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out, with 300 unscored warm-up bars. The same
  dataset and windows were used by earlier candidate studies, so this is
  retrospective candidate evidence.
- Funding: Hyperliquid hourly funding history, booked as a cost while a position
  is open (the manifest's funding attachment from the completed #1731 work).
- Explicit-close arms use the manifest execution specification: base-tier perps
  taker 0.045% and maker 0.015%, a per-coin half spread from the pinned L2
  snapshot plus 5 bps slippage per side, `szDecimals` lot flooring from the
  pinned venue meta, minimum order $10 with a 3% margin.
- The M1 incumbents run without explicit closes and use the flat model (taker
  fee, half spread plus slippage) with the same funding. They are a separate
  reference with different execution geometry.
- Cost stress: cost multiplier 2 doubles the half spread and slippage; fees stay
  at the base tier.

### Coverage gate (before selection or scoring)

For every dataset and window: the exact 4h grid with every warm-up and window
bar present; window edges at the window start and one interval before the end;
every row with finite positive prices, valid geometry and finite non-negative
volume; complete left-closed hourly funding coverage; and a funding record in
every hourly slot of the right-closed accrual span. Any failure gives an
`inconclusive` report naming the refused window, with no score.

## Selection rule (train only)

Eligible: complete candle and funding coverage, all three datasets scored, no
liquidated leg and at least 10 independent positions. Pick the highest mean net
Sharpe, then the highest mean drawdown-adjusted return, then the earliest grid
order. No eligible combination means no selection and an `inconclusive` verdict.

Independent positions: all positions of an arm across the three datasets,
sorted by entry time; a position that opens at or before the latest exit of the
current group joins it, otherwise it starts a new group. Nominal positions are
reported separately.

## Verdict gates (held-out only)

1. `inconclusive` when the coverage gate refuses a window, an arm has no
   eligible selection, any selected arm has fewer than 30 held-out independent
   positions, or the candidate has no long or no short position.
2. `pass` only when the candidate's net PnL is positive at base and at stress
   cost and its base mean net Sharpe and base mean drawdown-adjusted return are
   both strictly above each selected comparator's.
3. Otherwise `fail`.

Registry defaults are also scored at both costs, and the full candidate grid is
scored on the held-out window. These rows are descriptive and cannot change the
selection or the verdict.

## Reproduce

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/fisher_reversal_1646_manifest.json
uv run --no-sync python backtest/candidates/fisher_reversal_1646/run_study.py --manifest backtest/candidates/fisher_reversal_1646_manifest.json
uv run --no-sync python backtest/candidates/fisher_reversal_1646/run_study.py --render-only
```

The study writes `results.json`, `REPORT.md` and the full-precision evidence
files in `evidence/` (every input row and evaluator value for the selected
candidate parameters on both windows).

`discovery_before_{spot,futures}.json` and `discovery_after_{spot,futures}.json`
hold the `--list-json` output of the spot and futures shims before and after
this change; each pair is byte-identical.

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code
