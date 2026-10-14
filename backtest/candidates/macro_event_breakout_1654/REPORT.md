# macro_event_breakout study (#1654)

Verdict: **INCONCLUSIVE**. Reasons: candidate_full quote coverage 0.0 is below 0.9: no historical bid/ask record backs the spread gate; candidate_full entered on 0 independent official events in held_out, below 30 (the ungated price_only_event diagnostic entered on 3). Live status: production_refused regardless of the verdict; promotion needs a separate reviewed decision.

## Plain simple English

The strategy enters after a scheduled US rate statement when price breaks the range set before it. The study has no historical bid and ask quotes, so the full rule, which needs a spread check, never trades. The sample of statements is also too small. The result is inconclusive, and the strategy stays refused in production.

## Inputs

- Protocol sha256 `a33c975d8014c5da5aa383dde84fa51cfd6a50c0fa222a0139426c7778837969`; source commit `e2710a7ab648275e409e398e9242095e8ee0fb67`.
- Official calendar `backtest/candidates/macro_event_breakout_1654/data/calendar/fomc_calendar.json` sha256 `f5703dccc6ccc184b47303a32326ea5f4dba44930ed11fe82e3a877f624a0f85` (33 records).
- Matched control calendar `backtest/candidates/macro_event_breakout_1654/data/calendar/fomc_matched_control.json` sha256 `7f491d22eade82314fef779433687c34d7fad079f606476ef7f5a3d73748a933` (34 records; research controls, not official releases).
- Quotes: none. No historical Hyperliquid bid/ask archive with per-decision receipt times was available. The l2Book snapshot in the venue manifest is a cost assumption only.
- Proxy: Binance US spot 1h candles and Hyperliquid funding from `backtest/candidates/chande_momentum_1661/data` (hash-pinned in `candidate_spec.json`), `--funding charge` on every leg.
- Venue: Hyperliquid 1h candles, funding, meta and one l2Book snapshot in `backtest/candidates/macro_event_breakout_1654_venue_manifest.json` (schema `offline_candle_manifest/v4`).
- Invocation: `uv run --no-sync python backtest/candidates/macro_event_breakout_1654/run_study.py run`.

## Selection (train, proxy, price_only_event)

Selected parameters: `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 12}`.

| variant | mean return % | mean ddadj | positions | events with entries | funding complete |
|---|---|---|---|---|---|
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 3}` | -3.535 | -0.757 | 13 | 8 | True |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 3}` | -2.935 | -0.878 | 7 | 4 | True |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 3}` | -2.555 | -0.528 | 14 | 8 | True |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 3}` | -1.930 | -0.398 | 11 | 7 | True |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 6}` | -3.380 | -0.751 | 12 | 8 | True |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 6}` | -2.345 | -0.878 | 6 | 4 | True |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 6}` | -2.395 | -0.503 | 13 | 8 | True |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 6}` | -2.565 | -0.878 | 7 | 5 | True |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 12}` | -2.365 | -0.677 | 10 | 7 | True |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 12}` | -1.680 | -0.878 | 5 | 4 | True |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 12}` | -0.620 | -0.141 | 12 | 8 | True |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 12}` | -1.905 | -0.759 | 6 | 5 | True |

## Train window, selected parameters (proxy)

| arm | mean return % | mean Sharpe | worst max DD % | net PnL $ | positions (L/S) | fees $ | funding $ | turnover | largest position share | events with entries | quote coverage | funding complete |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate_full | 0.000 | 0.000 | 0.000 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0 | 0 | 0.000 | True |
| price_only_control | 1.160 | 0.309 | -2.330 | 23.18 | 11 (3/8) | 9.93 | 0.54 | 22.065 | 0.383 | 7 | gate off | True |
| price_only_event | -0.620 | -0.207 | -4.370 | -12.38 | 12 (2/10) | 10.72 | 0.32 | 23.814 | 0.484 | 8 | gate off | True |

## Held-out window (proxy)

| arm | mean return % | mean Sharpe | worst max DD % | net PnL $ | positions (L/S) | fees $ | funding $ | turnover | largest position share | events with entries | quote coverage | funding complete |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate_full | 0.000 | 0.000 | 0.000 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0 | 0 | 0.000 | True |
| price_only_control | -1.450 | -1.421 | -2.360 | -29.01 | 6 (3/3) | 5.36 | -0.09 | 11.903 | 0.598 | 4 | gate off | True |
| price_only_event | -2.680 | -2.420 | -3.470 | -53.69 | 6 (0/6) | 5.37 | 0.01 | 11.937 | 0.411 | 3 | gate off | True |

## Venue window 2026-04-01 to 2026-10-01 (Hyperliquid, manifest costs)

| arm | mean return % | mean Sharpe | worst max DD % | net PnL $ | positions (L/S) | fees $ | funding $ | turnover | largest position share | events with entries | quote coverage | funding complete |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| candidate_full@base | 0.000 | 0.000 | 0.000 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0 | 0 | 0.000 | True |
| candidate_full@stress | 0.000 | 0.000 | 0.000 | 0.00 | 0 (0/0) | 0 | 0.00 | 0 | 0 | 0 | 0.000 | True |
| price_only_control@base | -0.730 | -1.395 | -2.440 | -14.52 | 5 (2/3) | 4.51 | -0.14 | 10.018 | 0.592 | 3 | gate off | True |
| price_only_control@stress | -0.880 | -1.575 | -2.590 | -17.57 | 5 (2/3) | 4.51 | -0.12 | 10.013 | 0.586 | 3 | gate off | True |
| price_only_event@base | -2.855 | -2.562 | -3.780 | -57.12 | 6 (0/6) | 5.37 | 0.01 | 11.928 | 0.404 | 3 | gate off | True |
| price_only_event@stress | -3.035 | -2.643 | -3.930 | -60.69 | 6 (0/6) | 5.37 | 0.01 | 11.926 | 0.400 | 3 | gate off | True |

## Parameter stability (exploratory, held-out, price_only_event, after the freeze)

| variant | mean return % | positions |
|---|---|---|
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 3}` | -2.935 | 9 |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 3}` | -2.730 | 8 |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 3}` | -2.935 | 9 |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 3}` | -2.255 | 9 |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 6}` | -2.625 | 6 |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 6}` | -1.855 | 5 |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 6}` | -2.625 | 6 |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 6}` | -2.515 | 6 |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 12}` | -2.680 | 6 |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 3, "range_bars": 12}` | -1.855 | 5 |
| `{"confirm_bars": 1, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 12}` | -2.680 | 6 |
| `{"confirm_bars": 2, "cooldown_bars": 24, "post_window_bars": 6, "range_bars": 12}` | -2.515 | 6 |

## Costs and their provenance

- Venue leg: `{'maker_fee_pct': 0.00015, 'min_notional_margin': 0.03, 'min_notional_usd': 10.0, 'slippage_bps': 5.0, 'taker_fee_pct': 0.00045}` from the manifest, half spread per coin `{'BTC': 0.06, 'ETH': 0.1997}` bps from one l2Book snapshot, szDecimals lot flooring from the pinned meta, hourly Hyperliquid funding; stress doubles half spread and slippage, fees unchanged.
- Proxy leg: the cached-path platform fee model for Hyperliquid perps and hourly Hyperliquid funding (`--funding charge`); no spread, lot or minimum-size model.
- Unmodeled: per-decision spread (no quotes), queue position, partial fills, latency after the bar close, and event-time liquidity changes beyond the stress multiplier.

## Limits and unmodeled costs

- The full contract needs a supplied bid/ask quote at each decision. None exists for this window, so `candidate_full` holds every event-window decision with `quotes_unavailable`. A fixed spread assumption or the current l2Book snapshot cannot replace that input.
- The 2023 and 2024 tentative schedules name meeting dates but not the 2 p.m. ET statement time. Their records carry no publication time and are never eligible, so only 2025 and 2026 statements count.
- The proxy leg prices Binance US spot candles; it is not Hyperliquid execution. Its fees come from the cached-path platform model; spread, lot and minimum-size rules apply only on the venue leg.
- `price_only_event` and `price_only_control` drop the spread gate. They are diagnostics with identical calendar eligibility, exits, sizing and costs; they cannot produce a pass.
- `time_stop` needs `bars_held`, which the research simulator supplies. Live and paper checks do not supply it, so a runtime position would close only through its stop. The candidate is production-refused, so no runtime position exists.
- Unscheduled FOMC actions (the 2025-08-22 notation vote) are not scheduled releases and are excluded.

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code
