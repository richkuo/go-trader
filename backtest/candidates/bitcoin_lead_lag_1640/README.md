# Bitcoin lead-lag entries (#1640)

Research candidate `bitcoin_lead_lag` (tracking group
`hyperliquid-strategy-candidates-2026-10-02`): a futures-only, bidirectional
entry evaluator registered with `edge_status="no_edge"` and
`production_refused=True`. Every production check refuses it in every mode,
with or without `allow_no_edge`, for a positional, effective open or
close-fallback reference, in Go config loading, probe, read-only inspection,
reload, `buildAddStrategyEntry`, and the Python solo and batch check scripts.
The study result never changes that; a promotion needs a separate reviewed
change. The proposed short name `btclag` and direction `both` are recorded in
`candidate_spec.json` for that change; this change adds no scheduler short-name
or bidirectional entry.

Result: **FAIL** (`edge_source` `study_fail`). On the primary 1h held-out
window the selected candidate lost on average 9.125% per follower at base cost
and 13.65% at doubled spread and slippage; the secondary 4h venue study was
inconclusive (13 position clusters, below the frozen 30). Decision parity was
clean. `REPORT.md` has every number.

`candidate_spec.json` freezes the formula, boundary comparisons, parameter
domains, grids, arms, close and stop owners, costs, latency scenarios, gates
and verdict rules. It was committed with `run_study.py` and both manifests
before any held-out window was scored; `run_study.py --protocol-hash` prints
the sha256 written into `results.json`.

## Signal (frozen)

- Leader: the BTC perpetual on the follower's own timeframe. Follower: the
  strategy's traded symbol. Bars match by exact opening time; a follower bar
  with no matched leader bar is invalid; nothing is forward-filled.
- Decision `d` fits OLS `y = a + b x` over `training_bars` pairs, where
  `x = ln(BTC close[j] / BTC close[j-1])` and
  `y = ln(F close[j+lag+horizon-1]) - ln(F close[j+lag-1])`. The last pair's
  label ends at `d`, so every label completed before the decision.
- An impulse is a BTC bar return of at least `impulse_z` population standard
  deviations of the training BTC returns of its own decision. It is active for
  decisions `j .. j+lag+horizon-2` and enters once, at the first decision where
  the slope and both training-half slopes are positive, `R2 >= min_r2`, the
  forecast has the impulse's sign, and the signed forecast minus the follower
  move already completed exceeds `cost_bps + min_edge_bps`. Earlier decisions
  are reconstructed from their own fits, so later decisions, restarts and
  stop-outs report `event_consumed`.
- Emission needs `S = training_bars + 2*lag_bars + 2*horizon_bars - 2`
  contiguous valid rows; a decision with less holds (`insufficient_history`).
- Baselines with identical data eligibility: `follower_momentum` (the
  follower's own impulse, entered at its bar) and `btc_trend_filter` (the same
  entries kept only when the unlagged BTC return over the last `horizon_bars`
  bars agrees).
- Every row carries `bll_*` evidence; entry rows record the event id, impulse,
  coefficients, fit quality, training bounds, forecast, completed move,
  remaining edge, threshold, support bounds and the support sha256.

## Inputs and failure boundary

Runtime leader rows come only from the sealed market payload (Go adds the BTC
frame for this open name only, with timing; payload version 1).
`shared_tools/lead_lag_inputs.py` selects both frames at the payload's one
decision cutoff with `sealed_frame_timings` and `select_closed`, matches
opening and closing boundaries, and holds on a missing, not-ready, stale,
untimed, duplicate, reordered, invalid, short, gapped or boundary-mismatched
frame. The evaluator checks event age and receipt age against
`max_data_age_ms` on the last row. With a usable follower and unusable leader
input the composer holds the entry and the configured close owner still runs.
When the leader frame is missing from the seal, the payload builder refuses
the whole payload and the scheduler's degraded branch runs (no candle-derived
signal, verified protection continues).

Historical runs (`run_backtest.py --mode single`, `eval_windows.py` cached and
`--manifest`) attach the pinned BTC series with the follower's full warm-up
through `backtest/lead_lag_history.py`; the cached path reads the cache only
and never fetches. Other harness entry paths refuse the candidate.

## Study (frozen)

- Primary 1h: `../bitcoin_lead_lag_1640_manifest_1h.json`, the hash-pinned
  `chande_momentum_1661/data` Binance US spot candles as a Hyperliquid
  perpetual price proxy with Hyperliquid hourly funding, referenced in place.
- Secondary 4h: `../bitcoin_lead_lag_1640_manifest_4h.json`, the hash-pinned
  `on_balance_volume_divergence_1657/data` Hyperliquid perpetual candles and
  funding, referenced in place.
- Followers ETH and SOL; windows train [2024-09-01, 2025-09-01) and held-out
  [2025-09-01, 2026-09-01) (used by earlier studies, so retrospective).
- Close owner `tiered_tp_atr` (one 1.0 ATR full-close tier), stop owner
  `stop_loss_atr_mult` 1.0, direction both, strict comparison, the manifest
  execution spec (fees, per-coin half spread, slippage, lots, $10 minimum) and
  manifest funding on every leg; stress doubles spread and slippage; latency
  rows delay the fill by 1 and 2 bars.
- Each arm selects on train only; the held-out verdict rules are in
  `candidate_spec.json`. `REPORT.md` carries the numbers.

## Reproduce

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/bitcoin_lead_lag_1640_manifest_1h.json
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/bitcoin_lead_lag_1640_manifest_4h.json
uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py --protocol-hash
uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py verify
uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py run --workers 6
uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py --render-only
```

The study reads only local manifest inputs and needs no network access.
`discovery_before_{spot,futures}.json` and `discovery_after_{spot,futures}.json`
hold the `--list-json` output of the spot and futures shims before this change
(captured at `63d12919`) and after it (rebased on `62618cbc`); each pair is
byte-identical.

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code
