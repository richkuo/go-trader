# Backtest Audit Summary

Issue: https://github.com/richkuo/go-trader/issues/906
Last updated: 2026-06-10

This is the audit record for the backtesting subsystem. It captures verified
invariants, known live-only gaps, and the follow-up work split out from issue
#906.

## Current State

### D1 - Correctness

- The main `Backtester` documents and tests the next-bar execution contract:
  `signal`, `open_action`, column `close_fraction`, and entry-gating `regime`
  values are shifted before the per-bar loop consumes them.
- Close evaluators run at bar close and schedule their `close_fraction` for
  the next bar's open. This is covered by close-strategy and post-TP SL tests.
- Bar-level SL/TP races are intentionally resolved at bar close, not by
  intra-bar OHLC walking.
- A production-code scan found no direct `shift(-1)` forward-peek in
  `shared_strategies/open`, `shared_strategies/close`, `backtest`, or
  `shared_tools/regime.py`; the direct hit is the intentional look-ahead
  regression test.

### D2 - Backtester / Live Parity

- Live-only paths are rejected loudly where parity would otherwise be
  misleading:
  - `tiered_tp_atr_live_regime_dynamic`
  - `regime_directional_policy`
  - legacy `close_strategies` arrays longer than one ref
- Backtests model the co-located open/single-close strategy shape and preserve
  the legacy length-1 `close_strategies` adapter.
- This pass closed a regime parity gap: the backtester and `run_backtest.py`
  can now inject the shared composite regime classifier and live-style
  `regime.windows` specs instead of being limited to the legacy 3-label ADX
  path.
- Still live-only / not modeled:
  - scale-in / pyramiding (follow-up: #932)
  - manual resting limit orders (follow-up: #933)
  - regime divergence direction overrides
  - dynamic regime TP/SL hysteresis
- `backtest/parity_debug.py` now provides the D7.4 trace helper: it can emit a
  per-bar backtest trace and diff the canonical decision columns against a
  paper/live trace CSV.

### D3 - Coverage

- All registered open strategies have `DEFAULT_PARAM_RANGES`; optimize mode no
  longer falls back to single-point grids for any current open strategy.
- All registered close strategy names are mentioned in `backtest/tests`.
- This pass added backtester coverage for all seven composite regime labels:
  `ranging_quiet`, `ranging_volatile`, `ranging_directional`,
  `trending_up_clean`, `trending_up_choppy`, `trending_down_clean`, and
  `trending_down_choppy`.
- `backtest/parity_debug.py` has tests for shifted decision trace generation,
  numeric/string diffs, and missing-row detection.

### D4 - Reporting

- Existing regression coverage pins the #304 fixes for signal-domain
  validation, timeframe-aware Sharpe/volatility in the main backtester, options
  annualized return calendar-days math, HTF filter wiring, and theta force-close
  trade-log entries.
- `PairsBacktester` carries its own `bars_per_year` annualization control.
- Remaining reporting audit work should focus on unifying metric semantics
  across the four backtester variants and deciding whether recovery duration,
  net/gross fee reporting, and equity/trade exports belong in the common report
  surface.

### D8.4 - Prior Bug Regression Map

- #302 correctness bugs: look-ahead is covered by
  `test_backtester_lookahead.py` / `test_backtester_fills.py`; options IV math
  is covered by `test_options_iv_rank.py` and `test_options_vol_math.py`.
- #303 parity gaps: adapter/fee parity is covered by
  `test_options_adapter_parity.py`, `test_platform_fees.py`, regime parity
  tests, and the new `parity_debug.py` trace helper.
- #304 reporting bugs: signal-domain validation, Sharpe/volatility
  annualization, options calendar-day annualized return, HTF filter wiring, and
  theta force-close trade logs are covered by `test_backtest_reporting.py`.
- #715 `sl_after` same-bar SL hit: covered by `test_post_tp_sl.py`.
- #730 look-ahead hardening: covered by `test_backtester_lookahead.py`,
  `test_backtester_fills.py`, and `test_walk_forward_warmup.py`.
- #824 storage import / `ProtectSystem=strict`: covered by
  `shared_tools/test_storage.py`.

## Changes From This Pass

- `Backtester` now accepts `regime_classifier`, `regime_thresholds`,
  `regime_windows_spec`, and `regime_gate_window`, and passes them to
  `shared_tools.regime.ensure_regime_columns`.
- `run_backtest.py` exposes:
  - `--regime-classifier adx|composite`
  - `--regime-thresholds-json`
  - `--regime-windows-spec-json`
  - `--regime-gate-window`
  - composite labels in `--allowed-regimes`
- Walk-forward optimization threads the same regime options into its
  `Backtester` instance.
- `backtester.py` now adds the repo root to `sys.path` before lazy-loading
  close helpers, so direct backtest test invocations can import the
  `shared_strategies` package reliably.
- `Backtester.run(..., trace=True)` returns `debug_trace` rows for per-bar
  parity inspection.
- `backtest/parity_debug.py` emits and diffs backtest traces.

## Open Follow-ups

- #932: Decide whether scale-in is permanently live-only or needs explicit
  bar-level simulation.
- #933: Decide whether manual limit orders are permanently live-only or need
  explicit bar-level simulation.
- #934: Consolidate or document reporting metrics across annualization,
  drawdown recovery, net/gross fees, and export formats for
  main/options/theta/pairs.
- Keep regime divergence and dynamic regime TP/SL as explicit live-only
  constraints unless a future issue scopes reproducible bar-level semantics.

## Verification

- `uv run --extra test pytest backtest/tests`
- `uv run --extra test pytest shared_tools/test_regime.py`
- `uv run --extra test pytest shared_tools/test_storage.py`
