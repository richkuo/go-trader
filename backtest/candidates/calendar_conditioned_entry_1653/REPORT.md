# Calendar-conditioned entry study (issue 1653)

**Verdict: INCONCLUSIVE** (proxy 1h: inconclusive, venue 4h: inconclusive). Registry metadata after this study: `edge_status="no_edge"`, `edge_source="study_inconclusive"`, `edge_ref` = this file. The candidate stays hidden from discovery. Explicit paper mode is admitted; live use still needs `allow_no_edge: true`, and no result here adds one or approves deployment. Every command in this study is suggest-only and sends no order. The proxy group is proxy evidence (Binance US candles with Hyperliquid fees and funding); the venue group is Hyperliquid candle evidence. No positive economic outcome was required.

Group reasons: proxy_1h: inconclusive (oos independent position clusters 2 < 30); venue_4h: inconclusive (test independent position clusters 0 < 30).

## Signal

`calendar_conditioned_entry` (`shared_strategies/open/calendar_conditioned_entry.py`) is one pure, entry-only evaluator. Arithmetic open-to-open return fraction: label = open[e + holding_bars] / open[e] - 1 for entry bar e, the same nominal entry and exit opens as time_stop(max_bars=holding_bars) in the simulator (signal on closed bar N, entry at open N+1, bars_held counts the entry bar, the scheduled close fills at open N+holding_bars+1). A label enters a table only when its exit open (bar e + holding_bars) is at or before the refit bar, so every exit price is present at the refit decision; no future open is used. Cells: UTC weekday (Monday = 0) x UTC hour of the entry bar's opening time: 168 cells; naive indexes are UTC, aware indexes are converted to UTC; the next entry bar's opening time is the decision bar's opening time + expected_interval_ms. Refits: refits fall on closed-bar boundaries that are multiples of refit_bars x expected_interval_ms after the fixed epoch 1970-01-05T00:00:00Z (a Monday); a decision uses the last scheduled table, reconstructed from the frame's history on every evaluation, and eligibility stays fixed until the next refit; refit timing never depends on the first row or length of the frame. Training: the training_bars bars ending at the refit bar (inclusive); every one of them and every bar up to the decision bar must be present, consecutive on the expected_interval_ms grid and well formed, else the decision holds with a reason. Clusters: each cell's labels are averaged per UTC week (weeks anchored at the same epoch); the cell mean is the equal-weight mean of its weekly cluster means; repeated calendar cells within one week are never treated as independent. Uncertainty: Newey-West (Bartlett weights) long-run variance of the chronological weekly cluster series with lag L = max(1, ceil(holding_bars x expected_interval_ms / 1 week)), which covers every overlap between same-cell holding outcomes of consecutive weeks; the variance used is max(Newey-West, sample variance) x n / (n - 1), standard error sqrt(variance / n). Effective sample: n_eff = n x sample variance / max(Newey-West, sample variance), capped at n (the number of observed weekly clusters); a cell with n_eff < min_effective_obs holds; fewer than 2 weekly clusters, a zero or non-finite standard error holds (no_uncertainty_estimate or nonfinite_statistic). Edges: long net edge = mean - cost_hurdle; short net edge = -mean - cost_hurdle; costs are subtracted after the sign is applied, so negating a cost-adjusted value can never create a short edge. Threshold: one-sided normal approximation; family = 168 cells x 2 directions = 336 at every refit, empty cells included; Bonferroni alpha = (1 - confidence) / 336, z = inverse normal(1 - alpha); a direction qualifies only when its lower bound (net edge - z x standard error) is strictly greater than 0 (equality holds). Ties: if both directions qualify, the larger lower bound wins; an exact tie holds (direction_tie). The refit threshold controls the per-refit family only; it is not an experiment-wide significance claim. Cost hurdle: 0.002 round trip: Hyperliquid base taker fee 0.045% per side (0.09%) plus the engine's 5 bps slippage per fill (0.10%), rounded up; no funding reserve is included because the engine charges realized signed funding separately. Every hold carries a reason in `cce_reason`, and a data hold returns a zero-signal frame so native close and protective management continue.

The candidate reference (Finance Research Letters, S1544612324004598) is a research lead only; its contents were not checked and it supplies no formula, threshold or Hyperliquid edge claim used here.

## Protocol

`protocol.json` (sha256 `1d28184531a930a09e16b1fc15c5d507cae55cbe3b358499859628cac67fd222`) was locked by `run_study.py lock` at 2026-10-12T15:32:09Z (commit `0f9bec2dd3f40fac3b8f0cbcf1eb21a23f16c9b7+uncommitted-changes`), before any window was scored; `run` and `parity` refuse a different protocol, and `run` checks that the primary parameters equal the registry defaults, the variant grid equals optimizer `DEFAULT_PARAM_RANGES` and the warm-up equals `required_history_bars` ({"proxy_1h_required_history_bars": 4535, "venue_4h_required_history_bars": 1133}). Nothing is selected: the primary parameters are the registry defaults (1h) and their 4h equivalents (26 weeks of training, a 4-hour hold, weekly refits); the variant grid equals optimizer DEFAULT_PARAM_RANGES and is reported as stability and unsuccessful-variant evidence only; no variant can change a verdict.

Contract: every arm (candidate, variants, unconditional, randomized) and every M1 incumbent of a group runs under one contract: direction both, the single time_stop close owner with the group's holding horizon, the single fixed entry-ATR stop owner (3.0 x ATR, a protective stop wide enough that the horizon close normally owns the exit), comparison_mode approximate (time_stop reads simulator-only bars_held, so every close_validation block records RESEARCH_ONLY_CLOSE_CONTEXT and close parity is incomplete), no regime gate, the same capital, sizing, execution costs, funding mode, data and warm-up. Incumbents receive it through eval_windows compute_incumbent_legs(contract=...). Variants with another holding_bars use their own time_stop and are compared only descriptively.

| group | parameters | close | stop | warm-up bars |
|---|---|---|---|---|
| proxy_1h | `{"confidence": 0.9, "cost_hurdle": 0.002, "expected_interval_ms": 3600000, "holding_bars": 4, "min_effective_obs": 8, "refit_bars": 168, "training_bars": 4368}` | `[{"name": "time_stop", "params": {"max_bars": 4}}]` | 3.0 x ATR | 4535 |
| venue_4h | `{"confidence": 0.9, "cost_hurdle": 0.002, "expected_interval_ms": 14400000, "holding_bars": 1, "min_effective_obs": 8, "refit_bars": 42, "training_bars": 1092}` | `[{"name": "time_stop", "params": {"max_bars": 1}}]` | 3.0 x ATR | 1133 |

Baselines (implemented in `run_study.py`, never registered for production): `unconditional_entry_1653` — the candidate's estimator with every label pooled into one cell (no calendar conditioning): the same training sample, completed labels, weekly clusters, Newey-West error, effective sample, cost hurdle, refit grid, holds and tie rule; family size 2 (long and short), alpha = (1 - confidence) / 2; enters on every valid closed bar whose pooled direction qualifies. `randomized_entry_1653` (20 seeds) — within every refit block (decision bars that share the candidate's last scheduled refit), the same number of long and of short raw entry signals as the candidate, placed uniformly at random without replacement among that block's valid candidate decision bars (cce_valid = 1); the block counts come from the candidate's causal training tables only, so no held-out outcome sets timing or direction; generator numpy default_rng([1653, seed, refit_boundary_ms // 1000]); the composer then blocks entries while a position is open, so realized exposure can differ and is recorded. The candidate beats the randomized control when its summed net PnL is greater than every seed's summed net PnL in the same window (an empirical one-sided rank level of 1/21). Run_study.py installs both callables into the futures research registry instance that eval_windows uses in this process only and removes them in a finally block, also after a failure; they are never added to shared_strategies, PLATFORM_ORDER, discovery or any Go table (removed after the run: yes).

Departure from the frozen protocol: the randomized control that produced every published randomized number seeds numpy default_rng([1653, seed, refit_boundary_ms // 1000, salt]), where salt = round(open of the block's first valid decision row x 1e6) mod 2^32 (`randomized_entry` in `run_study.py`). The locked `protocol.json` text above omits the salt; the protocol file is left unchanged, so its lock still holds. With the salt, blocks of different markets that share a refit boundary and a row count get different permutations. The verdict does not depend on it: both groups stop at the independent-cluster sample floor before any randomized comparison can decide.

Warm-up: required history = training_bars + refit_bars - 1 bars ending at the decision bar (the training span plus the largest distance to the last scheduled refit; completed labels lie inside the training span, the 14-bar ATR stop history is shorter); the first scored decision is the first window bar on the M1 path and the boundary bar on the walk-forward path. Every arm and every incumbent of a window loads the same verified consecutive prefix (eval_windows warmup_bars on the cached proxy path, the manifest warmup_bars on the venue path); the evaluator reads it, the engine neither scores nor trades it, funding is attached to the scored window only, and every scored window starts flat. A window whose prefix is not fully present and consecutive is refused by the existing coverage check; it is recorded as an inconclusive window, never scored and never filled.

Economic correction: pooled held-out per-position net return (net PnL including fees and funding divided by entry notional); one-sided normal lower bound mean - z x sd / sqrt(independent clusters); 6 proxy variants (the grid, primary included) plus the venue primary: every economic significance statement is Bonferroni-corrected at alpha 0.05 / 7, whether or not a variant is the primary.

Proxy group rule: oos unscorable -> inconclusive; oos candidate clusters < min_held_out_clusters -> inconclusive; any check false -> fail; every check true -> pass; else inconclusive. Venue group rule: evidence incomplete -> inconclusive; test candidate clusters at x1 < min_held_out_clusters -> inconclusive; any check false -> fail; every check true -> pass. Overall: pass only if proxy 1h and venue 4h both pass; fail if any group is fail; otherwise inconclusive. The proxy group is proxy evidence and the venue group is venue evidence; the report states both.

| proxy window | start | end (exclusive) | role |
|---|---|---|---|
| oos | 2026-01-01 | 2026-10-01 | primary held-out |
| 2023 | 2023-01-01 | 2024-01-01 | refused (no prefix) |
| 2024 | 2024-01-01 | 2025-01-01 | secondary held-out |
| 2025H1 | 2025-01-01 | 2025-07-01 | secondary held-out |
| is | 2025-06-10 | 2026-01-01 | diagnostic |

Departures from the default M1 windows: oos end pinned at 2026-10-01, the end of the frozen candles (M1 leaves it open). The candles start 2023-01-01, so the 2023 window has no 4535-bar prefix and is refused (inconclusive); the frozen candles have a 10-bar gap on 2026-08-31, so oos decisions after it hold until 4535 clean bars exist again, which the frozen data never reaches.

| venue window | start | end (exclusive) | role |
|---|---|---|---|
| test | 2025-09-01 | 2026-09-01 | primary held-out: scored once under the frozen protocol with no parameter selection; previously the held-out window of the #1642 and later studies, so this is retrospective candidate evidence |

The venue candles start 2024-06-22, so only the test window (2025-09-01 to 2026-09-01) has the 1133-bar prefix; the earlier train window of prior studies cannot be scored.

## Inputs, costs and assumptions

- Proxy candles: Binance US spot USDT 1h candles stand in for Hyperliquid perpetual prices, so this is proxy evidence and not measured Hyperliquid candle evidence (every proxy file equals backtest/candidates/dual_thrust_1642/study_manifest.json (read in place)).
- Proxy funding: Hyperliquid hourly funding charged on every held hour (`--funding charge`, `eval_windows.run_leg(funding_mode="charge")`) for every candidate, baseline and incumbent leg; funding is attached to the scored window only, never to warm-up bars; unavailable funding is never a silent zero.
- Proxy costs: engine Hyperliquid commission `0.00045` per side and engine slippage `0.0005` per fill; the slippage term stands in for the spread. This legacy M1 screen replays no spread, lot rounding or minimum order, so it cannot by itself establish venue execution parity.
- Venue candles: Hyperliquid mainnet perpetual candles from the public info API (candleSnapshot); closed bars only (every venue file equals backtest/candidates/dual_thrust_1642/venue_manifest.json (read in place)), interval 4h, 1133 manifest warm-up bars read by the evaluator but never scored or traded; the cached warm-up is not passed on this path.
- Venue costs (manifest execution specification with the explicit close owner): taker fee 0.00045, maker fee 0.00015, slippage 5.0 bps per side plus the per-coin half spread from one frozen L2 snapshot, szDecimals lot rounding, minimum notional $10.0 with a 0.03 margin; cost x2 doubles spread and slippage. Funding uses the manifest's existing attachment and coverage semantics.
- Sizing: capital $1000 per leg, no leverage and no perps sizing block (Backtester leverage None, sizing defaults {"leverage": null, "risk_per_trade_pct": null}).

| venue dataset | size decimals | half spread bps | candles sha256 | funding sha256 |
|---|---|---|---|---|
| BTC 4h | 5 | 0.06 | `89f84288d462b830ceba8108ea9d69db4d3dac417c730dc9cdd13271c10fd0f8` | `b961391682a15a619fe131abedcc638fd2c311af7cd30a67cf4af32754cabc48` |
| ETH 4h | 4 | 0.19 | `a17523ecbb30f7b594aa80002b3c5ad80b73a7bdd8122d5512683585a8eec3ae` | `1b541abb8801a444cac4e23ebe31d71cbd7dc3b7d185456830daca1c09c6ea66` |
| SOL 4h | 2 | 0.42 | `5ca4fa74940f38f180a85eb81ccaa96bb10f08988d1351c280abb2ce175fdd4c` | `a498781bca4cf017f7f772f5a0c00c0853979bacfd147303394f63a63da5af40` |

| proxy file (read in place) | rows | sha256 |
|---|---|---|
| `backtest/candidates/dual_thrust_1642/data/proxy/BTC_1h_candles.csv.gz` | 32840 | `d107c60f72363372d03273ba6439ca42a0d867a9089c5de171e9306af3ba2d17` |
| `backtest/candidates/dual_thrust_1642/data/proxy/BTC_4h_candles.csv.gz` | 8211 | `dd0758a2862955eb53b060df5fbace3e98f8bc2a3e0254842bca0d36c96f106f` |
| `backtest/candidates/dual_thrust_1642/data/proxy/BTC_funding.csv.gz` | 29142 | `520fd979d5594b4c2fb950cd52aa327749a124d9971a59ec3419752dd24a27f8` |
| `backtest/candidates/dual_thrust_1642/data/proxy/ETH_1h_candles.csv.gz` | 32840 | `59d08735aecb22b63bab8189c7c32b11101e80da42b2ec724f39e308548b7913` |
| `backtest/candidates/dual_thrust_1642/data/proxy/ETH_4h_candles.csv.gz` | 8211 | `03df63c5dc65ba532a252e8c00118986321fcb124934bb7536e842d678fc9dd8` |
| `backtest/candidates/dual_thrust_1642/data/proxy/ETH_funding.csv.gz` | 29142 | `4b7694d8fc26cb7245ffa882cbdcb2d571d17f3a9f1abd6832fb0deed4985b41` |
| `backtest/candidates/dual_thrust_1642/data/proxy/SOL_1h_candles.csv.gz` | 32840 | `e4e5c15ffcce696253d78dc3b3c3448725e8cf3a0114a9fbc4b1df5f2a0efeec` |
| `backtest/candidates/dual_thrust_1642/data/proxy/SOL_4h_candles.csv.gz` | 8211 | `c1e5775973d00b0b07597da3db4213d62378ef566fe2e4a3b619612b59abc8ab` |
| `backtest/candidates/dual_thrust_1642/data/proxy/SOL_funding.csv.gz` | 29142 | `b29de7625f7484f0971336e31ba52f5b042127e4eef4d8ee1e4554994383cd2b` |

## proxy_1h

**Group verdict: inconclusive** (oos independent position clusters 2 < 30).

| check | value |
|---|---|
| m1_oos_pass | false |
| m1_secondary_passes | false |
| oos_beats_randomized | false |
| oos_beats_unconditional | false |
| oos_corrected_lower_bound_positive | false |
| oos_net_positive | false |

### `oos`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | degenerate | -0.23 | -0.29 | -8.63 | -1.37 | 2 (0/2) | 2 | 1.33 | 1.80 | -0.05 | 0 |
| unconditional_entry_1653 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |

Randomized control (20 seeds): net PnL max $31.32, median $0.69, min $-19.82; realized positions 2 to 2 per seed.

### `2024`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |
| unconditional_entry_1653 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |

### `2025H1`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | degenerate | -0.36 | -0.88 | -26.48 | -3.64 | 3 (3/0) | 3 | 1.99 | 2.69 | -0.14 | 0 |
| unconditional_entry_1653 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |

### `2023`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | refused: BTC/USDT 1h: warmup needs the 4535 consecutive bars from 2022-06-26 01:00:00 before 2023-01-01 00:00:00; found 0 rows, 4 | | | | | | | | | | |
| unconditional_entry_1653 | refused: BTC/USDT 1h: warmup needs the 4535 consecutive bars from 2022-06-26 01:00:00 before 2023-01-01 00:00:00; found 0 rows, 4 | | | | | | | | | | |

### `is`

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |
| unconditional_entry_1653 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |

Candidate economics (oos): per-position net return mean -0.00432, sd 0.00390, 2 independent clusters, z 2.450, corrected lower bound -0.01108; calendar concentration {"distinct_cells": 1, "top_cell": "3-11", "top_cell_share": 1.0, "top_weekday": 3, "top_weekday_share": 1.0}; close validation codes ['RESEARCH_ONLY_CLOSE_CONTEXT'].

Unscorable windows: 2023: candidate: refused (BTC/USDT 1h: warmup needs the 4535 consecutive bars from 2022-06-26 01:00:00 before 2023-01-01 00:00:00; found 0 rows, 4535 missing (first missing 2022-06-26 01:00:00)), unconditional_entry_1653: refused (BTC/USDT 1h: warmup needs the 4535 consecutive bars from 2022-06-26 01:00:00 before 2023-01-01 00:00:00; found 0 rows, 4535 missing (first missing 2022-06-26 01:00:00)).

### Variant grid (descriptive; no variant changes a verdict)

| variant | window | net PnL $ | mean Sharpe | positions (L/S) | clusters | corrected lower bound |
|---|---|---|---|---|---|---|
| T2184_H1 | 2024 | -15.23 | -0.06 | 4 (1/3) | 4 | -0.01894 |
| T2184_H1 | 2025H1 | -4.92 | -0.45 | 1 (0/1) | 1 | - |
| T2184_H1 | oos | -2.89 | -0.36 | 1 (0/1) | 1 | - |
| T2184_H4 | 2024 | -19.73 | -0.35 | 5 (4/1) | 5 | -0.01716 |
| T2184_H4 | 2025H1 | -7.87 | -0.38 | 5 (2/3) | 5 | -0.01306 |
| T2184_H4 | oos | -26.02 | -0.56 | 8 (0/8) | 4 | -0.01020 |
| T2184_H8 | 2024 | -89.95 | -1.26 | 4 (3/1) | 4 | -0.03969 |
| T2184_H8 | 2025H1 | -34.23 | -0.39 | 8 (2/6) | 7 | -0.01870 |
| T2184_H8 | oos | -15.84 | -0.52 | 1 (0/1) | 1 | - |
| T4368_H1 | 2024 | 0.00 | 0.00 | 0 (0/0) | 0 | - |
| T4368_H1 | 2025H1 | 0.00 | 0.00 | 0 (0/0) | 0 | - |
| T4368_H1 | oos | 0.00 | 0.00 | 0 (0/0) | 0 | - |
| T4368_H4 | 2024 | 0.00 | 0.00 | 0 (0/0) | 0 | - |
| T4368_H4 | 2025H1 | -26.48 | -0.36 | 3 (3/0) | 3 | -0.03286 |
| T4368_H4 | oos | -8.63 | -0.23 | 2 (0/2) | 2 | -0.01108 |
| T4368_H8 | 2024 | 218.79 | 0.52 | 6 (5/1) | 6 | -0.03123 |
| T4368_H8 | 2025H1 | -9.74 | -0.39 | 1 (0/1) | 1 | - |
| T4368_H8 | oos | -18.36 | -0.21 | 3 (0/3) | 3 | -0.02957 |

Start-of-window history and input holds (scored bars only; warm-up bars are read but not counted):

| window | dataset | scored bars | valid bars | first valid bar | entries L/S | hold reasons |
|---|---|---|---|---|---|---|
| 2023 | BTC/USDT 1h | 8753 | 3361 | 2023-08-13 23:00:00 | 0/0 | cadence_gap 1, history_cadence_gap 4522, insufficient_history 869 |
| 2023 | ETH/USDT 1h | 8753 | 3361 | 2023-08-13 23:00:00 | 0/0 | cadence_gap 1, history_cadence_gap 4522, insufficient_history 869 |
| 2023 | SOL/USDT 1h | 8753 | 3361 | 2023-08-13 23:00:00 | 0/0 | cadence_gap 1, history_cadence_gap 4522, insufficient_history 869 |
| 2024 | BTC/USDT 1h | 8784 | 8784 | 2024-01-01 00:00:00 | 0/0 | - |
| 2024 | ETH/USDT 1h | 8784 | 8784 | 2024-01-01 00:00:00 | 0/0 | - |
| 2024 | SOL/USDT 1h | 8784 | 8784 | 2024-01-01 00:00:00 | 0/0 | - |
| 2025H1 | BTC/USDT 1h | 4344 | 4344 | 2025-01-01 00:00:00 | 0/0 | - |
| 2025H1 | ETH/USDT 1h | 4344 | 4344 | 2025-01-01 00:00:00 | 3/0 | - |
| 2025H1 | SOL/USDT 1h | 4344 | 4344 | 2025-01-01 00:00:00 | 0/0 | - |
| is | BTC/USDT 1h | 4920 | 4920 | 2025-06-10 00:00:00 | 0/0 | - |
| is | ETH/USDT 1h | 4920 | 4920 | 2025-06-10 00:00:00 | 0/0 | - |
| is | SOL/USDT 1h | 4920 | 4920 | 2025-06-10 00:00:00 | 0/0 | - |
| oos | BTC/USDT 1h | 6543 | 5812 | 2026-01-01 00:00:00 | 0/2 | cadence_gap 1, history_cadence_gap 730 |
| oos | ETH/USDT 1h | 6543 | 5812 | 2026-01-01 00:00:00 | 0/0 | cadence_gap 1, history_cadence_gap 730 |
| oos | SOL/USDT 1h | 6543 | 5812 | 2026-01-01 00:00:00 | 0/0 | cadence_gap 1, history_cadence_gap 730 |

Refit tables: 108 tables across the datasets of the primary window (full per-cell tables in `evidence/refit_tables_proxy_1h_oos.json.gz`, uncompressed sha256 `b5ce17811534d4fc043d8535724b37082cfe6653c1f1d4548a745f4c5ab50223`); 2 eligible cell-directions in total; the largest directional lower bound of any cell in any of these tables is 0.00008 (a direction needs a value strictly above 0). Per refit (first and last per dataset):

| dataset | refit boundary (UTC) | training first open | labels | eligible L/S | z | best lower bound L/S | reasons |
|---|---|---|---|---|---|---|---|
| BTC/USDT 1h | 2025-12-29 00:00:00 | 2025-06-30 00:00:00 | 4364 | 0/0 | 3.434 | -0.00290/-0.00287 | {"below_threshold": 168} |
| BTC/USDT 1h | 2026-08-31 00:00:00 | 2026-03-02 00:00:00 | 4364 | 0/0 | 3.434 | -0.00164/-0.00338 | {"below_threshold": 168} |
| ETH/USDT 1h | 2025-12-29 00:00:00 | 2025-06-30 00:00:00 | 4364 | 0/0 | 3.434 | -0.00270/-0.00351 | {"below_threshold": 168} |
| ETH/USDT 1h | 2026-08-31 00:00:00 | 2026-03-02 00:00:00 | 4364 | 0/0 | 3.434 | -0.00138/-0.00362 | {"below_threshold": 168} |
| SOL/USDT 1h | 2025-12-29 00:00:00 | 2025-06-30 00:00:00 | 4364 | 0/0 | 3.434 | -0.00317/-0.00329 | {"below_threshold": 168} |
| SOL/USDT 1h | 2026-08-31 00:00:00 | 2026-03-02 00:00:00 | 4364 | 0/0 | 3.434 | -0.00191/-0.00295 | {"below_threshold": 168} |

Entry audit: 2 held-out candidate entries; each fill follows a valid raw signal on the prior closed bar, the selected cell's weekly sample and mean equal an independent recomputation from the raw opens, the table predates the decision, and every time_stop exit sits on the nominal horizon. First entries:

| dataset | side | signal bar | fill bar | cell (wd-h) | mean | lower bound L/S | weeks | n_eff | net $ |
|---|---|---|---|---|---|---|---|---|---|
| BTC/USDT 1h | short | 2026-03-26 10:00:00 | 2026-03-26 11:00:00 | 3-11 | -0.007049 | -0.014014/0.000084 | 26 | 21.00 | -1.57 |
| BTC/USDT 1h | short | 2026-04-02 10:00:00 | 2026-04-02 11:00:00 | 3-11 | -0.006975 | -0.013909/0.000041 | 26 | 21.69 | -7.07 |

## venue_4h

**Group verdict: inconclusive** (test independent position clusters 0 < 30).

| check | value |
|---|---|
| beats_randomized_x1 | false |
| beats_unconditional_x1 | false |
| corrected_lower_bound_positive_x1 | false |
| m1_test_pass_x1 | false |
| net_positive_x1 | false |
| net_positive_x2 | false |

### Held-out `test`, cost base

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |
| unconditional_entry_1653 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |

Randomized control (20 seeds): net PnL max $0.00, median $0.00, min $0.00; realized positions 0 to 0 per seed.

### Held-out `test`, cost stress

| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | clusters | turnover | fees $ | funding $ | incomplete legs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |
| unconditional_entry_1653 | degenerate | 0.00 | 0.00 | 0.00 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0.00 | 0 |

Randomized control (20 seeds): net PnL max $0.00, median $0.00, min $0.00; realized positions 0 to 0 per seed.

Candidate economics (test, cost x1): per-position net return mean -, sd -, 0 independent clusters, z 2.450, corrected lower bound -; calendar concentration {"distinct_cells": 0, "top_cell": null, "top_cell_share": null, "top_weekday": null, "top_weekday_share": null}; close validation codes ['RESEARCH_ONLY_CLOSE_CONTEXT'].

| dataset | candles complete | funding complete | rejected entries | exit reasons |
|---|---|---|---|---|
| BTC 4h | yes | yes | 0 | - |
| ETH 4h | yes | yes | 0 | - |
| SOL 4h | yes | yes | 0 | - |

Start-of-window history and input holds (scored bars only; warm-up bars are read but not counted):

| window | dataset | scored bars | valid bars | first valid bar | entries L/S | hold reasons |
|---|---|---|---|---|---|---|
| test | BTC 4h | 2190 | 2190 | 2025-09-01 00:00:00 | 0/0 | - |
| test | ETH 4h | 2190 | 2190 | 2025-09-01 00:00:00 | 0/0 | - |
| test | SOL 4h | 2190 | 2190 | 2025-09-01 00:00:00 | 0/0 | - |

Refit tables: 159 tables across the datasets of the primary window (full per-cell tables in `evidence/refit_tables_venue_4h_test.json.gz`, uncompressed sha256 `f323c77ccccadefa9eb552d5f5e1ecf8617262a978b7f81925dddb9fe166d79e`); 0 eligible cell-directions in total; the largest directional lower bound of any cell in any of these tables is -0.00024 (a direction needs a value strictly above 0). Per refit (first and last per dataset):

| dataset | refit boundary (UTC) | training first open | labels | eligible L/S | z | best lower bound L/S | reasons |
|---|---|---|---|---|---|---|---|
| BTC 4h | 2025-09-01 00:00:00 | 2025-03-03 00:00:00 | 1091 | 0/0 | 3.434 | -0.00408/-0.00347 | {"below_threshold": 42, "empty_cell": 126} |
| BTC 4h | 2026-08-31 00:00:00 | 2026-03-02 00:00:00 | 1091 | 0/0 | 3.434 | -0.00245/-0.00369 | {"below_threshold": 42, "empty_cell": 126} |
| ETH 4h | 2025-09-01 00:00:00 | 2025-03-03 00:00:00 | 1091 | 0/0 | 3.434 | -0.00520/-0.00465 | {"below_threshold": 42, "empty_cell": 126} |
| ETH 4h | 2026-08-31 00:00:00 | 2026-03-02 00:00:00 | 1091 | 0/0 | 3.434 | -0.00258/-0.00409 | {"below_threshold": 42, "empty_cell": 126} |
| SOL 4h | 2025-09-01 00:00:00 | 2025-03-03 00:00:00 | 1091 | 0/0 | 3.434 | -0.00599/-0.00525 | {"below_threshold": 42, "empty_cell": 126} |
| SOL 4h | 2026-08-31 00:00:00 | 2026-03-02 00:00:00 | 1091 | 0/0 | 3.434 | -0.00315/-0.00324 | {"below_threshold": 42, "empty_cell": 126} |

Entry audit: 0 held-out candidate entries; each fill follows a valid raw signal on the prior closed bar, the selected cell's weekly sample and mean equal an independent recomputation from the raw opens, the table predates the decision, and every time_stop exit sits on the nominal horizon. First entries:

| dataset | side | signal bar | fill bar | cell (wd-h) | mean | lower bound L/S | weeks | n_eff | net $ |
|---|---|---|---|---|---|---|---|---|---|

Verdict guards (each altered copy must not pass): proxy_1h: proxy_below_sample_floor -> inconclusive, proxy_oos_funding_incomplete -> inconclusive, proxy_oos_missing_baseline_dataset -> inconclusive, proxy_oos_refused_prefix -> inconclusive; venue_4h: venue_below_sample_floor -> inconclusive, venue_coverage_incomplete -> inconclusive, venue_funding_incomplete -> inconclusive, venue_proxy_provenance -> inconclusive.

Incumbent refusals: 2023/H4: BTC/USDT 1h: warmup needs the 4535 consecutive bars from 2022-06-26 01:00:00 before 2023-01-01 00:00:00; found 0 rows, 4535 missing (first missing 2022-06-26 01:00:00).

## Parity

Entry parity (pure evaluator through the futures registry; every prefix, every refit table, shifted starts): clean = yes.

| source | bars | valid bars | long/short | prefix mismatches | tables | table mismatches | shifted-start mismatches |
|---|---|---|---|---|---|---|---|
| proxy BTC/USDT 1h 2025-10-01..2026-10-01 | 8751 | 3533 | 0/0 | 0 | 22 | 0 | 1: 0, 167: 0, 168: 0, 25: 0 |
| synthetic seeded 1h walk with weekly cells (5300 bars, seed 1653) | 5300 | 933 | 18/5 | 0 | 6 | 0 | 1: 0, 167: 0, 168: 0, 25: 0 |
| venue BTC 4h test window with warmup | 3323 | 2191 | 0/0 | 0 | 53 | 0 | 1: 0, 25: 0, 41: 0, 42: 0 |

`backtest/parity_diff.py` CLI runs (non-batched, proxy BTC 1h from the frozen cache):

| argv | exit | bars compared | mismatches | backtest entries | live entries | close parity |
|---|---|---|---|---|---|---|
| `backtest/parity_diff.py --config backtest/candidates/calendar_conditioned_entry_1653/paper_parity_1h.json --strategy-id hl-cce-btc-1h --window 0 --comparison-mode approximate --funding charge --since 2025-10-01` | 3 | 8722 | 0 | 0 | 0 | incomplete |
| `backtest/parity_diff.py --config backtest/candidates/calendar_conditioned_entry_1653/paper_parity_1h.json --strategy-id hl-cce-btc-1h --window 200 --comparison-mode approximate --funding charge --since 2025-10-01` | 3 | 8552 | 0 | 0 | 0 | incomplete |

`compute_parity_frame` runs (non-batched and batched solo vs batch slots through the real Hyperliquid check evaluator):

| source | batched | window | bars compared | mismatches | open-action mismatches | close-fraction mismatches | batch mismatches | bt/live entries | close parity |
|---|---|---|---|---|---|---|---|---|---|
| proxy BTC/USDT 1h 2025-10-01..2026-10-01 | no | 200 | 8552 | 0 | 0 | 0 | - | 0/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| proxy BTC/USDT 1h 2025-10-01..2026-10-01 | no | every prefix | 8722 | 0 | 0 | 0 | - | 0/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| proxy BTC/USDT 1h 2025-10-01..2026-10-01 | yes | 200 | 8552 | 0 | 0 | 0 | 0 | 0/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| proxy BTC/USDT 1h 2025-10-01..2026-10-01 | yes | every prefix | 8722 | 0 | 0 | 0 | 0 | 0/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| synthetic seeded 1h walk with weekly cells (5300 bars, seed 1653) | no | 200 | 5101 | 23 | 23 | 0 | - | 23/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| synthetic seeded 1h walk with weekly cells (5300 bars, seed 1653) | no | every prefix | 5271 | 0 | 0 | 0 | - | 23/23 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| synthetic seeded 1h walk with weekly cells (5300 bars, seed 1653) | yes | 200 | 5101 | 23 | 23 | 0 | 0 | 23/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| synthetic seeded 1h walk with weekly cells (5300 bars, seed 1653) | yes | every prefix | 5271 | 0 | 0 | 0 | 0 | 23/23 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| venue BTC 4h test window with warmup | no | 200 | 3124 | 0 | 0 | 0 | - | 0/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| venue BTC 4h test window with warmup | no | every prefix | 3294 | 0 | 0 | 0 | - | 0/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| venue BTC 4h test window with warmup | yes | 200 | 3124 | 0 | 0 | 0 | 0 | 0/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |
| venue BTC 4h test window with warmup | yes | every prefix | 3294 | 0 | 0 | 0 | 0 | 0/0 | incomplete (RESEARCH_ONLY_CLOSE_CONTEXT) |

Backtest/parity_diff.py in approximate mode with the explicit time_stop close and funding charge: CLI non-batched runs at --window 0 and --window 200 on the frozen proxy BTC 1h cache, and compute_parity_frame non-batched and batched (solo vs batch slots through the real Hyperliquid check evaluator) on the same frames; every mismatch, hold and close_validation block is saved; exit 3 is decision agreement with incomplete close parity, exit 1 records mismatches, neither is reported as complete live parity, and a zero-bar comparison cannot pass. A 200-bar live frame lacks the 4535-bar (1h) or 1133-bar (4h) history, so it must hold every entry with insufficient_history; differences against the full-history backtest decisions are the expected bounded-history limitation. Close fractions agree in these runs only because neither side of the decision harness supplies `bars_held`: its position walk is stop-free and `time_stop` holds on both sides, so the simulator's horizon closes (verified separately against real simulator ledgers in the integration tests) are never compared, and the live check has no `bars_held` either. Solo and batched evaluation agree: yes. Complete live parity: no.

## Promotion boundary

Proposed future artifact contract: a frozen, versioned table artifact: market identity (venue, coin, perpetual), universe, timeframe and expected_interval_ms, UTC basis and the 1970-01-05 epoch, training cutoff (last completed label exit open), generation counter, expiry (the next scheduled refit boundary), the full per-cell statistics and a sha256; a live loader must hold on a stale, missing, corrupt or wrong-market table and may replace it only while flat. This study implements neither the loader nor a live holding-horizon close. Unresolved production limits:

- The production Hyperliquid check frame is 200 candles, far below the 4535-bar (1h) training and refit history, so every live or paper check holds with insufficient_history.
- The live close context has no bars_held, so time_stop never fires live; only the protective stop would manage a live position.

## Limits

- Proxy legs use Binance US spot candles; basis, venue wicks and funding-driven price effects are not modelled. The legacy M1 screen has no spread, lot rounding or minimum order, so venue execution claims rest on the venue group only.
- The 2023 proxy window has no 4535-bar prefix and the venue train window has no 1133-bar prefix; both are refused or unscored, never filled. The 2026-08-31 proxy gap holds every later oos decision.
- The venue window covers 2025-09-01 to 2026-09-01 only and earlier studies already used it, so the venue result is retrospective. Spread is a per-coin half spread from one frozen L2 snapshot plus flat slippage; queue position, partial fills and market impact are not modelled.
- Every economic run uses approximate close comparison: `time_stop` reads simulator-only `bars_held`, so no result here is complete live close parity.
- Every score reads only frozen inputs with network acquisition disabled. A research result alone cannot approve deployment.

## Reproduction

```
uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py lock
uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py run --cache-db /tmp/cce1653_run.db
uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py parity --cache-db /tmp/cce1653_parity.db
uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py render
```

Results: `results.json` (source commit `8a12abbb2b833c51e4ba327eb04d755effda8f2f`, runtime 206.5 s, manifest sha256 `50e87869492fc1212d78822ef29dfa6a29a9037b820abee07b52e58d35fa9051`, venue manifest sha256 `9338a5beb52a07093ddc18de07c53c517a4f82d45239504ae7bb5989186daa3d`), `parity_results.json` (source commit `8a12abbb2b833c51e4ba327eb04d755effda8f2f`).
