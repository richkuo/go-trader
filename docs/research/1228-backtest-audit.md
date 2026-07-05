# #1228 — End-to-end audit of the backtest system (look-ahead, live parity, fee/metric fidelity)

Audit of the backtest engine and harnesses at HEAD `d5840bd` (2026-07-05), per issue #1228.
Six parallel audit passes (look-ahead, fill/accounting, live-vs-backtest parity, metrics
math, M-series statistics, registry conformance); every finding re-verified against code
before any fix landed. Baseline before changes: `backtest/tests/` 1069 passed / 1 skipped;
remaining pytest suite 1234 passed.

## Per-subsystem verdicts

### 1. Look-ahead — CLEAN except one leak (fixed)

- The three #1153/#1154 HTF `.shift(1)`-before-`reindex` fixes (`_htf_trend_series`,
  `_aligned_regime_columns`, `_profile_label_series`) are intact and test-pinned.
- Signal shift, regime-gate N−1 read, close-evaluator closed-bar context (ATR/zscore/avwap),
  funding `merge_asof(direction="backward")` + `searchsorted(side="right")` accrual,
  `mtf_confluence`/`regime_adaptive_htf` in-frame resample visibility, and
  `analog_retrieval` eligibility windows: all verified SAFE with existing regression tests.
- **FIXED — EntryATR stamped from the fill bar's still-forming ATR** (`_stamp_entry_atr`
  read `atr_series.loc[fill_bar]`, whose value incorporates that bar's own high/low/close,
  unknown at the open where the entry fills). Live stamps the last closed bar's ATR at
  order time. One-bar, geometry-only leak (never creates/blocks a signal) that
  systematically flattered ATR-stop/TP stacks on volatile entry bars. Now reads the bar
  before the fill; a first-bar fill has no closed prior bar and stamps 0 (evaluators
  no-op, matching the existing no-usable-ATR convention). Regression:
  `test_backtester_lookahead.py::test_entry_atr_stamped_from_bar_before_fill`.
  Constant-ATR scenarios (most tests, and any run where ATR moves slowly) are unaffected;
  results with fast-moving ATR at entry may shift slightly, in the conservative direction.
- Not changed (documented conventions): trailing-trigger HWM seeded from the fill bar's
  close is self-cancelling (the same bar's end-of-bar walker would raise the HWM to that
  close anyway); the `_regime_bar_close` fallback branch when `regime_enabled` is false is
  unreachable via the shipped `--config` wiring.

### 2. Fill/accounting model — CLEAN (no fixes)

- Signal-at-N-fills-at-N+1-open verified on every entry/exit/flip/partial path; fees per
  leg mirror `fees.go` (parity-scraped test); funding sign/lag pinned; partial-close
  quantity/avg-cost/`initial_quantity`/dust math correct; #1005 sticky liquidation floor
  and its propagation through `eval_windows.py`/`fee_audit.py` fully verified and
  test-pinned. Leverage is explicitly not modeled (fail-loud at load), not a silent gap.
- Documented conventions, left as-is: funding books at loop-top (exit-bar charged,
  entry-bar skipped — pinned as intended); short-entry commission based on committed
  margin (~fee² divergence); per-trade `pnl` is gross of the entry fee (win-rate /
  profit-factor classification only; cash/equity accounting is exact) — follow-up filed.

### 3. Live-vs-backtest parity — two drifts (fixed), two latent gaps (fixed), one design gap (follow-up)

Close-evaluator math is shared (the backtester dispatches through the same
`shared_strategies/close/registry.py` the live check scripts use), so drift lives in
input assembly and parsing mirrors:

- **FIXED — unified #841 per-regime close block lost its stop-loss in backtest.** Live,
  the block is the sole SL owner (`validateUnifiedCloseSoleOwner`) and
  `unifiedCloseStopLossATR` arms the per-regime stop; the backtester discarded the SL the
  shared helper returns and no other owner could exist — a unified close with
  `stop_loss_atr` backtested with **no stop at all**, inflating results. The backtester
  now resolves the stamped regime's `stop_loss_atr` as the run's fixed-SL mult and
  rejects any second strategy-level SL owner, mirroring both Go behaviors.
- **FIXED — `parse_regime_tp_tiers` didn't strip `sl_after`** before the ATR-block
  allowlist parse (Go strips `close_fraction` AND `sl_after`; the CLAUDE.md guardrail was
  never ported to the Python mirror). A per-tier `sl_after` on a regime tiered close
  loaded in Go but errored in Python — failing backtest load AND silently never arming on
  the live Python fire path, which re-parses through the same helper.
- **FIXED — `parity_diff.py` never injected `user_defaults`** (live applies them
  unconditionally at loadConfig), so it could report CLEAN against effective params live
  never runs. Now loads with `inject_user_defaults=True`.
- **FIXED — backtest directional-policy lookup missed the live bare→sub fallback**
  (`Resolve` in `regime_directional_policy.go` lets a bare `ranging_directional` policy
  entry cover `_up`/`_down` stamps, exact match winning first; cert gating stays exact
  per #1124). Inert while the cert artifact is empty, but a real mirror gap once any cert
  lands. The backtest gate now expands a bare entry onto uncovered subs before the
  per-state cert check — so a sub honors the bare override only when that sub's OWN
  certification passes; runtime resolution against the gated policy stays exact-match,
  matching live's fail-closed `gatedDirectionalEntry` (a bare cert never certifies an
  uncertified sub). The shared `unified_regime_scalar_params` helper also gained the
  live bare→sub fallback (review round 1) so a bare-only unified close block arms its
  SL/TPs for a directional sub-label stamp.
- Verified PARITY: regime gating (#1025/#1124 one-directional family rule), #1085
  evidence gate (default-off, fail-closed, exact cert lookup), reject list otherwise
  exhaustive against `closeStrategyOwnedKeys` (mirror-enforced by Go test), default
  ladders vs `defaultHLProtectionTiers`/`ratchetTierGroupDefaults`, #1135 user-defaults
  injector mirror itself.
- **Follow-up (design decision):** backtest `--defaults` defaults to `system` while live
  applies `user_defaults` unconditionally — a default `--config` run can silently use
  different effective tiers than live. Flipping the default changes existing baselines,
  so it needs its own decision. Documented reverse drift (unchanged): `time_stop` /
  `zscore_target` fire in backtest but live check scripts don't pass their inputs yet.

### 4. Metrics — one ranking bug (fixed), two degenerate-input follow-ups

- Sharpe/Sortino annualization, max-drawdown, #1005 sticky floor + sentinel overrides,
  Calmar/annual-return guards: verified correct and test-pinned.
- **FIXED — DDadj not floored for liquidated legs.** A blown leg reads return −100% /
  DD −100%, so raw DDadj = −1.0 — outranking a surviving losing leg (−50%/25% DD =
  −2.0) in M1 `ddadj`/`beats_ddadj` and in the optimizer's `dd_adjusted_return` metric.
  Both consumers now floor liquidated legs to −100 (mirroring the #1005 Sharpe
  sentinel), with a constant-sync test against `LIQUIDATED_METRIC_FLOOR`.
- **Follow-ups:** Sortino uses sample-std of negative returns about their own mean and
  needs ≥2 losing bars (a near-perfect leg scores a neutral 0.0) — changing the
  definition re-baselines every documented study, so it needs its own pass;
  `profit_factor = inf` on all-win legs would emit nonstandard JSON `Infinity` if ever
  serialized (currently not forwarded to any JSON artifact).

### 5. M-series validators + auto_suggest statistics — CLEAN (no fixes)

- Benjamini–Hochberg: pooled once over the whole candidate family, correct step-up
  formula, noise-family dedup keyed by family (siblings can't skip the downgrade),
  `--only` runs stamped EXPLORATORY. Verified with existing tests.
- `gross_edge_noise` sign-flip permutation (one-sided, add-one smoothing), the #1054
  noise-before-selectivity ordering (enforced in `auto_suggest`), M6 exactly-one rule
  (enforced at both spec load and `exit_policy_ab`), bootstrap/Wilcoxon/sign-test
  implementations, and determinism (seeded stdlib `Random`): all correct.
- Minor noted, not a defect: adjacent M1 windows share their boundary bar because
  `load_ohlcv` uses an inclusive end bound while `gross_edge_noise` documents
  `[start, end)` — one bar of equity per boundary, nil trade-level effect by the
  N→N+1 fill construction. Tracked as a follow-up alongside the loader convention.

### 6. Registry/doc conformance — CLEAN (no code changes)

- Every executable `backtest/` script (25 root + 10 research) has a current registry
  row; no orphan or materially stale rows; documented run commands match argparse.
- Minor notes: `backtest/test_consolidation_research.py` sits at `backtest/` root
  outside `tests/`; `backtest/AUDIT.md`, `backtest/THETA_HARVEST_RESULTS.md`, and two
  in-tree `research/README_*.md` writeups fall outside the registry's declared
  results locations; `auto_suggest.py` is listed under the M-series table though it's a
  cross-harness driver. None affect correctness.

## Baseline impact

The EntryATR fix (and the unified-SL fix, for any config using a unified block) can shift
historical results where ATR moved sharply between the signal bar and the fill bar; the
DDadj floor changes rankings only for runs containing liquidated legs. Whether any
documented study verdict (#983/#984/#1054/#1152/#1211, #1181 baselines) moves under the
corrected geometry has NOT been re-measured here — re-running those studies is tracked as
a follow-up. Any future re-run will reflect the corrected (closed-bar, generally more
conservative) stop geometry.
