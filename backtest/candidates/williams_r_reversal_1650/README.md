# Williams %R reversal (#1650)

Research candidate `williams_r_reversal` (futures registry only,
`edge_status="no_edge"`, hidden from discovery). Tracking group:
`hyperliquid-strategy-candidates-2026-10-02`. Paper evaluation needs one
explicit `--mode=paper`; live use needs the existing operator acknowledgement
`allow_no_edge: true`, which this issue does not set. Nothing in this directory
writes a live default, a config or a deployment file.

The held-out result is in [`REPORT.md`](REPORT.md) (rendered from
[`results.json`](results.json)). The frozen choices are in
[`candidate_spec.json`](candidate_spec.json); this file explains them. They
were fixed before the held-out window was scored, and `run_study.py` refuses to
run when the registry seed or optimizer grid differs from the specification.

## Signal contract

Inputs: one closed-bar candle frame with `high`, `low` and `close`, plus
timestamps when the frame carries them (a datetime index or a `timestamp`
column). The evaluator reads no other data, fetches nothing, writes no book and
keeps no state between calls; it owns only evaluation-local arrays. Registry
metadata is populated on module load. The scheduler keeps all position, stop
and trade ownership.

- Window at bar t: the `lookback` bars ending at t (the decision bar is
  included).
- %R at t: `-100 x (highest high - close[t]) / (highest high - lowest low)`
  over that window. It lies in [-100, 0].
- Long entry at t: %R at t-1 is at or below `oversold` and %R at t is strictly
  above `oversold`.
- Short entry at t: %R at t-1 is at or above `overbought` and %R at t is
  strictly below `overbought`.
- %R at t equal to the threshold is not a crossing. Both values must be valid
  and consecutive; nothing is forward-filled.
- A decision needs `lookback + 1` consecutive valid candles. With `lookback` at
  most 100 this is at most 101 candles, inside the 200-candle live check
  window, so no feed, argument or startup protocol change is needed.
- Every term is a trailing window with exact maxima and minima, so the full
  series, every historical prefix and a bounded tail give the same decision for
  every bar with enough history.

### Parameters

`lookback` is an integer in [2, 100] (booleans, strings and fractional values
are refused; an integral float such as `14.0` is accepted). `oversold` and
`overbought` are finite numbers with -100 <= oversold < overbought <= 0. The
registry carries the binary constraints `lookback >= 2`, `lookback <= 100`,
`oversold >= -100`, `overbought <= 0` and `oversold < overbought`, and a
`param_validator` callback for the type rules. Invalid parameters raise
`ValueError` at evaluator entry, in `validate_params`, in this study's
preflight and in the Hyperliquid solo and batch preflight before any candle
fetch or shared-state build.

### Invalid market input

Market-data defects never raise. They give a zero entry signal with a reason in
`wr_reason` (text) and `wr_reason_code` (number):

| Reason | Code | Cause |
|---|---|---|
| `missing_columns:<names>` | 17 | `high`, `low` or `close` is absent (whole frame holds) |
| `nonfinite_input` | 11 | a missing, non-numeric or non-finite high, low or close |
| `nonpositive_price` | 12 | a high, low or close at or below zero |
| `inconsistent_hlc` | 13 | high below low, or close outside [low, high] |
| `timestamp_order` | 14 | a missing timestamp, or one not strictly after the previous candle's |
| `zero_range_window` | 16 | highest high equals lowest low in the window at t or t-1 |
| `insufficient_history` | 10 | fewer than `lookback + 1` candles |

A bad candle invalidates every decision whose `lookback + 1` span contains it;
decisions recover once the span is clean.

### Explanation columns

Each row carries `wr_highest_high`, `wr_lowest_low`, `wr`, `wr_prev`,
`wr_lookback`, `wr_oversold`, `wr_overbought`, `wr_eval_ts_ms` (the evaluated
bar's timestamp), `wr_valid`, `wr_reason`, `wr_reason_code` and `signal`, next
to the input columns. Values that are not valid are left unavailable (NaN), and
the Hyperliquid check output drops them from `indicators`.

### Truth table

`P` = %R at t-1, `C` = %R at t, `OS` = oversold, `OB` = overbought.

| Case | P | C | Signal | Reason |
|---|---|---|---|---|
| 1 | <= OS | > OS | +1 | entry_long |
| 2 | <= OS | = OS | 0 | no_cross |
| 3 | > OS | > OS | 0 | no_cross |
| 4 | >= OB | < OB | -1 | entry_short |
| 5 | >= OB | = OB | 0 | no_cross |
| 6 | < OB | < OB | 0 | no_cross |
| 7 | span has a bad candle | - | 0 | the defect's reason |
| 8 | zero range at t or t-1 | - | 0 | zero_range_window |
| 9 | fewer than `lookback + 1` candles | - | 0 | insufficient_history |

`shared_scripts/test_williams_r_integration.py` checks the cases through the
real registry and the real Hyperliquid check script, and
`backtest/tests/test_williams_r_backtest.py` checks them through the
simulator.

## Availability record

- Scheduler short name `wrr` (no collision), futures-only platform row, member
  of the bidirectional set (the Discord add command writes
  `direction: both` and `--mode=paper`), no default-list entry.
- Evidence row in the Python registry and in `scheduler/edge_status.go`:
  `study_fail`, pointing at `REPORT.md`. The registry started at `unvalidated`
  and moved to `study_fail` after the frozen verdict; a pass would have kept
  `unvalidated`. Every outcome keeps `edge_status="no_edge"`.
- `discovery_before.json` and `discovery_after.json` hold the spot and futures
  `--list-json` output before and after the change; they are byte-identical.
  `research_registry_diff.txt` holds the only research-registry difference,
  the new entry.

## Research settings (not trading settings)

- Seed: `lookback=14`, `oversold=-80`, `overbought=-20`. These values have no
  established trading edge.
- Optimizer grid (`DEFAULT_PARAM_RANGES`): `lookback` 7, 14, 21, 28;
  `oversold` -90, -80, -70; `overbought` -30, -20, -10.
- Every arm (candidate and comparators) uses direction `both`; one close owner,
  the existing `tiered_tp_atr` with one tier at `atr_multiple=2.0`,
  `close_fraction=1.0`; one stop owner, the fixed entry-ATR stop with
  multiplier 1.0 (simple ATR); strict close comparison; $1,000 capital per
  dataset; leverage 1; no scale-in and no implicit reversal. The composer and
  the simulator block new entries while a position is open.
- Range gate (the existing regime implementation): classifier `adx`, period 14,
  threshold 20, allowed label `ranging`, failure policy `closed`, timeframe
  `4h`. Each bar's label comes from `bounded_window_labels` over the trailing
  200 rows ending at that bar, with no shift. An entry decided on closed bar N
  is gated by bar N's label and fills at bar N+1 open. The gate blocks entries
  only; open positions keep their take-profit and stop. There is no
  higher-timeframe series, so no opening-time shift applies.
- Comparators: `rsi`, `stoch_rsi` and `mean_reversion_pro`, unchanged, each
  tuned on the training window over its own `DEFAULT_PARAM_RANGES` grid with
  the same rule and the same close, stop, gate, cost and direction settings.
  `rsi` and `stoch_rsi` register `short_entries=False`; under direction `both`
  the engine opens shorts on their -1 signals, which are a matched mechanical
  control and do not show native short-entry support. Long-only controls for
  every arm are reported separately and do not enter the verdict.
- The M1 incumbent bar (median of the eight incumbents, their own registry
  defaults, no close owner, the flat legacy cost model, the same frozen candles
  and funding) is the incumbent verdict check.

## Data and costs

[`study_manifest.json`](study_manifest.json) (schema
`offline_candle_manifest/v1`, read by `backtest/offline_manifest.py`) pins the
same frozen inputs as the #1645 and #1649 studies. Every file under `data/` is
a byte-identical copy of `backtest/candidates/connors_rsi_reversion_1645/data/`
with the same sha256, because manifest paths cannot leave the manifest
directory. That data pool was used by earlier studies; the held-out window is
held out only for this candidate's parameter selection.

- Venue candles: Hyperliquid mainnet 4h candles for BTC, ETH and SOL, closed
  bars only.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out. 300 warm-up bars before each window feed
  the indicators and labels and are never scored.
- Funding: the manifest funding files, attached as a cost through the existing
  manifest funding contract and checked by hourly bucket. Per-leg coverage and
  hashes are in `results.json`; incomplete coverage makes the verdict
  inconclusive. `--funding charge` applies to the ordinary loader, not to the
  manifest path; no ordinary (Binance US proxy) leg was run for this study.
- Fees: base-tier perps taker 0.045% and maker 0.015% (pinned documentation
  quote).
- Spread and slippage: a per-coin half spread from a pinned L2 snapshot plus
  5 bps of slippage per side, combined once at execution. The stress run
  doubles both through the existing cost multiplier.
- Lot size and minimum: `szDecimals` from the pinned venue meta, floored before
  accounting; minimum order $10 with the live close gate's 3% margin.

## Selection rule (train only)

Eligible combinations: all three datasets scored with complete candle and
funding coverage, no liquidated leg, and at least 10 independent positions.
Pick the highest mean Sharpe, then the highest mean drawdown-adjusted return,
then the earliest grid order. With no eligible combination the registry
defaults are used and the verdict is inconclusive.

## Verdict rule (held-out only)

Inconclusive when any held-out candidate or comparator leg has incomplete
candle or funding coverage, when no candidate combination was eligible on the
training window, when the candidate has fewer than 30 independent held-out
positions (a partially closed position counts once), or when any held-out
candidate or comparator leg has incomplete close parity.

Otherwise pass only when all hold: candidate mean net return above zero at cost
x1 and x2; candidate mean Sharpe and mean drawdown-adjusted return above each
comparator's (selected, cost x1); and the candidate's M1 incumbent verdict is
`pass`. Anything else is a fail. Held-out variant diagnostics never change the
selection. Aggregate rows are sums and means of independent per-dataset
studies and do not describe shared-wallet performance.

## Frozen-input parity

`run_study.py` calls `parity_diff.compute_parity_frame` (non-batched, futures
registry, the study close owner, direction `both`, the gate's label plan) on
each dataset's hash-verified held-out frame with the selected parameters, at
`window=None` (every historical prefix) and `window=200`, and records compared
bars, mismatches, their span and the mismatches inside the scored window.
Solo and batched paper parity on the real Hyperliquid slot evaluator runs in
`backtest/tests/test_williams_r_backtest.py`.

## Reproduce

Offline (no network; verifies hashes, then reruns everything):

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/williams_r_reversal_1650/study_manifest.json
uv run --no-sync python backtest/candidates/williams_r_reversal_1650/run_study.py
uv run --no-sync python backtest/candidates/williams_r_reversal_1650/run_study.py --render-only
uv run --no-sync python backtest/eval_windows.py --registry futures --candidate-json backtest/candidates/williams_r_reversal_1650/candidate_seed.json --manifest backtest/candidates/williams_r_reversal_1650/study_manifest.json --windows test
```

`candidate_seed.json` is the seed arm with the gate's resolved label plan; the
last command scores it against the M1 incumbent bar on the held-out window.
