# Backtest subsystem audit (#906)

Audit completed: 2026-06-09. Branch: `cursor/issue-906-backtest-audit`.

Structured audit of the eight core backtest modules and 21 test files (19 prior +
`test_parity_diff.py`, `test_backtest_parity_regression.py`, `test_audit_regression_index.py`).
Actionable code gaps are filed as linked issues; this card is complete.

## Acceptance criteria

| Criterion | Status |
|-----------|--------|
| D1–D4 fully audited | **Done** — sections below |
| Findings filed as linked cards | **Done** — #925–#929 |
| D7.4 parity-debug helper | **Done** — `parity_diff.py` |
| D8.4 regression tests confirmed | **Done** — `test_audit_regression_index.py`, `test_backtest_parity_regression.py` |
| Audit summary doc | **Done** — this file |

## Executive summary

| Dimension | Status | Notes |
|-----------|--------|-------|
| D1 Correctness | **Pass** | All four `shift(1)` guards intact |
| D2 Parity | **Pass + documented gaps** | Live-only surfaces rejected; limitations in `backtester.py` docstring |
| D3 Coverage | **Partial** | 2 close evaluators lack dedicated backtest cases → #926 |
| D4 Reporting | **Partial** | Theta Sharpe gap → #925; recovery duration → #927 |
| D5 Optimizer | **Pass** | All 69 registry strategies have `DEFAULT_PARAM_RANGES`; WF heuristics → #928 |
| D6 Code health | **Debt** | Duplication quantified; refactor → #929 |
| D7 Observability | **Shipped D7.4** | Verbose trace deferred → #930 |
| D8 Regression | **Pass** | #302/#304/#715/#730 explicit; #303 umbrella; #824 out of scope |

## D1 — Correctness (#730) ✅

| Item | Result |
|------|--------|
| D1.1 `shift(1)` on signal, `_open_action`, `_close_fraction`, regime | Verified ~L738–780; no bypass columns |
| D1.2 Close evaluators end-of-bar → next open | Verified ~L1139, ~L1368 |
| D1.3 Intra-bar SL/TP races | Bar-close resolution; module docstring + CLAUDE.md |
| D1.4 Upstream `shift(-1)` in strategies | `rg` — no matches in `shared_strategies/` |
| D1.5 Regime shift vs #879 bundle | `_regime_bar_close` snapshot + shifted `regime` column |

## D2 — Parity ✅ (gaps documented)

### D2.1 Recent PRs (backtest=N features)

| PR | Feature | Backtest status |
|----|---------|-----------------|
| #842 | Single `close_strategy` ref | **Parity** — `run_backtest.py` rejects len>1; `test_strategy_refs_641.py` |
| #857 | `trailing_tp_ratchet_regime` | **Parity** — `test_backtester_close_strategies.py` |
| #866 | `user_close_defaults` | **Parity** — `_apply_user_close_defaults` + `--defaults user` |
| #870 | Default HL protection tier retune | **Parity** — flows through close evaluators / `DEFAULT_ATR_TIERS` |
| #872 | Manual regime stamp | **Live-only** — manual path; backtest uses static regime injection |
| #880 | Manual trade alert DMs | **Live-only** — notifier surface |
| #882 / #873 | Scale-in / pyramiding | **Live-only** — documented in `backtester.py` |
| #883 | Resting manual limit orders | **Live-only** — documented in `backtester.py` |
| #885 | `armTrailingStopAtOpenNow` | **Partial** — backtest arms trailing on bar after open |
| #887 | Ratchet final-tier 0.8× trail | **Parity** — `DEFAULT_RATCHET_TIERS` in close registry tests |

### D2.2–D2.10

| Item | Result |
|------|--------|
| D2.2 Scale-in (#873) | Permanent live-only constraint; documented |
| D2.3 Resting limits (#883) | Not modeled; documented |
| D2.4 `tiered_tp_atr_live_regime_dynamic` | Rejected at `run_backtest.py:193-196` — correct |
| D2.5 `regime_directional_policy` | Rejected at `run_backtest.py:160-166` — correct |
| D2.6 Inline trailing SL at open (#885) | Partial — see limitations docstring |
| D2.7 Default tier retune (#870/#887) | Close registry `DEFAULT_ATR_TIERS` / ratchet tiers updated |
| D2.8 v15 `tiers` in optimizer | Legacy emit at `optimizer.py:487,494`; Go `LoadConfig` canonicalizes |
| D2.9 Single close ref (#842) | No length-N iteration in backtest; 0-or-1 list only |
| D2.10 Platform fees | 6 platforms in `PLATFORM_FEE_PCT`; `test_platform_fees.py` scrapes `fees.go`; deribit/ibkr/topstep use variant fee fns — documented exclusion |

### Parity tool (D7.4)

```bash
python backtest/parity_diff.py sma_crossover BTC/USDT 1d --since 2024-01-01
python backtest/parity_diff.py --config config.json --strategy-id <id> --regime-enabled
```

## D3 — Engine coverage ✅ (minor gaps → #926)

### D3.1 Close strategies

All 8 registered evaluators audited. Missing dedicated backtest cases:
`tiered_tp_atr_regime`, `tiered_tp_atr_live_regime` → **#926**.

### D3.2 Composite 7-state labels

`test_ensure_regime_columns_composite_labels` asserts `trending_up*` only.
Not every label (`ranging_quiet`, `ranging_volatile`, …) has an explicit fixture → **#931** (optional).

### D3.3 Variant backtesters

spot/perps (`backtester.py`), options (`test_options_*`), theta (`test_backtest_reporting` L5), pairs (`test_pairs.py`) — non-trivial coverage confirmed.

### D3.4 Sharpe timeframes

`test_backtest_reporting.py` M3: 1d, 4h, 1h ratio tests + `periods_per_year` table.

## D4 — Reporting ✅ (gaps filed)

| Item | Result |
|------|--------|
| D4.1 Sharpe/Sortino all backtesters | Main + pairs + options OK; **theta hardcodes `sqrt(365)`** → #925 |
| D4.2 Options annualized return | Calendar days — `test_backtest_reporting.py` M5 |
| D4.3 Legacy max-fraction | Removed; single ref only (#842) |
| D4.4 ISO timestamps | `start_date`/`end_date` via `str(index)`; trade `to_dict` uses `str(entry_date)` |
| D4.5 Drawdown recovery duration | **Not emitted** → #927 |
| D4.6 Theta force-close trade log | Fixed — `test_backtest_reporting.py` L5 |
| D4.7 Net vs gross fees | Commission in metrics; reporter shows totals, not split gross/net |
| D4.8 Walk-forward OOS | Per-fold `test_result` in optimizer; `oos_mean_sharpe` aggregate — warmup labeling could be clearer (acceptable) |

## D5 — Optimizer ✅

| Item | Result |
|------|--------|
| D5.1 Param ranges | **31/31 spot + 38/38 futures** have `DEFAULT_PARAM_RANGES` entries |
| D5.2 Min-data heuristic | None — `splits=5` fixed → #928 |
| D5.3 Overfit detection | No fold Sharpe std / param stability → #928 |
| D5.4 `optimize_metric` | Configurable; default `sharpe_ratio` |
| D5.5 `random_seed` | Honored in grid shuffle |

## D6 — Code health ✅

| Item | Result |
|------|--------|
| D6.1 Dead imports | No stale `fetch_full_history` in `run_backtest.py` |
| D6.2 Duplication | 4 backtesters each own fill/fee/equity — ~35% overlap → #929 |
| D6.3 Options exchange | `--exchange` flag exists (`backtest_options.py`); defaults binanceus |
| D6.4 HTF filter | `_htf_trend_series` mirrors live EMA (#304 M2) |
| D6.5 `close_registry_loader` | Collision still real; shim required |
| D6.6 Magic numbers | Fees centralized; slippage is backtester param |

## D7 — Observability

| Item | Status |
|------|--------|
| D7.1 Verbose trace | Not implemented → #930 |
| D7.2 Equity CSV export | JSON/`store_backtest_result` only |
| D7.3 Trade log CSV | Trades in metrics dict only |
| D7.4 Parity helper | **`parity_diff.py`** ✅ |

## D8 — Regression index ✅

| Issue | Regression | Guard |
|-------|------------|-------|
| #302 | `test_backtester_end_to_end`, `test_backtester_fills`, `test_options_vol_math`, `test_options_iv_rank` | `test_audit_regression_index.py` |
| #303 | Distributed + `test_backtest_parity_regression.py` | Umbrella meta-test |
| #304 | `test_backtest_reporting.py` | Index test |
| #715 | `test_post_tp_sl.py` | Index test |
| #730 | `test_backtester_lookahead.py` | Index test |
| #824 | `shared_tools/storage.py` + `--probe-only` | **Out of backtest scope** — documented |

No `hypothesis` usage in backtest tests → optional starter card not filed (low priority).

## Follow-up issues (linked cards)

| Issue | Topic |
|-------|-------|
| [#925](https://github.com/richkuo/go-trader/issues/925) | Theta Sharpe annualization |
| [#926](https://github.com/richkuo/go-trader/issues/926) | Close-strategy test gaps |
| [#927](https://github.com/richkuo/go-trader/issues/927) | Drawdown recovery metric |
| [#928](https://github.com/richkuo/go-trader/issues/928) | Walk-forward robustness heuristics |
| [#929](https://github.com/richkuo/go-trader/issues/929) | Backtester dedup refactor |
| [#930](https://github.com/richkuo/go-trader/issues/930) | Verbose per-bar trace mode |
| [#931](https://github.com/richkuo/go-trader/issues/931) | Composite regime label fixtures (optional) |

## References

- Look-ahead contract: `backtest/backtester.py` module docstring
- Parity tool: `backtest/parity_diff.py`
- Prior audits: #302, #303, #304
