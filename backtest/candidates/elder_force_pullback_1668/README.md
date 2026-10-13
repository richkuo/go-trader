# Elder Force Index pullback entries (#1668)

Research candidate `elder_force_pullback`. It is registered for the futures registry only, it declares
`short_entries=True`, and it carries `edge_status="no_edge"`. Profitability is not known. This study gives
retrospective evidence only. It does not change admission, live defaults or deployed configuration.

The specification below, `candidate_seed.json` and `backtest/candidates/elder_force_pullback_1668_manifest.json`
were frozen before any window was scored. `run_study.py lock` recorded their sha256 digests in
`protocol_lock.json`, and `run_study.py run` refuses to score when either digest differs. `REPORT.md`,
`results.json`, `parity_results.json`, `parity_results_batched.json` and `evidence/` are generated.

## Signal specification

Formula reference: Elder's Force Index. The external page motivates the candidate and supplies no
Hyperliquid edge evidence; the formula below is the contract.

**Raw force.** `F(t) = (close[t] - close[t-1]) * w(t)`. With `volume_weighting=1`, `w(t)` is the supplied
candle volume. With `volume_weighting=0` (the ablation), `w(t) = 1`, so the raw force is the close change.

**Fixed-length recursion.** For every timestamp `t` the smoothed force reads exactly the `L =
recursion_length` raw-force values ending at `t`:

```
alpha = 2 / (force_period + 1)
e = F(t - L + 1)
for k in t - L + 2 .. t:   e = alpha * F(k) + (1 - alpha) * e
EF(t) = e
```

Every bar restarts from its own seed, so `EF(t)` depends only on the trailing `L + 1` closes and is the same
in the full series, in every prefix and in any bounded window that contains them. The shared helper is
`fixed_length_ema` in `shared_strategies/open/indicators_core.py`. The unbounded `ema` helper is not used.
`L >= 10 x force_period` keeps the residual seed weight `(1 - alpha)^(L-1)` at or below about 2.4e-9 over
the allowed range, below one part in 10^8.

**Trend.** `SMA(t)` is the mean of the `trend_period` closes ending at `t`, summed in index order from each
endpoint's own window. Uptrend `U(t)`: `close[t] > SMA(t)` and `SMA(t) > SMA(t - slope_span)`. Downtrend
`D(t)` reverses both comparisons.

**Entries.** All comparisons are strict.

- Long on bar `t`: `EF(t-1) < 0`, `EF(t) > 0`, `U(t-1)` and `U(t)`.
- Short on bar `t`: `EF(t-1) > 0`, `EF(t) < 0`, `D(t-1)` and `D(t)`.
- Exact zero force on either candle never triggers. Constant closes give zero force and no entry.
- Both crossing candles need positive supplied volume in both variants, so eligibility is identical. An older
  zero-volume candle contributes zero raw force when weighted and the close change in the ablation.
- Volume units are the supplied units (Hyperliquid candleSnapshot `v`, base-asset units). Aggressor direction
  is never inferred from candle colour.
- Entries occur only while flat through the existing composer. Exits stay with one explicit close owner.

**Required history.** `max(L + 2, trend_period + slope_span + 1)` rows: 152 at the seeds, at most 199, so it
fits the closed portion of the default 200-row check frame. No feed, probe or window change is needed. Fewer
rows hold with `insufficient_history`; the requirement is never shortened.

**Validity.** A decision is made only when every row of its own required span has finite open, high, low,
close and volume, positive prices, `low <= open, close <= high`, volume `>= 0`, a timestamp and no
received-order defect (`received_order_defect_ms`, read even after the caller sorted the frame), every internal
timestamp edge equals the declared cadence, and `EF`, `SMA` and their lagged values are finite (overflow
holds with `nonfinite_derived`). Cadence and timestamp kind come from the `closed_bar` frame attributes; a frame
without them holds with `missing_cadence`. The first valid decision after a defect comes once the full span is
clean again. Every hold returns signal 0 with an `efp_reason`; close and protection evaluation continue.

**Parameters.** Seeds: `force_period=13`, `trend_period=50`, `slope_span=5`, `recursion_length=150`,
`volume_weighting=1`. These are research seeds, not endorsed trading settings. Integers only (booleans and
non-finite values are refused), `force_period >= 2`, `trend_period >= 2`, `slope_span >= 1`,
`volume_weighting` in {0, 1}, `recursion_length >= 10 x force_period`, `recursion_length <= 197`, required
history `<= 199` (so `force_period <= 19`). The registry runs the same validator as the evaluator.

**Live input convention.** With `closed_bar_decisions: true` (operator add writes it), the Hyperliquid slot
evaluator decides on closed candles and passes a separate protection frame. Without the flag (legacy), the last
row can be the forming candle, so its partial volume and close can change the entry decision. The study and the
parity configurations use closed decisions only.

## Protocol

`candidate_seed.json` holds the frozen protocol:

- Inputs: the hash-pinned Hyperliquid 4h BTC, ETH and SOL candles and funding referenced in place by the
  candidate-owned manifest (no copies). The same files were scored by earlier studies, including #1648, so the
  held-out window is retrospective evidence.
- Windows: selection `train` [2024-09-01, 2025-09-01), held-out `test` [2025-09-01, 2026-09-01), 300 warm-up
  bars. A coverage gate (exact candle grid, valid rows, left-closed funding hours and the right-closed accrual
  span) runs before selection.
- Contract for every arm and for the M1 incumbents (`incumbent_contract`): direction `both`, one explicit
  `tiered_tp_pct` close with tiers (0.01, 0.25), (0.02, 0.5), (0.03, 0.75), (0.04, 1.0), the sole stop owner
  `stop_loss_atr_mult=1.0`, strict comparison mode, `ohlc_walk` intrabar resolution, `--funding charge`, the
  manifest execution specification (fees, half spread plus slippage, szDecimals lots, $10 minimum).
- Arms: the candidate, its paired volume ablation (the candidate's selection with `volume_weighting=0`, never
  selected separately), `momentum_pro` and the landed `elder_ray_pullback` (formulas unchanged).
- Optimizer grid (54 combinations, all valid in both weightings): `force_period` [8, 13], `trend_period`
  [30, 50, 80], `slope_span` [3, 5, 10], `recursion_length` [150, 180, 197], `volume_weighting` [1].
  Comparators use their existing optimizer grids.
- Selection on `train` only: complete coverage, no liquidation, at least 10 independent positions; rank by
  mean net Sharpe, then mean drawdown-adjusted return, then grid order.
- Held-out gates: at least 30 independent positions per selected arm and both candidate directions. Base cost
  and a stress run with doubled half spread and slippage (fees unchanged).
- Verdict: pass needs complete evidence, every gate, positive candidate net PnL at both costs, and candidate
  base mean Sharpe and drawdown-adjusted return strictly above each comparator. A complete study that misses any
  pass condition fails. Coverage, funding or sample limits are inconclusive. Registry evidence source: `study_fail`
  for fail, `study_inconclusive` for inconclusive, `unvalidated` for a pass. `edge_status` stays `no_edge`.
- Numerical tolerances: independent scalar reference abs 1e-12 + rel 1e-12; zero crossings and trend tests use
  no epsilon; finite-versus-unrestricted sensitivity tolerance 1e-8 on the normalized difference.

## Outcome (added after scoring)

`REPORT.md` holds the generated result: **fail**, registry evidence source `study_fail`. Every coverage and sample
gate passed (87 independent held-out positions for the selected candidate, both directions present). The candidate's
held-out net PnL is negative at base cost (-108.00 USD) and at doubled spread and slippage (-332.80 USD), so the
frozen pass rule fails. Its mean Sharpe and drawdown-adjusted return are above the volume ablation, `momentum_pro`
and `elder_ray_pullback`, but every comparator also lost money in the same window. Training selection did not
carry over: the Spearman rank correlation of train and held-out Sharpe over the 54 variants is -0.536, while 36 of
the 54 variants were net positive in the held-out window. These observations are descriptive and do not change the
verdict. Results are retrospective on reused data; no result approves live use.

## Commands

```
uv run --no-sync python backtest/candidates/elder_force_pullback_1668/run_study.py lock
uv run --no-sync python backtest/candidates/elder_force_pullback_1668/run_study.py run
uv run --no-sync python backtest/candidates/elder_force_pullback_1668/run_parity.py
uv run --no-sync python backtest/candidates/elder_force_pullback_1668/run_parity.py --batched
uv run --no-sync python backtest/candidates/elder_force_pullback_1668/run_study.py render
```

`run` writes `results.json`, `evidence/` and `REPORT.md`. `run_parity.py` checks the registry evaluator against an
independent scalar reference, every prefix, the required-history, 199- and 200-row tails, synthetic boundary and
defect/recovery frames, and `parity_diff.compute_parity_frame` decisions at window 200 and window 0 in entry and
composed (close plus stop) modes; `--batched` adds the explicit-paper batch slot comparison. `render` rebuilds the
report, including the parity summary. Every command reads local frozen files only and sends no order.

Research tools stay suggest-only. No result promotes, deploys or trades the candidate.
