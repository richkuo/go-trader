# #1277 — Wilder ATR cutover: roster, baseline impact, and re-check list

**Status at merge: gate OFF.** `atr_method` defaults to `"simple"` everywhere —
the frozen legacy rolling mean with the #887 `>= 100` integer rounding. Every
live check script, the backtester, and strategy discovery are byte-identical to
pre-#1277 behavior until an operator explicitly sets `atr_method: "wilder"`
(global or per-strategy) and restarts / SIGHUPs while flat. Verified in-branch:
a `run_backtest.py` single run without the flag diffs byte-identical against an
explicit `--atr-method simple` run, and the `--list-json` discovery snapshot is
unchanged against `main`.

## Cutover roster — what follows `atr_method`, what stays frozen

### Follows the config gate (the `standard_atr` surface)

| Surface | Site | Notes |
|---|---|---|
| EntryATR stamping | all 5 check scripts (`ensure_atr_indicator`) | Only when the open strategy emits **no** `atr` column — a strategy-emitted column always wins (see below) |
| Live close-evaluator ATR | all 5 check scripts (`latest_atr` → `market_ctx["atr"]`) | Always standard_atr (computed on the raw OHLCV frame) — gated for every strategy |
| Manual-open fetched ATR | `check_hyperliquid.py --fetch-atr` (#689) | Go forwards the strategy's resolved method so manual EntryATR matches check-cycle stamping |
| Backtester injected ATR | `Backtester(atr_method=…)`, `run_backtest.py` ensure_atr sites | `--config` reads the config's `atr_method`; CLI `--atr-method` drives config-less runs (single/compare/multi; optimize rejects it) |
| Tuner simulate preview | `simulate_strategy.py` via the Go-stamped payload key | Go resolves per-strategy > global so the preview matches the live cycle |

### Stays frozen (deliberately NOT config-driven)

| Surface | Why frozen |
|---|---|
| **Regime classification** (`shared_tools/regime.py`, 3 sites pinned `method="simple"`) | Composite thresholds and the #1085 directional certifications were calibrated on simple-mean `atr_pct`. Letting wilder flow into labels would silently invalidate every certification. Re-run the bounded-window / regime-promotion validation before ever unpinning. |
| **Strategy-internal ATR** (supertrend, squeeze_momentum, breakout, atr_breakout, order_blocks, session_breakout, sweep_squeeze_combo, chart_patterns, anchored_vwap family, atr_band_revert, regime_adaptive, regime_adaptive_htf, momentum_pro, vol_momentum, analog_retrieval) | These sites call `indicators_core` with hardcoded per-strategy conventions (incl. the `round_large=False` Keltner/supertrend split) inside their cores; their tuned/validated parameters assume the current math. Each would need its own re-validation to switch — a per-strategy decision, not a fleet gate. A strategy-emitted `atr` column also wins over injection at stamping time, so these strategies' EntryATR stays on their own math either way. |
| **M-harness engines** (`eval_windows.py`, `optimizer.py`) | Construct their own engines on the default; `run_backtest.py --mode optimize` rejects `--atr-method` rather than silently sweeping under changed math. Use single/compare/multi modes for wilder runs. |

## Measured wilder-vs-simple delta (representative run)

`mean_reversion_pro` (no strategy-emitted `atr` → injection gated), BTC/USDT 1h
since 2025-01-01 (13,337 candles to 2026-07-10), `tiered_tp_atr`
[2.0×/0.5, 3.0×/1.0] + `stop_loss_atr_mult 2.0`, ohlc_walk:

| Metric | simple | wilder |
|---|---|---|
| Total return | -6.14% | -7.40% |
| Sharpe | -0.412 | -0.489 |
| Max drawdown | -15.15% | -14.05% |
| Trades | 49 | 46 |
| Win rate | 51.0% | 47.8% |
| Profit factor | 0.792 | 0.756 |

Entries are identical (the open strategy is untouched); every delta is exit
geometry — wilder's smoother, slower ATR sets different stop/TP distances and
occasionally merges or splits re-entries. The point of committing this run is
the *magnitude*: same-entry verdicts move by whole percentage points of return
and ~3pp of win rate, i.e. **ATR-exit study verdicts do not automatically
transfer between methods.**

## Study verdicts that must be re-checked before being cited under wilder

Existing documented verdicts remain valid **for simple** — the default at
merge — and nothing needs re-running while the gate is off. Any future
promotion decision made *under wilder* must not cite the following without a
re-run under `--atr-method wilder`, because their harnesses wired
standard-ATR-derived exits (`tiered_tp_atr*`, `atr_stop`, scalar/trailing ATR
stops):

- `docs/research/1031-session-breakout-short-m1.md` — M1 incumbent-relative validation under ATR-tiered exits
- `docs/research/1152-ranging-exit-geometry-m6.md` — M6 regime exit A/B; the exit geometry under test IS ATR-derived
- `docs/research/1166-momentum-pro-directional-gate.md` — gate study scored under ATR exits
- `docs/research/1228-backtest-audit.md` — audit slices that pinned ATR-exit behavior
- `docs/research/consolidation-findings.md` — consolidation exits parameterized in ATR multiples
- `docs/research/fee-audit-m5.md` + `docs/research/1315-fee-rescreen-m5.md` — M5 verdicts where the close leg was ATR-tiered; gross-edge classifications are entry-dominated but the fee-per-exit counts shift with exit cadence
- Generally: **any `docs/backtesting-registry.md` row whose run passed `--config` with a `tiered_tp_atr*`/`atr_stop` close or a scalar/trailing ATR stop** on an open strategy that emits no `atr` column.

Unaffected (verified): `docs/research/1181-htf-filter-baseline-revalidation.md`
— its runs exit open-signal-as-close with no close evaluator, so no ATR series
is ever injected; the corrected HTF baselines stand under either method.
Strategy-internal-ATR studies (supertrend/Keltner parameter sweeps) are also
unaffected because those sites are frozen (roster above).

## Operator cutover procedure

1. Flatten the strategy (the hot-reload guard blocks the flip while any
   position is open — EntryATR/frozen stop geometry must not be re-based).
2. Set `atr_method: "wilder"` (per-strategy, or global to cut the fleet over).
3. SIGHUP or restart. The startup summary tags affected strategies `atr=wilder`;
   `inspect` shows the resolution source.
4. Re-establish any backtest baseline you intend to cite for that strategy
   under `--atr-method wilder` (or `--config` once the config carries it).

The wilder path never applies the `>= 100` integer rounding; the simple path
keeps it frozen (#887) so historical baselines stay reproducible.

---
Created with LLM: Fable 5 | high | Harness: Claude Code
