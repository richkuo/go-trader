# Inversion fair value gap retest (#2012)

Research candidate `inversion_fvg`, registered for spot and futures with
`edge_status="no_edge"`, `short_entries=True` and `edge_ref` set to
[`REPORT.md`](REPORT.md). Its `edge_source` was `unvalidated` until the frozen
study below produced its verdict, FAIL; it is now `study_fail`. The source
hashes in `REPORT.md` were taken at scoring time, before that one metadata edit
in `shared_strategies/open/registry.py`; the edit changes no evaluation.
Ordinary no-edge admission applies: discovery hides it, an explicit
`--mode=paper` check evaluates it, and live or missing mode needs
`allow_no_edge: true`. It is not `production_refused`. A study verdict changes
no admission, default or live configuration. Nothing in this directory writes a
live default, a config or a deployment file.

This file and [`protocol.json`](protocol.json) are the frozen specification.
The signal contract, the research settings, the control definitions, the
comparators, the selection rule, the evidence rule and the verdict rule were
fixed before any window was scored. The result is in [`REPORT.md`](REPORT.md),
rendered from [`results.json`](results.json) by [`run_study.py`](run_study.py).

## Why a new entry

`amd_ifvg` is an ICT session model in which "IFVG" means an implied gap: it
enters on a retest of a gap in the trade direction, so no inversion occurs.
`sweep_absorption_ifvg` is a true inversion setup, but it also needs a session
sweep, Hyperliquid trade-flow absorption and one-hour structure; it is
production-refused and cannot run on candles alone. Before this change the
open registry had no standalone candle-only inversion-and-retest entry.

## Signal contract

Owner: `shared_strategies/open/inversion_fvg.py`. The evaluator reads one
closed-bar candle frame (`open`, `high`, `low`, `close`, and a timestamp per row
from a datetime index, else a `timestamp` column). It does not read volume,
fetches nothing, writes no book, position, trade or stop state, and returns a
copy of the frame.

- Gap at bar k: bullish when `low[k] > high[k-2]` (zone `[high[k-2], low[k]]`),
  bearish when `high[k] < low[k-2]` (zone `[high[k], low[k-2]]`). Comparisons are
  strict, so a zero-width gap is not a gap. The width must be at least
  `min_fvg_atr` times the simple 14-bar ATR at k: the mean of the 14 true ranges
  of bars k-13..k, each using the previous close (method `simple`).
- Inversion: a bearish gap inverts to a long zone on the first bar j with
  `k < j <= k + max_gap_age` whose close is strictly above the zone top; a
  bullish gap inverts to a short zone on the first close strictly below the
  zone bottom. A gap inverts at most once; one that reaches `max_gap_age`
  without inversion expires.
- Entry level: `bottom + entry_level * (top - bottom)` for a long,
  `top - entry_level * (top - bottom)` for a short (`entry_level = 0.5` is the
  consequent encroachment).
- Retest: a long fires on the first bar m with `j < m <= j + retest_window`,
  `low[m] <= level <= high[m]` and `close[m] >= bottom`; a short needs the same
  range containment and `close[m] <= top`. A candle wholly beyond the level
  cannot qualify, and the inversion bar cannot be a retest.
- Order at every bar: first clear expired zones and zones closed through (long:
  close below bottom; short: close above top), then process inversions and
  replace the armed zone per side, then evaluate retests of zones armed on an
  earlier bar. The most recent inversion wins per side; several same-bar
  inversions keep the greatest gap bar k. A newer inversion replaces the older
  zone even on that zone's potential retest bar. Replaced, cleared or consumed
  zones never resume. Each qualifying first retest consumes its zone, including
  one that holds on a conflict or on spot.
- Conflict: both raw sides qualifying on one bar consume both zones and hold
  (`both_sides_qualified`) before platform suppression.
- Platform binding: the registry binds a fixed function per platform. The
  futures evaluator emits `+1` and `-1`. The spot evaluator never emits a
  negative entry; a short qualification on spot holds with
  `short_suppressed_spot`. No parameter can enable spot shorts.

### Parameters and bounded history

`min_fvg_atr`: finite in [0, 10]. `max_gap_age` and `retest_window`: integers in
[1, 200] (integral floats accepted, booleans and fractions refused).
`entry_level`: finite in [0, 1]. Combined bound:
`max_gap_age + retest_window + 15 <= 200`. Research seeds: 0.25, 48, 24, 0.5.
The registry `constraints` and the `param_validator` (`invfvg_param_validator`,
which also refuses unknown keys and enforces the combined bound) run on every
registry entry point, and the evaluator validates again on a direct call.
Invalid parameters are configuration errors (`ValueError`).

`required_history_bars = max_gap_age + retest_window + 15`: the earliest gap
that can still affect a decision precedes it by both windows, and its 14 true
ranges need one earlier close. The seed needs 87 rows; every admitted
combination fits the 200-row live check window. The optimizer warm-up hook
(`backtest/optimizer.py` `strategy_warmup_bars`) and this study use the same
support function. Each gap's first inversion and each zone's first clear or
retest event are fixed from its own bars, so a decision depends only on its own
support span, uses no process-global state, and a full-series evaluation equals
evaluation on every prefix and on the 200-row window.

### Input validity

Market-data defects never raise. They hold with `signal = 0` and a reason; no
row is repaired, sorted, filled or dropped to join separated candles.

| Defect | Reason | Code |
|---|---|---|
| Missing `open`/`high`/`low`/`close` column | `missing_columns` (every row) | 11 |
| Non-finite price | `nonfinite_input` | 12 |
| Price <= 0 | `nonpositive_price` | 13 |
| `high < low`, or `open` or `close` outside [low, high] | `inconsistent_ohlc` | 15 |
| Missing (NaT) timestamp | `missing_timestamp` | 16 |
| Duplicate or non-increasing timestamp step | `timestamp_order` | 17 |
| Overflowing ATR, width or entry level | `nonfinite_derived` | 18 |
| Fewer rows than `required_history_bars` | `insufficient_history` | 10 |

A decision at bar N is evaluated only when its whole support span, rows
`N - required_history_bars + 1 .. N` and the timestamp steps between them, is
clean. That span contains every bar from the earlier close a relevant gap's ATR
needs through the retest, so a defect anywhere inside a zone's span invalidates
that zone. The reported reason is the most recent defect in the span. New gaps
form again once their own 15-bar ATR history is clean, and decisions recover on
the first bar whose full span is clean again; an old defect never clears every
later decision.

Valid-decision reasons: `entry_long` 1, `entry_short` -1, `no_armed_zone` 0,
`awaiting_retest` 2, `zone_armed_this_bar` 3, `zone_invalidated` 4,
`short_suppressed_spot` 5, `both_sides_qualified` 6.

### Explanation columns

Per side (`long`, `short`), for the armed zone at the bar:
`invfvg_<side>_zone_bottom`, `_zone_top`, `_level`, `_gap_atr`, `_gap_ts_ms` and
`_armed_ts_ms` (timestamp-based event identifiers, stable when older rows leave
the frame), `_bars_since_armed`, and `_qualified` (the raw qualification before
conflict handling and spot suppression). Common: `invfvg_min_fvg_atr`,
`invfvg_max_gap_age`, `invfvg_retest_window`, `invfvg_entry_level`,
`invfvg_required_history`, `invfvg_short_entries` (1 futures, 0 spot),
`invfvg_valid`, `invfvg_reason`, `invfvg_reason_code` and `signal`. Unavailable
values are NaN in the frame and absent in check output. The spot
(`check_strategy.py`), Robinhood, Hyperliquid (solo and batch), OKX and Topstep
checks emit the `invfvg_*` numbers at full precision and the text reason under
`indicator_labels.invfvg_reason`. The prefix is `invfvg_` because `amd_ifvg`
already writes `ifvg_high` and `ifvg_low`.

### Closed bars, close and stop

Research decisions use closed bars and fill no earlier than the next bar open.
On a check script, the existing `closed_bar_decisions` selector decides on the
last closed bar; a legacy check can read the forming bar. Robinhood live keeps
its existing closed-bar refusal. Paper evaluation, for example
`check_hyperliquid.py inversion_fvg BTC 4h --mode=paper --closed-bar-decisions
--strategy-refs '{"open":{"name":"inversion_fvg"},"closes":[{"name":"tiered_tp_pct"}]}'`,
needs explicit paper mode; live use needs `allow_no_edge: true` in the strategy
config. The strategy is entry-only: pair it with exactly one explicit close
strategy and one valid stop owner. The composer blocks new entries while a
position is open, so there is no implicit reversal and no scale-in. An
entry-data defect returns a reasoned hold, and the close evaluators and
protection still run with their own valid inputs.

## Study

Reproduce offline (no network):

```bash
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/inversion_fvg_2012/study_manifest.json
uv run --no-sync python backtest/candidates/inversion_fvg_2012/run_study.py --check-protocol
uv run --no-sync python backtest/candidates/inversion_fvg_2012/run_study.py
```

`run_study.py` refuses to score when `protocol.json` differs from its frozen
sha256 (`def1df13ff852c138b749993c26118ee652946ccc16e0aa0374e558405f11da1`).
`--render-only` re-renders `REPORT.md` from the committed `results.json`; the
committed `REPORT.md` is that render (a full run renders the same lines with the
arms in protocol order instead of sorted order).

### Inputs and prior use

The study reuses the native Hyperliquid 4h BTC, ETH and SOL candles
(`candleSnapshot`), hourly funding and venue references frozen by
`chaikin_money_flow_1649` and copied by `volume_spike_breakout_2008`, copied
byte for byte again into `data/` (the manifest path rule needs inputs inside the
manifest directory). No new data was acquired. Hashes:

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

The study tests the pattern on 4h candles only and does not establish
performance on shorter timeframes. The 1h inputs of `chande_momentum_1661`,
`ease_of_movement_1671` and `ultimate_oscillator_1664` are Binance US spot
candle proxies and are not used.

Prior use: 23 earlier study manifests or protocols pin the same BTC 4h candle
file; `protocol.json` lists them. `volume_spike_breakout_2008`, whose windows
this study reuses, scored the same held-out span. The test window is
chronological retrospective evidence and is not claimed to be previously unseen
data.

Windows: train [2024-09-01, 2025-09-01) for selection only; test
[2025-09-01, 2026-09-01) held out, with four predefined quarters for window
stability and worst-window drawdown; 300 warm-up bars before each window feed
the evaluators only. The largest support of any scored arm is 159 bars.

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

### Arms

- `candidate_both` (primary) and `candidate_long`: the 108-combination grid
  `min_fvg_atr` in {0, 0.25, 0.5, 1}, `max_gap_age` in {24, 48, 96},
  `retest_window` in {12, 24, 48}, `entry_level` in {0.25, 0.5, 0.75} (the
  optimizer grid; every combination passes the validator).
- `noninverted_both` and `noninverted_long`: the non-inverted gap retest
  control. The same gaps are armed in their original direction at formation
  bar k, replaced by the greatest k per side, and the first contained retest
  qualifies only at `k < m <= k + retest_window` (long close >= bottom, short
  close <= top). An original long clears on a close below bottom, an original
  short on a close above top, before retest evaluation. This isolates the
  inversion. `max_gap_age` has no effect on this predicate, so its grid fixes
  it at 48 (36 combinations).
- `inversion_close_both` and `inversion_close_long`: the inversion-close
  control. It uses the candidate's first-inversion age and tie rules, enters on
  the inversion bar itself and omits the later retest. This isolates the
  retest. `retest_window` and `entry_level` have no effect, so its grid fixes
  them at 24 and 0.5 (12 combinations).
- Both controls are study-local modes of the same evaluator, routed through the
  same M1, composer and simulator stack under names that are never registered.
  They apply the candidate's data validity, required history, conflict handling
  before direction suppression, costs and close and stop owners. No production
  bypass parameter exists.
- Incumbents, tuned only on train with their native optimizer grids:
  `breakout` (long-only; it declares no short entries) and `donchian_breakout`
  (long-only and bidirectional).
- Excluded: `sweep_absorption_ifvg`, because it needs Hyperliquid trade-flow
  data that the frozen candle sets do not contain.

### Selection, evidence and verdict

- Selection on train: eligible combinations score all three datasets with
  complete candle and funding coverage, no liquidated leg and at least 10
  independent position clusters; pick the highest mean net Sharpe, then the
  highest mean drawdown-adjusted return, then the earliest grid order.
- Independent position clusters: every position interval [entry, exit] of an
  arm and window, merged across datasets when intervals overlap or touch.
- Parity gates: the selected candidate and both control signals equal the
  independent sequential reference (`reference.py`) on every dataset frame
  (train and test, warm-up included), and the selected `candidate_both`
  decisions match the 200-bar live window on every held-out dataset frame
  (`parity_diff`, strict).
- Inconclusive: incomplete coverage, a matched arm with no eligible selection,
  fewer than 30 independent held-out clusters for `candidate_both`, or a failed
  parity gate.
- Pass requires all of: `candidate_both` mean held-out net return above 0 at
  cost x1 and x2; in each matched contract (long-only and bidirectional) the
  candidate's mean held-out net return above each control and each incumbent;
  in each matched contract the candidate's worst quarterly drawdown no worse
  than each control and each incumbent.
- Fail: adequate evidence and any pass requirement false. A pass would keep
  `edge_status=no_edge` and `edge_source=unvalidated` pending a separate
  promotion decision.

### Result

FAIL with adequate evidence (52 independent held-out clusters, floor 30; every
parity gate held with 0 mismatches). `candidate_both` lost 4.68% mean held-out
net return at cost x1 and 5.52% at x2. The inversion-close control
(+8.87%) and the non-inverted retest control (-0.38%) both did better
bidirectionally; long-only, the candidate (-4.27%) lost to `breakout`
(+3.40%) and the inversion-close control (-1.21%). See `REPORT.md`.

### Discovery evidence

`discovery_{before,after}_{spot,futures}.json` are the ordinary `--list-json`
captures before and after registration; they are byte-identical because
no-edge names stay hidden. `evidence/catalog_{before,after}_{spot,futures}.json`
are full `shared_tools/strategy_catalog.py --list-json --platform` captures; the
only difference is the new `inversion_fvg` open entry on each platform.
`backtest/tests/test_strategy_backtest_contract.py` checks both.

### Limits

Three coins, one interval and one year of held-out data that earlier studies
also scored. Results are research evidence only; they never write live
defaults, configs or pull requests.
