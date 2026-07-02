# Pre-#1180 `--htf-filter` baseline re-validation — inventory + corrected baselines (#1181)

PR #1180 (merged to `main` as `952144a`, closes #1154) fixed a look-ahead leak
in `_htf_trend_series` (`backtest/run_backtest.py`): the HTF trend was
reindex-ffilled onto LTF bars without the one-HTF-row `.shift(1)`, so every
LTF bar inside a still-forming HTF candle saw that candle's final close. Every
`run_backtest.py --htf-filter` result produced before that merge is inflated.
This document is the #1181 deliverable: a written inventory of every recorded
pre-fix `--htf-filter` result, before/after diffs re-measured on identical
cache data, and an explicit disposition for each affected decision.

**Verdict: the inventory is empty beyond PR #1180's own before/after table.**
No research doc, issue thread, or recorded optimizer/compare output carries a
pre-fix `--htf-filter` number, and no promotion, deprecation, or
capital-allocation decision traced to one. The corrected (post-fix) baselines
are recorded below and reproduce PR #1180's "after" column exactly.

## Inventory (acceptance criterion 1)

Sweep performed at `952144a`, 2026-07-01. Method: repo-wide grep of `docs/`,
`README.md`, and all tracked markdown for `--htf-filter` / `htf_filter` /
`htf-filter`; GitHub search of all issues and PRs in `richkuo/go-trader` for
`htf-filter` (16 issue hits, 18 PR hits), with every hit's body and full
comment thread grepped for the literal `--htf-filter` and for result metrics.

| Source | What it contains | Pre-fix result numbers? |
|---|---|---|
| PR #1180 body | Before/after table: `sma_crossover --htf-filter --mode single`, BTC/USDT 1h and ETH/USDT 4h | **Yes — the only recorded instance.** Superseded by its own "after" column; re-verified below |
| Issue #304 (M2) + PR #307 | Where the `--htf-filter` flag was introduced; comments record the implementation checklist and a `--help` smoke check | No |
| Issue #906 (backtest audit) | Checklist item D6.4: verify `--htf-filter` parity with live — a task description | No |
| Issue #1154 / #1181 | The leak report and this re-validation issue; numbers quoted there are PR #1180's | No new numbers |
| `docs/research/*` (all files) | Only `982-chart-pattern-htf-gate-m1.md` mentions HTF filtering — it explains why the chart-pattern gate deliberately does **not** reuse the DSL `HTFFilter`; its M1 validation ran through `eval_windows.py` (unaffected) | No |
| `docs/ARCHITECTURE.md`, `README.md`, `CLAUDE.md` | Flag documentation and the #1154/#1181 inflation warning | No |
| Issues #67/#103/#652/#839/#937/#956/#957/#978/#1048/#1095 (remaining search hits) | Live-side HTF filter work, strategy-internal HTF mechanisms, or incidental token matches | No (zero literal `--htf-filter` mentions in body or comments) |
| Recorded optimizer/compare outputs | None exist in the repo for `--htf-filter` runs (no committed artifacts reference the flag) | No |
| `scheduler/config.example.json` | Does not set `htf_filter` on any strategy | n/a |

**Live-config caveat:** deployment configs live out of tree
(`/var/lib/go-trader[/<instance>]/config.json`, #1056) and are not visible
from this checkout (no local `scheduler/config.json` exists). The repo records
no promotion decision for an `HTFFilter`-configured live strategy that leaned
on a `--htf-filter` backtest, but an operator with a live strategy setting
`htf_filter: true` should confirm that strategy's sizing/enable decision did
not come from a pre-`952144a` run — if it did, the corrected procedure is a
re-run at current `main` with identical arguments.

## Before/after diffs, re-measured (acceptance criterion 2)

Both directions re-run on the **identical cache snapshot**
(`shared_tools/trading_bot.db`, last modified 2026-07-01 20:58 — before the
fix merged), at `952144a` for "after" and with `run_backtest.py` from
`e002a3f` (the parent of the fix merge) for "before". Every figure in PR
#1180's table reproduces exactly:

`sma_crossover --symbol BTC/USDT --timeframe 1h --mode single --htf-filter` (HTF=4h, period 2023-01-01 → 2026-06-04):

| | Total return | Sharpe | Sortino | Max DD | Trades | Win rate |
|---|---:|---:|---:|---:|---:|---:|
| before (leaky, `e002a3f`) | +188.11% | 1.150 | 1.060 | -26.10% | 73 | 34.2% |
| **after (corrected, `952144a`)** | **+147.80%** | **0.998** | **0.932** | **-36.85%** | **75** | **32.0%** |

`sma_crossover --symbol ETH/USDT --timeframe 4h --mode single --htf-filter` (HTF=1d, period 2023-01-01 → 2026-06-04):

| | Total return | Sharpe | Sortino | Max DD | Trades | Win rate |
|---|---:|---:|---:|---:|---:|---:|
| before (leaky, `e002a3f`) | +180.51% | 0.960 | 0.894 | -42.69% | 14 | 28.6% |
| **after (corrected, `952144a`)** | **+7.94%** | **0.248** | **0.222** | **-51.79%** | **13** | **38.5%** |

The bolded rows are the canonical post-fix baselines for these two
strategy/symbol/timeframe combinations; any future comparison uses these, not
the PR's "before" column. The ETH 4h/1d collapse (+180.51% → +7.94%) confirms
the leak's severity scales with the LTF→HTF ratio's candle span.

## Dispositions (acceptance criterion 3)

| Decision surface | Disposition | Why |
|---|---|---|
| PR #1180 before-numbers | **Superseded** — corrected baselines recorded above | Only recorded pre-fix results; produced expressly to measure the leak, never consumed by a decision |
| M1–M6 research verdicts (`eval_windows.py` / `optimizer.py` / `fee_audit`) | **No action** | Neither harness wires `htf_filter` (verified by grep); no M-series baseline touched `_htf_trend_series` |
| Strategy-internal HTF mechanisms (`mtf_confluence`, `regime_adaptive_htf`, `chart_patterns` `htf_gate_factor`) | **No action** | Resample inside the strategy DataFrame (`shared_strategies/open/registry.py`); never touch `_htf_trend_series`. Their own open-time-index alignment is a separate, already-tracked concern (#1153-family invariant) |
| Live `HTFFilter` promotions | **No repo-recorded decision affected**; operator-side caveat above | No `--htf-filter` backtest evidence recorded anywhere a promotion could have consumed it |
| Strategy audit rankings (#956/#978) | **No action** | Zero `--htf-filter` mentions in either thread; audit used registry-wide harnesses that don't wire the flag |

No verdict flips. No deprecation, promotion, or capital-allocation decision
rested on a leak-inflated number.

## Reproduce

```
uv run --no-sync python backtest/run_backtest.py --strategy sma_crossover \
  --symbol BTC/USDT --timeframe 1h --mode single --htf-filter
uv run --no-sync python backtest/run_backtest.py --strategy sma_crossover \
  --symbol ETH/USDT --timeframe 4h --mode single --htf-filter
```

"Before" rows: check out `backtest/run_backtest.py` from `e002a3f` first
(`git show e002a3f:backtest/run_backtest.py`). Results depend on the OHLCV
cache state; the tables above were produced on the 2026-07-01 20:58 snapshot.

---
Created with LLM: Fable 5 | high | Harness: Claude Code
