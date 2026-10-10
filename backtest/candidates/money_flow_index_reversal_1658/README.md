# Money Flow Index reversals (#1658)

Candidate `money_flow_index_reversal` (futures registry only, `short_entries=True`,
`edge_status="no_edge"`, hidden from discovery). It follows the current no-edge
availability rule: exactly one explicit `--mode=paper` runs it without an
acknowledgement; `--mode=live` or a missing `--mode` needs `"allow_no_edge": true`.
No study outcome in this directory changes that rule, grants an acknowledgement,
promotes the strategy or writes a live default, a config or a deployment file.

This file is the frozen specification. The signal contract, the parameter grid,
the study design, the selection rule and the verdict gates below were fixed
before any candidate result on the held-out window was read. The held-out result
is in [`REPORT.md`](REPORT.md), rendered from [`results.json`](results.json).

## Registration record

- Short name: `mfi` (unique in the scheduler short-name map), direction `both`.
  The Discord and status-page add-strategy actions create an explicit paper entry
  `hl-mfi-<asset>` with `--mode=paper`, `direction: both` and no acknowledgement.
- Go mirrors: `knownShortNames`, `registeredOpenStrategyPlatforms` (`futures`) and
  `bidirectionalPerpsStrategies` in `scheduler/init.go`; `noEdgeStrategies` in
  `scheduler/edge_status.go` (same `edge_source` and `edge_ref` as Python).
- Evidence metadata: registered first with `edge_source="unvalidated"` and this
  file as `edge_ref`; after the held-out run it records `edge_source="study_fail"`
  and `edge_ref` [`REPORT.md`](REPORT.md) in both Python and Go, as the verdict
  gates below require.
- Not in any starter or default strategy selection. The hidden discovery set is
  derived from the no-edge metadata; there is no second hidden-name list.
- History need: `lookback + 2` consecutive valid bars, at most 200 with the bounds
  below. The live check window is 200 bars, so no feed key, private fetch, check
  lookback or probe flag changes.

## Signal contract

Inputs: one closed-bar frame with `high`, `low`, `close` and `volume`. The
evaluator fetches nothing, writes no book and keeps no state between calls. The
scheduler owns positions, trades, stops and persisted risk state.

- Typical price `TP(N) = high/3 + low/3 + close/3` (each term divided first, so
  the mean of finite prices never overflows).
- Raw flow `RF(N) = TP(N) x volume(N)`, using the supplied volume. Missing volume
  is a hold; volume is never invented or replaced.
- Directional flow at N needs valid bars N-1 and N. `TP(N) > TP(N-1)` puts `RF(N)`
  in positive flow; `TP(N) < TP(N-1)` puts it in negative flow; equal typical
  prices contribute to neither. The first bar of a frame has no prior typical
  price and contributes no flow.
- Totals at N: positive total `P(N)` and negative total `Q(N)` are the exactly
  rounded sums (`math.fsum`) of the directional flows of the `lookback` bars
  ending at N. Each total is computed from its own window only, so the full
  series, every prefix and a bounded 200-bar tail give bit-identical totals.
- Oscillator `MFI(N) = 100 / (1 + Q/P)` when `P > 0` (equal to `100 P / (P + Q)`
  without forming `P + Q`); all-positive flow (`Q = 0`, `P > 0`) gives 100;
  all-negative flow (`P = 0`, `Q > 0`) gives 0. Zero total flow (`P = Q = 0`)
  has the neutral diagnostic value 50 and is not valid for a decision
  (`zero_total_flow`).
- Warm-up: the first valid oscillator needs `lookback + 1` valid bars; a decision
  needs a valid oscillator at N and at N-1, so `lookback + 2` valid bars.
- Long entry at N: `MFI(N-1) <= oversold` and `MFI(N) > oversold`.
- Short entry at N: `MFI(N-1) >= overbought` and `MFI(N) < overbought`.
- An oscillator equal to the threshold at N is not a cross. Equal to the
  threshold at N-1 followed by a strict move past it at N is a cross. Because
  `oversold < overbought`, a bar can never be both a long and a short entry.

### Parameters

Parameters are validated and invalid values raise `ValueError` (a configuration
error, never a hold): `lookback` is an integer in [2, 198] (booleans, fractional
and non-finite values are refused); `oversold` and `overbought` are finite
numbers (not booleans) with `0 <= oversold < overbought <= 100`.

### Invalid market data

Market-data defects never raise; they give `signal = 0` with a reason in
`mfi_reason` (text) and `mfi_reason_code` (number), and the supplied columns and
index stay unchanged. Reasons: `missing_columns:<names>` (16), `nonfinite_input`
(11, any of high/low/close/volume not finite), `nonpositive_price` (12),
`inconsistent_hlc` (13, high below low or close outside the range),
`negative_volume` (15), `timestamp_order` (14, a timestamp not strictly after the
prior one when the frame has a datetime index or a `timestamp` column),
`insufficient_history` (10), `zero_total_flow` (17) and `arithmetic_overflow`
(18, a raw flow or a window total that is not representable). A bad bar
invalidates its own flow, the next bar's comparison and every window that
contains either; recovery needs `lookback + 2` valid bars after the defect.
Zero volume is valid input and contributes zero flow.

A reasoned hold keeps the composer running, so the close and stop evaluators
still process an open position when the entry inputs fail.

### Explanation columns

Each row carries `mfi_high`, `mfi_low`, `mfi_close`, `mfi_volume` (the evaluated
inputs), `mfi_typical`, `mfi_raw_flow`, `mfi_positive_flow`, `mfi_negative_flow`,
`mfi_positive_total`, `mfi_negative_total`, `mfi_prev_positive_total`,
`mfi_prev_negative_total`, `mfi_value`, `mfi_prev_value`, `mfi_lookback`,
`mfi_oversold`, `mfi_overbought`, `mfi_eval_ts_ms`, `mfi_valid`, `mfi_reason`,
`mfi_reason_code` and `signal`. The Hyperliquid check script exports every
numeric `mfi_` column in `indicators` at full float precision (other indicators
keep the existing six-decimal rounding), so an emitted entry can be reproduced
from the check output alone.

### Truth table

`L` = oversold, `U` = overbought.

| Case | MFI(N-1) | MFI(N) | Signal | Reason |
|---|---|---|---|---|
| 1 | <= L | > L | +1 | entry_long |
| 2 | = L | > L | +1 | entry_long |
| 3 | < L | = L | 0 | no_cross |
| 4 | > L | > L | 0 | no_cross |
| 5 | >= U | < U | -1 | entry_short |
| 6 | = U | < U | -1 | entry_short |
| 7 | > U | = U | 0 | no_cross |
| 8 | zero total flow at N or N-1 | - | 0 | zero_total_flow |
| 9 | fewer than `lookback + 2` valid bars | - | 0 | insufficient_history |
| 10 | a defect inside the decision span | - | 0 | the defect's reason |

`backtest/tests/test_money_flow_index_backtest.py` checks these cases through the
real registry, composer and simulator.

## Research settings (not trading settings)

- Seed: `lookback=14`, `oversold=20`, `overbought=80`.
- Optimizer grid (`DEFAULT_PARAM_RANGES`, 36 combinations): lookback 7, 14, 28,
  56; oversold 10, 20, 30; overbought 70, 80, 90. The supported bounds
  (lookback 2 and 198, thresholds 0 and 100) are tested separately and are not
  study arms.
- Every arm (candidate and both comparators) uses the same settings:
  - direction `both`;
  - one close owner `tiered_tp_atr` with `tp_tiers` `[{atr_multiple: 2,
    close_fraction: 1}]`, strict comparison mode, the existing Hyperliquid
    resting-limit take-profit model and the default `ohlc_walk` intrabar walk;
  - one stop owner: the fixed entry-ATR stop at 1 ATR;
  - regime gate: the existing ADX classifier (period 14, threshold 20), allowed
    regime `ranging`, gate failure behavior `closed`, with its existing
    prior-bar timing; no higher timeframe;
  - $1,000 capital, fills at the next bar open.
- One close owner and one stop owner; the composer blocks new entries while a
  position is open, so there is no reversal or scale-in.
- Comparators: `rsi` and `mean_reversion_pro`, unchanged, each tuned on the
  training window over its own existing `DEFAULT_PARAM_RANGES` grid (27 and 972
  combinations) with the same selection rule.
- The M1 incumbent bar that `eval_windows.evaluate_window` computes is recorded
  as a reference only. It does not decide this study.

## Data and costs

[`study_manifest.json`](study_manifest.json) (schema
`offline_candle_manifest/v1`, read by `backtest/offline_manifest.py`) pins the
inputs. `data/` holds byte-identical copies (same sha256) of the #1649 frozen
Hyperliquid files, as copied for #1656; manifest paths must stay inside the
manifest's own directory.

- Venue candles: Hyperliquid mainnet 4h candles for BTC, ETH and SOL with native
  venue volume, closed bars only. No proxy price or volume is used.
- Windows: `train` [2024-09-01, 2025-09-01) for selection and `test`
  [2025-09-01, 2026-09-01) held out, with 300 unscored warm-up bars. The same
  windows were already used by the #1645, #1647, #1649, #1656, #1660 and #1666
  studies, so this is retrospective candidate evidence.
- Funding: Hyperliquid hourly funding history, booked as a cost while a position
  is open (the manifest's frozen funding attachment).
- Fees: base-tier perps taker 0.045% and maker 0.015% (pinned documentation
  quote). The simulator's existing resting-limit path books a take-profit tier
  fill at the maker fee; other fills pay the taker fee.
- Spread and slippage: a per-coin half spread from the pinned L2 snapshot plus 5
  bps of slippage per side.
- Lot size from the pinned venue meta (`szDecimals`); minimum order $10 with the
  live close gate's 3% margin.
- Cost stress: [`study_manifest_fee_x2.json`](study_manifest_fee_x2.json) is the
  same manifest with taker and maker fees doubled; the stress run uses it with
  cost multiplier 2, which doubles spread and slippage. The existing cost
  multiplier alone does not scale fees.

### Coverage gate (before selection or scoring)

`run_study.py` checks every manifest, dataset and window before any selection or
score, and refuses economic scoring when a check fails:

- every manifest hash (a mismatch or a missing file is an explicit failure);
- the candle grid: every warm-up and window bar present on the exact 4h grid,
  the first window bar at the window start and the last one interval before
  the window end;
- every warm-up and window bar valid: finite positive open, high, low and close,
  high not below low, close inside the range, finite non-negative volume;
- funding coverage: the manifest's left-closed hourly coverage of the window,
  and a funding record in every hourly slot from one interval before the window
  start through one interval before the window end (the right-closed accrual
  span of the scored bars). The manifest's left-closed convention is unchanged.

An incomplete window gives a structured `inconclusive` report that names each
refused window. It writes no success score and never infers zero funding for a
missing record.

## Selection rule (train only)

Eligible combinations: complete coverage, all three datasets scored, no
liquidated leg and at least 10 independent trade clusters across datasets. Pick
the highest mean net Sharpe, then the highest mean drawdown-adjusted return,
then the earliest grid order. With no eligible combination the arm has no
selection, and the verdict is `inconclusive`; registry defaults are never
substituted silently.

Independent trade clusters: all positions of an arm across the three datasets,
sorted by entry time; a position whose entry is at or before the latest exit of
the current cluster joins it, otherwise it starts a new cluster. Nominal
positions are reported separately.

## Verdict gates (held-out only)

Inputs: each arm's selected parameters at base cost, and the candidate's selected
parameters at stress cost.

1. `inconclusive` when the coverage gate refuses any window, or when any arm has
   no eligible training selection.
2. `inconclusive` when the candidate has fewer than 30 independent held-out
   clusters, or fewer than 10 nominal long or 10 nominal short positions.
3. `pass` only when all hold: aggregate net PnL > 0 at base and at stress cost;
   no liquidated leg; worst per-dataset maximum drawdown at most 20 percent; mean
   net Sharpe and mean drawdown-adjusted return both above each selected
   comparator's.
4. Otherwise `fail`.

The full candidate grid is also scored on the held-out window and reported as
descriptive evidence; it cannot change the selected parameters or the verdict.
A pass would keep `edge_status="no_edge"` with `edge_source="unvalidated"`
pending a separate reviewed decision.

## Reproduce

Offline (no network; verifies hashes, then reruns everything):

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/money_flow_index_reversal_1658/study_manifest.json
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/money_flow_index_reversal_1658/study_manifest_fee_x2.json
uv run --no-sync python backtest/candidates/money_flow_index_reversal_1658/run_study.py --manifest backtest/candidates/money_flow_index_reversal_1658/study_manifest.json --stress-manifest backtest/candidates/money_flow_index_reversal_1658/study_manifest_fee_x2.json --json backtest/candidates/money_flow_index_reversal_1658/results.json --report backtest/candidates/money_flow_index_reversal_1658/REPORT.md
uv run --no-sync python backtest/candidates/money_flow_index_reversal_1658/run_study.py --render-only --json backtest/candidates/money_flow_index_reversal_1658/results.json --report backtest/candidates/money_flow_index_reversal_1658/REPORT.md
uv run --no-sync python backtest/candidates/money_flow_index_reversal_1658/parity_check.py --registry futures --window 0 --json backtest/candidates/money_flow_index_reversal_1658/parity_window0.json
uv run --no-sync python backtest/candidates/money_flow_index_reversal_1658/parity_check.py --registry futures --window 200 --json backtest/candidates/money_flow_index_reversal_1658/parity_window200.json
uv run --no-sync python backtest/candidates/money_flow_index_reversal_1658/parity_check.py --registry futures --window 0 --batched --json backtest/candidates/money_flow_index_reversal_1658/parity_window0_batched.json
uv run --no-sync python backtest/candidates/money_flow_index_reversal_1658/parity_check.py --registry futures --window 200 --batched --json backtest/candidates/money_flow_index_reversal_1658/parity_window200_batched.json
```

`parity_check.py` compares every optimizer combination plus the three bound
fixtures (lookback 2, lookback 198, thresholds 0 and 100) on the last 400 frozen
bars of BTC, ETH and SOL, in three modes: entry only; composed with the
`tiered_tp_atr` close owner and direction `both` (flat, long and short position
contexts with quantity, entry price and entry ATR); and composed with the ADX
regime label compared as well. `--window 0` compares the full series with every
prefix, `--window 200` with every trailing 200-bar frame, and `--batched` also
compares an explicit-paper batch slot with the solo evaluator. It exits 1 on any
mismatch, any empty cell, a mode without both entry directions or a composed mode
without both position sides. The four `parity_window*.json` files hold the
results.

`discovery_before.json` and `discovery_after.json` hold the spot and futures
`--list-json` output before and after this change; they are byte-identical.
