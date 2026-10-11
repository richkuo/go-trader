# Ease of Movement breakout confirmation (#1671)

Candidate `ease_of_movement_breakout` (futures registry only,
`short_entries=True`, `edge_status="no_edge"`, `edge_source="study_fail"` after
the frozen study below; it was `unvalidated` until the report existed). Discovery hiding comes from the
no-edge label. Under the current no-edge policy an explicit `--mode=paper`
check may evaluate it; live or missing mode needs `allow_no_edge: true`.
Nothing in this directory writes a live default, a config or a deployment file.

This file is the frozen specification. The signal contract, the research
settings, the selection rule, the evidence rule and the verdict rule were fixed
before any window was scored. The result is in [`REPORT.md`](REPORT.md),
rendered from [`results.json`](results.json) by [`run_study.py`](run_study.py).

## Signal contract

Inputs: one candle frame with `high`, `low`, `close` and `volume` (base-asset
units), a timestamp per row (datetime index, else a `timestamp` column in
milliseconds), and the frame's declared bar cadence. The evaluator fetches
nothing, writes no position, trade or stop state, and owns only
evaluation-local arrays. It returns a copy of the frame.

- Midpoint: `(high + low) / 2`.
- One-bar Ease of Movement at bar i: `(midpoint[i] - midpoint[i-1]) * (high[i] - low[i]) / volume[i]`.
  The scale factor is 1; only the sign of the average is consumed.
- Smoothed value at bar N: the simple mean of the one-bar values for bars
  N-P+1..N (P = `eom_period`). Each mean is that window's own sum divided by P,
  so the full series, every prefix and any bounded window of at least the
  warm-up length give the same value.
- Prior levels at bar N: the highest high and lowest low of bars N-B..N-1
  (B = `breakout_window`). The evaluated bar is excluded.
- Setup at N: +1 when `close[N]` is strictly above the prior high, -1 when it is
  strictly below the prior low, else 0. Equality is not a breakout.
- Long entry at N: setup +1 at N, setup not +1 at N-1, smoothed value strictly
  above zero. Short entry mirrors with setup -1 and a strictly negative value.
- A setup that continues from N-1 never enters, even when the indicator turns
  to agree later (no delayed confirmation). A new setup with a zero smoothed
  value holds (`zero_indicator`); a new setup with the opposite sign holds
  (`indicator_disagrees`).
- Warm-up: a decision at N reads P + 1 bars for the smoothed value and B + 2
  bars for the current and previous setup, so it needs `max(P + 1, B + 2)` bars.

Parameters: `eom_period` and `breakout_window` are integers in [2, 100];
booleans, fractions and non-finite values raise `ValueError`. The largest
warm-up is 102 bars, inside the 200-bar check window. The optimizer grid
(`eom_period` 7/14/21/28, `breakout_window` 10/20/40/60) needs at most 62.

### Cadence and timestamp contract

- The cadence comes from the frame, never from the data: the producer stamps
  `df.attrs["bar_interval_ms"]` (the fixed duration of the declared timeframe,
  `shared_tools/closed_bar.py` `FIXED_DURATION_MS`) and, when it is proven,
  `df.attrs["bar_timestamp_kind"]` (`open` or `native_close_inclusive`).
- Stamping owners: `shared_tools/data_fetcher.py` (cached and fetched frames,
  `open`), `backtest/offline_manifest.py` `window_frame` (`open`), and
  `shared_scripts/check_hyperliquid.py` (the check timeframe for every frame; the
  timestamp kind only on the closed-bar decision frame, from the selector's
  per-row timing evidence, so a legacy slot reports the same diagnostics solo
  and inside a batch that also carries a closed-bar slot). A prebuilt frame
  passed to the check keeps its own declaration.
- No declared cadence: every row holds with `missing_cadence`. The evaluator
  never estimates a frequency from the timestamps. Check scripts other than the
  Hyperliquid one (for example an OKX swap check) do not stamp cadence, so the
  candidate holds there.
- Every adjacent pair of rows inside a decision's dependency window must step by
  exactly the declared interval. A smaller or larger step is `cadence_gap`; a
  step of zero or less is `timestamp_order`; a missing timestamp is
  `missing_timestamps`. Only steps inside the window count, so a defect before
  the window never extends a hold.
- `eom_eval_ts_ms` is the evaluated bar's opening time: the row timestamp for
  `open`, the row timestamp minus the interval plus 1 ms for
  `native_close_inclusive`, and null when the kind is not proven. It is a
  diagnostic only; no decision reads it.

### Input validity (local to each dependency window)

| Defect | Effect | Reason |
|---|---|---|
| Missing `high`, `low`, `close` or `volume` column | every row holds | `missing_columns:<names>` |
| Non-finite price | the row invalidates every one-bar value and setup that reads it | `nonfinite_input` |
| `high < low`, or `close` outside [low, high] | same | `inconsistent_range` |
| Volume zero or negative | invalidates that bar's one-bar value and every smoothing window that contains it; setups are unaffected | `invalid_volume` |
| Non-finite volume | same as zero volume | `nonfinite_input` |
| Overflow in the one-bar value or the mean | that value is invalid | `nonfinite_derived` |
| Zero range with positive volume | contributes exactly 0 | (valid) |
| Fewer bars than the warm-up | hold | `insufficient_history` |

Dependency windows for a decision at N: price rows N-need+1..N, volume rows
N-P+1..N, timestamp steps inside those rows. The reported reason is the most
recent defect inside them. A hold recovers on the first bar whose full
dependency window is valid again. No row is repaired, removed or forward-filled.

### Explanation columns

`eom_eval_ts_ms`, `eom_interval_ms`, `eom_mid_change`, `eom_raw`,
`eom_smoothed`, `eom_prior_high`, `eom_prior_low`, `eom_setup`, `eom_valid`,
`eom_reason`, `eom_reason_code` and `signal`. Unavailable values are NaN in the
frame and absent (never a fake number) in the check output. The Hyperliquid
check emits `eom_*` numbers at full precision and the text reason under
`indicator_labels.eom_reason`.

Reason codes: `entry_long` 1, `entry_short` -1, `no_breakout` 0,
`indicator_disagrees` 2, `zero_indicator` 3, `breakout_continuation` 4,
`insufficient_history` 10, `missing_cadence` 11, `cadence_gap` 12,
`timestamp_order` 13, `missing_timestamps` 14, `nonfinite_input` 15,
`invalid_volume` 16, `inconsistent_range` 17, `nonfinite_derived` 18,
`missing_columns` 19.

### Closed-bar contract

Research decisions use completed candles and fill no earlier than the next bar
open (the engine shifts every signal one bar). In a Hyperliquid check, only
`closed_bar_decisions: true` (the existing selector in
`shared_scripts/check_hyperliquid.py`) decides on the last closed bar while the
protection frame keeps the current mark. Verification configurations enable it
explicitly. A legacy check without it can consume the forming candle, so its
output is not proof of this contract. An invalid entry window returns a
reasoned entry hold; the close evaluators and protection still run.

## Close, stop and exposure

The component is entry-only. Every configuration in this study and every
verification configuration uses exactly one explicit close owner,
`tiered_tp_atr` with the shipped ladder (1.5 ATR closes 40%, 3.0 ATR 80%,
5.0 ATR 100%), and one stop owner, the scalar entry-ATR stop with multiplier
1.0 (the live `DefaultStopLossATRMult`). The composer blocks new entries while a
position is open, so there is no reversal and no scale-in. The Discord and
status-page add paths write the same explicit `tiered_tp_atr` reference; the
stop comes from the loader's existing default.

## Study design

Data path: the default M1 cached path (`backtest/eval_windows.py` `run_leg`
without `--manifest`). Candles and base volume are Binance US spot
`BTC/USDT`, `ETH/USDT` and `SOL/USDT` at 1h and 4h, used as proxies for
Hyperliquid perpetual prices and volume. Funding is Hyperliquid hourly funding
charged through the shared helper (`--funding charge`). `run_study.py freeze`
copies the exact rows into `data/` (gzip CSV, sha256 in
[`study_manifest.json`](study_manifest.json)); `run_study.py run` rebuilds a
private SQLite cache from those files, verifies every hash first, and replays
with network fetches disabled (a missing row is a recorded gap, never a
download).

Windows (M1 names, finite ends):

| Window | Range | Role |
|---|---|---|
| `is` | [2025-06-10, 2026-01-01) | parameter selection only |
| `2024` | [2024-01-01, 2025-01-01) | scored |
| `oos` | [2026-01-01, 2026-08-01) | scored; the declared cutoff replaces M1's open end |
| `2023` | [2023-01-01, 2024-01-01) | reported only; the Hyperliquid funding history starts 2023-05-12, so held hours before it cannot be priced |
| `2025H1` | [2025-01-01, 2025-07-01) | reported only; it overlaps `is` by 21 days |

Every leg starts cold at the window start (M1 behavior), so the first
`max(P + 1, B + 2)` bars of each leg cannot enter for any arm that needs
history.

Common settings for every arm: direction `both`, the close and stop above,
$1,000 capital, Hyperliquid taker fee 0.045% per fill, the engine's default
slippage of 0.05% per fill, `intrabar_resolution` `ohlc_walk`, no regime gate,
funding charge.

Arms (each a separate M1 candidate with the same windows and costs):

- `ease_of_movement_breakout` at the selected parameters and at the seed
  (P = 14, B = 20), plus every grid variant for parameter stability.
- `donchian_breakout` with `entry_period = B`: the unconfirmed strict prior-bar
  breakout baseline (same transition rule, no indicator, no validity checks).
- `chaikin_money_flow_breakout` with `breakout_window = B` and its registry
  defaults otherwise.
- `volume_weighted` with registry defaults.

The M1 long-only incumbent bar is not used; every comparison is between the
matched arms above.

## Selection rule (`is` only)

Eligible grid points: all six datasets scored, no liquidated leg, no
funding-incomplete leg, and at least 20 positions across datasets. Pick the
highest mean Sharpe, then the highest mean drawdown-adjusted return, then the
earliest grid order. With no eligible point the seed is used. The selection is
written to `results.json` before any scored window runs.

## Evidence and verdict rule (scored windows `2024`, `oos`)

Independent positions: one per opening event (entry time and side), so the
partial closes of one position count once.

1. Inconclusive when any scored leg of any arm has no data or is
   funding-incomplete (an unpriced held hour), whatever the runner's exit code.
2. Inconclusive when the selected candidate has fewer than 30 independent
   positions across the 12 scored legs.
3. Fail when the selected candidate's mean net return is not above zero at the
   x1 cost, or not above zero at the x2 slippage stress (0.10% per fill).
4. Fail when its mean Sharpe or its mean drawdown-adjusted return is not above
   each comparator's (`donchian_breakout`, `chaikin_money_flow_breakout`,
   `volume_weighted`, all at the matched B).
5. Otherwise inconclusive: the candles and volume are spot proxies, the fixed
   slippage does not prove low-volume executability, and the lot grid and
   venue minimum are not modeled on this path, so this data cannot support a
   pass.

A fail maps to `edge_source="study_fail"`, an inconclusive result to
`study_inconclusive`. The label stays `no_edge` in every case.

Disclosure: while wiring the evaluator into M1, before this specification was
frozen, two smoke runs of the seed were viewed: BTC/USDT 4h over
[2024-01-01, 2024-03-01) (a gain) and BTC/USDT 1h over 2024 (a large loss). No
other window or arm was viewed before freezing. The selection rule reads only
`is`.

## Diagnostics reported

Per arm and window: net return, Sharpe, maximum drawdown, drawdown-adjusted
return, independent positions (long and short), trade records, fees, funding,
turnover (traded notional over capital), concentration (share of positive PnL
from the five best positions), the largest number of positions open at the
same time across the six datasets and the hours with the same coin open on both
timeframes. Per dataset and window: volume quantiles, zero-volume bars, the
candidate's invalid-input bars after warm-up, and the share of candidate entries
whose signal bar volume is below that window's 10th percentile. Slippage
sensitivity: the selected candidate at 0.05%, 0.10% and 0.20% per fill.

## Reproduce

From the repository root:

    uv run --no-sync python backtest/candidates/ease_of_movement_1671/run_study.py freeze --source-db <cache.db>
    uv run --no-sync python backtest/candidates/ease_of_movement_1671/run_study.py run

`freeze` needs network access to refill missing Hyperliquid funding hours in the
source cache (a copy is safest). It retries rate-limited refills in up to eight
passes, records each window's funding coverage, and then rewrites `data/` and
the hashes in `study_manifest.json`. `run` needs no network and rewrites `results.json` and
`REPORT.md`.
