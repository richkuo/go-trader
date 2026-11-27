# Volume spike breakout (#2008)

Research candidate `volume_spike_breakout`, registered for spot and futures with
`edge_status="no_edge"`, `short_entries=True` and `edge_ref` set to
[`REPORT.md`](REPORT.md). Its `edge_source` was `unvalidated` until the frozen
study below produced its verdict, FAIL; it is now `study_fail`. The source
hashes in `REPORT.md` were taken at scoring time, before that one metadata edit
in `shared_strategies/open/registry.py`; the edit changes no evaluation. Ordinary no-edge admission applies: discovery
hides it, an explicit `--mode=paper` check evaluates it, and live or missing
mode needs `allow_no_edge: true`. A study verdict changes no admission, default
or live configuration. Nothing in this directory writes a live default, a
config or a deployment file.

This file and [`protocol.json`](protocol.json) are the frozen specification.
The signal contract, the research settings, the comparator mappings, the
selection rule, the evidence rule and the verdict rule were fixed before any
held-out window was scored. The result is in [`REPORT.md`](REPORT.md), rendered
from [`results.json`](results.json) by [`run_study.py`](run_study.py).

## Signal contract

Owner: `shared_strategies/open/volume_spike_breakout.py`. The evaluator reads
one candle frame (`open`, `high`, `low`, `close`, `volume`, and a timestamp per
row from a datetime index, else a `timestamp` column). It fetches nothing,
writes no book, position, trade or stop state, and returns a copy of the frame.

- Range at bar N: `range_high[N] = max(high[N-lookback .. N-1])`,
  `range_low[N] = min(low[N-lookback .. N-1])`. Bar N is excluded from its own range.
- Relative volume at bar N: `rel_vol[N] = volume[N] / mean(volume[N-vol_period .. N-1])`.
  Bar N is excluded from its own baseline. Each mean is that window's own
  sequential sum divided by `vol_period`.
- Body fraction at bar N: `|close-open| / (high-low)`; undefined for a zero-range bar.
- Qualified long at N: `close[N] > range_high[N]` and `rel_vol[N] > vol_multiplier`
  and, when `min_body_frac > 0`, `body_fraction[N] >= min_body_frac` (a zero-range
  bar fails). Qualified short mirrors it with `close[N] < range_low[N]`.
  Comparisons with the range and the multiplier are strict.
- Entry at N: the qualified condition is true on N and false on N-1, and both
  conditions are known (both bars fully valid). A new volume or body
  qualification after a false bar starts a new event even when price stays
  beyond the range. If both sides qualify, the bar holds (`both_sides_qualified`).
- Platform binding: the registry binds a fixed function per platform. The
  futures evaluator emits `+1` and `-1`. The spot evaluator never emits a
  negative entry; a qualified short edge on spot holds with
  `short_suppressed_spot`. No parameter can enable spot shorts.
- History: a decision needs `max(lookback, vol_period) + 2` clean rows (the
  current and the previous qualified condition). With the bounds below it is at
  most 102 rows, inside the 200-row live check window.

### Parameters

`lookback` and `vol_period`: integers in [2, 100] (integral floats accepted,
booleans and fractions refused). `vol_multiplier`: finite, `>= 1`.
`min_body_frac`: finite, in [0, 1); 0 turns the body filter off. Research seeds:
20, 20, 2.0, 0.0. The registry `constraints` and the `param_validator`
(`vsb_param_validator`, which also refuses unknown keys) run on every registry
entry point, and the evaluator validates again on a direct call. Invalid
parameters are configuration errors (`ValueError`).

### Input validity

Market-data defects never raise. They hold with `signal = 0` and a reason; no
row is repaired, sorted, filled or invented.

| Defect | Reason | Code |
|---|---|---|
| Missing `open`/`high`/`low`/`close`/`volume` column | `missing_columns` (every row) | 11 |
| Non-finite price or volume | `nonfinite_input` | 12 |
| Price <= 0 | `nonpositive_price` | 13 |
| Volume <= 0 | `invalid_volume` | 14 |
| `high < low`, or `open` or `close` outside [low, high] | `inconsistent_ohlc` | 15 |
| Missing (NaT) timestamp | `missing_timestamp` | 16 |
| Duplicate or non-increasing timestamp step | `timestamp_order` | 17 |
| Overflowing baseline or relative volume | `nonfinite_derived` | 18 |
| Fewer rows than the history need | `insufficient_history` | 10 |

A decision at N reads rows `N-1-max(lookback, vol_period) .. N` and the
timestamp steps between them. The reported reason is the most recent defect in
that span. A hold recovers on the first bar whose span is clean again; an
invalid or unavailable previous condition never creates a false-to-true edge.

Valid-decision reasons: `entry_long` 1, `entry_short` -1, `no_breakout` 0,
`volume_below_threshold` 2, `body_below_threshold` 3, `breakout_continuation` 4,
`short_suppressed_spot` 5, `both_sides_qualified` 6.

### Explanation columns

`vsb_range_high`, `vsb_range_low`, `vsb_base_volume`, `vsb_rel_vol`,
`vsb_body_frac`, `vsb_long_qualified`, `vsb_short_qualified`,
`vsb_prev_long_qualified`, `vsb_prev_short_qualified`, `vsb_lookback`,
`vsb_vol_period`, `vsb_vol_multiplier`, `vsb_min_body_frac`,
`vsb_required_history`, `vsb_short_entries` (1 futures, 0 spot), `vsb_valid`,
`vsb_reason`, `vsb_reason_code` and `signal`. Unavailable values are NaN in the
frame and absent in check output. The spot (`check_strategy.py`), Robinhood,
Hyperliquid (solo and batch), OKX and Topstep checks emit the `vsb_*` numbers at
full precision and the text reason under `indicator_labels.vsb_reason`.

### Closed bars, close and stop

Research decisions use closed bars and fill no earlier than the next bar open.
On a check script, only the existing `closed_bar_decisions` selector decides on
the last closed bar; a legacy check can read the forming bar. The strategy is
entry-only: pair it with exactly one explicit close strategy and one valid stop
owner. The composer blocks new entries while a position is open, so there is no
implicit reversal and no scale-in. An entry-data defect returns a reasoned hold
and the close evaluators and protection still run with their own valid inputs.

## Study

Reproduce offline (no network):

```bash
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/volume_spike_breakout_2008/study_manifest.json
uv run --no-sync python backtest/candidates/volume_spike_breakout_2008/run_study.py --check-protocol
uv run --no-sync python backtest/candidates/volume_spike_breakout_2008/run_study.py
```

`run_study.py` refuses to score when `protocol.json` differs from its frozen
sha256 (`1ee4ebffc89c3ad97a68a0c87aa9e503c4f0518ae9156d4bac60c7bb52a32b2c`).
`--render-only` re-renders `REPORT.md` from the committed `results.json`; the committed `REPORT.md` is that render (a full run renders the same lines with the arms in protocol order instead of sorted order).

### Inputs and prior use

The study reuses the frozen Hyperliquid 4h BTC, ETH and SOL candles, hourly
funding and venue references of `chaikin_money_flow_1649`, copied byte for byte
into `data/` (the manifest path rule needs inputs inside the manifest
directory). No new data was acquired. Hashes:

| File | sha256 |
|---|---|
| `BTC_4h_candles.csv.gz` | `89f84288d462b830ceba8108ea9d69db4d3dac417c730dc9cdd13271c10fd0f8` |
| `BTC_funding.csv.gz` | `b961391682a15a619fe131abedcc638fd2c311af7cd30a67cf4af32754cabc48` |
| `ETH_4h_candles.csv.gz` | `a17523ecbb30f7b594aa80002b3c5ad80b73a7bdd8122d5512683585a8eec3ae` |
| `ETH_funding.csv.gz` | `1b541abb8801a444cac4e23ebe31d71cbd7dc3b7d185456830daca1c09c6ea66` |
| `SOL_4h_candles.csv.gz` | `5ca4fa74940f38f180a85eb81ccaa96bb10f08988d1351c280abb2ce175fdd4c` |
| `SOL_funding.csv.gz` | `a498781bca4cf017f7f772f5a0c00c0853979bacfd147303394f63a63da5af40` |
| `venue_meta.json` | `458d89887417a477a93db9c93607de5f64b8813d88013e0f4883fc06d6c35249` |
| `venue_l2_snapshot.json` | `d5e5369d80547b45ed22dc92a107db1af9303d997a0eef6c347994858b0b9407` |

Prior use: 21 earlier studies pin the same BTC 4h candle file and score the
same held-out span; `protocol.json` lists them. The test window is chronological
retrospective evidence and is not claimed to be previously unseen data.

Windows: train [2024-09-01, 2025-09-01) for selection only; test
[2025-09-01, 2026-09-01) held out, with four predefined quarters for window
stability and worst-window drawdown; 300 warm-up bars before each window feed
indicators only.

### Execution and costs

Every arm runs through the M1 manifest path (`eval_windows.evaluate_window`
with the futures registry and `funding_mode="charge"`, the equivalent of
`--manifest` + `--registry futures` + `--funding charge`), the composer and the
simulator: one explicit `tiered_tp_pct` close (a single full 2% target), one
fixed entry ATR x1.0 stop owner, strict close-capability validation,
`ohlc_walk`, capital $1000 per dataset, bar-N signals filled at the bar N+1
open, manifest fees, per-coin spread, slippage, lot rounding and the venue
minimum order value, and hourly funding. The stress run doubles spread and
slippage; fees stay at the manifest values. The M1 incumbents run under each
arm's matched close, stop and direction contract (`incumbent_contract`) as a
descriptive reference only.

### Arms and comparators

- `candidate_both` (primary) and `candidate_long`: the 300-combination grid
  `lookback` and `vol_period` in {10, 20, 40, 80, 100}, `vol_multiplier` in
  {1, 1.5, 2, 3}, `min_body_frac` in {0, 0.25, 0.5}.
- `multiplier_one_both`: the same grid restricted to `vol_multiplier = 1`. It
  still requires relative volume strictly above 1; it is a sensitivity arm,
  not a volume-free control.
- `price_only_both` and `price_only_long`: a study-local adapter over the same
  evaluator core with the relative-volume predicate removed before the current
  and previous qualified conditions are computed. Data validity (volume
  validity included), the body filter, platform behavior and execution rules
  are unchanged. It is routed through the same M1, composer and simulator stack
  under a study-local name that is never registered; no production bypass
  parameter exists.
- Comparators, each tuned only on train with its native optimizer grid and
  mapping (see `protocol.json`): `session_breakout` (lookback counts sessions),
  `volume_weighted` (`sma_period`, price-cross entry), `breakout` (range plus
  true-range expansion), `donchian_breakout` (`entry_period`) and
  `chaikin_money_flow_breakout` (flow and breakout windows). All five run under
  the common long-only contract. The comparators that declare short entries
  (`session_breakout`, `donchian_breakout`, `chaikin_money_flow_breakout`) also
  run bidirectionally against `candidate_both`. `breakout` and
  `volume_weighted` declare `short_entries=False` and run long-only.

### Selection, evidence and verdict

- Selection on train: eligible combinations score all three datasets with
  complete candle and funding coverage, no liquidated leg and at least 10
  independent position clusters; pick the highest mean net Sharpe, then the
  highest mean drawdown-adjusted return, then the earliest grid order.
- Independent position clusters: every position interval [entry, exit] of an
  arm and window, merged across datasets when intervals overlap or touch.
- Parity gates: the selected candidate and price-only control signals equal an
  independent per-bar reference on every dataset frame (train and test, warm-up
  included), and the selected `candidate_both` decisions match the 200-bar live
  window on every held-out dataset frame (`parity_diff`, strict).
- Inconclusive: incomplete coverage, a matched arm with no eligible selection,
  fewer than 30 independent held-out clusters for `candidate_both`, or a failed
  parity gate.
- Pass requires all of: `candidate_both` mean held-out net return above 0 at
  cost x1 and x2; in each matched contract the candidate's mean held-out net
  return above the price-only control and every comparator; `candidate_both`
  worst quarterly drawdown no worse than `price_only_both` and every
  bidirectional comparator; the median held-out mean return of the selected
  parameters' one-step grid neighbours above 0.
- Fail: adequate evidence and any pass requirement false.

### Discovery evidence

`discovery_{before,after}_{spot,futures}.json` are the ordinary `--list-json`
captures before and after registration; they are byte-identical because
no-edge names stay hidden. `evidence/catalog_{before,after}_{spot,futures}.json`
are full `shared_tools/strategy_catalog.py --list-json --platform` captures; the
only difference is the new `volume_spike_breakout` open entry on each platform.
`backtest/tests/test_strategy_backtest_contract.py` checks both.

### Limits

Three coins, one interval and one year of held-out data that earlier studies
also scored. Candle volume is venue-reported traded volume per 4h bar, so a
spike can come from a single large trade. Results are research evidence only;
they never write live defaults, configs or pull requests.
