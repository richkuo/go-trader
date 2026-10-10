# On Balance Volume divergence (#1657)

Research candidate `on_balance_volume_divergence` (futures registry, `short_entries=True`,
`edge_status="no_edge"`), tracking group `hyperliquid-strategy-candidates-2026-10-02`.
The evaluator is `shared_strategies/open/on_balance_volume.py`; the registry wrapper is in
`shared_strategies/open/registry.py`. The result is in `REPORT.md` (generated) and
`results.json`. Nothing in this study activates trading, writes an acknowledgement or changes a
deployed configuration.

## Signal contract

Rows are indexed by position `j` in the supplied frame.

- **Signed volume.** For `j >= 1`, `sv[j] = +volume[j]` when `close[j] > close[j-1]`,
  `-volume[j]` when it is lower and `0` when it is equal. On Balance Volume (OBV) is the running
  sum of `sv`.
- **Pivots.** Row `k` is a low pivot when `low[k]` is strictly lower than every low in the
  `left_span` rows before it and the `right_span` rows after it; a high pivot is the strict
  mirror on `high`. Equal values never form a pivot. A pivot is usable only at its
  confirmation row `c = k + right_span`, after its right-side bars have closed.
- **Pairing.** For each newly confirmed low `q`, the partner is the nearest earlier low pivot
  `p` with `min_separation <= q - p <= max_separation` (both bounds inclusive). No partner gives
  `no_pivot_pair`.
- **Divergence measure.** Over the rows `p+1 .. q`, `S = sum(sv)` and `A = sum(|sv|)`, both
  over the same interval and computed directly from those increments (each term is scaled by the
  interval's largest `|sv|` before an exact `math.fsum`, so very large volumes cannot overflow).
  `D = S / A` is unitless and lies in `[-1, 1]`. An equal-close row adds 0 to both sums.
  `A = 0` gives `zero_volume_span`. `D` never reads a cumulative OBV value, so moving the OBV
  origin cannot change a decision.
- **Arming.** Long needs a strictly lower low (`low[q] < low[p]`) and `D > volume_threshold`;
  short needs a strictly higher high and `D < -volume_threshold`. Equal pivot prices give
  `no_price_divergence`; `S = 0` gives `D = 0`, which never passes the strict test
  (`volume_not_confirmed`).
- **Reclaim level.** Long: the highest high over rows `q .. c` inclusive. Short: the lowest low
  over the same rows.
- **Lifetime.** Every newly confirmed pivot of a kind first clears that side's previous setup,
  then may arm a replacement; a failed pair leaves the side unarmed. Confirmations are processed
  before reclaim checks, so a setup cannot fire on its own confirmation row. The first eligible
  row is `c + 1`, the last is `c + setup_expiry` (inclusive). The setup emits once, on the first
  eligible row whose close is strictly above the long reclaim level (strictly below for short),
  and is then consumed. The first reclaim consumes the setup even when the composer cannot open a
  position (for example, one is already open).
- **Simultaneous events.** If both sides would emit on one row, the row holds with
  `opposite_events` and both setups are consumed. This rule is implemented, but the contract makes
  it unreachable: for both to fire on row `t`, every row between the two confirmations would
  have to close at or below the long reclaim and keep its low at or above the short reclaim, which
  forces both confirmations onto one row and both pivots onto one bar, where the long reclaim
  (at least that bar's high) is above the short reclaim (at most its low). No fixture can reach
  it.
- **Ablation.** `volume_test=false` skips only the sign and threshold test on `D`. Price
  divergence, reclaim, lifetime, data validation and the `A = 0` hold stay on.

## Finite support

`H = left_span + max_separation + right_span + setup_expiry + 1` rows. The decision for row `t`
reads exactly rows `t-H+1 .. t`:

- a setup that can fire at `t` was confirmed at `c >= t - setup_expiry`, so its first pivot is
  at `p >= t - setup_expiry - right_span - max_separation` and that pivot's left window starts at
  or after `t - H + 1`;
- any pivot that can clear that setup, or be its partner, lies inside the same rows;
- consumption reads only closes between `c` and `t`.

The evaluator computes pivots, pairs and the first reclaim once over the frame, then publishes a
decision only where all `H` rows are present and valid; fewer than `H` rows gives
`insufficient_history`. By the three points above this equals replaying the state machine on
exactly the trailing `H` rows. A scratch row-by-row replay written during development (not committed)
agreed on all 53,757 rows it compared across 400 random parameter sets and frames.
It never reads its own earlier output. The bounds below give `H <= 171`, inside the scheduler's
200-candle Hyperliquid limit (`scheduler/hl_batch.go`, unchanged). The defaults give `H = 59`.

## Invalid input

Required columns: `open`, `high`, `low`, `close`, `volume`, and a `DatetimeIndex` or a
`timestamp` column. A row is valid when every price is finite and positive, `low <= open, close
<= high`, volume is finite and `>= 0`, and its timestamp is finite. A support window is valid
when every row in it is valid and its `H - 1` timestamp steps are positive and equal. Any defect
inside the support holds that row with a reason and zero signal; the frame keeps its index, row
count and market columns, and the evaluator never raises for market data (parameter errors raise
at validation). Decisions recover once the defect leaves the support. The composer evaluates
entry before exits, so a hold (never a throw) keeps explicit close and stop management running.

| Reason | Code | Meaning |
|---|---|---|
| `entry_long` / `entry_short` | 1 / -1 | the setup fired on this row |
| `no_setup` | 0 | no live setup |
| `setup_armed` | 2 | a setup was armed on this confirmation row |
| `awaiting_reclaim` | 3 | an armed setup is waiting for its reclaim |
| `opposite_events` | 4 | both sides fired (unreachable, see above) |
| `no_pivot_pair` / `no_price_divergence` / `volume_not_confirmed` / `zero_volume_span` | 5 / 6 / 7 / 8 | the pivot confirmed on this row failed |
| `insufficient_history` | 10 | fewer than `H` rows |
| `nonfinite_input` / `invalid_price` / `inconsistent_ohlc` | 11 / 12 / 13 | bad price row inside the support |
| `timestamp_order` / `cadence_gap` | 14 / 15 | missing, duplicate or reversed timestamp; unequal steps |
| `invalid_volume` | 16 | missing, negative or non-finite volume |
| `missing_columns` / `missing_timestamps` | 17 / 18 | whole-frame hold |

`obvd_reason` carries the text; `obvd_reason_code` carries the number, because the Hyperliquid
check script exports only finite numeric columns as indicators.

### Explanation columns

Per side (`obvd_long_*`, `obvd_short_*`), for the latest confirmation within `setup_expiry` rows:
`state` (0 none, 2 armed this row, 3 awaiting, 1 fired this row, 9 consumed, 5-8 the pair
failure code, 19 pair interval not observable), `pivot1_ts_ms`, `pivot1_price`, `pivot2_ts_ms`,
`pivot2_price`, `obv1`, `obv2`, `signed_sum`, `abs_sum`, `divergence`, `reclaim`,
`confirm_ts_ms`, `expiry_ts_ms`. Shared: `obv`, `obvd_support_bars`,
`obvd_support_start_ts_ms`, `obvd_eval_ts_ms`, `obvd_volume_test`, `obvd_valid`.

`obv`, `obv1` and `obv2` are diagnostics only. Their origin is the first row of each run of valid
rows in the supplied frame (it restarts at 0 after an invalid row), so a frame that starts earlier
shifts them by a constant; `obv2 - obv1` equals `S`. Every other field is identical across
full-series, prefix and 200-row evaluations.

## Parameters

| Parameter | Type | Bounds | Default |
|---|---|---|---|
| `left_span`, `right_span` | integer, not boolean | 1-10 | 3, 3 |
| `min_separation`, `max_separation` | integer, not boolean | 1-100, `min <= max` | 5, 40 |
| `volume_threshold` | finite number, not boolean | 0-1 | 0.10 |
| `setup_expiry` | integer, not boolean | 1-50 | 12 |
| `volume_test` | boolean | | true |

The registry constraints repeat the scalar and pairwise bounds; the evaluator also enforces types,
finiteness and `H <= 200`. These are research inputs with no trading endorsement.

Optimizer grid (`backtest/optimizer.py`, 972 combinations): spans `[2, 3, 5]` each, minimum
separation `[3, 5, 8]`, maximum separation `[20, 40, 60]`, threshold `[0, 0.05, 0.10, 0.20]`,
expiry `[6, 12, 24]`, `volume_test` `[true]`. Synthetic fixture seed: 1657.

## Availability (unchanged admission)

- Python: `edge_status="no_edge"`, `edge_source` from the verdict (`study_fail` for fail,
  `study_inconclusive` for inconclusive; a pass keeps `unvalidated`, because the supported source
  set has no approved-study state), `edge_ref` this study's `REPORT.md`. This study's frozen outcome
  is inconclusive, so both languages record `study_inconclusive`. Go mirrors the same pair
  in `noEdgeStrategies` (`scheduler/edge_status.go`).
- Hidden from discovery: the spot and futures `--list-json` output is byte-identical before and
  after (`discovery_before_*.json`, `discovery_after_*.json`).
- Explicit `--mode=paper` runs it; live or missing mode needs `--allow-no-edge`
  (`allow_no_edge: true`), for an open reference and for a close-fallback reference alike.
- Go metadata: short name `obvdiv`, `registeredOpenStrategyPlatforms` `{"futures"}`,
  `bidirectionalPerpsStrategies` true. The name is in no starter or default strategy list, and
  both compatibility shims are unchanged. The short name lets the Discord and status-page add
  paths build a configuration; the existing admission gate and warnings still apply to it.

## Close, stop and bar contract

- Every arm: one close owner `tiered_tp_pct` with one tier (profit 0.02, cumulative close fraction
  1.0), one stop owner (fixed entry ATR x1.0), comparison mode `strict` (the close is live
  supported, so there is no approximation), `ohlc_walk`, capital $1000. No reversal or scale-in:
  the composer blocks new entries while a position is open. No regime gate is used, so no
  higher-timeframe series is involved.
- A bar N entry or take-profit decision fills at the bar N+1 open; the protective stop keeps the
  engine's intrabar timing.
- The scheduler's feed frame can include a forming bar. With `closed_bar_decisions` enabled the
  existing contract drops it, so a forming confirmation bar cannot arm a setup and a forming
  reclaim bar cannot enter; an already closed final bar is kept; protective closes still read the
  current mark (`shared_scripts/test_closed_bar_decisions.py`). With the option disabled the
  evaluator reads the forming row as if closed: a confirmation or reclaim can act on a partial
  bar, and those decisions are not closed-bar evidence. Repeated checks of one closed bar repeat
  the same decision under the existing retry contract; this candidate adds no consumed-bar
  watermark.

## Data and costs

`study_manifest.json` and `data/` are byte-identical copies (same sha256) of the frozen
Hyperliquid 4h candles, volume, funding, venue metadata and spread snapshot first frozen by
`chaikin_money_flow_1649` and copied by `relative_vigor_index_1666`. This reuses a historical
panel that earlier candidates also studied. Candles and volume come from the Hyperliquid
`candleSnapshot` rows, so neither is a proxy. Windows: train [2024-09-01, 2025-09-01) for
selection, test [2025-09-01, 2026-09-01) held out, 300 unscored warm-up bars. Costs come from the
manifest execution spec: taker 0.045%, maker 0.015%, per-coin half spread plus 5 bps slippage per
side (both scaled by the cost multiplier), `szDecimals` lot rounding and the $10 venue minimum.
Funding is the manifest's frozen hourly attachment, charged while positions are open; the report
shows coverage per dataset. No legacy (`--funding`) leg is used; a legacy run would load Binance
US spot candles and volume as proxies with a flat spread/slippage and no venue lot rules, and
could not support a Hyperliquid pass.

## Frozen protocol

Frozen before held-out scoring, protocol sha256
`2c7f72720a6664a05dba04ac0f7187c702eb2c7db1d42153ac9c2fb5925be1e4`
(`run_study.py --protocol-hash`; it covers both seed files, the grid, every rule below, the cost
multipliers and the parity runs). A plumbing dry run of the driver (two grid points, truncated
parity frames, output kept outside the repository) ran before the final run; no protocol element
changed after it.

- **Arms.** Candidate both sides (primary), candidate long only (primary), price-pivot ablation
  both sides and long only (selected parameters with `volume_test=false`, not a registered
  strategy), and unchanged `volume_weighted` defaults long only (`baseline_seed.json`; it
  declares `short_entries=False`, so it appears only in the long-only comparison). Data, costs,
  close and stop settings are identical within each comparison.
- **Selection (train only).** Candidate, both sides, cost x1. Eligible: every dataset scored,
  complete candle and funding coverage, no incomplete-funding leg, no liquidated leg, at least 10
  positions. Rank by mean Sharpe, then mean drawdown-adjusted return, then earliest grid order.
  No eligible combination is inconclusive.
- **Held-out sample.** Independent position clusters: every position across BTC, ETH and SOL is
  merged with any position whose holding span overlaps it (any side), and each merged group counts
  once. Raw position counts are reported separately. Floor: 30 clusters for each primary arm.
- **Verdict.** Inconclusive when evidence is incomplete (coverage, incomplete funding, proxy-only
  data), selection had no eligible combination, or a primary arm is under the floor. Otherwise
  pass needs all of: both primary arms net positive at cost x1 and x2; candidate long above
  ablation long and baseline long on mean net return, with a worst drawdown no deeper than each;
  candidate both above ablation both on mean net return. Anything else is fail. Held-out
  diagnostics never change selection or verdict rules. The driver re-applies the verdict rule to
  altered copies of its own result (incomplete funding, incomplete coverage, proxy-only, below the
  floor, no selection) and stops if any of them could pass.

## Frozen-input parity

`compute_parity_frame` runs directly on each hashed held-out frame (with warm-up) for the selected
parameters: non-batched with `window=None` (every historical prefix) and `window=200`, both with
the study close and direction, and batched (`batched=True`, explicit paper mode, solo versus
batch) with `window=200`. Equivalent command-line checks would be `parity_diff.py --window 0` and
`--window 200`, but that command loads cache data, so the study records the frozen frames' input
hashes instead. Development integration tests also cover synthetic frames, every prefix, every
200-row tail, accumulator-origin shifts and future-bar edits
(`backtest/tests/test_on_balance_volume_backtest.py`).

## Reproduce

```bash
uv run --no-sync python backtest/candidates/on_balance_volume_divergence_1657/run_study.py
uv run --no-sync python backtest/candidates/on_balance_volume_divergence_1657/run_study.py --render-only
uv run --no-sync python backtest/candidates/on_balance_volume_divergence_1657/run_study.py --protocol-hash
```

Single arms through the existing M1 manifest path (`held_out_candidate.json` and
`held_out_ablation.json` are written by the full run from the frozen selection):

```bash
M=backtest/candidates/on_balance_volume_divergence_1657
uv run --no-sync python backtest/eval_windows.py --registry futures --manifest $M/study_manifest.json --windows test --candidate-json $M/held_out_candidate.json
uv run --no-sync python backtest/eval_windows.py --registry futures --manifest $M/study_manifest.json --windows test --candidate-json $M/held_out_candidate.json --direction long
uv run --no-sync python backtest/eval_windows.py --registry futures --manifest $M/study_manifest.json --windows test --candidate-json $M/held_out_ablation.json
uv run --no-sync python backtest/eval_windows.py --registry futures --manifest $M/study_manifest.json --windows test --candidate-json $M/held_out_ablation.json --direction long
uv run --no-sync python backtest/eval_windows.py --registry futures --manifest $M/study_manifest.json --windows test --candidate-json $M/baseline_seed.json
uv run --no-sync python backtest/eval_windows.py --registry futures --manifest $M/study_manifest.json --windows test --cost-multiplier 2 --candidate-json $M/held_out_candidate.json
```

Discovery snapshots:

```bash
uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json > discovery_after_spot.json
uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json > discovery_after_futures.json
```
