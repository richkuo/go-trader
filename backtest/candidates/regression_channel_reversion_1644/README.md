# Regression-channel reversion candidate (#1644)

Research study for the `regression_channel_reversion` entry evaluator on
Hyperliquid perpetuals. Tracking group:
`hyperliquid-strategy-candidates-2026-10-02`. The study is suggest-only: it
writes no live configuration, no live default and no order. The registry
keeps `edge_status="no_edge"` in every outcome. Explicit paper mode runs the
strategy; live or missing mode needs `allow_no_edge: true` (the repository's
acknowledged-live exception). Promotion is a separate reviewed action.
`REPORT.md` holds the result; this file holds the frozen specification.

## Signal contract

Implementation: `shared_strategies/open/regression_channel_reversion.py`,
registered in `shared_strategies/open/registry.py` (futures only,
`short_entries=True`).

- Fit at decision row `N`: the `W = fit_window` closes of rows `N-W .. N-1`
  (strictly before `N`), local coordinates `0 .. W-1`. Centered least squares:
  `mean` = window mean, `slope = sum((x - xbar) * (y - mean)) / sum((x - xbar)^2)`.
  `intercept` is the fitted value at coordinate 0. Residual deviation is the
  population value (divisor `W`). The forecast for row `N` is the line at
  coordinate `W`. Bands: `forecast +/- band_multiplier * deviation`. The fit is
  frozen before row `N` is scored; the previous row uses its own fit over
  rows `N-1-W .. N-2`, so no historical reading is refitted with later input.
- Normalized slope: `abs(slope) * (W - 1) / mean` (absolute fitted price change
  across the fit window divided by the window mean; dimensionless).
- Long at row `N`: previous close `<=` previous lower band, current close `>`
  current lower band and `<=` current upper band. Short mirrors it: previous
  close `>=` previous upper band, current close `<` current upper band and `>=`
  current lower band. An entry also needs the current normalized slope `<=`
  `slope_ceiling` and the current residual deviation `>=` `min_residual_std`.
- Dependency span: `W + 2` rows (both fits plus the decision row) and the `W + 1`
  cadence steps between them. Only that span is validated, so a defect stops
  affecting decisions once it leaves the span.
- Degenerate fit: a residual deviation at or below `1e-10 * mean` (constant
  windows, exact lines and every `W = 2` fit, which passes through both points)
  holds. Roundoff on an exact line cannot create a band.
- Cadence: the evaluator reads the frame's declared bar interval and timestamp
  kind (`bar_interval_ms` / `bar_timestamp_kind` attributes from
  `shared_tools/closed_bar.py`, set by the check scripts, the data fetcher and
  the offline manifest). A frame without a declared interval holds. Every step
  inside the span must equal the interval.
- Parameters: `fit_window` is an integer from 2 through 197; `band_multiplier`
  is finite and greater than 0; `slope_ceiling` is finite and at least 0;
  `min_residual_std` is finite and at least 0, in quote-price units (so the same
  value is a much weaker floor for BTC than for SOL). Booleans, strings, nulls,
  fractional windows and out-of-range values are refused before any indicator
  work by the registry validators (`validate_params`, `validate_param_value`, the
  evaluator), by `check_hyperliquid.py` before the solo market fetch and before
  each batch slot, and by the Go config loader for the effective structured open
  reference and any open-strategy close fallback.
- Holds give `signal = 0` with a reason and a numeric code:

  | Reason | Code |
  |---|---|
  | `entry_long` | 1 |
  | `entry_short` | -1 |
  | `no_reentry` | 0 |
  | `slope_gate` (re-entry with normalized slope above the ceiling) | 2 |
  | `deviation_gate` (re-entry with deviation below the floor) | 3 |
  | `insufficient_history` | 10 |
  | `missing_cadence` | 11 |
  | `cadence_gap` | 12 |
  | `timestamp_order` (repeated or reversed timestamps) | 13 |
  | `missing_timestamps` | 14 |
  | `nonfinite_input` (missing or non-finite close) | 15 |
  | `nonpositive_price` | 16 |
  | `degenerate_fit` | 17 |
  | `nonfinite_derived` | 18 |
  | `missing_columns` | 19 |

- Diagnostic columns (prefix `rcr_`): row and evaluated timestamps, interval,
  current and previous close, current and previous slope, intercept, fit mean,
  residual deviation, normalized slope, forecast and bands, the four parameters,
  `rcr_valid`, `rcr_reason`, `rcr_reason_code`. `check_hyperliquid.py` keeps the
  `rcr_` numbers at full precision and passes `rcr_reason` as a text label.
- Ownership: the evaluator reads only the supplied frame and keeps
  evaluation-local arrays. It fetches nothing and writes no book, cache or state.
  The scheduler keeps position, stop, fill and storage ownership. A data hold
  still lets the explicit close and the stop owner run.
- Decision timing: the evaluator scores the last row of the frame it receives.
  With `closed_bar_decisions` enabled, the shared selector removes the forming
  candle first. With it disabled (the default), the last row can be the forming
  candle; `shared_scripts/test_closed_bar_decisions.py` shows both paths.
- Input bound: the largest fit window, 197, needs 199 rows. The default check
  input is 200 rows; with one forming candle removed, 199 closed rows remain.
  Feed, single check and batch check keep that bound, and no runtime argument or
  probe change is needed.

## Study contract (frozen before held-out scoring)

`run_study.py --protocol-hash` prints the protocol digest from the code and
`candidate_seed.json`. The digest recorded before the scoring run was
`e3aab415aa0d6a432ff215f2128ae2e15af25cf3880f53fb8113645100892b88`;
`REPORT.md` records the digest of the run.

- Every arm (candidate, `mean_reversion_pro` defaults, `atr_band_revert`
  defaults) and every M1 incumbent uses the same contract: direction `both`, one
  `tiered_tp_pct` close with a single 3% profit tier closing the full position,
  one fixed entry ATR x1.0 stop, `strict` close comparison mode, capital $1,000.
  The incumbents receive the contract through
  `eval_windows.compute_incumbent_legs(contract=...)` /
  `evaluate_window(incumbent_contract=...)`.
- Research seed (registry defaults): `fit_window` 50, `band_multiplier` 2,
  `slope_ceiling` 0.10, `min_residual_std` 0. These are research settings with
  no trading approval.
- Grid (`backtest/optimizer.py`, 420 sets): `fit_window` 20, 35, 50, 75, 100,
  150, 197; `band_multiplier` 1.5, 2, 2.5, 3; `slope_ceiling` 0, 0.025, 0.05,
  0.10, 0.20; `min_residual_std` 0, 0.1, 1.
- Selection: candidate only on the selection window; eligible = every dataset
  scored, no incomplete funded leg, no liquidated leg, venue coverage complete,
  at least 10 opening events (positions); rank by mean Sharpe, then mean
  drawdown-adjusted return, then the earliest grid order. No eligible set is
  inconclusive.
- Sample floor: at least 30 independent held-out position clusters
  (overlapping positions across datasets form one cluster).
- Proxy study: the six default M1 Binance US datasets (BTC, ETH, SOL at 1h and
  4h), Hyperliquid fees, Hyperliquid hourly funding charged on every leg.
  Selection on `is` [2025-06-10, 2026-01-01). Held out: `oos` [2026-01-01,
  2026-10-01) (finite end frozen), `2023`, `2024`, `2025H1`. `is` overlaps
  `2025H1` for 21 days, so that span is not independent evidence. Pass needs a
  positive `oos` mean net return, a contract-aware M1 pass in `oos` and in at
  least two of `2023`, `2024`, `2025H1`, and an `oos` mean Sharpe above both
  baselines. Incomplete funding or a missing dataset in any held-out window, no
  eligible selection or a missed sample floor is inconclusive; a complete study
  that misses an economic rule fails. Proxy candles are a price proxy and never
  measured Hyperliquid profitability.
- Venue study: `backtest/candidates/regression_channel_reversion_1644_manifest.json`
  references the hash-pinned #1649 Hyperliquid 4h candles, funding and venue
  references in `backtest/candidates/chaikin_money_flow_1649/data/` in place
  (no copies). Selection on `train` [2024-09-01, 2025-09-01); held out `test`
  [2025-09-01, 2026-09-01), scored at cost x1 and with spread and slippage x2.
  Every leg uses the manifest execution specification (maker 0.015% and taker
  0.045% fees, per-coin half spread, 5 bps slippage, lot rounding to szDecimals,
  $10 minimum order with a 3% margin, hourly funding). Pass needs a positive
  mean net return at x1 and x2, a contract-aware M1 pass at x1, and a mean
  Sharpe above both baselines at x1. Incomplete candle or funding coverage,
  non-venue provenance, no eligible selection or a missed sample floor is
  inconclusive. Funding is never filled with zero and charging is never turned
  off to reach a pass.
- Overall: pass only if both studies pass; fail if either fails; otherwise
  inconclusive. Registry evidence source by outcome: pass `unvalidated`
  (awaiting a separate approval), fail `study_fail`, inconclusive
  `study_inconclusive`; the same value is set in Python and Go.
- Parity: composed decision parity (`parity_diff.compute_parity_frame`) on the
  frozen venue frames for every prefix, the 200-row and 199-row bounded inputs
  and the batched explicit-paper dimension; a grid decision parity sweep over
  all 420 sets plus three boundary sets; and `parity_diff.py` CLI runs on the
  isolated proxy cache with network fetches blocked.

## Frozen inputs and reproduction

- Proxy cache: an isolated SQLite file outside the repository. It was seeded
  from the #1643 proxy cache; `proxy_freeze.json` records every frame and
  funding digest and `identical_to_aroon_trend_1643_freeze: true` (the same
  Binance US candles and Hyperliquid funding as
  `backtest/candidates/aroon_trend_1643/proxy_freeze.json`). A run refuses any
  digest that differs and blocks network fetches while scoring.
- Venue inputs: verified by sha256 on every manifest load.

```bash
uv run --no-sync python backtest/candidates/regression_channel_reversion_1644/run_study.py --protocol-hash
uv run --no-sync python backtest/candidates/regression_channel_reversion_1644/run_study.py --cache-db /path/to/rcr_1644_proxy.db --freeze-proxy
uv run --no-sync python backtest/candidates/regression_channel_reversion_1644/run_study.py --cache-db /path/to/rcr_1644_proxy.db
uv run --no-sync python backtest/candidates/regression_channel_reversion_1644/run_study.py --render-only
```

`--freeze-proxy` fetches only what the cache lacks (Binance US candles since
2022-12-01, Hyperliquid funding through 2026-10-01). A full run writes
`results.json`, `REPORT.md` and the held-out candidate specifications
`held_out_candidate_proxy.json` and `held_out_candidate_venue.json`.

Discovery output: `discovery_before_{spot,futures}.json` were captured before
registration and `discovery_after_{spot,futures}.json` after; they are
byte-identical, so default discovery is unchanged.
