# Backtest Subsystem Audit — 2026-06 (#906)

Structured audit of correctness, parity, coverage, reporting, and debt across
the backtesting subsystem (engine at the time of audit: 4,659 LOC across the
8 core modules, 20 test files / ~1,180 passing tests). Findings were filed as
cards #942–#945; this doc captures the verified state so future contributors
don't re-derive it. Line numbers reference the audit-time tree — anchor on
symbol names if they drift.

## D1 — Look-ahead contract: INTACT

The contract (`backtester.py` module docstring) holds for every decision
input. Verified end to end:

- `signal`, `_open_action`, `_close_fraction`, `regime` are all `shift(1)`'d
  in the normalization blocks (`backtester.py:738, 748, 749, 780`) before the
  per-bar loop reads them; fills use the current bar's `open` (contractual).
- Close evaluators run end-of-bar against bar N's close mark and bar N's ATR
  (`_evaluate_close_strategies`, dispatched at `backtester.py:1139`); their
  output becomes `pending_close_fraction`, applied at bar N+1's open.
- `_regime_bar_close` is snapshotted **before** the regime shift
  (`backtester.py:762`) and is only fed to close evaluators as
  `market_regime` — bar-N data for an end-of-bar decision, matching live.
- Same-bar SL re-fire after a post-TP bump (the #715 class) is gated by
  `sl_after_just_applied` (`backtester.py:1175`).
- SL/TP intra-bar races resolve at bar close (no OHLC walking), documented in
  the contract and deterministic: TP evaluators → SL bump → suppressed hit
  check → next-bar fill.
- Strategy sweep: no `shift(-n)`, `center=True` rolling, or forward `iloc`
  in `shared_strategies/open/registry.py` or `shared_strategies/close/*.py`.
  (Full-frame normalization — e.g. a signal keyed on the whole series' mean —
  is *not* statically detectable; that class is what `parity_diff.py` exists
  to catch.)
- Composite (#861/#862) and shared-regime-bundle (#879) work did not bypass
  the shift: only the `regime` column is a decision input; `adx`/`regime_score`
  /±DI columns are display-only.

Regression guards: `test_backtester_lookahead.py`, `test_post_tp_sl.py`
(same-bar fire), `test_backtester_regime.py` (gate timing + live↔backtest
label parity).

## D2 — Parity with the live scheduler

| Surface | State |
|---|---|
| Fees | ✅ `PLATFORM_FEE_PCT` matches `fees.go`; `test_platform_fees.py` scrapes the Go source so drift fails CI. deribit/ibkr/topstep flow through option/futures fee functions, not the spot table. |
| Initial/trailing ATR SL (#885) | ✅ Same trigger formula (`entry ± mult×EntryATR`) and same-bar arming on both sides. |
| Default tier ladders (#870/#887) | ✅ Values synced across Go and all three Python mirrors — but unpinned by tests (#944). |
| Single close ref (#842) | ✅ `--config` rejects legacy `len>1` arrays with the same semantics live rejects them; the engine's max-wins multi-ref path remains for direct-constructor/test use only. |
| `tiered_tp_atr_live_regime_dynamic` (#843) | ✅ Loudly rejected at config load (`run_backtest.py:193-196`); no evaluator registered under the name. |
| `regime_directional_policy` (#822/#1025) | ✅ Backtested through the per-cycle direction/invert resolver; `--config` requires `regime.enabled=true`, matches live flat/open regime source, and parity diff transforms the same decision layer. |
| Regime-aware `sl_after` (#736) | ✅ Loudly rejected at `Backtester` init; scalar forms backtestable. |
| `user_close_defaults` (#866) | ✅ `--defaults system\|user` mirrors the live three-layer resolution. |
| Scale-in (#873), manual limit orders (#883) | Live-only **by design** (HL perps/manual execution mechanics, no signal-path component). Config keys are ignored by the backtest loader; acceptable while the features stay execution-side. |
| **v13/v14 legacy close keys** | ❌ **#942** — the `--config` gate admits pre-v15 configs whose `tiers`/alias keys silently no-op in the Python evaluators while live canonicalizes them. |
| **`regime_window_divergence`** | ❌ **#943** — still loudly rejected by `--config`; no backtest resolver yet for the live short/medium window override. |

## D3/D8 — Coverage

- Every registered close strategy has at least one engine-level test except
  the rejected dynamic variant (correct). `trailing_tp_ratchet_regime` and
  `tiered_tp_atr_live_regime` are covered.
- ADX 3-label vocabulary covered; **composite 7-label vocabulary has zero
  backtest tests** (#944).
- Sharpe annualization tested at 4 timeframes (1d/4h/1h/1w) — meets the ≥3 bar.
- Variant depth: main engine ~strong (incl. shorts), pairs and options
  moderate, **theta thin** (one force-close test, #944).
- `DEFAULT_PARAM_RANGES` completeness is enforced by
  `test_registry_loader.py::test_param_ranges_cover_every_registered_strategy`.
- No property-based (`hypothesis`) tests anywhere; grid search is fully
  deterministic so seed plumbing is moot.
- Regression tests exist for every prior backtest bug: #302 (fills + vol
  math), #303 (regime/BS parity), #304 M3/M5/L5 (annualization, calendar
  days, theta force-close log), #715 (same-bar SL), #730 (look-ahead),
  #824 (storage lazy-init).

## D4/D5 — Reporting & optimizer

- `periods_per_year()` drives Sharpe/volatility in the main engine; options
  uses check-interval-aware sampling; pairs uses `bars_per_year`. Theta
  hardcodes daily — valid only because theta data is always `1d` (#945).
- Options annualized return uses elapsed calendar days (the #304 M5 fix is in
  and commented).
- Walk-forward separates train/test per fold and aggregates OOS-only stats
  (`oos_mean_*`, `oos_std_return`). The boundary-bar inclusion at fold start
  is **intentional** (its raw signal becomes row 1's shifted signal — matches
  live) and is regression-tested in `test_walk_forward_warmup.py`; don't
  "fix" it.
- Optimizer is a deterministic grid; `optimize_metric` accepts any result-dict
  key. No fold-to-fold robustness gate (param stability is reported via
  `most_common_best_params`, not enforced).
- Known gaps, not bugs: drawdown has no recovery-duration metric; the main
  engine doesn't emit a separate `total_fees`; timestamps serialize as
  `str(pd.Timestamp)` (space separator), not strict ISO-8601.

## D6 — Debt

- `close_registry_loader` shim is still required (open and close registries
  both resolve as module name `registry`).
- `backtest_options.py` exchange is configurable (`--exchange`, #304 L2 fixed).
- HTF filter plumbs through the same `shared_tools/htf_filter.py` as live,
  fed by cached candles; missing HTF cache fails open to neutral trend.
- Residual items in **#945**: unused imports, dead close-name optimizer range
  entries, theta daily-only comment.
- Metrics/reporting logic is re-implemented per variant (~150–200 LOC
  duplication across options/theta/pairs); extraction is optional until one
  of them next drifts.

## D7 — Observability

- `parity_diff.py` (added by this audit, D7.4) replays the vectorized
  backtest path and the trailing-window live path over the same candles and
  emits a per-bar diff of signal / open_action / close_fraction / regime —
  the tool to reach for first on any backtest-vs-paper mismatch. The live
  side calls the actual check-script helpers (`prepare_check_regime` →
  `evaluate_open_close`/`finalize_decision` with
  `close_registry_loader.evaluate`), not a model of them; registry close
  evaluators are compared through the same evaluator on both sides with a
  shared simulated position context, so a diff isolates window-derived
  inputs (trailing-window ATR/regime vs full-frame). `--config
  <live-config> --strategy-id <id>` replays the exact live refs via the
  #641 loader; `--fills` lines the decision diff up against the engine's
  simulated entry/exit fills; `--csv`/`--jsonl` dump the full frame, which
  also carries the post-`shift(1)` `backtest_effective_*` engine inputs per
  bar. See the module docstring for usage.
- Still absent (scope when needed): per-bar verbose trace inside
  `Backtester.run`, equity-curve/trade-log CSV export, `exit_reason` and
  entry/exit regime fields on `Trade`.

## Known live-only surfaces (decision record)

`scale_in` (#873), manual resting limit orders (#883), per-cycle regime
hysteresis (`tiered_tp_atr_live_regime_dynamic`, #843), regime-aware
`sl_after` (#736), and `regime_window_divergence` (#907) are execution/per-cycle
mechanics with no bar-level equivalent yet; the first two are silently ignored
by design (no signal path), while the remaining three are loudly rejected.
