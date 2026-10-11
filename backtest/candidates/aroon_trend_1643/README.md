# Aroon trend candidate (#1643)

Research study for the `aroon_trend` entry evaluator on Hyperliquid perpetuals.
Tracking group: `hyperliquid-strategy-candidates-2026-10-02`. The study is
suggest-only: it writes no live configuration, no live default and no order.
The registry keeps `edge_status="no_edge"` in every outcome. Explicit paper
mode runs the strategy; live or missing mode needs `allow_no_edge: true`
(the repository's acknowledged-live exception). Promotion is a separate
reviewed action. `REPORT.md` holds the result; this file holds the frozen
specification.

## Signal contract

Implementation: `shared_strategies/open/aroon_trend.py`, registered in
`shared_strategies/open/registry.py` (futures only, `short_entries=True`).

- Parameters: `period` is an integer from 2 through 198. `strength` is a
  finite number from 0 through 100. Booleans, strings, nulls, fractional
  periods and out-of-range values are refused before any indicator work: by
  the registry validators (`validate_params`, `validate_param_value`, the
  evaluator itself), by `check_hyperliquid.py` before the solo market fetch
  and before each batch slot is evaluated, and by the Go config loader for the
  effective structured open reference and for any open-strategy close
  fallback that names `aroon_trend`. Ignored legacy `--params` argument
  overrides are not active inputs and are not validated.
- Indicator window: each reading at row `i` uses the `period + 1` closed
  rows `i - period .. i` (the decision row included). The age of an extremum
  is the number of rows between it and row `i` (0 through `period`). On a
  tie the most recent row wins.
- `Up = 100 * (period - age of the highest high) / period`.
  `Down = 100 * (period - age of the lowest low) / period`.
- Long event at row `i`: `prev Up <= prev Down`, `Up > Down` and
  `Up > strength` (strict). Short event: the mirror with Down. The previous
  reading must come from its own valid window at row `i - 1`. An event fires
  only on the crossing row; a crossing whose strength is not met is not
  delayed to a later row.
- Warm-up: a decision needs two adjacent valid windows, so `period + 2`
  rows (27 at the seed period 25).
- Holds give `signal = 0` with a reason and a numeric code, and never carry
  an earlier valid reading across a bad window:

  | Reason | Code |
  |---|---|
  | `entry_long` | 1 |
  | `entry_short` | -1 |
  | `no_cross` | 0 |
  | `strength_not_met` (crossing without strength) | 2 |
  | `insufficient_history` | 10 |
  | `nonfinite_input` (missing or non-finite high, low or close) | 11 |
  | `inconsistent_hlc` (high below low, or close outside the bar range) | 12 |
  | `timestamp_order` (missing, repeated or reversed timestamps) | 13 |
  | `flat_range` (highest high equals lowest low in the window) | 14 |
  | `missing_columns` (high, low or close column absent) | 15 |

- Output columns: `aroon_up`, `aroon_down`, `aroon_prev_up`,
  `aroon_prev_down`, `aroon_high_age`, `aroon_low_age`, `aroon_eval_ts_ms`,
  `aroon_period`, `aroon_strength`, `aroon_valid`, `aroon_reason`,
  `aroon_reason_code`, `signal`. The check scripts copy the finite numeric
  columns into `indicators`.
- Ownership: the evaluator reads only the supplied closed-bar frame and keeps
  evaluation-local arrays. It fetches nothing and writes no book, cache or
  state. The scheduler keeps position, stop, fill and storage ownership. A
  data hold still lets the explicit close and the stop owner run.

## Study contract (frozen before held-out scoring)

`run_study.py --protocol-hash` prints the protocol digest from the code and
`candidate_seed.json`; `REPORT.md` records the digest of the run.

- Every arm (candidate, `adx_trend` defaults, `breakout` defaults) and every
  M1 incumbent uses the same contract: direction `both`, one `tiered_tp_pct`
  close with a single 3% profit tier closing the full position, one fixed
  entry ATR x1.0 stop, `strict` close comparison mode. The incumbents receive
  the contract through `eval_windows.compute_incumbent_legs(contract=...)` /
  `evaluate_window(incumbent_contract=...)`; the default M1 callers are
  unchanged and the contract is part of the incumbent memo key.
- Grid (`backtest/optimizer.py`): period 10, 14, 25, 50, 100; strength 50,
  70, 85. Every period satisfies `period + 2 <= 200`, the default
  Hyperliquid candle window, so feed and probe changes are unnecessary.
- Selection: candidate only on the selection window; eligible = every
  dataset scored, no incomplete funded leg, no liquidated leg, no funding
  fetched while scoring, at least 10 positions (venue: candle and funding
  coverage complete); maximum mean Sharpe, then mean drawdown-adjusted
  return, then grid order. No eligible combination gives inconclusive.
- Sample floor: 30 independent held-out position clusters (overlapping
  positions across datasets count once).

### Proxy M1 study

- Datasets: the six M1 datasets (BTC, ETH, SOL USDT at 1h and 4h) from the
  default Binance US loader, `--registry futures`, Hyperliquid fees.
- Windows: the M1 windows with the open `oos` end frozen at 2026-10-01.
  Selection reads only `is` (2025-06-10 to 2026-01-01). Held out: `oos`,
  `2023`, `2024`, `2025H1`. M1's `2025H1` overlaps `is` by three weeks; the
  windows are M1's and stay unchanged.
- Funding: `charge` on every leg (candidate, comparators, incumbents),
  priced from an isolated cache frozen with `--freeze-proxy`. Hyperliquid
  funding starts on 2023-05-12, so a held position before that date leaves
  unpriced held hours. A `--funding off` sensitivity on the same inputs is
  reported separately and never qualifies a result.
- Freeze: `proxy_freeze.json` stores the digest, row count and gap
  inventory of every dataset and window frame and the digest and coverage of
  each coin's funding rows. The run verifies every digest before scoring and
  replaces the candle and funding fetchers with a refusal while scoring.
- Limitations: proxy legs model no lot rounding, no minimum order and no
  spread; the report compares the smallest simulated proxy order with the
  pinned venue gate.
- Verdict: see `PROXY_VERDICT_RULE` in `run_study.py`. Any missing dataset,
  incomplete funded leg or funding fetched while scoring in any held-out
  window gives inconclusive.

### Venue-input study

- Inputs: byte-identical copies of the frozen chaikin_money_flow_1649
  Hyperliquid 4h candles, hourly funding, `venue_meta.json` and the L2
  snapshot (`study_manifest.json`, sha256 values unchanged).
- Windows: `train` 2024-09-01 to 2025-09-01 (selection), `test` 2025-09-01
  to 2026-09-01 (held out), 300 warm-up bars.
- Costs: the manifest execution specification for every arm and incumbent:
  taker and maker fees, per-coin half spread plus 5 bps slippage per side,
  szDecimals lot rounding, the $10 minimum order with a 3% margin, hourly
  funding. Cost x2 doubles spread and slippage.
- Verdict: see `VENUE_VERDICT_RULE` in `run_study.py`.

### Overall rule

Pass only if both conclusions pass; fail if either fails; otherwise
inconclusive. A failed or inconclusive result completes the research task.

## Reproduction

```
uv run --no-sync python backtest/candidates/aroon_trend_1643/run_study.py --protocol-hash
uv run --no-sync python backtest/candidates/aroon_trend_1643/run_study.py \
    --cache-db /tmp/aroon_1643_proxy.db --freeze-proxy
uv run --no-sync python backtest/candidates/aroon_trend_1643/run_study.py \
    --cache-db /tmp/aroon_1643_proxy.db
uv run --no-sync python backtest/candidates/aroon_trend_1643/run_study.py --render-only
```

`--freeze-proxy` on a fresh cache refetches the proxy inputs; it rewrites
`proxy_freeze.json`, and `git diff` on that file shows whether the venue
returned the same rows. The scoring run refuses to start when any digest
differs. The run also executes `backtest/parity_diff.py` against the frozen
proxy cache with a temporary explicit-paper study configuration
(`--window 0`, `--window 200`, `--window 200 --batched`); `--skip-cli-parity`
omits those subprocesses.

Discovery output: `discovery_before_{spot,futures}.json` were captured
before registration and `discovery_after_{spot,futures}.json` after; they
are byte-identical because the candidate is hidden.
