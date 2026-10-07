# Backtest Subsystem Audit — 2026-06 (#906)

Structured audit of correctness, parity, coverage, reporting, and debt across
the backtesting subsystem (engine at the time of audit: 4,659 LOC across the
8 core modules, 20 test files / ~1,180 passing tests). Findings were filed as
cards #942–#945; this doc captures the verified state so future contributors
don't re-derive it. Line numbers reference the audit-time tree — anchor on
symbol names if they drift.

Refreshed 2026-10 against main `6d8c1e49` for issue 1682 finding L.
Corrections are marked inline; the audit-time text stays where it is still
useful history.

## D1 — Look-ahead contract: INTACT

The contract holds for every decision input. Audit-time (2026-06): the
contract lived in the `backtester.py` module docstring. Correction (2026-10,
issue #1724): PR 1466 removed the docstring; the contract now lives in
CLAUDE.md ("Backtest" section, look-ahead bullets) and is pinned by the
regression suite `backtest/tests/test_backtester_lookahead.py`. Verified end
to end:

- `signal`, `_open_action`, `_close_fraction`, `regime` are all `shift(1)`'d
  in the normalization blocks before the per-bar loop reads them; fills use
  the current bar's `open` (contractual).
- Close evaluators run end-of-bar against bar N's close mark and bar N's ATR
  in `_evaluate_close_strategies`. Audit-time (2026-06): their output became
  `pending_close_fraction`, applied at bar N+1's open. Correction (2026-10,
  issue #1724): with `platform=hyperliquid` the engine sets the resting
  take-profit model and sends `tp_model: resting_limit` to the evaluators; a
  tier that the bar close crosses is booked in that same bar at the
  quantity-weighted tier fill price with timing `intrabar_trigger_fill`,
  charged the execution-spec maker fee when one is set, else the platform
  commission (#1576, PR 1590). Other platforms keep the next-open fill.
- `_regime_bar_close` is snapshotted **before** the regime shift and is only
  fed to close evaluators as `market_regime` — bar-N data for an end-of-bar
  decision. Audit-time (2026-06): "matching live". Correction (2026-10,
  issue #1724): issue #1712 measured that live and paper checks decide on the
  forming bar by default; PR 1720 added the opt-in `closed_bar_decisions`
  flag, which moves signals, entry ATR and sizing to the last closed bar.
  Stops, trailing stops, ratchets, take-profits and close evaluators keep the
  current mark, ATR and regime with the flag on or off. The backtest-side
  contract is intact; the parity claim holds only with that qualifier. The
  paper measurement with the flag on is open in issue #1723.
- Same-bar SL re-fire after a post-TP bump (the #715 class) is gated by
  `sl_after_just_applied`.
- SL/TP intra-bar races resolve under `intrabar_resolution`. Correction
  (2026-10, issue #1724): the default mode is `ohlc_walk` (#1271, PR 1292) —
  an armed stop exits inside the bar at the trigger, or at the open on a gap
  through (`_intrabar_sl_fill`), on both the open/close path and the
  plain-signal path. The legacy `bar_close` mode is still available and
  matches the audit-time description: TP evaluators → SL bump → suppressed
  hit check → next-bar fill. Audit-time (2026-06): "races resolve at bar
  close (no OHLC walking)" described the only mode that existed then.
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
| Fees | Matches — `PLATFORM_FEE_PCT` matches `fees.go`; `test_platform_fees.py` scrapes the Go source so drift fails CI. deribit/ibkr/topstep flow through option/futures fee functions, not the spot table. |
| Initial/trailing ATR SL (#885) | Matches — same trigger formula (`entry ± mult×EntryATR`) and same-bar arming on both sides. Correction (2026-10, issue #1724): issue #1684 (PR 1698) made the engine arm the same single stop owner that live arms (trailing, fixed ATR, percent, margin percent with verified leverage, drawdown fallback), with refusals for unsupported owners, pinned by the Go and Python fixture `backtest/testdata/stop_geometry_parity.json`. |
| Default tier ladders (#870/#887) | Matches — values synced across Go and all three Python mirrors, now pinned: `backtest/tests/test_default_tier_ladders.py` reads `defaultHLProtectionTiers()` from `scheduler/hyperliquid_protection.go`, and `backtest/testdata/tp_tier_parity.json` exists. Issue #944 is closed. |
| Single close ref (#842) | Matches — `--config` rejects legacy `len>1` arrays with the same semantics live rejects them; the engine's max-wins multi-ref path remains for direct-constructor/test use only. |
| `tiered_tp_atr_live_regime_dynamic` (#843) | Refused (live-only) — Audit-time (2026-06): "loudly rejected at config load; no evaluator registered under the name." Correction (2026-10, issue #1724): the evaluator is registered in `shared_strategies/close/registry.py`; the backtester refuses it through the central close-capability policy from issue #1683 (PR 1688) — `CLOSE_LIVE_ONLY` in `CLOSE_CAPABILITIES`, enforced by `validate_close_capabilities` in both comparison modes. |
| `regime_directional_policy` (#822/#1025) | Matches — backtested through the per-cycle direction/invert resolver; `--config` requires `regime.enabled=true`, matches live flat/open regime source, and parity diff transforms the same decision layer. Open/close-path entry gate re-resolves after the close leg, so a same-bar full close→reopen uses the current-bar regime (#1025 review). |
| `allowed_regimes` entry-gate (#482/#1025) | Matches — `--config` threads the strategy's `allowed_regimes` into `self.allowed_regimes` (was dropped — only the `--allowed-regimes` CLI flag fed it; CLI flag now rejected alongside `--config`). Backtester models only the legacy single-lookback ADX regime, so an active gate keyed off a named `regime_gate_window` (#792) is rejected at load. |
| Regime-aware `sl_after` (#736) | Refused (live-only) — loudly rejected at `Backtester` init; scalar forms backtestable. |
| `user_defaults.close` / `.regime_atr` (#866/#1135) | Matches — `--defaults system\|user` mirrors the live three-layer resolution; deprecated `user_close_defaults` aliases are accepted only when non-conflicting. |
| Scale-in (#873) | Matches — **#1276** — simulated: the engine ports `perpsScaleInDecision` (caps/spacing gate, decision at bar N's close, fill at N+1's open) and `applyScaleIn` (blend for PnL, frozen `RiskAnchorPrice` for every SL/TP geometry site, `InitialQuantity` growth, per-add taker fee); `--config` threads `allow_scale_in`/`scale_in` and mirrors the live validateConfig rejects. Adds create no Trade rows (live `#T` parity); `results["scale_in_adds"]` counts them. |
| Manual limit orders (#883) | Live-only **by design** (HL perps/manual execution mechanics, no signal-path component). Config keys are ignored by the backtest loader; acceptable while the feature stays execution-side. |
| Lot and minimum sizing (#1716) | Matches — the backtest `execution_spec` lot gate floors quantities to the venue lot size; paper Hyperliquid perps now book venue lot sizes on entries and partial closes (issue #1716, PR 1721, closed). |
| **v13/v14 legacy close keys** | Matches — **#942 (closed)** — Audit-time (2026-06): the `--config` gate admitted pre-v15 configs whose `tiers`/alias keys silently no-op'd in the Python evaluators while live canonicalized them. Correction (2026-10, issue #1724): `--config` now refuses `config_version` below 15 at load. |
| **`regime_window_divergence`** | Refused (live-only) — **#943 (closed)** — still loudly rejected by `--config`; the live short/medium window override is a deliberate live-only surface, not an open gap. |

## D3/D8 — Coverage

- Every registered close strategy has at least one engine-level test except
  the rejected dynamic variant (correct). `trailing_tp_ratchet_regime` and
  `tiered_tp_atr_live_regime` are covered.
- ADX 3-label vocabulary covered. Audit-time (2026-06): "composite 7-label
  vocabulary has zero backtest tests (#944)". Correction (2026-10, issue
  #1724): `backtest/tests/test_backtester_composite_regime.py` and
  `test_backtester_composite_regime_wiring.py` now exist. Issue #944 is
  closed.
- Sharpe annualization tested at 4 timeframes (1d/4h/1h/1w) — meets the ≥3 bar.
- Variant depth: main engine ~strong (incl. shorts), pairs and options
  moderate. Audit-time (2026-06): "theta thin (one force-close test, #944)".
  Correction (2026-10, issue #1724): `backtest/tests/test_backtest_theta_branches.py`
  now covers the theta branches. Issue #944 is closed.
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
- `backtest/ledger_compare.py` (issues #1686, #1700, #1710) is the tool for
  booked-versus-simulated mismatches: it replays the issue #1685 ledger
  export against a simulated run of the same strategy and diffs the engine's
  `ledger_events` row by row.
- Still absent (scope when needed): per-bar verbose trace inside
  `Backtester.run`, equity-curve/trade-log CSV export. Audit-time (2026-06):
  "`exit_reason` and entry/exit regime fields on `Trade`" were absent.
  Correction (2026-10, issue #1724): `Trade` now has `exit_reason`.

## Known live-only surfaces (decision record)

Manual resting limit orders (#883), per-cycle regime hysteresis
(`tiered_tp_atr_live_regime_dynamic`, #843), regime-aware `sl_after` (#736),
and `regime_window_divergence` (#907) are execution/per-cycle mechanics with
no bar-level equivalent yet; the first is silently ignored by design (no
signal path), while the remaining three are loudly rejected. Correction
(2026-10, issue #1724): `tiered_tp_atr_live_regime_dynamic` has a registered
evaluator; the rejection is the central `CLOSE_CAPABILITIES` /
`CLOSE_LIVE_ONLY` policy from issue #1683 (PR 1688), not a missing
evaluator. `scale_in` (#873) left this list in #1276: the engine simulates
add legs with the live gate/blend/frozen-anchor semantics (`allow_scale_in` /
`scale_in` params, `--config`-threaded).

## Known limits

Open parity gaps left by issue #1682, each owned by a follow-up issue (all
open as of this refresh):

- Leverage, margin and liquidation (finding C): the backtester floors equity
  at 0 after the first bust and does not model venue liquidation, and the
  ledger comparison refuses `leverage` above 1 — #1728.
- Ratchet alert geometry (finding D): trail geometry is pinned by #1684, but
  the alert's reported stop price is not measured against the trailing stop —
  #1725.
- Paper funding (finding E): the issue #1682 production run recorded no
  funding events in paper exports while the simulation charges funding —
  #1729.
- Resting take-profit fills (finding F): the backtest books a tier only when
  the bar close crosses it, while a resting venue order can also fill on a
  touch inside the bar; not measured — #1727.
- Fees and slippage (finding H): the production fee tier is not verified and
  paper slippage is not reconciled with recorded fills — #1726.
- Live-strategy comparability: the ledger comparison refuses regime gating
  fields, the directional policy and `margin_per_trade_usd` sizing, and the
  2026-10-06 run found no strict-comparable live Hyperliquid strategy — #1730.
- Funding in backtests outside the comparison tool: Hyperliquid perps
  backtests charge no funding, so their PnL omits a cost live pays — #1731.
- Named regime windows in `run_backtest.py --config`: a named regime gate or
  ATR window is refused, and the directional policy applies to the wrong
  window — #1732.
- Decision timing: live and paper decide on the forming bar by default; the
  opt-in `closed_bar_decisions` flag exists, and the paper measurement with
  the flag on is open — #1723.
